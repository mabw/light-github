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
	"time"

	"github.com/energye/systray"
)

// template 图标：GitHub Octicons mark-github（黑色 + alpha），
// macOS 经 SetTemplateIcon 自动适配浅/深菜单栏。@1x/@2x 两份。
//
//go:embed assets/icon16.png
var icon16 []byte

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
// ctx 取消（信号/服务侧退出）时自动结束托盘循环。
func Run(ctx context.Context, deps Deps) error {
	if err := deps.validate(); err != nil {
		return err
	}
	go func() {
		<-ctx.Done()
		systray.Quit()
	}()
	systray.Run(func() { onReady(deps) }, nil)
	return nil
}

func onReady(deps Deps) {
	systray.SetTemplateIcon(icon16, icon32) // Windows/Linux 分支同 API，等效 SetIcon
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

	m.accel.Click(func() { deps.ToggleAccel(!m.accel.Checked()); syncMenu(deps, m) })
	if deps.ToggleSysProxy != nil {
		m.sys.Click(func() { _ = deps.ToggleSysProxy(!m.sys.Checked()); syncMenu(deps, m) })
	}
	if deps.ToggleAutostart != nil {
		m.auto.Click(func() { _ = deps.ToggleAutostart(!m.auto.Checked()); syncMenu(deps, m) })
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

// syncMenu 从回调拉取实际状态刷到菜单（回调失败/外部变更后被下一轮纠正）。
func syncMenu(deps Deps, m menuSet) {
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
