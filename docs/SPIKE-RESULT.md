# Spike 验证报告（M0）

> 时间：2026-09-25 · 环境：macOS arm64 (Darwin 25.6.0) / Go 1.27.1 / CGO=1
> 代码：`spike/{a-connect,b-doh,c-source,d-tray}/`（可抛弃验证码，结论已回写设计文档）

## 概览

| Spike | 验证点 | 结论 |
|---|---|---|
| A | CONNECT 隧道代理 MVP | ✅ 通过；实证单 IP 无容错不可用 |
| B | DoH 多源解析 + 中位数测速 | ✅ 通过；DoH 穿透污染实证 |
| C | steampp 数据源 + CNAME 通道 | ✅ 通过；新增两类降级规则 |
| D | 托盘（energye/systray） | ✅ 通过；macOS 需 SetOnClick+ShowMenu |

## Spike A：CONNECT 隧道代理

**实现**：纯 stdlib ~160 行，监听 127.0.0.1:12800，白名单域名（github.com/api.github.com）拨固定 IP（SNI 保持原域名），其余 CONNECT 直通。

**结果**：
- `curl -x` 经代理访问 github.com → 200（TLS 握手 0.35s）
- 透传路径（baidu.com）→ 200，行为等同未开代理
- `git clone --depth 1` 经代理成功

**关键教训（实证了设计文档预判）**：
1. **单固定 IP 不可用**：同一 IP `20.207.73.82` 一次 dial 154ms、下一次直接 i/o timeout——git clone 因此失败过一次。多 IP 候选 + 容错不是优化项，是必需品
2. **拨号层 fallback 必须在回 200 之前完成**：客户端（git/curl）收到 502 不会重试 CONNECT，失败不能泄漏给客户端
3. 测试环境干扰：测试期间本机恰有 Watt Toolkit 加速运行（hosts 全劫持到 127.0.0.1 + 443 被其反代占用），第一次 git clone 的"直连兜底"实际落到了 Watt 的 443 上（借道成功）——说明测速/拨号必须剔除回环 IP，已在 Spike B 固化

## Spike B：DoH 解析 + TCP 中位数测速

**实现**：阿里(223.5.5.5)/DNSPod(120.53.53.53, doh.pub) 三端点并发查询取并集 → 每候选 IP TCP 建连 :443 × 3 次取中位数（1s 超时按 1000ms 惩罚）。

**结果（干净环境实测）**：

| 域名 | DoH 候选 | 中位建连 |
|---|---|---|
| github.com | 20.205.243.166 | 103ms ★ |
| api.github.com | 20.205.243.168 / 140.82.112.5 | 117ms / 239ms |
| codeload.github.com | 20.205.243.165 | 102ms ★ |
| raw.githubusercontent.com | 185.199.108-111.133 | 133-140ms（另一时段 110/109 曾 1s 超时） |

**关键结论**：
1. **DoH 防污染有实证价值**：测试时系统解析全部返回 127.0.0.1（hosts 被 Watt 劫持），DoH 稳定拿到真实 IP
2. **中位数测速能区分质量**：同时段候选间差异明显（133ms vs 1s 超时），且不同时段同一 IP 表现漂移——支持"周期重测 + 缓存 5min"的设计
3. **候选 IP 必须过滤**：回环/私有/链路本地一律剔除（`isUsableCandidate`，对应 GitHub520 的 DISCARD_LIST 教训）

## Spike C：steampp 数据源 + CNAME 通道

**实现**：POST `api.steampp.net/accelerator/projectgroups`（带官方格式 Referer `spp://macOS/...` + 浏览器 UA）→ 归一化 Github 分组 20 条规则 → CNAME 通道端到端验证。

**归一化规则表（实测产出）**：
- FixedIP × 10：github.com/pages/gist → 20.207.73.82；githubapp.com → 140.82.112.29；hub.docker.com → 54.208.73.48；github.io → 185.199.110.153 等
- CNAME × 7：api.github.com → githubapi.rmbgame.net；githubassets → githubdocs.rmbgame.net；education → educationgithub.rmbgame.net 等
- Dynamic × 3：resources/uploads/archiveprogram.github.com
- **新增降级类**：
  - `Relay`（Forward 为 `http://` scheme + 非标端口，如 huggingface.co → `http://nl.mossimo.top:41080/`、greasyfork → `pt.mossimo.net:41080`）——**走 steampp 官方服务器带宽的真中转，本项目不使用**，降级 Dynamic
  - `fakeSNI 非空`（如 githubusercontent.com 带 fakeSNI=Github）——纯隧道无法改写客户端 SNI，降级 Dynamic

**CNAME 通道端到端验证（本 spike 最关键数据）**：
```
api.github.com → DoH 解析 githubapi.rmbgame.net → 20.205.243.168
  → TCP + TLS(SNI=api.github.com) → 证书 CN=*.github.com（GitHub 官方证书）
  → GET /zen → 200 OK "Speak like a human."
```
SNI 保持原域名 + CNAME 通道 IP 的组合被端到端证实，且拿到的是 GitHub 官方证书（证书校验天然通过）。

**附带发现**：steampp 顶层 JSON 为 emoji 键混淆（🦓 为分组数组，🦄🐴 为数字），解析需按键值形态过滤。

## Spike D：托盘（energye/systray v1.0.3）

**结果**：CGO+clang 编译通过（单二进制 2.9MB arm64），菜单栏图标 + 菜单交互 + "退出"全部正常。

**API 细节（与 getlantern 原版不同，实现时注意）**：
1. 菜单项为回调式：`item.Click(fn)`，无 `ClickedCh` channel
2. **macOS 左键点击默认无行为**：必须 `systray.SetOnClick(func(m IMenu){ m.ShowMenu() })` 才弹菜单（第一版"有图标无菜单"的根因）
3. 图标用 template PNG（黑色+alpha），正式版需 @1x/@2x 两套 + 抗锯齿（spike 版距离场 AA 已验证可行）

## 对 DESIGN.md 的修订

1. §3.2：出站候选增加回环/私有 IP 过滤（必须项）
2. §3.3：归一化规则新增 Relay（不使用，降级 Dynamic）与 fakeSNI（降级 Dynamic）两类
3. §3.7：托盘选型定为 energye/systray v1.0.3，记录 macOS 点击菜单细节
4. §6：M0 完成
5. §7：风险表更新——托盘风险已消除（剩 Wails 共存性 M2 验证）；新增"steampp HF/greasyfork 为官方带宽中转，明确不使用"
