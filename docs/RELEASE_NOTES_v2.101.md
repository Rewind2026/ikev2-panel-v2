# v2.101 — iOS 关闭 VPN 后手机断网：按需自动连接默认翻为手动

发布日期：2026-10-30
镜像：`ghcr.io/rewind2026/ikev2-panel-v2:v2.101`（linux/amd64）

> 完整变更清单见 [CHANGELOG.md](CHANGELOG.md) 的 v2.101 小节。
> ⚠️ **本次必须重新安装描述文件**：改的是描述文件里的字段，
> 装到手机上的旧描述文件是静态快照，**不会**随镜像升级自动更新。

---

## 一句话

**用户诉求：「iOS 下发的描述文件可以使用，但是关闭 VPN 的时候有时候会导致手机连接不上网络，包括 APP 和浏览器都打不开，需要删除描述文件后才解决，或者连接其他 VPN。」**

根因是**两个出厂配置叠加**，不是服务端故障：

1. `IncludeAllNetworks=true` —— Apple 语义是 VPN 接管全部流量并启用 **default drop**：
   隧道不在时丢弃所有未进隧道的流量（防隧道抖动期间泄漏）。Apple 官方确认：
   *"Once the VPN config is enabled, default drop is enabled, meaning traffic will be
   dropped unless they are tunneled."*
2. `OnDemandEnabled=1` + 无条件 `Action=Connect`（v2.100 及以前的出厂默认）——
   按需连接引擎在用户手动关闭 VPN 后仍会**后台自动重连**。

手动关闭 → OnDemand 拉起重连 → 隧道抖动（tunnel flap）→ default drop 状态
没随断开撤销 → **整机断网**（APP、浏览器全打不开，DNS 也进不了网）。

这同时解释了用户的两个「民间解法」为什么有效：

| 操作 | 为什么有效 |
|------|-----------|
| 删除描述文件 | 整份 VPN 配置卸载，default drop 解除 |
| 连接其他 VPN | 新隧道建立，流量重新"被隧道化"，满足 default drop 放行条件 |

---

## 改了什么

| 文件 | 改动 |
|------|------|
| `internal/cert/mobileconfig.go` | `BaseMobileConfigDefaults()` 的 `OnDemandEnabled` 默认 `true` → **`false`**（完全手动）；`PanelVersion` → v2.101 |
| `internal/cert/mobileconfig_opts_test.go` | 默认值断言同步翻转 |
| `internal/cert/generate_test.go` | 渲染断言：`OnDemandEnabled=0`、无 `OnDemandRules`、无 `Action=Connect` |
| `web/templates/admin_mobileconfig_defaults_content.html` | 后台「出厂默认值参考」文案同步 |
| `docs/TROUBLESHOOTING.md` | **新增 §2.6**「iOS 关闭 VPN 后手机断网」（机理 / 自救 / 解法 / 验证） |
| `docs/CHANGELOG.md` | v2.101 条目 |

服务器端（strongSwan / swanctl / 证书 / 路由）**零改动**，纯粹是描述文件里的
客户端侧字段。

---

## ⚠️ 行为变更

| 字段 | 旧默认 | 新默认 | 说明 |
|------|--------|--------|------|
| `OnDemandEnabled` | `true`（无条件 Connect） | **`false`（完全手动）** | 关 VPN 就是真关闭；代价是 Wi-Fi↔蜂窝切换后需手动重连 |
| `OnDemandProfile` | `always` | `always`（**不变**） | 只描述"开启按需时用什么规则"，关闭时不输出 `OnDemandRules`，无实际作用 |
| `IncludeAllNetworks` | `true` | `true`（**不变**） | 见下方「为什么不动 IncludeAllNetworks」 |

---

## 为什么不动 IncludeAllNetworks

`IncludeAllNetworks=true` 确实参与了断网链条，但它是 **v2.86-PR12.13** 为修
另一个问题专门开的：iOS 17+ 对 ESP 隧道默认按 **IPv6 做 split-tunnel**，
而本项目服务器是 IPv6-only —— 不开它，v4 流量可能不进隧道，变成
「VPN 连上了但一堆 App 裸奔 / 不通」。

直接翻回 false 是拿一个已知修复换另一个已知 bug，所以**保留默认开启**，
改为在文档和后台里给出「按需关闭」的路径（见下方「仍然断网怎么办」）。

---

## 升级方法

```bash
docker compose pull
docker compose up -d
```

然后**重新下发并安装描述文件**（后台重新下载 `.mobileconfig` → iPhone 打开安装 →
覆盖安装）。只换镜像不重装描述文件，断网问题不会消失。

### 验证

1. 手机装上新描述文件 → 连上 VPN → 手动**关闭** VPN
2. 立刻打开 APP / 浏览器 → 应正常上网
3. 设置 → VPN → 应停在「未连接」，**不再自动跳回「连接中」**

---

## 仍然断网怎么办（少数情况）

按需已关还是偶发断网的（iOS 残留状态），在后台
（管理员全局默认或每用户详情页）把「全局路由(IncludeAllNetworks)」也关掉，
再重装描述文件。

⚠️ 注意回归风险：关闭后如果出现「VPN 连上了但部分 App 流量不走隧道」，
把它开回来。

### 临时自救（不用删描述文件）

开一次**飞行模式**再关 —— 强制重建路由状态，多数情况能恢复；不行就重启手机。

---

## 客户端影响

| 客户端 | 变化 |
|--------|------|
| iOS | **需重装描述文件**；关闭 VPN 后不再自动重连（切网需手动重连） |
| macOS | 无变化（描述文件主要用于 iOS；同源默认值，若用 macOS 描述文件同样生效） |
| Android | 无变化（描述文件不下发到 Android） |

---

## 文档

* [TROUBLESHOOTING.md §2.6](TROUBLESHOOTING.md) —— iOS 关闭 VPN 后断网完整排查
* [CHANGELOG.md](CHANGELOG.md) —— v2.101 完整变更清单
