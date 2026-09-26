# 技术债务清单

> 用途：避免遗忘。每条含影响与建议方案；完成后移入"已清偿"并注明提交号。
> 命名：DEBT-N；严重度：🔴 bug 级 / 🟡 功能缺陷 / 🟢 已知取舍

## 已全部清偿（2026-09-25，见下方各条）

### DEBT-1 🔴 metrics 实时速率恒为 0（RateUp/RateDown 无数据来源）

- **位置**：`internal/metrics/metrics.go` — `RecordConn()`
- **描述**：滑动窗口样本只在 `Add()` 中写入（metrics.go:65），但业务侧（proxy）只调用 `RecordConn()` 累计总量，从不调用 `Add()`。因此 `Snapshot()` 的 `RateUp`/`RateDown` 永远为 0。
- **影响**：M2 Web UI 的实时速率图表将无数据；当前 CLI 无直接表现，故 M1 验收未暴露。
- **建议**：`RecordConn()` 内同时追加窗口样本（复用 `Add` 的 append+trim 逻辑）；补一条"RecordConn 后 Snapshot 速率 > 0"的测试。
- **目标**：M2 开始前必修。

### DEBT-2 🟡 隧道无 idle timeout，半开连接泄漏 goroutine

- **位置**：`internal/proxy/proxy.go` — `tunnel()`
- **描述**：双向 `io.Copy` 无空闲超时。若上游（或客户端）长期不发包也不关闭（如 keep-alive 挂起、异常半开），该连接的两个 goroutine 永久阻塞。
- **影响**：长时间运行的常驻进程（这正是本工具的形态）goroutine 缓慢累积；M1 验收中 `TestProxy_RecordsMetrics` 曾因 httptest 上游 keep-alive 不关闭而卡住隧道，即此问题的实证。
- **建议**：为隧道实现 idle watchdog（任一方向 N 分钟无字节则双向关闭；参考 Watt Toolkit/dev-sidecar 的做法，idle 阈值 3-5 分钟可配置）；测试用慢速/挂起上游验证 goroutine 回收。
- **目标**：M2（做流量图表前需要准确的连接生命周期）。

### DEBT-3 🟡 首次请求含同步测速延迟（冷启动慢）

- **位置**：`internal/selector/selector.go` — `Pick()` 持锁测速
- **描述**：域名首次请求时在 Pick 内同步完成全部候选的 TCP 测速（每 IP 3 次×1s 超时上限）。M1 验收实测：`raw.githubusercontent.com` 首请求 8.9s（4 候选×3 轮），命中缓存后恢复正常。
- **影响**：每个新域名的第一个请求体验差；测速失败惩罚值（1s）使慢速网络下首请求可能达 10s 级。
- **建议**：启动/规则刷新后对白名单域名**后台预热**测速；Pick 首次未命中缓存时先用 DoH 原始顺序返回（不阻塞），测速异步进行、完成后更新排序（借鉴 dev-sidecar"边用边探"）。
- **目标**：M2。

### DEBT-4 🟢 selector 测速期间持全局锁（同域名串行、跨域名互斥）

- **位置**：`internal/selector/selector.go` — `Pick()` 的 `s.mu.Lock()`
- **描述**：Pick 全程持锁（含 buildCandidates 的 DoH 解析与测速网络 IO）。域名 A 测速期间，域名 B 的 Pick 也会排队。
- **影响**：单用户桌面场景并发量极小（个位数域名），当前无可感知影响；但违反锁内不做 IO 的原则。
- **建议**：按域名分片锁（`map[domain]*sync.Mutex` 或 singleflight），缓存读写与测速分离。仅在出现并发需求（如 M2 引入批量预热）时处理。
- **目标**：按需（M2 预热落地时一并评估）。

### DEBT-5 🟢 拨号使用 context.Background()，连接取消不传播

- **位置**：`internal/proxy/proxy.go` — `handleConnect()` 中 `s.Dialer.Pick(context.Background(), ...)`
- **描述**：逐候选拨号未关联连接生命周期 ctx；客户端断开时当前候选仍会拨完（单候选有 DialTimeout=5s 兜底）。
- **影响**：最坏浪费一个 DialTimeout 窗口的资源，无正确性问题。
- **建议**：handleConn 入口为每个连接建 `ctx, cancel := context.WithCancel(...)`，客户端读端关闭时触发 cancel，拨号与测速链路传播该 ctx。
- **目标**：M2/M3 顺手处理。

### DEBT-6 🟢 0.0.0.0 监听（WSL 场景）无访问控制

- **位置**：`cmd/light-github/main.go`（`-addr` 已支持 0.0.0.0）；设计承诺见 `docs/DESIGN.md` §7
- **描述**：设计要求"0.0.0.0 模式强制开启 API Token"，尚未实现。当前 `-addr 0.0.0.0` 会将未认证代理暴露给局域网。
- **建议**：实现监听地址非 loopback 时强制 Token（配置项 + 401 拒绝无凭据请求）；Web UI 显示安全状态。
- **目标**：M3（多平台分发前必须）。

## 清偿记录

| 债务 | 提交 | 验证 |
|---|---|---|
| DEBT-1 速率恒0 | fix: DEBT-1 RecordConn 喂入速率窗口 | 新增用例 TestStore_RecordConnFeedsRateWindow，metrics 98% |
| DEBT-2 隧道泄漏 | fix: DEBT-2/5 隧道空闲watchdog双向关闭+连接级ctx传播 | TestProxy_IdleTimeoutClosesTunnel（挂起上游 150ms 关闭），proxy 91% |
| DEBT-5 ctx 不传播 | 同上 | 拨号改 DialContext + 候选间检查取消 |
| DEBT-3 冷启动慢 | fix: DEBT-3/4 selector 分片锁+Preload 后台预热 | TestSelector_PreloadFillsCache + cmd 启动预热接入 |
| DEBT-4 全局锁 | 同上 | TestPick_DifferentDomainsDoNotBlockEachOther（真阻塞探针：fast 31ms vs slow 300ms），selector 93.7% |
| DEBT-6 0.0.0.0 无鉴权 | fix: DEBT-6 cmd 强制校验 + proxy 407 | TestValidateListen 6 用例 + TestProxy_TokenAuth + 实测拒绝启动 |

---

## 记录约定

- 发现新债务：加条目，标注发现场景（测试/验收/评审），不立即修的写明原因
- 清偿债务：移入"已清偿"，注明 commit 与验证方式；优先级按目标里程碑排列
- 与 `docs/DESIGN.md` §7 风险表区分：那边是"外部风险与对策"，这边是"代码内部欠账"

## 待清偿

### DEBT-9 🔴 托盘 UI 死锁：点击「系统代理」后菜单永久无响应（macOS 实发，2026-09-26）

- **位置**：`internal/tray/tray.go`（syncMenu ticker / toggleUnderLock）与 energye/systray darwin 原生层交互
- **复现**（用户真机，v0.3.0 部署版）：09:31:01-02 连续开关「加速」正常（有日志），随后点击「系统代理」——**无任何日志**（setSysProxy 未被调用，连失败路径的 Warn 都没有），此后图标点击永久无响应。
- **证据链**：
  - 进程存活，HTTP 服务正常（/pac 返回 200），Go 层信号处理正常；
  - SIGTERM 后退出清理**完整执行**（PAC 还原、会话统计落盘），但进程不退出（systray.Quit → NSApp terminate 未生效）→ 卡点在 AppKit 主线程/事件循环，而非 Go 逻辑；
  - networksetup 手测 0.019s 正常，排除系统命令挂起。
- **初判**：跨语言死锁——`syncMenu` 2s ticker 持 `menuMu` 调 native（SetTitle/SetChecked 需在 AppKit 主线程执行），与 AppKit 弹出菜单（ShowMenu 阻塞主线程模态循环）、native 点击回调进 Go（toggleUnderLock 抢同一把锁）构成三方循环等待。menuMu（review H5）只防了 Go 层并发，未防 native 重入。
- **修复方向**（待网络恢复后实施，v0.4.0 tag 前必须）：
  1. ticker syncMenu 改 TryLock，拿不到即跳过本轮（打断「持锁等 native」一环）；
  2. native 调用是否可重入/是否必须主线程，读 energye/systray 的 systray_darwin.m 确认；
  3. 回归用例：模拟「菜单打开期间 ticker 触发」的并发路径。
- **临时规避**：进程卡死后 kill -TERM 可完整还原系统代理（退出清理先于 Quit 执行的设计救了场）；kill 不掉时 kill -9 后 PAC 已是还原态（清理在 Quit 前完成）。

### DEBT-7 🟢 sysproxy 的 Windows/Linux 分支未经真机验证

- **位置**：`internal/sysproxy/sysproxy_windows.go`、`sysproxy_linux.go`
- **描述**：macOS 分支已完整验证（接入/还原/退出闭环）；Windows 用 `reg add/delete AutoConfigURL`（WinINET 可能需 InternetSetOption 广播才即时生效），Linux 仅支持 GNOME gsettings（KDE 无统一接口）。
- **进展（2026-09-26）**：linux 分支命令行为已有单元测试覆盖（`sysproxy_linux_test.go`，gsettings 断言），并在 linux 容器与本机双环境通过；仍缺真机 GNOME 桌面的端到端验证。
- **真机验证清单**（用 release 产物逐项过）：
  - [ ] GNOME：托盘/控制台开启系统代理 → `gsettings get org.gnome.system.proxy mode` 为 `'auto'`，浏览器走 PAC
  - [ ] GNOME：退出 → mode 还原 `'none'`
  - [ ] KDE：开启应得到「仅支持 GNOME」报错而非假成功
  - [ ] Windows：开启 → `reg query "HKCU\Software\Microsoft\Windows\CurrentVersion\Internet Settings" /v AutoConfigURL` 有值，**浏览器是否即时生效**（不即时 = WinINET 未广播，需改 syscall InternetSetOption）
  - [ ] Windows：退出 → AutoConfigURL 删除且浏览器恢复直连
- **影响**：M4 三平台分发前必须真机过一遍；WinINET 即时生效问题可能需要改用 syscall（InternetSetOption）。
- **目标**：M4。

### DEBT-8 🟢 tray/autostart 的 Windows/Linux 分支未经真机验证

- **位置**：`internal/tray/tray.go`（systray 交互）、`internal/autostart/autostart_windows.go`、`autostart_linux.go`
- **描述**：macOS 全项实测（图标/菜单/开关闭环/退出清理）。未验证部分：① Windows/Linux 的 systray 菜单渲染与回调（XDG AppIndicator 依赖桌面环境）；② **非 darwin 平台的 `systray.Quit()` 语义**——darwin 是进程终止，Win/Linux 上 `systray.Run` 可能正常返回、`shutdown()` 由 main 底部兜底执行（`sync.Once` 已防重，逻辑上闭环，未实测）；③ HKCU Run / XDG autostart 写入效果。
- **进展（2026-09-26）**：linux autostart 的 XDG 条目写入/删除/幂等已有单元测试（`autostart_linux_test.go`，linux 容器通过）；托盘与注册表项仍需真机。
- **真机验证清单**（用 release 产物逐项过）：
  - [ ] Windows：托盘图标渲染、三开关勾选状态、打开控制台
  - [ ] Windows：托盘「退出」→ 进程结束且系统代理已还原（验证 Quit 语义闭环）
  - [ ] Windows：勾选开机自启 → 注册表 Run 键出现；重启登录后自动拉起
  - [ ] GNOME：AppIndicator 扩展下托盘菜单可用（无扩展时图标不显示属预期，`-no-tray` 兜底）
  - [ ] Linux：勾选开机自启 → `~/.config/autostart/light-github.desktop` 生成；注销重登后拉起
  - [ ] Linux：托盘「退出」→ 进程结束、PAC 还原（Quit 语义闭环）
- **影响**：M4 三平台分发前真机过一遍；若 Win/Linux 上 Quit 后 Run 不返回，需在 `tray.Run` 返回路径上加超时兜底。
- **目标**：M4。

### 设计决策记录：加速域名拨号全败时不直连兜底（review M8，2026-09-26 定案）

- **位置**：`internal/proxy/proxy.go` handleConnect 的 accel 分支
- **决策**：白名单域名在「DoH 全端点失败且无缓存候选」或「全部候选拨号失败」时返回 502，**不回落直连**。
- **理由**：① 该域名之所以进白名单，正因直连路径通常已被污染/劣化——回退直连大概率把污染 IP 交给 git/curl，故障从「快速失败」变成「静默错误数据」，更难排障；② 502 语义诚实（「加速路径不可用」），连接日志记录真实失败原因（批1 已修 dialErr 透传）；③ 防污染是本工具的核心承诺。
- **代价**：DoH 全挂 + 无缓存的极端冷启动下，白名单域名暂时不可用（直连本可一试）。可接受的缓解已在：候选缓存 TTL 5min + failure 记忆跨刷新保留 + 内置兜底清单不依赖 DoH 端点存活。
- **不做**：不加「测速失败的候选才允许直连」的混合策略（复杂度高、收益场景极窄）。

