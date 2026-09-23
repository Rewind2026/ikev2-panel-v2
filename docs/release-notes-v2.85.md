# v2.85 Release Notes

> 发布日期:2026-09-19
> 主要内容:**8 个 PR 全量合并 + Phase 5 A+B 增量** —— 部署一致性、默认密码持久化、时区统一、main 退出链路、alidns 错误码与签名、DDNS 持久化与节流、IPv6 tc 限速、PR-8 健康检查 + 审计日志 + 双确认删除、Phase 5 A(audit 90 天 retention cron)+ Phase 5 B(TCP MSS clamp)
>
> 累计工作量:**6.0 人日**(PR-1 ~ PR-8 共 5.2d,Phase 5 A+B 共 0.8d)
> 关联 proposal:`docs/proposal-v2.85-pr1.md` ~ `docs/proposal-v2.85-pr8.md` + `docs/audit-2026-09-summary.md`

---

## 概要

v2.85 是 v2-79.2 之后的一次**综合质量提升**。v2-79 / v2-79.1 / v2-79.2 重点在自动部署 + 兼容性,留下了审计报告里的 23+ 项待修。v2.85 把"部署一致性 + 持久化 + 健壮性 + 可观测性 + 运维"五个维度全部清掉,并在 Phase 5 引入两个不在 v2.85 scope 但用户已点要的增量。

**v2.85 八大 PR**(合计 5.2d):

| PR | 工作量 | 主题 | 用户感知 |
|----|-------|------|----------|
| **PR-1** | 0.5d | **部署一致性 + Dockerfile race-test** | `PANEL_TAG` 跟 build-arg 同步,版本文件 `/etc/ikev2-panel-version` 强校验 |
| **PR-2** | 0.4d | **默认密码持久化 + bcrypt cost 12** | 首次启动随机密码写盘,重启不换;bcrypt 算力从 10 升 12 |
| **PR-3** | 0.3d | **时区显示统一 + 续签日志路径修复** | mobileconfig 与审计页都用 IANA TZ;LE 续签日志写到正确路径 |
| **PR-4** | 0.5d | **main.go WaitGroup + charon ready wait** | 优雅退出不再泄漏 goroutine;swanctl 启动前等 charon 真 ready |
| **PR-5** | 0.5d | **alidns 错误码精确匹配 + HMAC-SHA256** | 4xx/5xx 区分定位 + 签名算法严格按阿里云文档 |
| **PR-6** | 0.6d | **DDNS throttle 持久化 + stopCh sync.Once + collector REKEYING** | throttle 跨重启保留;goroutine 收尾零泄漏;REKEYING 事件不再吞 |
| **PR-7** | 0.9d | **tc IPv6 限速(flower classifier)** | IPv6-only VPS 也能限速,dual 模式 v4+v6 共享 class |
| **PR-8** | 1.5d | **onboarding checklist + /healthz/readyz/metrics + audit log + 双确认 delete** | 5 项可观测 + 7 项可观测 + 完整操作审计 |

**Phase 5 增量**(0.8d):

| Phase | 工作量 | 主题 | 用户感知 |
|-------|-------|------|----------|
| **5-A** | 0.3d | **audit_log 90 天 retention cron** | 自动清理过期 audit,默认 90 天可调 |
| **5-B** | 0.5d | **TCP MSS clamp**(iptables mangle) | 解决 VPN 路径 MTU 黑洞,SSH/curl/大包 HTTP 不再卡死 |

---

## ⚠️ 行为变更(升级前请看)

### 1. **PR-2 默认密码持久化**(影响范围:所有用户)

- **首次启动**会生成 32 字符随机密码 → 写入 `/etc/ikev2-panel/admin-credentials`
- 容器重启**不再换密码**(v2-84 之前每次重启都会换)
- 升级路径:v2-84 → v2.85 **第一次启动**会自动写一份持久化文件,新密码会在控制台打 INFO 日志
- 老用户如果丢了密码 → `docker compose exec ikev2-panel cat /etc/ikev2-panel/admin-credentials`(容器内只能 root 看)

### 2. **PR-2 bcrypt cost 12**

- 老用户升级后**首次登录**会触发重新 hash(从 cost 10 → 12),耗时 +50ms 量级
- 登录接口在重 hash 期间正常返回,无感知
- **登录失败次数限制窗口延长**(从 5min/10 次 → 5min/5 次),抗爆破更严

### 3. **PR-3 时区显示**

- mobileconfig `PayloadDescription` 和 `/audit` 页面时间戳都按 `IKEV2_TIMEZONE` 渲染
- 默认 UTC;在 `.env` 设 `IKEV2_TIMEZONE=Asia/Hong_Kong` 即可本地化
- 老用户(没设 TZ)行为不变

### 4. **PR-6 DDNS throttle 持久化**

- `enabled=true` + throttle 冷却中的用户,升级 v2.85 后会**立即**重试一次(因为 v2.84 throttle 只在内存,重启会丢失)
- 仅一次,后续按 v2.85 节流规则走
- `family` 状态保留(老 INI 自动迁移 v2.84 已就位)

### 5. **PR-7 tc IPv6 限速(IPv6-only 用户首次启用)**

- 老 v2-84 用户的 v6 限速规则**不会**自动迁移到 tc flower 格式(因为老规则根本不存在)
- 第一次启用"限速"功能时会重建规则,可能短时间(秒级)丢包
- v4-only / dual 用户无影响

### 6. **PR-8 `/users/{id}/delete` 路由变 GET(双确认)**

- v2-84:`POST /users/{id}/delete` 一键删除
- v2.85:`GET /users/{id}/delete` 拉确认页(列 4 个不可撤销副作用)→ `POST /users/{id}/delete/confirm` 真删
- 老用户书签/脚本里的 POST URL 自动 405,**不会**误删
- API 客户端(如有)需要更新到两阶段

### 7. **PR-8 审计日志表 + 索引**(自动迁移)

- 首次启动自动 `CREATE TABLE IF NOT EXISTS audit_log` + 2 索引
- 表不存在 → 自动建,**不会**因为审计初始化失败阻断启动
- 老表数据不丢

### 8. **Phase 5-A audit retention cron**

- 容器内每天 04:00 自动清理 90 天前 audit
- 自签 / LE 模式**统一启动** cron 守护
- 可调:`IKEV2_AUDIT_RETENTION_DAYS=30`(默认 90)→ 删 30 天前
- **完全 disable**:进容器手动 `rm /etc/cron.d/ikev2-audit-retention`

### 9. **Phase 5-B TCP MSS clamp**

- 容器启动自动加 4 条 iptables 规则(mangle/FORWARD + mangle/OUTPUT,iptables + ip6tables 各 2)
- 解决 VPN 路径 MTU 黑洞:客户端 → VPN 服务器 → 公网路径某段 MTU 小于 1500 时,SSH/curl/大包 HTTP 偶发卡死
- **完全 disable**:`IKEV2_DISABLE_MSS_CLAMP=true`
- 老用户升级后**第一次重启**会自动加规则,后续启动幂等(`-C` 检查)

---

## 升级说明

### 自动迁移清单(无感升级)

| 项 | 老版本 | v2.85 行为 |
|----|--------|-----------|
| 默认密码 | 重启换 | 持久化 `/etc/ikev2-panel/admin-credentials` |
| bcrypt cost | 10 | 首次登录自动升 12 |
| 时区显示 | UTC hardcode | 按 `IKEV2_TIMEZONE` 渲染 |
| DDNS throttle | 内存 | 持久化 `ddns.conf` + 重启不丢 |
| DDNS family | INI(v2.84 已就位) | 兼容 |
| alidns 错误码 | 4xx/5xx 一锅炖 | 精确匹配(404 vs 500 区分定位) |
| audit_log 表 | 不存在 | 自动 `CREATE TABLE IF NOT EXISTS` |
| audit 索引 | 无 | 自动加 `idx_audit_ts` / `idx_audit_event` |
| MSS clamp | 无 | 自动加 4 条 iptables 规则 |
| audit retention | 无 | 自动写 cron 每天 04:00 跑 |

### 升级步骤

```bash
# 1. 拉新镜像
git pull
docker compose build --build-arg PANEL_VERSION=v2.85

# 2. (可选)调整 .env
echo "IKEV2_TIMEZONE=Asia/Hong_Kong" >> .env         # 本地时区
echo "IKEV2_AUDIT_RETENTION_DAYS=180" >> .env         # 半年保留
# echo "IKEV2_DISABLE_MSS_CLAMP=true" >> .env         # 关闭 MSS clamp(罕见)

# 3. 重启
docker compose up -d

# 4. 验证 6 项
docker compose logs ikev2-panel 2>&1 | grep version=    # v2.85
docker compose exec ikev2-panel cat /etc/ikev2-panel-version  # v2.85
curl -s http://localhost:9000/healthz | jq              # {status: ok, checks: 4/4}
curl -s http://localhost:9000/readyz                     # ready
curl -s http://localhost:9000/metrics | head -5         # 暴露 Prometheus 指标
docker compose exec ikev2-panel iptables -t mangle -L    # FORWARD/OUTPUT 各一条 TCPMSS
docker compose exec ikev2-panel cat /etc/cron.d/ikev2-audit-retention  # 04:00 行
```

### 从 v2.83 / 更老版本升级

中间 v2.84(DDNS family)+ v2.85 这套完整路径见 [`docs/upgrade-path-v2.83-to-v2.85.md`](upgrade-path-v2.83-to-v2.85.md)(如有疑问可单独走 `v2-83 → v2-84 → v2.85` 两步)。

---

## PR-1 部署一致性 + Dockerfile race-test(0.5d)

### 改动

- `Dockerfile` L161-162:`ARG PANEL_VERSION` 默认值 `dev` 不变,但 `/etc/ikev2-panel-version` 写入逻辑强化
- `docker-compose.yml` L38-44:`image: ikev2-panel:${PANEL_TAG:-v2.85}` + `PANEL_VERSION: ${PANEL_TAG:-v2.85}` 双向同步
- `cmd/ikev2-panel/main.go` 启动时读 `/etc/ikev2-panel-version`,与启动日志 `version=` 比对,**不一致 WARN**(但不阻断)

### 用户感知

- build 时漏传 `PANEL_VERSION` 会被立即发现(启动日志 WARN)
- `docker compose config | grep image` 永远跟 `cat /etc/ikev2-panel-version` 一致

### 兼容性

完全兼容;不设 `PANEL_TAG` 默认 `v2.85`。

---

## PR-2 默认密码持久化 + bcrypt cost 12(0.4d)

### 改动

- `internal/auth/password.go`:首次启动生成 32 字符随机密码(密码学安全),写 `/etc/ikev2-panel/admin-credentials`(root 0o600)
- `internal/store/store.go`:admin 表加 `password_bcrypt_cost INTEGER`,默认 12
- `internal/auth/handlers_auth.go`:首次登录检测 cost < 12 → 自动重 hash

### 用户感知

- 容器重启**不再换密码**(v2-84 之前每次重启都换)
- 首次升级 v2.85 会在控制台 INFO 日志打印新密码,**只此一次**
- 登录耗时 +50ms 量级(单次,重 hash 后正常)

### 兼容性

完全兼容;密码丢失 → `docker compose exec ikev2-panel cat /etc/ikev2-panel/admin-credentials`。

---

## PR-3 时区显示统一 + 续签日志路径修复(0.3d)

### 改动

- `internal/cert/mobileconfig.go`:`BuildTimestamp` 渲染按 `tzName` 走,FuncMap 注入 `IKEV2_TIMEZONE`
- `internal/web/templates.go`:`formatUnixNano(nano, tz)` helper
- `web/templates/audit_content.html` + `web/templates/home_content.html`:全部走 `formatUnixNano`
- `scripts/entrypoint.sh` LE 续签日志路径:`/var/log/ikev2-le-renew.log` 修正(之前写到 `/tmp` 会被清)

### 用户感知

- iOS 装 mobileconfig 后看 `PayloadDescription` 时间戳与本地一致
- 审计页面时间戳与本地一致
- LE 续签日志可查(以前 `/tmp` 会被 systemd-tmpfiles 清掉)

### 兼容性

完全兼容;不设 `IKEV2_TIMEZONE` → 默认 UTC(行为不变)。

---

## PR-4 main.go WaitGroup + charon ready wait(0.5d)

### 改动

- `cmd/ikev2-panel/main.go`:所有 goroutine 加入 `sync.WaitGroup`,`signal.Notify` 收到 SIGTERM/SIGINT 后 `wg.Wait()` 等所有 goroutine 退出再返回
- `internal/swanctl/client.go`:启动后调 `WaitForCharonReady(timeout=30s)`,swanctl.conf 加载前等 charon 真 ready

### 用户感知

- `docker compose down` 不再有 goroutine 泄漏警告
- 容器启动日志看到 `charon ready (took 1.5s)` → swanctl 加载 → users 子配置加载,链路清晰
- 启动期 race condition(`swanctl --load-all` 时 charon 还没注册 VICI socket)消失

### 兼容性

完全兼容;启动时间 +1~2s(等 charon)。

---

## PR-5 alidns 错误码精确匹配 + HMAC-SHA256(0.5d)

### 改动

- `internal/dns/aliyun.go`:HTTP 错误码精确分类 — 4xx 客户端错(凭证 / 参数),5xx 服务端错(retry),`DomainRecordNotBelongToUser` 业务码特殊处理
- 签名算法从 `HMAC-SHA1` 严格迁到 `HMAC-SHA256`(阿里云 2018+ 新 SDK 默认)
- 重试策略:`DomainRecordNotBelongToUser` / `InvalidAccessKeyId` **不重试**,其他 5xx 最多 3 次指数退避

### 用户感知

- 凭证错(401 / 403)立刻在面板报错,不再 retry 浪费 9s
- 服务端错(500 / 503)自动重试,网络抖动场景成功率 ↑
- 签名算法升级避免被阿里云标记"使用过时 SDK"

### 兼容性

完全兼容;签名算法变化不影响 API 行为(阿里云两种都接受)。

---

## PR-6 DDNS throttle 持久化 + stopCh sync.Once + collector REKEYING(0.6d)

### 改动

- `internal/dns/upsert.go` + `internal/dns/state.go`:`next_allowed_at` 持久化到 `ddns.conf`(`throttle_v4=2026-09-19T04:00:00Z`)
- `internal/ddns/sync.go`:`stopCh` 用 `sync.Once` 关闭,多次 SIGTERM 不再 `panic: close of closed channel`
- `internal/swanctl/collector.go`:监听 charon `REKEYING` 事件,过期 SA 重新写库,不再被 `IKE_SA` 老事件吞掉

### 用户感知

- 容器重启后,DDNS throttle 冷却时间不重置
- goroutine 泄漏零告警
- 用户连接长期(>8h)触发 REKEYING 后,面板 SA 状态自动刷新

### 兼容性

完全兼容;老 `ddns.conf` 自动加 `throttle_v4=0` / `throttle_v6=0` 字段。

---

## PR-7 tc IPv6 限速(flower classifier)(0.9d)

### 改动

- `internal/limit/limiter.go`:新增 `BuildTcCommands(family, ...)` 纯函数,返回 IPv4 / IPv6 / dual 三种命令集
- `scripts/ikev2-updown`:`flower_supported()` 探测用 `tc filter add flower help | grep src_ip`;filter 用 `src_ip` 关键字(tc 按 IP 字面量自动判 v4/v6)
- dual 模式 v4+v6 共享同一 class ID(避免 "HTB class in use")
- down 顺序:filter del → class del(避免 "HTB class in use")
- `scripts/entrypoint.sh` §4.5 写 `/etc/ikev2-panel/family.env`

### 用户感知

- IPv6-only VPS 现在可以限速(以前完全不行,iptables 限速 v6 不可靠)
- dual VPS 限速对 v4 / v6 客户端同时生效,共享带宽预算

### 兼容性

- v4-only 用户无感知
- IPv6-only 用户首次启用限速会重建规则(秒级丢包)
- dual 用户无感知

### 已知限制

- 需要 Linux 4.1+(flower classifier),Docker 镜像 `kernel 5.10+` 默认满足
- host 网络下 tc 命令直接生效;bridge / ipvlan 网络下需要在宿主手动加 tc

---

## PR-8 onboarding checklist + /healthz/readyz/metrics + audit log + 双确认 delete(1.5d)

### 改动

- `internal/store/store.go`:加 `audit_log` 表 + 2 索引(`idx_audit_ts` / `idx_audit_event`)
- `internal/store/audit.go`(新):`WriteAudit` / `ListAudit(limit)`,limit > 1000 自动截断
- `internal/web/audit_helper.go`(新):`writeAudit(r, event, details)` 从 ctx 拿 admin username
- `internal/web/handlers_users.go`:create / reset_password / enable / disable / delete 五处写 audit
- `internal/web/handlers_aliyun.go`:`save` 记录 `key_id_masked`,`clear` 记录 `event=aliyun_clear`
- `internal/web/handlers_ddns.go`:`toggle` / `family` 写 audit
- `internal/web/handlers_auth.go`:`incLoginAttempt(result)` helper,登录 3 路径各调一次(2 fail + 1 success)
- `internal/web/handlers_audit.go`(新):`/audit` 渲染最近 100 条
- `internal/web/server.go`:路由改 `GET /users/{id}/delete`(确认页)+ `POST /users/{id}/delete/confirm`(真删)
- `internal/web/server.go`:`/healthz` 改 JSON 4 check(db / vici / le / ddns),2s timeout,失败 503;`/readyz` 简单 ping;`/metrics` Prometheus text format
- `internal/metrics/metrics.go`(新):自实现 Prometheus 客户端(无外部依赖),`IncHTTPRequest` / `ObserveHTTPDuration` / `IncLoginAttempts` / `SetVPNActiveSAs` / `SetDDNSLastSync` / `SetLECertExpiry`
- `web/templates/home_content.html`:U05 onboarding checklist(TotalUsers==0 时显示)+ Q1-04 fallback
- `web/templates/audit_content.html`(新):4 列时间 / 操作者 / 事件 / 详情
- `web/templates/user_delete_confirm_content.html`(新):双确认页(列 4 个不可撤销副作用)
- `docs/TROUBLESHOOTING.md`(新):7 章节 + 一键诊断 `docker compose exec ...`

### 用户感知

- 新用户第一次登录看到 onboarding checklist,引导配 Aliyun / DDNS / LE / 创建用户
- K8s / Prometheus 监控可以 scrape `/metrics`,4 类核心指标(HTTP req/dur、login、VPN SAs、DDNS、LE cert 过期)
- `/healthz` 返回 4 check JSON,K8s liveness 用得上
- `/audit` 看到所有 admin 操作,合规审计
- 删除用户前看 4 个不可撤销副作用(SAs 终止 / 子配置移除 / 流量记录保留 / 无法撤销)

### 兼容性

- 完全兼容;`audit_log` 表自动建
- `POST /users/{id}/delete` → 405,API 客户端需更新到两阶段
- `/healthz` 路径不变,行为升级(从 `200 OK` → JSON)

### 已知限制

- 自实现 Prometheus 客户端支持 counter / gauge / histogram,**不支持** summary / labels 维度(`/users/123` 归一化为 `/users/{id}` 防 cardinality 爆炸)
- audit log 不存 IP(避免 GDPR 风险),需要审计 IP 走 `/var/log/ikev2-panel/access.log`

---

## Phase 5-A audit retention cron(0.3d)

### 改动

- `scripts/audit-retention.sh`(新):sqlite3 + `DELETE FROM audit_log WHERE timestamp < cutoff_nano` + `VACUUM INCREMENTAL` + log 1MB rotation
- 环境变量:`IKEV2_AUDIT_RETENTION_DAYS`(默认 90)、`IKEV2_PANEL_DB`(默认 `/data/panel.db`)、`IKEV2_AUDIT_RETENTION_LOG`
- `scripts/entrypoint.sh` §7.7:写 `/etc/cron.d/ikev2-audit-retention`(每天 04:00 跑)
- 自签 / LE 模式**统一启动** cron 守护
- `internal/store/audit_retention_test.go`(新):2 个 unit test(retention SQL 边界 + limit clamp)

### 用户感知

- audit 表自动 90 天清理,DB 大小有界
- 默认行为零配置;`IKEV2_AUDIT_RETENTION_DAYS=180` 半年保留

### 兼容性

完全兼容;不设 env → 默认 90 天。

### 已知限制

- 单线程 sqlite3 操作,DB 大(>10GB)会阻塞面板 100ms 量级 — 实际 90 天 audit 不会超过 1GB,无须担心
- 无 cron daemon 的极简镜像(arm32v7/alpine) → 手动跑 `scripts/audit-retention.sh`

---

## Phase 5-B TCP MSS clamp(0.5d)

### 改动

- `scripts/entrypoint.sh` §4.4:`apply_mss_clamp()` 函数,iptables / ip6tables 各 2 条规则:
  - `mangle / FORWARD`:`-p tcp --tcp-flags SYN,RST SYN -j TCPMSS --clamp-mss-to-pmtu`
  - `mangle / OUTPUT`:同上
- 启动幂等(`-C` 检查已存在)
- 环境变量:`IKEV2_DISABLE_MSS_CLAMP=true` 可关闭

### 用户感知

- **MTU 黑洞场景修复**:VPN 路径某段(常见 PPTP/L2TP 中转、Wi-Fi 热点、IPv6 tunnel)MTU < 1500 时,SSH / curl / 大包 HTTP 不再卡死
- MSS clamp 后 TCP MSS = MTU - 40(IPv6+IPsec 头),避免 PMTU 发现失败场景下的分包

### 兼容性

完全兼容;iptables / ip6tables 命令用 `-C` 检查幂等。

### 已知限制

- host 网络下规则直接生效;bridge / ipvlan 网络下需要在宿主手动加 iptables
- 极少数场景(自家网络完全无 PMTU 黑洞)clamp 多此一举但无害,带宽影响 < 0.1%

---

## 测试覆盖

### 单元测试

| 包 | 测试 | 覆盖 |
|----|------|------|
| `internal/auth` | password | bcrypt 升级 / 持久化路径 |
| `internal/cert` | generate / mobileconfig | PanelVersion 注入 / PayloadDescription |
| `internal/dns` | alidns | 错误码分类 / HMAC-SHA256 签名 |
| `internal/ddns` | sync | throttle 持久化 / family 切换 |
| `internal/limit` | limiter | BuildTcCommands 三种 family / down 顺序 |
| `internal/metrics` | metrics | histogram 累积语义 / 6 桶 |
| `internal/store` | store + audit + retention | 表迁移 / 索引 / retention SQL |
| `internal/swanctl` | ipv6watch | 探测周期 / ready wait |
| `internal/web` | server + handlers + smoke | 路由 / 双确认 delete / healthz JSON |

**总测试数**:14 包全 PASS,新增 7 个 E2E(`pr8_smoke_test.go`)+ 2 个 retention unit test。

### 集成验证

- `go build ./...` → 0 错误
- `go test -count=1 ./...` → 14 包 PASS
- `bash -n scripts/audit-retention.sh` → 语法 OK
- `bash -n scripts/entrypoint.sh` → 语法 OK
- netns 跑 PR-7 tc 命令真执行(双栈规则生效)
- httptest server 跑 PR-8 完整登录 + 路由 + audit 写入

### 兼容性矩阵

| 升级路径 | 行为 |
|---------|------|
| v2-84 → v2.85 | 完全自动迁移(8 项无感升级 + Phase 5 A+B 启用) |
| v2-83 → v2.85 | 中间 v2-84(DDNS family)+ v2.85 |
| v2-82 → v2.85 | 路径:v2-83(凭证统一)→ v2-84(family)→ v2.85 |
| v2-80 → v2.85 | 路径:v2-82(DDNS)→ v2-83 → v2-84 → v2.85 |
| IPv4-only 新装 | `IKEV2_DDNS_FAMILY=v4` + host 网络 |
| IPv6-only 新装 | `IKEV2_DDNS_FAMILY=v6` + 默认 host 网络 + 启用限速 |
| 双栈新装 | `IKEV2_DDNS_FAMILY=dual`(默认)+ host 网络 + 限速同时生效 |

---

## 风险与缓解

| 风险 | 缓解 |
|------|------|
| PR-2 密码文件 `/etc/ikev2-panel/admin-credentials` 容器内 root 可见 | 0o600 权限;非 root 进容器看不到 |
| PR-2 bcrypt cost 12 首次登录慢 50ms | 仅一次,后续登录正常 |
| PR-4 charon ready wait 增加 1~2s 启动时间 | 比 race condition 失败强多了 |
| PR-5 alidns 错误码精确后某些老 RAM 策略报"权限不足"立即失败 | 用户立刻看到错(以前要等 9s retry 完) |
| PR-7 IPv6 tc 重建规则时丢包 | 秒级,不可见 |
| PR-8 自实现 Prometheus 客户端不支持 label 维度 | `normalizePath` 防 cardinality 爆炸 |
| PR-8 POST delete → 405 影响 API 客户端 | release notes 强提示;API 路径改为两阶段 |
| Phase 5-A 4:00 cron 在容器重启窗口运行 | sqlite3 单线程无锁问题,DB < 10GB 实际不阻塞 |
| Phase 5-B MSS clamp 极少数场景无意义 | 带宽影响 < 0.1%,无害 |

---

## 文件变更清单(汇总)

### 新增(13)

- `internal/store/audit.go`
- `internal/store/audit_retention_test.go`
- `internal/web/audit_helper.go`
- `internal/web/handlers_audit.go`
- `internal/metrics/metrics.go`
- `internal/web/pr8_smoke_test.go`
- `web/templates/audit_content.html`
- `web/templates/user_delete_confirm_content.html`
- `docs/TROUBLESHOOTING.md`
- `docs/proposal-v2.85-pr1.md` ~ `docs/proposal-v2.85-pr8.md`(8 个 proposal)
- `scripts/audit-retention.sh`
- `docs/release-notes-v2.85.md`(本文件)

### 修改(约 25)

- `Dockerfile`(race-test + 版本注入)
- `docker-compose.yml`(PANEL_TAG / PANEL_VERSION 同步)
- `cmd/ikev2-panel/main.go`(WaitGroup + 版本文件校验)
- `internal/auth/password.go`(持久化 + bcrypt cost 12)
- `internal/auth/handlers_auth.go`(login 计数 + 重 hash)
- `internal/cert/mobileconfig.go`(PanelVersion v2-80 → v2.85 + tzName)
- `internal/dns/aliyun.go`(错误码精确匹配 + HMAC-SHA256)
- `internal/dns/state.go` / `internal/dns/upsert.go`(throttle 持久化)
- `internal/ddns/sync.go`(stopCh sync.Once)
- `internal/swanctl/client.go`(WaitForCharonReady)
- `internal/swanctl/collector.go`(REKEYING 事件)
- `internal/limit/limiter.go`(BuildTcCommands + flower)
- `internal/store/store.go`(audit_log 表 + 索引)
- `internal/web/server.go`(路由 + healthz/readyz/metrics + 双确认)
- `internal/web/handlers_users.go`(5 处 writeAudit + confirm)
- `internal/web/handlers_aliyun.go`(2 处 writeAudit)
- `internal/web/handlers_ddns.go`(2 处 writeAudit)
- `internal/web/handlers_auth.go`(3 处 incLoginAttempt)
- `internal/web/templates.go`(formatUnixNano)
- `web/templates/home_content.html`(onboarding + Q1-04)
- `web/templates/layout.html`(顶部 nav 加 /audit)
- `web/templates/user_detail_content.html`(delete form → a)
- `web/templates/users_list_content.html`(delete form → a)
- `scripts/entrypoint.sh`(§4.4 MSS clamp + §4.5 family.env + §7.7 audit cron)
- `scripts/ikev2-updown`(flower 探测 + src_ip 语法)

---

## 后续 backlog(不在 v2.85 范围)

- **Phase 5 C**:ingress 限速 IFB(1.0d,需要单独评估)—— 当前 tc flower 只对出向限速,客户端上传不限
- v3.0 RBAC + 多管理员 + 2FA
- v3.0 流量统计精度(当前 5min 一次,精确到 KB)

---

**Release tag**:`v2.85.0`
**Docker image tag**:`ikev2-panel:v2.85`
**Build arg**:`--build-arg PANEL_VERSION=v2.85`
**Go version**:go 1.26(继续沿用 v2.84)
**Linux kernel**:≥ 4.1(flower classifier,实际 ≥ 5.10 推荐)