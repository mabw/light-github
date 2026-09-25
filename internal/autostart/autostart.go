// Package autostart 开机自启三平台实现（DESIGN §3.7）。
//
// 语义：Enable/Disable 仅写/删注册文件，变更在下次登录生效，不立即启动
// 新进程（避免与已运行实例双开抢端口）；Enabled 读注册文件判断。
// macOS：~/Library/LaunchAgents LaunchAgent（登录时由 launchd 拉起）
// Windows：HKCU Run 注册表（无 UAC）
// Linux：XDG autostart .desktop
package autostart
