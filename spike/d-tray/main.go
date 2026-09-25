// Spike D：托盘库选型验证（energye/systray，CGO）
//
// 验证目标（DESIGN.md §3.7 平台层 / §7 风险表第 1 条）：
//  1. energye/systray 在 macOS（arm64 + CGO/clang）编译通过
//  2. 菜单栏显示图标 + 菜单可交互（打开管理界面 / 退出）
//  3. 与 Wails v2 的共存性留待 M2 处理（本 spike 仅验证独立形态）
//
// 验收：菜单栏出现图标，菜单项可点击，退出菜单能结束进程。
package main

import (
	_ "embed"
	"log"
	"os/exec"
	"runtime"

	"github.com/energye/systray"
)

//go:embed icon.png
var iconBytes []byte // macOS 菜单栏 template 图标（16x16 黑色透明）

const uiURL = "http://127.0.0.1:12801"

func main() {
	log.Printf("[spike-d] systray starting on %s", runtime.GOOS)
	systray.Run(onReady, onExit)
}

func onReady() {
	systray.SetIcon(iconBytes)
	systray.SetTitle("LG")
	systray.SetTooltip("light-github spike-d")
	systray.SetTemplateIcon(iconBytes, iconBytes)

	mStatus := systray.AddMenuItem("light-github（spike）", "")
	mStatus.Disable()
	systray.AddSeparator()
	mOpen := systray.AddMenuItem("打开管理界面", "在浏览器打开 Web UI")
	systray.AddSeparator()
	mQuit := systray.AddMenuItem("退出", "退出 spike-d")

	// energye v1.0.3 为回调式 API（无 ClickedCh channel）
	mOpen.Click(func() { openBrowser(uiURL) })
	mQuit.Click(func() { systray.Quit() })

	// macOS 上左键点击默认无行为，需在回调中主动弹出菜单
	systray.SetOnClick(func(menu systray.IMenu) {
		if err := menu.ShowMenu(); err != nil {
			log.Printf("show menu: %v", err)
		}
	})
	log.Printf("[spike-d] tray ready")
}

func onExit() {
	log.Printf("[spike-d] tray exited")
}

// openBrowser 跨平台打开浏览器（M1 的接入助手同款逻辑）
func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	if err := cmd.Start(); err != nil {
		log.Printf("open browser: %v", err)
	}
}
