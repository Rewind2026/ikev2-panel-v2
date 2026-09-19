# Phase 2-4 审计汇总 — 2026-09-19

> 起始:Phase 1(7 份模块审计 + Top-10 安全合规汇总)
> 本批:Phase 2(正确性)+ Phase 3(易用性)+ Phase 4(借鉴优化)
> 关联:[audit-framework.md](file:///opt/ikev2-panel-v2-main/docs/audit-framework.md) / [audit-2026-09-summary.md](file:///opt/ikev2-panel-v2-main/docs/audit-2026-09-summary.md) Phase 1 Top-10

---

## TL;DR

| 维度 | 报告 | issue 数 | 关键 HIGH |
|------|------|---------|----------|
| Phase 2 Correctness | [audit-2026-09-correctness.md](file:///opt/ikev2-panel-v2-main/docs/audit-2026-09-correctness.md) | **31(2H + 22M + 7L)** | main.go 6 goroutine 缺统一 WaitGroup;RAM 错误码匹配过宽;HMAC-SHA1 vs SHA256 注释矛盾 |
| Phase 3 Usability | [audit-2026-09-usability.md](file:///opt/ikev2-panel-v2-main/docs/audit-2026-09-usability.md) | **18(4H + 8M + 6L)** | docker image tag 与 README 不一致;默认密码仅 stdout;/healthz 空壳;时区格式混乱 |
| Phase 4 Borrow | [audit-2026-09-borrow.md](file:///opt/ikev2-panel-v2-main/docs/audit-2026-09-borrow.md) | **7 组件分析(2 P0 + 2 P1/P2 + 3 P3)** | 借鉴 tc IPv6 限速 + bcrypt cost 10→12;4 个组件明确"保持自研" |

**累计已发现(Phase 1-4 合计)**:147 个 issue(36 HIGH + 62 MED + 49 LOW)+ 7 个借鉴组件评审。

---

## Phase 2 — Correctness 关键发现

完整列表见 [audit-2026-09-correctness.md](file:///opt/ikev2-panel-v2-main/docs/audit-2026-09-correctness.md)。这里只列 **HIGH + 关键 MED**(剩余 MED/LOW 在原报告)。

### 🔴 HIGH(2 个)

#### ISSUE-Q2-01:main.go 6 个后台 goroutine 无统一 WaitGroup,SIGTERM 可能丢数据
- **位置**:[main.go:413-540](file:///opt/ikev2-panel-v2-main/cmd/ikev2-panel/main.go#L413-L540)
- **问题**:collector / expiry / le-certcheck / ipv6watch / ddns / token-sweep 全部 `go func(){}()`,`httpSrv.Shutdown(ctx, 5s)` 超时后直接 `logger.Info("bye")` 退出 — 不 wait 后台 goroutine,可能 swanctl 半截 reload → conf.d 不一致,下次启动需 reloadAll。
- **修复**:`sync.WaitGroup` 跟踪 + Shutdown 后 wg.Wait(5s)。
- **工作量**:**0.3d**

#### ISSUE-Q3-01:阿里云 RAM 错误码匹配过宽
- **位置**:[alidns.go](file:///opt/ikev2-panel-v2-main/internal/dns/aliyun.go) `strings.Contains(err.Error(), "Forbidden")`
- **问题**:任何含 "Forbidden" 子串的 error 都被判为凭证失效,实际可能是 RAM 子权限缺失、签名错误、quota 等。
- **修复**:解析 alidns `Code` 字段(JSON 响应里),精确匹配 `DomainRecordNotBelongToRAM` / `Forbidden.RAM` / `InvalidAccessKeyId` 等。
- **工作量**:**0.3d**

> ⚠️ **注**:Phase 1 Top-10 #8(RAM 错误码)已列此项,本报告进一步细化修复路径。

### 🟡 关键 MED(10 个,其余见原报告)

| # | Issue | 简述 | 工作量 |
|---|-------|------|--------|
| Q1-01 | [sync.go:396](file:///opt/ikev2-panel-v2-main/internal/ddns/sync.go#L396) runFamily 无 panic recover | 引入新逻辑时易触发,DDNS 静默停止 | 0.2d |
| Q1-05 | 沙箱无 gcc → `-race` 不可跑 | Dockerfile 加 `runner-test` target 含 gcc | 0.1d |
| Q3-02 | retry sleep 不响应 ctx.Done | SIGTERM 时 retry 阻塞 9s | 0.2d |
| Q5-01 | DDNS throttle 时间戳不持久化 | 重启后撞 alidns 限流 | 0.2d |
| Q5-02 | 启动 ReloadAll 不等 charon ready | boot loop 风险 | 0.3d |
| Q7-03 | panelstate 改凭证 acme.sh 不同步 | 30 天后续签失败 | **1.0d(留 v3)** |
| Q8-01 | HMAC-SHA1 vs 注释"SHA256"矛盾 | 阿里云 v3 已推 SHA256,改签名算法 + 修正注释 | 0.2d |
| Q9-02 | collector 跳过 REKEYING SA | 流量统计少算 | 0.1d |
| Q9-03 | terminate 多 SA 错误聚合差 | 部分 SA 未终止但 caller 不知 | 0.2d |
| Q10-03 | v6 watch 60s polling | 改 netlink 订阅减到 <1s | **1.0d(留 v3)** |

### ✅ 验证项

| 验证 | 结果 |
|------|------|
| `go test ./...` 全包 | ✓ PASS(13/13) |
| `go test -race ./...` | ✗ 沙箱无 gcc(留给 Dockerfile runner-test target) |
| `grep _ = .*(` internal/ | 36 处,绝大多数合理 |
| `grep "errors.Is"` internal/ | 8 处,`errors.As` 0 处(可改进) |
| 时区 | UTC 路径全 OK,4 处 `.UTC()` ✓;11 处 `time.Now().Unix()`(epoch,与时区无关 ✓) |

---

## Phase 3 — Usability 关键发现

完整列表见 [audit-2026-09-usability.md](file:///opt/ikev2-panel-v2-main/docs/audit-2026-09-usability.md)。只列 HIGH + 关键 MED。

### 🔴 HIGH(4 个)

#### ISSUE-U01:docker-compose image tag 与 README 构建命令不一致
- **位置**:[docker-compose.yml:38](file:///opt/ikev2-panel-v2-main/docker-compose.yml#L38) `image: ikev2-panel:v2-84` + [README.md:179](file:///opt/ikev2-panel-v2-main/README.md#L179) `docker build -t ikev2-panel:v2-80 .`
- **问题**:README 至少 7 处写 `v2-80`(build tag、up.sh、默认值、字段名),compose 用 `v2-84` → 首次部署用户卡在镜像版本不一致;如果走 `build: .` fallback 又会出无名 tag。
- **修复**:
  1. Dockerfile 加 `ARG PANEL_VERSION=dev`,build 时 `--build-arg PANEL_VERSION=v2.84`
  2. README 同步 tag + 字段名(改 `IKEV2_ALIYUN_KEY_ID` 等 v2-83 字段)
  3. README 加"验证部署正确性"小节
- **工作量**:**0.2d**

#### ISSUE-U02:默认管理员密码仅 stdout,容器销毁后无法找回
- **位置**:[main.go:100-107](file:///opt/ikev2-panel-v2-main/cmd/ikev2-panel/main.go#L100-L107) `fmt.Println` 到 stdout;[README.md:446-448](file:///opt/ikev2-panel-v2-main/README.md#L446-L448) 只走启动日志
- **问题**:docker 日志有轮转(默认 10MB × 3);容器重启不再打印;面板无修改密码页;无 forgot-password CLI。
- **修复**(短期 0.2d):
  1. 启动时同步写 `/data/panel-state/INITIAL_ADMIN_PASSWORD.txt`(`chmod 0600`)
  2. 面板顶部加 7 天提示横幅
  3. 中期加 admin 修改密码页 + CLI 重置工具(1.0d)
- **工作量**:**0.2d(短)/ 1.0d(中)**

#### ISSUE-U03:`formatTime` 时区格式混乱(UTC vs 本地混用)
- **位置**:[templates.go:89-94](file:///opt/ikev2-panel-v2-main/internal/web/templates.go#L89-L94) `time.Unix` 按容器 TZ(默认 UTC);[handlers_ddns.go:58](file:///opt/ikev2-panel-v2-main/internal/web/handlers_ddns.go#L58) 显式 `UTC().Format(...Z)`;handlers_home.go:190 同上
- **问题**:同时间在 3 处显示 3 种格式:`2026-09-19 08:30`(本地 UTC)/ `2026-09-19T08:30:15Z`(显式 UTC)/ mobileconfig RFC3339 UTC。HKT admin 看面板误判时间窗口。
- **修复**:
  1. 加 `IKEV2_DISPLAY_TIMEZONE`(默认 `UTC`)
  2. 统一格式:`2026-09-19 16:30 HKT` / 带秒 / mobileconfig ISO8601+offset
  3. (最佳)模板用 `<time datetime="...">` 让浏览器本地化显示
- **工作量**:**0.3d**

#### ISSUE-U04:`/healthz` 仅返回 "ok",监控无法判断真实健康
- **位置**:[server.go:76,196-199](file:///opt/ikev2-panel-v2-main/internal/web/server.go#L196-L199)
- **问题**:不检查 DB ping / VICI socket / LE 续签状态 / DDNS last-sync;无 `/readyz` / `/metrics`。
- **修复**:
  - `/healthz` 升级 JSON + 各组件状态(0.3d)
  - `/readyz` 额外要求至少一个 swanctl connection loaded(0.1d)
  - `/metrics` Prometheus(0.5d):`vpn_active_sas` / `ddns_last_sync_unixtime` / `le_cert_expiry_unixtime` / `http_requests_total` / `panel_login_attempts_total`
- **工作量**:**0.9d(合并做)**

### 🟡 关键 MED(5 个)

| # | Issue | 简述 | 工作量 |
|---|-------|------|--------|
| U05 | [home_content.html:25](file:///opt/ikev2-panel-v2-main/web/templates/home_content.html#L25) 无 onboarding checklist | 新 admin 看空面板不知道干啥 | 0.5d |
| U07 | [renew-cert.sh:59](file:///opt/ikev2-panel-v2-main/scripts/renew-cert.sh#L59) vs [handlers_home.go:142](file:///opt/ikev2-panel-v2-main/internal/web/handlers_home.go#L142) 日志路径不一致 | 面板说看 ikev2-renew.log,实际在 acme-renew.log | 0.1d(短) |
| U08 | README 697 行 + design.md 1810 行 + architecture.md 1887 行无 troubleshooting 索引 | 新人 1h 找不到 mobileconfig 安装步骤 | 0.5d |
| U10 | 强 delete 用 `onsubmit=confirm()` 依赖 JS | 无 JS 仍可 POST 删;无 CSRF 双确认 | 0.1d |
| U11 | DDNS toggle + 凭证变更无 audit log | admin 改了啥没法追溯(关联 W10) | 0.3d |

### 🟢 LOW(6 个,详见原报告)

CSS 类名不匹配、QR 图 alt 信息缺失、颜色对比度 WCAG AA、`onsubmit` 无 JS 兜底、`<label>` 缺 `for`、表单错误无 `aria-live`。

---

## Phase 4 — Borrow & Improve 关键结论

完整分析见 [audit-2026-09-borrow.md](file:///opt/ikev2-panel-v2-main/docs/audit-2026-09-borrow.md)。**深度优先**,挑了 7 个最值得评审的组件,**2 个借鉴、2 个待条件触发、3 个明确保持自研**。

### 借鉴路线图(汇总)

| 优先级 | 组件 | 改动 | 工作量 | 触发条件 |
|--------|------|------|--------|----------|
| **P0** | **tc 限速 IPv6** | 改 ikev2-updown + limiter.go,用 flower | 0.5d | **v2.85 立即做**(Phase 1 Top-10 #7 也列) |
| **P0** | **bcrypt cost 10→12** | 改 1 行 `bcrypt.GenerateFromPassword` | 0.1d | **v2.85 立即做**(OWASP 2023 推荐 ≥12) |
| **P1** | govici 升级 review | 跟 Go 版本升级一起跑测试 | 0.2d | 每次 Go 升级 |
| **P2** | DDNS provider interface 抽离 | `Provider` iface + Cloudflare stub | 1.5-3.5d | **如果**用户要求换 DNS |
| **P2** | OCSP stapling(certmagic 借鉴) | `tls.Config.OCSPStaple` 注入 | 0.5d | **如果**面板对外公开 |
| **P3** | Argon2id + rehash on login | 改 hash + 迁移逻辑 | 1.0d | v3.0 |
| **P3** | govici Subscribe 替代 polling | 重写 collector | 1.5d | **如果**加"实时活跃用户" |

### 保持自研(明确不借)

| 组件 | 理由 |
|------|------|
| **Web 框架(chi/gin)** | Go 1.22 stdlib mux 已够用,chi 给我们的都是用不到的 middleware |
| **日志(zap/zerolog)** | slog 性能 2024 已追平 zerolog 95%,零依赖符合"轻量" |
| **配置(viper/caarlos0/env)** | 25 env 字段稳定,struct tag 收益 < 维护成本 |
| **VPN 密码明文存储** | EAP-MSCHAPv2 协议限制(NT-Hash 不能做 bcrypt) |

### 关键反向建议(防过度借鉴)

**不要**因为"看起来专业"就加:
- ❌ caarlos0/env — 25 env 字段不值得换
- ❌ chi / gin — stdlib mux Go 1.22+ 已 90% 等价 chi
- ❌ zerolog / zap — slog 性能足够
- ❌ certmagic — 我们已用 acme.sh,certmagic 重写整个续签栈不划算
- ❌ Argon2id — VPN 密码受协议限制,纯改 hash 算法无收益

**自研的简洁性本身是项目竞争力**,不要因为"借鉴框架"建议就堆依赖。

---

## Phase 2-4 合并 Top-10(按工作量升序)

| # | Issue | 工作量 | 来源 |
|---|-------|--------|------|
| 1 | U10 强 delete 双确认(无 JS 兜底) | 0.1d | Usability |
| 2 | Q1-05 Dockerfile race-test target | 0.1d | Correctness |
| 3 | Q9-02 collector 包含 REKEYING | 0.1d | Correctness |
| 4 | Q1-02 Sync.stopCh 改 sync.Once | 0.1d | Correctness |
| 5 | U07 续签日志路径修复 | 0.1d | Usability |
| 6 | **U01 docker-compose tag 同步 README** | 0.2d | Usability |
| 7 | Q5-01 DDNS throttle 持久化 | 0.2d | Correctness |
| 8 | Q8-01 HMAC-SHA1 → SHA256 + 注释修正 | 0.2d | Correctness |
| 9 | Q3-02 retry sleep ctx.Done | 0.2d | Correctness |
| 10 | Q1-01 runFamily panic recover | 0.2d | Correctness |
| 11 | U02 默认密码持久化到文件 | 0.2d | Usability |
| 12 | Q1-04 home ListSAs timeout 截断 | 0.2d | Correctness |
| 13 | U03 时区显示统一 | 0.3d | Usability |
| 14 | **Q2-01 main.go 统一 WaitGroup** | 0.3d | Correctness |
| 15 | **Q3-01 RAM 错误码精确匹配** | 0.3d | Correctness(已并 Top-10 #8) |
| 16 | Q5-02 charon ready wait | 0.3d | Correctness |
| 17 | U11 DDNS / 凭证变更 audit log | 0.3d | Usability(关联 W10) |
| 18 | U05 onboarding checklist | 0.5d | Usability |
| 19 | U08 troubleshooting 总入口 | 0.5d | Usability |
| 20 | **U04 /healthz 升级 + /metrics** | 0.9d | Usability |
| 21 | Borrow-P0 tc IPv6 限速 | 0.5d | Borrow |
| 22 | Borrow-P0 bcrypt cost → 12 | 0.1d | Borrow |
| 23 | Borrow-P2 OCSP stapling | 0.5d | Borrow(可选) |
| **小计(全部)** | | **~6.5d + 1d 借鉴 P0/P2** | |

### 立即可做的小事(< 0.3d,本周内)

- 1-5(全部 LOW/短)→ 共 0.5d
- 6-12(关键 MED)→ 共 1.5d
- **本周小计:2.0d,可一次性 PR 收**

### 中等工作(0.3-0.9d,本月内)

- 13-20 → 共 3.9d
- **本月小计:3.9d**

### P0 借鉴(本周内,跟 Phase 1 合并)

- Borrow-P0:tc IPv6 + bcrypt cost = 0.6d(已并入 Phase 1 Top-10 #7 + 借用 OWASP 建议)

---

## 与 Phase 1 Top-10 关系

| Phase 1 Top-10 | Phase 2-4 关联 |
|----------------|---------------|
| #1 登录限速 | 仍未覆盖,留 Web 后续审计 |
| #2 IKEv2 套件去 MODP2048 | 仍未覆盖 |
| #3 cert 切换 atomic | Q7-03 部分覆盖(panelstate→acme.sh 同步),留 v3 |
| #4 Docker privileged | 未覆盖 |
| #5 HTTP security headers | 未覆盖 |
| #6 VPN 密码明文 + DB 加密 | Borrow 明确"明文受协议限制"(永不修);DB 加密需专项设计 |
| #7 IPv6 限速 | **Borrow-P0 完全覆盖** |
| #8 RAM 错误码 | **Q3-01 详细化修复路径** |
| #9 供应链 GPG + commit pin | 未覆盖 |
| #10 DDoS / TCP MSS clamp | 未覆盖 |

**Phase 2-4 没重复覆盖 Phase 1 已确认的问题**,只做精细化 + 新维度补充。

---

## 全 4 Phase 累计工作

| Phase | 维度 | 报告数 | HIGH | MED | LOW | 借鉴组件 |
|-------|------|--------|------|-----|-----|----------|
| 1 | 安全 + 合规(7 模块) | 7 + summary | 36 | 62 | 49 | - |
| 2 | 正确性 | 1 | 2 | 22 | 7 | - |
| 3 | 易用性 | 1 | 4 | 8 | 6 | - |
| 4 | 借鉴优化 | 1 | - | - | - | 7(2 P0 + 2 P1/P2 + 3 P3) |
| **合计** | | **11 份** | **42** | **92** | **62** | **7** |

**总工作量估算**:Phase 1 Top-10 (6.5d) + Phase 2-4 新增 (~4d 立即 + ~2d P3 借鉴) ≈ **12.5d(2.5 周单人)**。

---

## 后续动作

1. **本周(2.0d)**:Top-10 立即可做的小事(全部 < 0.3d)+ Borrow-P0(0.6d)→ 一次性 PR 收
2. **本月(3.9d)**:中等工作(13-20)
3. **下版本**:P3 借鉴 + Q7-03 / Q10-03 等专项设计
4. **永不**:不要为"显得勤奋"加 caarlos0/env / chi / zerolog / viper / Argon2id(明文受协议限制)
5. **下一轮审计**:Phase 5 是否要加?**建议**(单独一份"运维"维度):监控 / 告警 / 备份 / 灾备 — **不在本批**,**等你拍板**

---

> 关联文档:
> - [audit-2026-09-correctness.md](file:///opt/ikev2-panel-v2-main/docs/audit-2026-09-correctness.md) — 31 个 issue
> - [audit-2026-09-usability.md](file:///opt/ikev2-panel-v2-main/docs/audit-2026-09-usability.md) — 18 个 issue
> - [audit-2026-09-borrow.md](file:///opt/ikev2-panel-v2-main/docs/audit-2026-09-borrow.md) — 7 组件评审
> - [audit-2026-09-summary.md](file:///opt/ikev2-panel-v2-main/docs/audit-2026-09-summary.md) — Phase 1 Top-10
> - [audit-framework.md](file:///opt/ikev2-panel-v2-main/docs/audit-framework.md) — 5 维度框架
