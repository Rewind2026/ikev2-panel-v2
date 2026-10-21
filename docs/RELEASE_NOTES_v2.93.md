# v2.93 — 首页 8 张就绪卡片说实话 + 自签模式 conn 漏加载

发布日期：2026-10-21
镜像：`ghcr.io/rewind2026/ikev2-panel-v2:v2.93`（linux/amd64）

> 完整变更清单见 [CHANGELOG.md](CHANGELOG.md) 的 v2.93 小节。
> 排查指引见 [TROUBLESHOOTING.md](TROUBLESHOOTING.md) §2.5 / §9.11。

---

## 一句话

**用户反馈「改了密码这还显示默认」，顺着查下去发现 8 张就绪卡片里 3 张显示与实际不符，
还牵出一个真 bug：自签模式下 `swanctl --list-conns` 一直是空的 —— connection 压根没注册进
charon，客户端建不了隧道，而面板显示的是「0 在线」，看着像一切正常。**

---

## 症状

首页顶部一排 8 张卡片（截图）：

| 卡片 | 显示 | 实际 |
|------|------|------|
| 管理口令 | 🔴 默认 | 密码**早就改过**（初始密码登录 401，审计日志有 `admin.password.change`） |
| IP 池 | ⚪ 未配置 | 池**活着**：`swanctl --list-pools` 有 `ikev2-pool 10.10.0.1 0/0/254` |
| 服务端证书 | ⚪ 未配置 | 自签证书**一直都在用**：`/data/server/server.cert.pem`、`/data/panel-tls/cert.pem` 都在，8443 正常 LISTEN |
| LE 续签 | ⚪ 不适用 | 跟证书卡同源（判据取的是配置态） |
| IKE_SA | ⚪ 0 在线 | charon 里**一个 conn 都没加载**（真故障，但显示得像正常空闲） |

---

## 根因：拿「配置态」当「运行态」

这排卡片是**服务就绪链路**，必须反映**当前实际**的状态。
而旧判据取的是**配置态** —— 语义是「**用户配过没有**」：

| 字段 | 语义 | 后果 |
|------|------|------|
| `SubnetConfigured` | `subnet.conf` 文件是否存在 | 走 env / compose 默认池的用户从没在面板设过 → 永远 false → 「未配置」 |
| `CertConfigured` | `cert.conf` 文件是否存在 | 自签模式（默认）根本不产生 `cert.conf` → 永远 false → 「未配置」 |
| `INITIAL_ADMIN_PASSWORD.txt` | 改密成功后**从不删** | 卡片永远「默认」 |

于是三张卡都在说假话。

---

## 修复

### 一、管理口令卡不再显示「默认」

改密成功后删掉 `INITIAL_ADMIN_PASSWORD.txt`（`clearInitialPasswordFile()`）。
删失败**只记 warn 不中断** —— 密码已经落库，回滚不了。

顺带清掉那份初始明文密码副本（`INITIAL_ADMIN_PASSWORD.txt` 里躺着明文，
改完密码后它就是纯垃圾）。

### 二、IP 池卡显示运行态实际生效的段

判据换成从 `swanctl.conf` 读到的运行态，并新增**漂移检测**：

| 情况 | 卡片 |
|------|------|
| 运行态有段 | 🟢 显示实际 CIDR |
| 面板配的段 ≠ 运行态在用的段 | 🟡 **未生效**（warn，不报绿 —— 绿 = "配置已落地"） |
| 什么都没有 | ⚪ 未配置 |

### 三、服务端证书 / LE 续签卡改用运行态口径

新增两个字段，把「配置里写的」和「实际在跑的」分开：

| 字段 | 语义 |
|------|------|
| `CertMode` | **配置**里写的模式（`cert.conf` 优先，其次 env）—— 表单 / radio 用它 |
| `CertLiveMode` | **运行中实际生效**的模式 —— 卡片 / metric 用它 |
| `CertInPlace` | 该模式的证书文件是否真的在位 |

`CertLiveMode` 按 `main.go` 的启动分支反查文件判定（LE 看 `le/fullchain.pem`，
自签看 `server/server.cert.pem`）。于是：

- 自签模式不再误报「未配置」，显示「自签」+ 黄（能用但客户端需手动信任 CA）；
- 面板里存了 LE 但**没重启容器**的窗口期，如实显示「自签」+ 明确提示
  「需 `docker compose restart` 后生效」，不撒谎显示「LE」。

附带修掉一个小毛病：证书卡上「配置来源：panelstate（需 restart 生效）」以前
**只要有 `cert.conf` 就一直显示**（重启过了还在提示要重启）。现在只在
「配置与运行态确实不一致」时才提示。

### 四、自签模式下 connection 从未注册进 charon（客户端连不上）

**这是本轮最严重的一条。**

63 实机启动日志：

```
[entrypoint] WARN: swanctl --load-all failed (will retry after Go installs certs)
swanctl --load-all failed: exit status 22 (
  loading certs certificate '/etc/swanctl/x509/server.cert.pem' failed: No such file or directory
  loaded 0 of 1 connections, 1 failed to load, 0 unloaded
  loaded pool 'ikev2-pool' / successfully loaded 1 pools, 0 unloaded)
vici load-creds (initial) ok
```

**四步链条**：

1. `entrypoint.sh` §7 的第一次 `swanctl --load-all` 发生在**证书装好之前**
   （自签证书是 Go 进程启动后才生成的）→ conn 引用 `send_cert=always` 的
   服务端证书，证书不在 → conn 整个注册不进去；
2. Go 进程启动时的 `scm.ReloadAll()` 同样失败 —— 那时证书还没装；
3. `entrypoint.sh` §7.6「证书装好后补一次 load-all」**只在 LE 模式跑**
   （`SWAN_X509` 只在 §7.5 的 LE 分支里赋值）→ 自签模式整段跳过；
4. Go 进程最后只调了 `load-creds` —— 它**只装证书，不装 conn**。

⇒ 自签模式下 conn 永远不在 charon 里。全新部署（一个用户都没有）就一直空着，
客户端建不了隧道；等到「新增/启用用户」触发写 `conf.d` + reload 时才会顺带装上
—— 现象像偶发，实际是必中。

**修复**：Go 进程装完自签证书后补一次**完整的** `ReloadAll`
（`--load-all` = conns + creds + authorities + pools），把 conn 真正注册进去。
失败不致命，退一步至少 `load-creds` 把证书装上。

### 顺带：IKE_SA 卡能区分两种「0」

| 状态 | 卡片 | 含义 |
|------|------|------|
| swanctl 超时 | 🔴 超时 | 查不到，不能当成「没人连」 |
| charon 里一个 conn 都没加载 | 🔴 **未加载** | 客户端建立不了隧道（故障） |
| 0 条活跃 SA | ⚪ 0 在线 | 真的只是当前没人连 |

新增 `swanctl.ListLoadedConns()` 查已加载的 conn（charon 的 VICI 没有
`list-conns` 命令，CLI 是唯一入口）。**只在 SA 数为 0 时才查**（有 SA 说明 conn
必然已加载），且**查不到 ≠ 没加载** —— 查询失败时保持原样，不误报故障。

---

## ⚠️ 行为变更

- 首页「服务端证书」「LE 续签」两张卡显示的是**运行中实际生效**的证书，
  不再是「配置里写的」。存了 LE 但没 `docker compose restart` 的窗口期，
  卡片显示「自签」，证书卡会明确提示需 restart。
- 首页「IP 池」卡显示**运行态实际生效**的 IP 段。走 env / compose 默认池的
  部署（`subnet.conf` 不存在）现在显示真实 CIDR + 绿色，而不是「未配置」。
- IKE_SA 卡在 charon 没有已加载的 connection 时显示红色「未加载」。
- 改密成功后 `INITIAL_ADMIN_PASSWORD.txt` 会被删除 → 顶部默认密码横幅消失。

---

## 回归测试

新增 **19 条**（全部通过）：

| 文件 | 条数 | 覆盖 |
|------|------|------|
| `internal/web/lane_cards_test.go` | 15 | 8 张卡片的判据分支，含 63 实机的三种部署形态（无 `subnet.conf` 但运行态有池 / 自签证书在位 / 配了 LE 但没重启），以及「查询失败不误报故障」 |
| `internal/swanctl/conns_test.go` | 4 | `--list-conns` 输出解析（只认第 0 列的顶层 conn 头，子 SA 段的同名缩进行不算） |

静态检查：`go build` / `go vet` / `go test ./...` 全部通过。

---

## 升级方法

### 用 docker compose 部署的

```bash
docker compose -f <你的compose文件> pull
docker compose -f <你的compose文件> up -d
```

### 用 1Panel 部署的

1Panel 里点「重建」/「更新」镜像到 `v2.93` 即可。

### 不升级也能立刻缓解

旧版本上「管理口令」卡一直显示默认，可以手工删标记文件（不影响登录）：

```bash
docker compose exec ikev2-panel rm -f /data/panel-state/INITIAL_ADMIN_PASSWORD.txt
```

旧版本上 `swanctl --list-conns` 为空（客户端连不上），可以手工补一次加载
（治标，容器重建后会复现）：

```bash
docker compose exec ikev2-panel swanctl --load-all --noprompt
```

---

## 升级后自查

```bash
# 1. conn 注册上了吗？（应至少有一行 conn 头）
docker compose exec ikev2-panel swanctl --list-conns

# 2. 启动日志里的这行应该出现
docker compose logs ikev2-panel | grep "conns registered"

# 3. 首页 8 张卡片是否与实际一致（对照命令见 TROUBLESHOOTING §9.11）
```

---

## 完整变更清单

见 [docs/CHANGELOG.md](CHANGELOG.md) 的 v2.93 小节。
