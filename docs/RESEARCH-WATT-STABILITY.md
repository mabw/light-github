# 调研：Watt Toolkit (Steam++) 的稳定性机制

> 2026-09-26 · 背景：v0.4.0 发布当天两次环境级 GitHub 阻断（官方 IP 段 TCP 全断、
> 社区 IP 可用）暴露 light-github 选路稳定性与 Watt Toolkit 的差距，对其源码
> （同机 SteamTools 仓库）做针对性调研。本文是 M5 的决策依据。

## 1. 核心架构发现（推翻两个常见推测）

- **hosts 不写真实 IP，全部指向本机回环**（`ProxyService.Operate.cs:247-265`）：
  流量进本地 443 的 Kestrel+YARP 反向代理，**每次建连时**实时解析上游再拨号。
  它不维护「IP 池轮换」，稳定性 = **每连接实时选路 + 连接池生命周期回收**。
- **无失败摘除/重试状态机**：全仓库无 retry/backoff/失败计数摘除；
  `DomainResolver.TestSpeedAsync` 直接 `NotImplementedException`。
  等价效果由「建连时验证 + 池回收」两机制自然获得。
- 反代核心移植自 **FastGithub 2.1.4**（文件头标注）；隧道层依赖 YARP/Kestrel
  成熟基础设施，相当部分稳定性来自「不手写」。

## 2. 六维度机制清单（代码级证据）

### 2.1 节点验证

| 机制 | 位置 | 要点 |
|---|---|---|
| UI 手动连通性测试测到 HTTP 完整响应层 | `NetworkTestService.cs:55-74` | `HttpCompletionOption.ResponseContentRead` 全量拉取计时；>20s 记超时；纯手动、无周期重测 |
| 运行时验证 = 建连时刻 TCP+TLS 握手 | `ReverseProxyHttpClientHandler.cs:268-317` | 10s 预算/IP，握手失败立即换下一候选——「测速即拨号、拨号即验证」 |
| 候选顺序 | 同上 `:344-370` | ①固定 IP → ②CNAME 通道（ForwardDestination）解析 → ③域名自身解析 |

### 2.2 拨号策略

| 机制 | 位置 | 要点 |
|---|---|---|
| 多候选**串行**尝试（非 Happy Eyeballs） | `ReverseProxyHttpClientHandler.cs:265-289`、`TcpReverseProxyHandler.cs:42-64` | 逐个试成功即返回；全败抛 `AggregateException` 聚合每个 IP 的失败原因 |
| 单候选超时 10s | 同上 `:11` | 它候选少（2-3 个），期望「正常一次成」 |
| DNS 层并发竞速 | `ProxyService.Operate.cs:512-548` | 多 DNS/DoH 服务器 `Task.WhenAny` 取首个成功，3s 探测超时，专用测速域 `dnscheck-test.steampp.net` |

### 2.3 hosts / IP 管理

- hosts 启动时全量写 `localhost`，停止按 tag 清理，**清理失败阻止停止代理**
  （防 hosts 残留导致断网，`ProxyService.Operate.cs:344-364`）；进程退出兜底恢复。
- 上游真实 IP 来自服务端下发的加速配置（`IPAddress`/`ForwardDestination`/`Timeout`/
  `TlsSniPattern` 字段），**应用初始化时拉一次，无定时刷新**；失败用本地
  MessagePack 缓存兜底（`ProxyService.cs:424-483`）。
- DNS 拦截模式（WinDivert 劫持 53 应答）伪造 TTL 固定 5min（`DnsInterceptor.cs:20`）。

### 2.4 DNS

| 机制 | 位置 | 要点 |
|---|---|---|
| DoH（RFC8484），默认 doh.pub | `DnsDohAnalysisService.cs:27-44` | 常量表全套（阿里/DNSPod/Google/Cloudflare/360/TUNA） |
| DoH 缓存 TTL **9.9min** | 同上 `:97` | 避开整 10min 防缓存同时过期的解析风暴 |
| 解析结果污染校验 | `HttpReverseProxyMiddleware.cs:100-111` | 空值/回环 IP → 500 + 「可能被 DNS 污染」提示 |
| hosts 模式强制绕开系统 DNS | `DomainResolver.cs:21-45` | 否则解析到本机拦截器无限循环（我们同款防回环） |
| IPv6 能力探测 | `DnsAnalysisServiceImpl.cs:114-142` | AAAA 探测失败保守降级 IPv4 |

### 2.5 连接生命周期

| 机制 | 位置 | 要点 |
|---|---|---|
| **Handler/连接池生命周期回收（全项目最有特色）** | `ReverseProxyHttpClientFactory.cs:16-21`、`LifetimeHttpHandler.cs:15-30`、`LifetimeHttpHandlerCleaner.cs:25-127` | 每域名 Handler **首次 10s、之后每 100s** 强制退役→新池重新解析选路；旧池弱引用延迟清理，在途请求不断连。任何「已建连但变坏」的 IP 上界 100s 自然淘汰 |
| HTTP/2、HTTP/3 多连接 | `ReverseProxyHttpClientHandler.cs:54-67` | `EnableMultipleHttp2/3Connections` |
| 隧道双向拷贝任一方向结束即整体结束 | `TcpReverseProxyHandler.cs:27-34` | 无心跳、无 TCP keepalive（依赖池回收） |
| 每请求整体超时（可选） | 同上 `:41-47` | 服务端下发的 per-domain Timeout |
| 入站不限流 | `KestrelServerOptionsExtensions.cs:13-18` | 移除请求体大小/速率限制防大文件被掐 |

### 2.6 其他

| 机制 | 位置 | 要点 |
|---|---|---|
| SNI 随机化伪装 | `ReverseProxyHttpClientHandler.cs:34,309` | 防 SNI 阻断/QoS 识别 |
| 后台服务异常不杀宿主 | `YarpReverseProxyServiceImpl.Startup.cs:13-18` | `BackgroundServiceExceptionBehavior.Ignore` |
| 根证书到期热重启 | `YarpReverseProxyServiceImpl.cs:43-88` | 到期前重生成证书并重启代理 |
| 启动失败 650ms 内感知 | 同上 `:212-233` | `app.Run()` 与定时器 `Task.WaitAny` |
| 错误分类：客户端主动断降级 Warning | `RequestLoggingMiddleware.cs:44-100` | 防客户端行为污染错误指标 |
| 失败原因逐 IP 聚合上报 | `ReverseProxyHttpClientHandler.cs:276-288` | 超时转译 + `ForwarderErrorFeature` 带进响应 |

## 3. 借鉴决策

### 采纳（M5 任务来源）

| # | 机制 | 我们的落地方式 | 替代的复杂方案 |
|---|---|---|---|
| 1 | TLS 级验证（§2.1） | MedianProber 测速增加 `tls.Client` 握手验证（SNI=域名），握手失败即弃 | 排掉「TCP 通 TLS 被 RST」的假好候选——两次断网的主因 |
| 2 | 生命周期回收（§2.5） | 每域名「选路结果」绑定出生时间：首 10s/后续 100s 失效重选 | 取代失败摘除状态机；坏 IP 影响上界 5min → 100s |
| 3 | 后台异常不杀宿主（§2.6） | 预热/刷新/测速等后台 goroutine 加 panic recover + 日志 | 任意协程 panic 不再终止整个代理 |
| 4 | DoH 缓存 + TTL 抖动（§2.4） | DoH 结果缓存 ~10min 并加 ±10% 抖动 | 防每次现查的延迟与解析风暴 |
| 5 | 拨号候选快超时/并行（自有判断，它没有） | 首候选超时 2-3s 或并行赛跑 | 它候选仅 2-3 个可串行；我们候选多（三源合并），串行 5s×N 最坏 150s |

### 不采纳（含理由，防止后人误抄）

| 机制 | 不采纳理由 |
|---|---|
| DNS `Task.WhenAny` 竞速取首个成功 | 竞速下被污染端点的假 IP 会抢跑；我们的「并发查 + 信任序采纳」（M3 review 修复）是防污染取向，更优 |
| SNI 随机化伪装 | 端到端隧道架构下 TLS 由客户端握手，SNI 不可触碰（零证书设计的代价与洁净） |
| 装根证书 + 到期热重启 | 我们不解密 TLS，无证书体系（设计优势，非缺失） |
| hosts 写回环 | 我们走 PAC/显式代理入口，无需改 hosts |

### 已领先项（无需对齐）

- TCP 隧道 idle watchdog 双向关闭（DEBT-2）——它无任何 keepalive；
- 三层数据源 + 1h 热刷新 + 本地缓存——它初始化拉一次无刷新；
- 零侵入退出还原（PAC）——它 hosts 清理失败会阻止停止代理。

## 4. 对 M5 的结论

两次断网的实测（官方段 TCP 全断时社区 IP TLS 级可用、`api.github.com` 拨号 150s
才轮到好候选）与本次调研相互印证：差距不在数据源（同源），在**选路验证深度与
坏 IP 淘汰速度**。M5 按 §3 采纳清单 1→5 实施，前三条覆盖主要差距。
