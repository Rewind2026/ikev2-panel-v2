# v2.104 — 修好 Windows / Linux 的拓扑图：在线判定改认 EAP 用户名；彻底移除 .sswan 导出

发布日期：2026-11-10
镜像：`ghcr.io/rewind2026/ikev2-panel-v2:v2.104`（linux/amd64）

> 完整变更清单见 [CHANGELOG.md](CHANGELOG.md) 的 v2.104 小节。

---

## 一句话

**Windows / Linux 连上后网络是通的，但首页拓扑图不显示"已连线"** —— 根因是面板拿
strongSwan 的 `remote-id`（IKE 身份 IDi）去比用户名，而 IDi 在 Windows 上是机器证书
主体、在 Linux 上是主机名，**永远不是登录用户名**。iOS 之所以正常，是因为它的
mobileconfig 恰好把 `LocalIdentifier` 设成了用户名。

本次改为优先用 `remote-eap-id`（EAP-Identity，即真正的登录用户名），并顺带移除
`.sswan` 客户端配置导出。

---

## 改了什么

### 一、在线判定改用 `remote-eap-id`（3 个文件）

| 文件 | 改动 |
|------|------|
| `internal/swanctl/parser.go` | 新增 `pickRemoteID()`：`remote-eap-id` 优先，缺失时回退 `remote-id`；`parseSingleSA` 改用它 |
| `internal/swanctl/terminate.go` | 抽出 `saMatchesUsername()`：有 `remote-eap-id` 就**只**按它匹配（不误伤同机器的其他用户），缺失才回退 `remote-id` |
| `internal/swanctl/listener.go` | 审计事件（`ike-updown`）同样优先取 `remote-eap-id` |

**影响的不只是拓扑图**，同一个错误匹配逻辑在三处生效，本次一并修好：

| 位置 | 之前的症状 | 现在 |
|------|-----------|------|
| 首页拓扑图 / 用户列表在线态 | Windows / Linux 已连接但**不显示在线** | 正确显示 |
| 流量归账（`limit/collector.go`） | Windows / Linux 的流量**记不到对应用户名** | 正确归属 |
| 「强制下线」按钮 | 找不到 SA → 返回**幂等成功的假象**（实际没踢掉） | 真正踢掉，且不会误踢同机器其他用户 |

**为什么 Windows / Linux 一定会落空：**

strongSwan 官方 vici 文档写明 `remote-eap-id = <remote EAP identity, if EAP-authenticated>`，
即**只有 EAP 认证**才带上这个字段；而 `remote-id` 是 IKE 身份（IDi）：

| 平台 | IDi（`remote-id`）实际是什么 | 能否匹配用户名 |
|------|---------------------------|--------------|
| iOS / macOS | mobileconfig 里 `LocalIdentifier = 用户名` | **恰好能**（巧合） |
| Windows | 机器证书主体 DN（如 `CN=DESKTOP-ABC123, DC=corp`） | 不能 |
| Linux | 客户端不发 `id =` 时落到主机名 / IP | 不能 |

EAP 认证成功后 charon **不会**把 EAP 用户名回写进 IKE_SA 的身份，所以只能取
`remote-eap-id` 这一个来源。

**兼容性：** 老版本 charon 不推 `remote-eap-id` 字段时，`getString` 返回空串，自动
回退到原来的 `remote-id` 路径（并保留原有的 "N " 前缀 / `CN=` 剥离逻辑），行为不变。

### 二、彻底移除 `.sswan` 客户端配置导出

删除了 `GET /users/{id}/sswan` 端点、`renderSSwanTemplate()` 实现与配套测试文件，
并同步清理用户详情页的「Linux」标签面板、Android 说明页、README、API 文档与故障排查文档。

**为什么移除：**

`.sswan` 是 Android **strongSwan App 的专有导入格式**，离开该 App 无法使用；
而 Linux 桌面发行版**没有系统自带 IKEv2 客户端**，必须自行安装 strongSwan。
即"为了用配置文件，先装一个第三方 App / 客户端"——与"所有连接都走操作系统自带客户端"
的目标相悖。

**移除后各平台的路径：**

| 平台 | 方式 |
|------|------|
| iOS / macOS | 系统自带 VPN → 扫二维码装 `.mobileconfig`（不变） |
| Windows | 系统自带 VPN → 手动填 EAP-MSCHAPv2（不变） |
| Android 12+ | 系统自带 VPN → 手动填 EAP-MSCHAPv2（不变） |
| Android 11 及以下 | 无系统 IKEv2 支持，需自行安装 strongSwan App 手工填参（面板不再提供导入文件） |
| Linux | 无系统自带客户端，需自行安装 strongSwan 或 NetworkManager IPsec 插件（面板不再提供配置文件） |

---

## ⚠️ 行为变更

| 项 | 之前 | 现在 |
|----|------|------|
| `GET /users/{id}/sswan` | 返回 `.sswan` 客户端配置 | **端点已删除**（404） |
| 在线判定依据 | `remote-id`（IDi，机器身份） | **`remote-eap-id`（EAP 用户名）**，缺失时回退 |
| 强制下线 | Windows / Linux 找不到 SA，假成功 | 真正踢掉对应会话，不误伤同机器其他用户 |
| 用户详情页 OS 标签 | iOS / macOS、Android 12+、Android 11-、Linux、CA 证书 | iOS / macOS、Android 12+、CA 证书 |

**API 破坏性变更**：若有脚本调用 `/users/{id}/sswan`，会拿到 404，需自行移除该调用。

**镜像内配置零变更**：本次纯 Go 代码 + 模板改动，**没有改 Dockerfile**，
`/etc/strongswan.d/` 下无任何差异，`raw_esp` / 分片上限等均保持 v2.102 起的状态。
仍然**必须换镜像**才能生效（代码改动不在配置里）。

**描述文件不变**：`.mobileconfig` 的内容与 `PanelVersion` 均未改动，
**不需要重新安装 iOS 描述文件**。

---

## 升级方法

```bash
docker compose pull
docker compose up -d
```

自检（Windows / Linux 连上后面板应立刻显示在线）：

```bash
# 1. 确认 SA 上带了 remote-eap-id（EAP 用户名）
docker compose exec ikev2-panel swanctl --list-sas | grep -A3 'ikev2-rw:'
```

预期能看到 `remote alice`（用户名）而不是 `remote CN=DESKTOP-XXXX`：

```
ikev2-rw: #4, reqid 1, INSTALLED, TUNNEL, ESP:AES_GCM_16-256
   local  192.168.50.63[500]
   remote 10.10.0.2[4500]
   remote alice
```

---

## 测试

新增 3 组表驱动测试：

| 测试 | 覆盖 |
|------|------|
| `TestParseSingleSA_WindowsClientEapID` | Windows DN 形态的 `remote-id` + `remote-eap-id` → 取用户名 |
| `TestParseSingleSA_EapIDFallbackToRemoteID` | 无 `remote-eap-id`（老 charon）→ 回退 `remote-id` |
| `TestSAMatchesUsername_EapIDPriority` | 7 个 case：命中 / 不误踢 / 老 charon 回退 / 两者皆空 |
| `TestHandleEvent_EapIDPriority` | 审计事件同样优先 `remote-eap-id` |

`go build ./...`、`go vet ./...`、`go test ./internal/web/ ./internal/swanctl/` 全部通过。

---

## 客户端影响

| 客户端 | 变化 |
|--------|------|
| iOS / macOS | 无变化（本来就正常） |
| Windows 10/11 | **修复**：拓扑图 / 在线数 / 流量归账 / 强制下线全部恢复正确 |
| Linux strongSwan | **修复**：同上 |
| Android 12+ | 无变化 |

---

## 文档

* [CHANGELOG.md](CHANGELOG.md) —— v2.104 完整变更清单
* [TROUBLESHOOTING.md](TROUBLESHOOTING.md) —— 拓扑不显示在线的排查方法
