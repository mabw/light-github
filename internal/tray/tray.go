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

	"github.com/energye/systray"
)

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

// onReady 全部菜单读写只发生在 AppKit 主线程（onReady/Click/SetOnClick 回调），
// 单线程访问库的裸字段，既无数据竞争（原 review H5 由轮询 goroutine 引入）
// 也无可死锁的跨线程锁（原 2s ticker 持锁调 native 曾与点击回调互等，
// 造成托盘永久卡死，DEBT-9 实证）。
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

	// macOS 左键点击默认无行为，需主动弹菜单（spike-d 实证）。
	// 打开前刷新：每次点开看到的必是当下状态（替代定时轮询——
	// 外部（Web 控制台/配置热应用）改动由下一次打开纠正，无陈旧窗口）。
	systray.SetOnClick(func(menu systray.IMenu) {
		refreshMenu(deps, m)
		_ = menu.ShowMenu()
	})

	m.accel.Click(func() { toggleMenu(deps, m, func() { deps.ToggleAccel(!m.accel.Checked()) }) })
	if deps.ToggleSysProxy != nil {
		m.sys.Click(func() { toggleMenu(deps, m, func() { _ = deps.ToggleSysProxy(!m.sys.Checked()) }) })
	}
	if deps.ToggleAutostart != nil {
		m.auto.Click(func() { toggleMenu(deps, m, func() { _ = deps.ToggleAutostart(!m.auto.Checked()) }) })
	}
	open.Click(deps.OpenUI)
	quit.Click(deps.Quit)

	refreshMenu(deps, m)
}

// toggleMenu 点击回调路径：执行动作（IO 在主线程，正常毫秒级）后同步刷新。
// 网络异常时 SysProxyState 的 networksetup 查询最坏阻塞数秒——菜单迟钝
// 但不再死锁（DEBT-9 的底线：单线程内等待永远可解）。
func toggleMenu(deps Deps, m menuSet, action func()) {
	action()
	refreshMenu(deps, m)
}

// menuStates 三开关快照（IO 结果的纯数据，便于无 CGO 环境测试）。
type menuStates struct {
	accel bool
	sys   bool
	auto  bool
}

// snapshotStates 拉取三开关实际状态。SysProxyState 含 networksetup 查询
// （TTL 缓存 3s），可选回调未注入时该状态落 false。
func snapshotStates(deps Deps) menuStates {
	s := menuStates{accel: deps.AccelState()}
	if deps.SysProxyState != nil {
		s.sys = deps.SysProxyState()
	}
	if deps.AutostartState != nil {
		s.auto = deps.AutostartState()
	}
	return s
}

// refreshMenu 快照（IO）→ 应用（native）。仅在主线程调用。
func refreshMenu(deps Deps, m menuSet) {
	s := snapshotStates(deps)
	m.status.SetTitle(statusTitle(s.accel))
	if s.accel {
		m.accel.Check()
	} else {
		m.accel.Uncheck()
	}
	if s.sys {
		m.sys.Check()
	} else {
		m.sys.Uncheck()
	}
	if s.auto {
		m.auto.Check()
	} else {
		m.auto.Uncheck()
	}
}
