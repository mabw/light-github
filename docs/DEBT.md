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

### DEBT-7 🟢 sysproxy 的 Windows/Linux 分支未经真机验证

- **位置**：`internal/sysproxy/sysproxy_windows.go`、`sysproxy_linux.go`
- **描述**：macOS 分支已完整验证（接入/还原/退出闭环）；Windows 用 `reg add/delete AutoConfigURL`（WinINET 可能需 InternetSetOption 广播才即时生效），Linux 仅支持 GNOME gsettings（KDE 无统一接口）。
- **影响**：M4 三平台分发前必须真机过一遍；WinINET 即时生效问题可能需要改用 syscall（InternetSetOption）。
- **目标**：M4。

### DEBT-8 🟢 tray/autostart 的 Windows/Linux 分支未经真机验证

- **位置**：`internal/tray/tray.go`（systray 交互）、`internal/autostart/autostart_windows.go`、`autostart_linux.go`
- **描述**：macOS 全项实测（图标/菜单/开关闭环/退出清理）。未验证部分：① Windows/Linux 的 systray 菜单渲染与回调（XDG AppIndicator 依赖桌面环境）；② **非 darwin 平台的 `systray.Quit()` 语义**——darwin 是进程终止，Win/Linux 上 `systray.Run` 可能正常返回、`shutdown()` 由 main 底部兜底执行（`sync.Once` 已防重，逻辑上闭环，未实测）；③ HKCU Run / XDG autostart 写入效果。
- **影响**：M4 三平台分发前真机过一遍；若 Win/Linux 上 Quit 后 Run 不返回，需在 `tray.Run` 返回路径上加超时兜底。
- **目标**：M4。

