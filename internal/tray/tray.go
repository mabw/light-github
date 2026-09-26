// Package tray 托盘常驻（DESIGN §3.7）：菜单栏图标 + 状态总闸。
//
// 与 systray 库的隔离层——动作全部经 Deps 回调注入（cmd 组装），
// 本包只负责菜单结构、状态展示与事件分发；CGO 交互部分不做单测，
// 由 macOS 真机验收覆盖（M3 验收清单）。
package tray

import (
	"context"
	_ "embed"
	"fmt"
	"sync"
	"time"

	"github.com/energye/systray"
)

// menuMu 保护菜单项读写：systray 库的 SetTitle/Check/Checked 是裸字段
// （已核实 v1.0.3 无锁），ticker goroutine 与 Cocoa 主线程 click 回调
// 并发触达会构成数据竞争（review H5）。
var menuMu sync.Mutex

// template 图标：GitHub Octicons mark-github（黑色 + alpha，256px 矢量直渲
// 后 BOX 面积采样降至 32px）。macOS 经 SetTemplateIcon 自动适配浅/深菜单栏。
//
//go:embed assets/icon32.png
var icon32 []byte

// Deps 托盘动作（cmd 注入；Toggle* 返回 error 时仅刷新状态不弹窗——
// 失败详情由 cmd 侧写运行日志，控制台可见）。
type Deps struct {
	Version string
	UIURL   string // 控制台地址（tooltip 展示）

	OpenUI func() // 浏览器打开控制台
	Quit   func() // 触发服务退出（cancel ctx；tray 感知 ctx done 后自行收尾）

	ToggleAccel     func(on bool) // 加速开关（复用 /api/accel 同一逻辑）
	AccelState      func() bool
	ToggleSysProxy  func(on bool) error // 系统代理 PAC 接入/还原
	SysProxyState   func() bool
	ToggleAutostart func(on bool) error // 开机自启
	AutostartState  func() bool
}

// validate 必填项检查（可选三对 Toggle/State 中加速开关必填——它是产品核心）。
func (d Deps) validate() error {
	if d.UIURL == "" {
		return fmt.Errorf("tray: UIURL 为空")
	}
	if d.OpenUI == nil {
		return fmt.Errorf("tray: OpenUI 未注入")
	}
	if d.Quit == nil {
		return fmt.Errorf("tray: Quit 未注入")
	}
	if d.ToggleAccel == nil || d.AccelState == nil {
		return fmt.Errorf("tray: Accel 开关未注入")
	}
	return nil
}

func statusTitle(accelerating bool) string {
	if accelerating {
		return "● 加速中"
	}
	return "○ 已直通"
}

func tooltip(version string) string {
	return fmt.Sprintf("light-github v%s — GitHub 加速", version)
}

const (
	menuAccel = "加速"
	menuSys   = "系统代理（PAC）"
	menuAuto  = "开机自启"
	menuOpen  = "打开控制台…"
	menuQuit  = "退出"
)

type menuSet struct {
	status *systray.MenuItem
	accel  *systray.MenuItem
	sys    *systray.MenuItem
	auto   *systray.MenuItem
}

// Run 阻塞主线程直到托盘退出（macOS 要求 Cocoa 主线程）。
//
// 关键契约（darwin 实证 2026-09-26）：systray.Quit() 即 [NSApp terminate:]，
// 进程随即终止且不再返回 Go——因此 cleanup 必须在 Quit 之前同步执行完毕。
// 无论退出来自托盘菜单还是 OS 信号（ctx done），顺序恒为 cleanup → Quit。
func Run(ctx context.Context, deps Deps, cleanup func()) error {
	if err := deps.validate(); err != nil {
		return err
	}
	go func() {
		<-ctx.Done()
		if cleanup != nil {
			cleanup() // 服务收尾（还原系统代理/会话统计/关监听）
		}
		systray.Quit() // darwin：进程在此终止，下方 return 不可达
	}()
	systray.Run(func() { onReady(deps) }, nil)
	return nil
}

func onReady(deps Deps) {
	// 库 darwin 实现只用第一参数（单 PNG + setSize 16pt）：传 32px 源，
	// retina @2x 原生像素匹配零缩放（16px 源会被系统上采样→糊/毛刺，实证）
	systray.SetTemplateIcon(icon32, icon32)
	systray.SetTooltip(tooltip(deps.Version))

	m := menuSet{
		status: systray.AddMenuItem(statusTitle(true), ""),
		accel:  systray.AddMenuItemCheckbox(menuAccel, "关闭后白名单也直通（端口与观测保持）", true),
		sys:    systray.AddMenuItemCheckbox(menuSys, "与其他代理软件的系统代理互斥", false),
		auto:   systray.AddMenuItemCheckbox(menuAuto, "登录时自动启动", false),
	}
	m.status.Disable()
	systray.AddSeparator()
	open := systray.AddMenuItem(menuOpen, deps.UIURL)
	systray.AddSeparator()
	quit := systray.AddMenuItem(menuQuit, "还原系统代理并退出")

	// macOS 左键点击默认无行为，需主动弹菜单（spike-d 实证）
	systray.SetOnClick(func(menu systray.IMenu) { _ = menu.ShowMenu() })

	m.accel.Click(func() { toggleUnderLock(deps, m, func() { deps.ToggleAccel(!m.accel.Checked()) }) })
	if deps.ToggleSysProxy != nil {
		m.sys.Click(func() { toggleUnderLock(deps, m, func() { _ = deps.ToggleSysProxy(!m.sys.Checked()) }) })
	}
	if deps.ToggleAutostart != nil {
		m.auto.Click(func() { toggleUnderLock(deps, m, func() { _ = deps.ToggleAutostart(!m.auto.Checked()) }) })
	}
	open.Click(deps.OpenUI)
	quit.Click(deps.Quit)

	syncMenu(deps, m)
	go func() {
		t := time.NewTicker(2 * time.Second)
		defer t.Stop()
		for range t.C {
			syncMenu(deps, m)
		}
	}()
}

// toggleUnderLock 读 Checked → 执行动作 → 刷新菜单，全程持锁：
// 与 ticker 的 syncMenu 互斥，消除库裸字段上的并发读写（review H5）。
func toggleUnderLock(deps Deps, m menuSet, action func()) {
	menuMu.Lock()
	defer menuMu.Unlock()
	action()
	syncMenuLocked(deps, m)
}

// syncMenu 从回调拉取实际状态刷到菜单（回调失败/外部变更后被下一轮纠正）。
func syncMenu(deps Deps, m menuSet) {
	menuMu.Lock()
	defer menuMu.Unlock()
	syncMenuLocked(deps, m)
}

// syncMenuLocked 需持 menuMu 调用（读状态回调并写菜单项字段）。
func syncMenuLocked(deps Deps, m menuSet) {
	m.status.SetTitle(statusTitle(deps.AccelState()))
	if deps.AccelState() {
		m.accel.Check()
	} else {
		m.accel.Uncheck()
	}
	if deps.SysProxyState != nil && deps.SysProxyState() {
		m.sys.Check()
	} else {
		m.sys.Uncheck()
	}
	if deps.AutostartState != nil && deps.AutostartState() {
		m.auto.Check()
	} else {
		m.auto.Uncheck()
	}
}
