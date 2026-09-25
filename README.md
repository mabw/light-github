# light-github

轻量、跨平台（macOS / Windows / Linux）的 GitHub 加速代理。单二进制、零证书、不占 443、不改 hosts——从 [Watt Toolkit (Steam++)](https://github.com/BeyondDimension/SteamTools) 的网络加速能力提炼而来，只保留单一场景所需的稳定内核。

## 特性

- **CONNECT 隧道加速**：白名单域名（GitHub / Docker Hub / HuggingFace）经本地代理选路，其余域名原样直通——开了等于没开
- **运行时测速容错**：DoH 多端点解析（阿里/DNSPod，防 DNS 污染）+ TCP 建连中位数测速 + 逐候选回退 + 连续失败自动沉底
- **三层数据源降级**：steampp 官方加速配置（公开 API）→ GitHub520（社区测速 hosts）→ 内置兜底清单；任一源失效不影响可用性，全失效走本地缓存
- **零侵入**：不解密 TLS（端到端证书校验完好）、不写 /etc/hosts、不设系统代理、不占用 443 端口（与本地 dev server 零冲突）
- **WSL 友好**：WSL 内一条环境变量即可接入（Watt Toolkit 的 hosts→127.0.0.1 方案会导致 WSL 内 git/curl 完全不可用，本工具无此问题）

## 快速开始

```bash
go build -o light-github ./cmd/light-github && ./light-github
# 或直接
go run ./cmd/light-github
```

启动后监听 `127.0.0.1:12800`，按需接入：

```bash
# git（仅 GitHub 生效，不影响 gitee/公司 GitLab）
git config --global http.https://github.com.proxy http://127.0.0.1:12800

# 终端临时
export https_proxy=http://127.0.0.1:12800 http_proxy=http://127.0.0.1:12800

# WSL（NAT 模式，宿主机用 -addr 0.0.0.0 启动）
export https_proxy=http://$(ip route show default | awk '{print $3}'):12800

# 快速验证
curl -x http://127.0.0.1:12800 -sI https://github.com | head -1
```

## 工作原理

```
git / 浏览器 / curl（CONNECT 127.0.0.1:12800）
   ├─ 白名单域名 ──▶ 出站选择器：规则策略（固定IP/CNAME通道）→ DoH 解析
   │                  → TCP-443 中位测速（3 次取中位，缓存 5min）
   │                  → 逐候选拨号，失败回退，连续失败沉底
   │                  → TLS 端到端（SNI=原域名，证书校验正常）
   └─ 其余域名 ────▶ 原样直通
```

数据源（1h 刷新 + 本地缓存，规则热更新）：

| 优先级 | 源 | 用途 |
|---|---|---|
| 1 | steampp `accelerator/projectgroups` | Github 分组策略（固定 IP / CNAME 防污染通道）；不使用其官方带宽中转（Relay 类）与需账号的 ServerAccelerate |
| 2 | GitHub520 `hosts.json` | 社区定时测速的固定 IP 补充 |
| 3 | 内置清单（编译期嵌入） | GitHub + Docker Hub + HuggingFace 核心域名，Dynamic 策略保底 |

设计与验证细节见 [docs/DESIGN.md](docs/DESIGN.md) 与 [docs/SPIKE-RESULT.md](docs/SPIKE-RESULT.md)。

## 开发

```bash
go test -race -cover ./...   # 全部包测试（当前覆盖率 85%+）
```

里程碑：M0 spike ✅ → **M1 核心 CLI ✅** → M2 Web 管理 UI → M3 托盘/自启/桌面窗口 → M4 三平台打包分发

## 致谢（Acknowledgments）

本项目的实现大量借鉴了以下开源项目，在此致谢（按贡献维度排列；本项目为独立 Go 实现，未直接复制其代码）：

- **[Watt Toolkit / Steam++](https://github.com/BeyondDimension/SteamTools)**（原 [FastGithub](https://github.com/dotnetcore/FastGithub) 的继承者）
  逐 IP 容错的候选链设计、FlowAnalyzer 滑动窗口速率统计思路、防解析回环与 CNAME 转发通道（`ForwardDomainNames`）的领域知识，以及本 README 所述公开加速配置 API 的数据格式。感谢 rmbgame 团队持续维护加速配置服务。
- **[GitHub520](https://github.com/521xueweihan/GitHub520)**
  TCP-443 建连中位数测速法（3 次取中位、失败按超时惩罚）、DoH 端点灾备顺序（阿里→DNSPod→Google）、40 域名覆盖清单（转译为本项目内置兜底清单），以及作为备份数据源的 hosts.json。该项目采用 CC BY-NC-ND 4.0 许可，本项目仅借鉴其算法思想与使用其公开数据，未复制其代码。
- **[dev-sidecar](https://github.com/docmirror/dev-sidecar)**
  动态优选的失败统计自动切换算法（连续失败降级、成功率驱动切换）与"定时重测 + 按需探测"的节奏设计。
- **[fetch-github-hosts](https://github.com/Licoy/fetch-github-hosts)**
  GUI/CLI 单二进制双形态的构建组织方式（本项目以 Go build tags 实现同等能力）。

## 许可证

MIT
