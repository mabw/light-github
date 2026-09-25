# light-github 设计文档

> 轻量、跨平台（macOS / Windows / Linux）的 GitHub 加速代理，从 Watt Toolkit (Steam++) 的加速能力提炼而来，仅保留单一场景所需内核。

## 1. 背景与目标

### 1.1 要解决的问题

- GitHub（及 Docker Hub、HuggingFace）在国内网络环境下访问慢 / 不可达
- Watt Toolkit 功能冗余（MITM、脚本注入、Steam 生态），且其 hosts→127.0.0.1 + 自签证书架构导致：占用 443 端口（与本地 Node dev server 冲突）、WSL 内 git/curl 完全不可用
- 需要一个托盘级常驻工具：稳定、不污染其他网络访问、免维护

### 1.2 目标

| 目标 | 手段 |
|---|---|
| 稳定 | 运行时逐 IP 测速容错（学 Watt Toolkit）+ 官方 CNAME 通道（白嫖 steampp 数据）+ 多数据源降级 |
| 不影响其他访问 | 仅处理白名单域名的 CONNECT 请求；其余域名原样直通；不修改 hosts、不设置系统全局代理（可选 PAC） |
| 不占用 443 | 监听高位端口（默认 12800，可配置） |
| 零证书负担 | 不解密 TLS——纯 TCP 隧道，端到端证书校验完好 |
| WSL 可用 | WSL 内 `export https_proxy` 指向宿主机即可（见 §7.4） |
| 轻量免维护 | Go 单二进制；数据源全部由他人维护（steampp API / GitHub520），核心能力（DoH 解析 + 测速）自有，数据源消失仍可工作 |

### 1.3 非目标（明确不做）

- ❌ MITM 解密 / 自签证书 / 脚本注入（与零证书原则冲突，且非本场景需求）
- ❌ 修改系统 hosts / 系统全局代理强制接管
- ❌ Steam 社区加速、油猴脚本、账号切换等 Watt Toolkit 其他功能
- ❌ 需要账号体系的 `ProxyType=ServerAccelerate` 官方中转（X-Watt-Token）

## 2. 架构总览

```
接入层（谁把流量送进来——全部可选、组合使用）
  git per-URL 配置 │ 浏览器 PAC/系统代理 │ 终端环境变量 │ docker daemon proxy │ WSL env
        │ 全部指向
        ▼
┌─────────────────────────────────────────────────────┐
│  light-github 单进程（Go）                            │
│                                                      │
│  ┌──────────────────┐   ┌─────────────────────────┐ │
│  │ CONNECT 隧道代理  │──▶│      出站选择器          │ │
│  │ 127.0.0.1:12800  │   │ (OutboundSelector)      │ │
│  │ 白名单域名→加速   │   │ 1. 规则策略:固定IP/CNAME │ │
│  │ 其余域名→直通     │   │ 2. DoH 解析(阿里/DNSPod)│ │
│  │ 双向字节搬运      │   │ 3. TCP-443 握手测速     │ │
│  │      │           │   │    (3次取中位,缓存5min) │ │
│  │      ▼           │   │ 4. 失败自动换IP重试     │ │
│  │ 日志/流量统计     │   └───────────┬─────────────┘ │
│  └──────────────────┘               │ 规则来源       │
│                                     ▼               │
│  ┌─────────────────────────────────────────────┐    │
│  │ 数据源层（可插拔, 1h 刷新, 本地缓存）          │    │
│  │ steampp API → GitHub520 hosts.json → 内置清单 │    │
│  └─────────────────────────────────────────────┘    │
│                                                     │
│  Web UI (127.0.0.1:12801) ◀── 同一套页面── 桌面窗口  │
│  网络诊断 │ 设置(自启/DoH/端口/域名)                 │
└─────────────────────────────────────────────────────┘
        │
        ▼
GitHub / Azure Front Door / Akamai 真实最优节点（TLS 端到端）
```

## 3. 核心模块设计

### 3.1 CONNECT 隧道代理（`internal/proxy`）

- 监听 `host:port`（默认 `127.0.0.1:12800`，可配置 `0.0.0.0` 供 WSL/局域网使用）
- 解析 HTTP 请求行：`CONNECT host:port` → 隧道；绝对 URI GET（普通 http 代理请求）→ 仅对白名单域名重定向至目标，其余 302 直连
- 白名单命中 → 走 OutboundSelector 拨号；未命中 → 直接 `net.Dial` 原目标（透传，保证"开了等于没开"）
- 隧道建立后 `io.Copy` 双向搬运，连接粒度记录：域名、选中 IP、拨号耗时、上下行字节、关闭原因
- 依赖：仅 Go stdlib（`net/http` + `net`）

### 3.2 出站选择器（`internal/selector`）

对每个加速域名，按优先级产生候选拨号目标，首个成功即用：

```go
type Candidate interface {
    // Resolve 产出候选 IP 列表（按优先级排序）
    Resolve(ctx context.Context, domain string) ([]net.IP, error)
}

// 固定 IP（来自数据源策略，如 github.com → 20.207.73.82）
type FixedIPSource struct{ IPs []net.IP }

// CNAME 通道（如 api.github.com → 查 githubapi.rmbgame.net 的 CNAME 链拿真实 IP，
// 连接时 SNI 仍为 api.github.com —— 已实测 200 OK）
type CNAMESource struct{ QueryDomain string }

// DoH 动态解析（阿里 223.5.5.5 / DNSPod 119.29.29.29 / Google 兜底，RFC8484 JSON API）
type DoHSource struct{ Endpoint string }

// 测速缓存：domain → []scoredIP{ip, median RTT, lastProbe}，TTL 5min
// 命中缓存的 IP 按历史中位耗时排序；全部失败时强制重测并逐个重试
```

**候选 IP 过滤（必须项，spike-b 实证）**：剔除回环/私有/链路本地/未指定地址——本机 hosts 可能被其他加速工具劫持（实测 Watt Toolkit 留有 域名→127.0.0.1 条目，且 127.0.0.1:443 建连 0ms 会污染排序）。

**拨号容错（spike-a 实证）**：单固定 IP 不可靠（同一 IP 一次 154ms、下次超时）；候选逐个尝试必须在回复客户端 200 之前完成——git/curl 收到 502 不会重试 CONNECT。

- 测速：TCP `net.DialTimeout` 到 `:443` 计握手耗时，3 次取中位数，单次 1s 超时（GitHub520 的中位数法）
- 容错：拨号失败 / 隧道内 30s 零字节 → 标记该 IP 失败（连续失败移出候选），下个候选重拨（dev-sidecar DynamicChoice 思想：成功率统计驱动切换）

### 3.3 数据源层（`internal/source`）

```go
type Source interface {
    Name() string
    // Fetch 拉取并归一化为规则表；实现必须无副作用、可独立失败
    Fetch(ctx context.Context) ([]Rule, error)
}

type Rule struct {
    Domain   string   // 精确域名或 *.suffix 通配
    Strategy Strategy // FixedIP | CNAME(dname) | Dynamic 之一或链式
    Origin   string   // 来源标识，用于 UI 展示与诊断
}
```

| 数据源 | 地址 | 归一化规则 |
|---|---|---|
| steampp（主） | `POST https://api.steampp.net/accelerator/projectgroups` | 取 Github 分组（含 Docker Hub 子项）：`Forward` 为 IP → FixedIP；为域名且 ≠ 自身 → CNAME 通道；= 自身 → Dynamic；**`http://` scheme（如 huggingface.co→nl.mossimo.top:41080）→ Relay 类，不使用（走官方带宽），降级 Dynamic；`FakeServerName` 非空 → 降级 Dynamic（纯隧道无法改写客户端 SNI）**（spike-c 实证） |
| GitHub520（备） | `https://raw.hellogithub.com/hosts.json`（注意：数据源服务器 2026-12-31 到期风险） | `[["ip","domain"],...]` → 全部 FixedIP |
| 内置清单（兜底） | 编译期嵌入 `assets/builtin-rules.json` | GitHub + Docker Hub + HuggingFace 核心域名的 Dynamic 规则（HuggingFace 只在此源维护） |

> 解析注意（spike-c 实证）：steampp 顶层 JSON 为 emoji 键混淆（🦓 为分组数组，🦄🐴 为数字），需按键值形态过滤后再反序列化。

- 刷新：1h 定时 + 启动时；三源独立失败互不影响；全部失败用本地缓存（`~/.light-github/rules.cache.json`）
- 请求头：携带与官方一致的 `Referer: spp://...` 格式与浏览器 UA（克制、不伪装过度）；频率 ≥1h

### 3.4 日志与流量统计（`internal/metrics`）

- 连接日志：环形缓冲（默认 1000 条）+ 可选落盘（`--log-file`）；字段：时间、域名、选中 IP、候选数、拨号耗时、上下行字节、结果
- 流量统计：进程级累计 + 滑动窗口（5s）实时速率（Watt Toolkit FlowAnalyzer 思路）
- 对外：`GET /api/stats`（JSON）与 `GET /api/logs`（分页）供 Web UI 消费

### 3.5 网络诊断（`internal/diag`）

- DoH 节点延迟对比（各端点并发测）
- 指定域名：展示当前规则来源、候选 IP 明细、各 IP 测速耗时、最近失败记录
- 直连 vs 加速对比测试（同域名两条路径各发一次 TCP 探测）

### 3.6 Web UI（`web/` + `internal/webui`）

**定位（2026-09-25 定案）**：仅控制项与观测窗口，无业务逻辑——因此不引入桌面框架，纯静态页 + REST。

- 技术栈：单文件 HTML + 原生 JS（<30KB，零构建链），1s 轮询
- 唯一入口：浏览器 `http://127.0.0.1:12801`（托盘菜单/点击 Dock 图标打开）
- API 面（约 7 端点）：`GET /api/status` `/api/stats` `/api/logs` `/api/rules` `/pac`；`POST /api/rules/refresh` `/api/config`
- 页面 tab：仪表盘 · 连接日志 · 规则与测速明细 · 接入指引 · 设置（监听地址/DoH/自启）

### 3.7 平台层（`internal/platform`）

| 能力 | macOS | Windows | Linux |
|---|---|---|---|
| 托盘/菜单栏 | `energye/systray` v1.0.3（spike-d 已验证：CGO 编译链 OK、2.9MB 单二进制；注意其 API 与 getlantern 原版不同——菜单项为回调式 `Click(fn)`，macOS 左键点击需 `SetOnClick(m.ShowMenu)` 才弹菜单；图标为 template PNG 需 @1x/@2x 抗锯齿两套） | 同左 | 同左（XDG AppIndicator） |
| 开机自启 | `SMAppService`（Login Items，via `go-hybrid/cocoa` 或 exec `osascript`） | 注册表 `HKCU\...\Run`（无 UAC） | `~/.config/autostart/*.desktop` 或 systemd user unit |
| 桌面图标/窗口 | 仅菜单栏（默认）或常驻 Dock（点击=浏览器打开 UI） | 托盘 + 快捷方式（打开 UI） | 托盘 + .desktop（打开 UI） |

- 桌面窗口 = 内嵌 WebView 加载 §3.6 页面；`--headless` 模式跳过窗口仅托盘+Web

### 3.8 接入配置助手（`internal/onboarding`）

不替用户静默改系统，只生成/展示命令（Web UI 一键复制）：

- git：`git config --global http.https://github.com.proxy http://127.0.0.1:12800`（per-URL，不影响 gitee/公司 GitLab；卸载即 `--unset`）
- 浏览器：生成 PAC 文件 URL（`http://127.0.0.1:12801/pac`，仅加速域名走代理其余 DIRECT）；或手动系统代理指引
- 终端：`export https_proxy=http://127.0.0.1:12800 http_proxy=...`（写入 shell rc 的建议命令）
- Docker：Docker Desktop proxy 设置指引 / `~/.docker/config.json` proxies 段
- HuggingFace：`export HF_ENDPOINT` 不需要（走代理即可）；提供 `https_proxy` 方案
- WSL：见 §7.4

## 4. 技术选型

| 项 | 选择 | 备选与理由 |
|---|---|---|
| 语言 | Go | 单二进制交叉编译、std 库覆盖代理所需全部能力；Rust 开发周期长 |
| 桌面框架 | **不使用**（2026-09-25 定案）：`energye/systray` + 系统浏览器打开 Web UI | 曾评估 Wails：v2 无内置托盘（与 systray 主线程共存未验证）、v3 托盘仍 alpha（macOS/Linux 多个 open bug，见 wailsapp/wails#6045 等）；Web UI 仅控制+观测无业务逻辑，binding 价值为零。3MB vs 10MB+ 二进制、零 WebView 依赖矩阵、页面 100% 复用随时可反悔加壳 |
| 依赖总量目标 | ≤10 个直接依赖 | proxy/doh 测速均 stdlib 实现；仅 systray、wails、可选 JSON 库 |
| 打包 | goreleaser | 三平台矩阵（darwin amd64/arm64、windows amd64/arm64、linux amd64/arm64） |

## 5. 关键设计决策记录

1. **CONNECT 隧道而非 hosts**：hosts 无法指定端口且全局污染；WSL 同步 Windows hosts 是 Watt Toolkit 场景下的故障源
2. **不解密 TLS**：无证书体系 = 无信任库维护、无安全审计负担、git/curl 零配置兼容
3. **数据源白嫖策略的边界**：只用"零成本资源"（公开 JSON API + 公共 CNAME DNS 记录），不碰需 token 的付费中转；拉取频率 ≥1h
4. **自有保命内核**：即使三源全失效，内置清单 + DoH + 测速仍构成完整可用链路
5. **SNI 保持原域名**：CNAME 通道仅替代 DNS 解析环节，TLS 层完全正常（已实测 api.github.com@20.205.243.168 → 200）

## 6. 里程碑

| 阶段 | 内容 | 验收 |
|---|---|---|
| **M0 spike** ✅ 2026-09-25 完成 | 结论见 docs/SPIKE-RESULT.md | 4 个风险点全部验证通过 |
| **M1 核心 CLI** | proxy + selector + source + metrics，headless 运行 | `git clone` 经代理成功且快于直连；日志/统计 API 可用 |
| **M2 Web UI** | 全部页面 + API | 浏览器完成所有管理操作 |
| **M3 平台层** | 托盘/自启/桌面窗口 | 三平台手工验证清单通过 |
| **M4 发布** | goreleaser + 安装文档 | 三平台产物 + 一键接入文档 |

## 7. 风险与对策

| 风险 | 等级 | 对策 |
|---|---|---|
| ~~Wails v2 无内置托盘~~ | 已消除 | 选型定案不使用 Wails：`energye/systray` 独立可用（spike-d 验证），UI 走浏览器（§4）；M3 的 Dock/桌面图标用系统原生机制（osascript/open 等），不引 WebView |
| steampp 的 HF/greasyfork 配置走官方中转带宽（mossimo.top） | — | 明确不使用：归一化时标记 Relay 降级 Dynamic，白嫖边界仅限公开 JSON API + CNAME DNS 记录 |
| steampp API 变更/关闭 | 中 | 可插拔三源 + 本地缓存 + 内置兜底清单 |
| GitHub520 数据源服务器 2026-12-31 到期 | 低 | 仅作备源；可切换 raw.githubusercontent.com 镜像 |
| 监听 0.0.0.0 供 WSL 时的安全面 | 中 | 默认 127.0.0.1；0.0.0.0 模式强制要求开启 API Token（UI 请求头校验） |
| Docker Hub registry 鉴权流程 | 低 | auth.docker.io token 交换为普通 HTTPS，隧道透明转发，理论无障碍（M1 验证） |
| HF 大文件 CDN 跳转 | 低 | redirect 后仍是 HTTPS GET，隧道透明（M1 验证） |
| macOS 分发签名/公证 | 中 | M4 处理；个人使用可免签名（右键打开）；Windows SmartScreen 同理 |
| Linux GUI 依赖 webkit2gtk | 低 | `--headless` 模式无 GUI 依赖；主流发行版自带 |

### 7.4 WSL 接入专项

- WSL2 NAT 模式：宿主监听 `0.0.0.0`（配合 Token），WSL 内 `export https_proxy=http://$(ip route show default | awk '{print $3}'):12800`
- WSL2 mirrored 网络模式（Win11 22H2+）：直接 `http://127.0.0.1:12800`
- Web UI 生成上述命令（自动探测两种模式）

## 8. 技术债务

见 [DEBT.md](DEBT.md)（DEBT-1 至 DEBT-6，按目标里程碑排序）。

## 9. 参考资产索引（复用来源）

| 来源 | 复用内容 | 位置 |
|---|---|---|
| SteamTools | steampp API 结构与字段语义；逐 IP 容错思路；FlowAnalyzer 滑动窗口 | `~/work/github/SteamTools/src/BD.WTTS.Client.Plugins.Accelerator*` |
| GitHub520 | TCP-443 中位数测速法；DoH 端点清单；40 域名覆盖清单（转译为内置清单，不抄代码——CC BY-NC-ND） | `~/work/github/GitHub520/fetch_ips.py` |
| dev-sidecar | DynamicChoice 成功率统计自动切换；5min 定时重测 + 按需探测 | `~/work/github/dev-sidecar/packages/mitmproxy/src/lib/{speed,choice}` |
| fetch-github-hosts | （可选 hosts 兜底模式时）跨平台 hosts 写入/提权/DNS flush 参考 | `~/work/github/fetch-github-hosts/src-tauri/src/hosts.rs` |
