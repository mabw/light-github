# 安装指南

> 面向 [Releases](https://github.com/marvin/light-github/releases) 产物的最终用户。开发者构建见 [README](../README.md)。

## 下载哪个包

| 文件 | 平台 |
|---|---|
| `light-github_darwin_arm64.tar.gz` | macOS（Apple Silicon：M1/M2/M3/M4） |
| `light-github_darwin_amd64.tar.gz` | macOS（Intel） |
| `light-github_windows_amd64.zip` | Windows 10/11 |
| `light-github_linux_amd64.tar.gz` | Linux（x86-64，纯静态无依赖） |

不确定架构：macOS 看「关于本机 → 芯片」（Apple silicon / Intel）；Linux 执行 `uname -m`（x86_64 对应 amd64）。

## macOS

```bash
tar xzf light-github_darwin_*.tar.gz
# 未签名/未公证的二进制，首次运行需去除隔离属性（只需一次）：
xattr -d com.apple.quarantine ./light-github
./light-github
```

菜单栏出现 octocat 图标即运行成功。控制台：<http://127.0.0.1:12800>。

> 若不执行 `xattr`，也可在 Finder 右键二进制 →「打开」绕过 Gatekeeper。

## Windows

解压 zip 后在 PowerShell / 终端运行 `light-github.exe`。首次运行 SmartScreen 可能拦截：「更多信息 → 仍要运行」。托盘图标出现在任务栏托盘区。

## Linux

```bash
tar xzf light-github_linux_amd64.tar.gz && chmod +x light-github && ./light-github
```

托盘需要桌面环境支持 AppIndicator（GNOME 需装 AppIndicator 扩展；KDE/XFCE 原生支持）。无桌面环境用 `-no-tray` 运行。
系统代理一键接入仅支持 GNOME（gsettings）；KDE 等请在桌面设置中手动配置 PAC `http://127.0.0.1:12800/pac`。

## 接入方式（按需选用）

```bash
# git（仅 GitHub 生效，不影响 gitee/公司 GitLab）
git config --global http.https://github.com.proxy http://127.0.0.1:12800

# 终端临时
export https_proxy=http://127.0.0.1:12800 http_proxy=http://127.0.0.1:12800

# 系统全局（托盘勾选「系统代理」，或控制台设置页开启「启动时自动接入」）
# PAC 地址：http://127.0.0.1:12800/pac —— 白名单走代理，其余直连

# 快速验证
curl -x http://127.0.0.1:12800 -sI https://github.com | head -1
```

## 开机自启

托盘菜单勾选「开机自启」即可（写入各平台标准位置，下次登录生效）：

| 平台 | 位置 |
|---|---|
| macOS | `~/Library/LaunchAgents/com.marvin.light-github.plist` |
| Windows | 注册表 `HKCU\...\Run` |
| Linux | `~/.config/autostart/light-github.desktop` |

取消勾选即删除，无残留。

## 注意事项

- **单实例运行**：同时运行两个实例会互相抢占系统代理（PAC）——后接入者生效，先前的实例退出时会把 PAC 关掉。本工具设计为单用户单实例常驻。
- **与其他代理软件互斥**：Clash/v2ray 等的「系统代理」与本工具的 PAC 同属一项系统设置，后设置者生效。
- **退出即还原**：托盘退出、SIGTERM、SIGINT 均会自动关闭系统代理 PAC，不留悬空配置。
- **数据与配置**：全部在 `~/.light-github/`（config.json、缓存、日志），卸载删除该目录即净。

## 卸载

托盘退出 → 删除二进制 →（可选）删除 `~/.light-github/`。无系统级残留。
