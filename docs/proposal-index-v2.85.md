# v2.85 Release — Proposal 总目录

> **8 个 PR**,覆盖 Phase 1-4 审计中的 27 个 issue,合计工作量 **5.9d**
> **关联审计**:[audit-2026-09-summary.md](file:///opt/ikev2-panel-v2-main/docs/audit-2026-09-summary.md) + [audit-2026-09-phase2-4.md](file:///opt/ikev2-panel-v2-main/docs/audit-2026-09-phase2-4.md)

---

## 总览表

| PR | 标题 | 工作量 | 覆盖 issue 数 | 严重度分布 | 关联审计 |
|----|------|--------|--------------|------------|----------|
| PR-1 | 部署一致性 + Dockerfile race-test target | **0.4d** | 2 | 1H + 1M | U01 + Q1-05 |
| PR-2 | 默认密码持久化 + bcrypt cost 12 | **0.3d** | 2 | 1H + 1 Borrow P0 | U02 + Borrow |
| PR-3 | 时区显示统一 + 续签日志路径修复 | **0.4d** | 2 | 1H + 1M | U03 + U07 |
| PR-4 | main.go WaitGroup + charon ready wait | **0.6d** | 2 | 1H + 1M | Q2-01 + Q5-02 |
| PR-5 | alidns 错误码精确匹配 + HMAC-SHA256 | **0.5d** | 2 | 1H + 1M | Q3-01 + Q8-01 |
| PR-6 | DDNS throttle 持久化 + stopCh + collector | **0.4d** | 3 | 3M | Q5-01 + Q1-02 + Q9-02 |
| PR-7 | tc 限速 IPv6 支持(flower) | **0.5d** | 1 | Borrow P0 + Phase 1 Top-10 #7 | Borrow + Top-10 |
| PR-8 | onboarding + healthz/readyz/metrics + audit log + low | **2.6d** | 7 | 1H + 3M + 3L | U04/U05/U08/U10/U11 + Q1-04 + Q4-02 |
| **合计** | | **5.9d** | **27** | **6H + 11M + 3L + 2 Borrow P0** | |

---

## 按优先级排(每周可挑)

### Week 1(2.6d)— 关键基础 + 用户感知
- **PR-1**(0.4d):新人部署不卡
- **PR-2**(0.3d):密码管理 + 安全基线
- **PR-3**(0.4d):时区 + 日志
- **PR-4**(0.6d):进程管理 + 启动稳定
- **PR-5**(0.5d):DDNS 重试逻辑
- **PR-7**(0.5d):IPv6 限速(Phase 1 Top-10 #7)

### Week 2(3.3d)— 体验 + 可观测性
- **PR-6**(0.4d):DDNS 持久化
- **PR-8**(2.6d):onboarding + 健康检查 + metrics + audit log + 杂项

---

## 按维度排(方便按"关心什么"挑)

### 部署 / 用户入门(PR-1)
- ✅ README 同步 v2.85 + image tag 一致
- ✅ Dockerfile `runner-test` target 给 race detector 铺路
- ✅ 启动日志显示版本号

### 安全 / 凭证(PR-2)
- ✅ 默认密码持久化到 `/data/panel-state/INITIAL_ADMIN_PASSWORD.txt`
- ✅ 7 天"还在用默认密码"横幅
- ✅ bcrypt cost 10 → 12(OWASP 2023 推荐)

### 用户体验(PR-3, PR-8)
- ✅ 时区显示统一(`IKEV2_DISPLAY_TIMEZONE` + FuncMap)
- ✅ 续签日志路径修复
- ✅ onboarding checklist(home page)
- ✅ troubleshooting 总入口(`docs/TROUBLESHOOTING.md`)
- ✅ 强 delete 双确认(无 JS 兜底)

### 进程管理 / 启动(PR-4)
- ✅ main.go 统一 WaitGroup + Shutdown 后 wg.Wait
- ✅ charon ready wait(10s 重试)

### DDNS / alidns(PR-5, PR-6)
- ✅ 错误码精确匹配(凭证 / RAM / 限流 / 临时)
- ✅ HMAC-SHA1 → SHA256 + 注释修正
- ✅ throttle 时间戳持久化
- ✅ stopCh sync.Once 改写
- ✅ collector 包含 REKEYING SA

### 可观测性(PR-8)
- ✅ `/healthz` 升级 JSON
- ✅ `/readyz` 区分 liveness/readiness
- ✅ `/metrics` Prometheus 端点(6 个关键指标)

### 审计 / 合规(PR-8)
- ✅ `audit_log` 表 + 各 handler 写审计
- ✅ 面板"操作日志"页面

### 限速(PR-7)
- ✅ tc flower 支持 IPv6(v4+v6 双栈各自限速)

### 杂项(PR-8)
- ✅ home ListSAs timeout 截断(3s)
- ✅ GeneratePassword 注释修正

---

## 明确剔除(留 v3)

- ❌ Q7-03 panelstate → acme.sh 实时凭证同步(1.0d,设计变更)
- ❌ Q10-03 v6 watch netlink 订阅(1.0d,改架构)
- ❌ Borrow-P2 OCSP stapling / DDNS provider 抽象(无触发条件)
- ❌ Borrow-P3 Argon2id + rehash / govici Subscribe 重写
- ❌ Phase 1 Top-10 #1-#6、#9、#10(等你单独决策)

---

## 单独决策项(Phase 1 Top-10)

Phase 1 的 10 个 HIGH 中,**#7(IPv6 限速)由 PR-7 覆盖**,**#8(RAM 错误码)由 PR-5 覆盖**。

剩下 8 个**不在 v2.85 自动 scope 内**,需要你单独拍板:

| # | 标题 | 工作量 | 备注 |
|---|------|--------|------|
| 1 | 登录 / POST 无限速 | 1.0d | 真实安全风险,建议加进 v2.85 或 v2.86 |
| 2 | IKEv2 套件去 MODP2048/modpnone/SHA1 | 0.5d | RFC 8247 不推荐 |
| 3 | 续签 cert 切换非 atomic | 0.5d | 跟 Q7-03 部分关联 |
| 4 | Docker privileged 容器逃逸 | 1.0d | 大改动,可能拆 PR |
| 5 | HTTP 安全 header 全缺失 | 0.3d | 简单可加 |
| 6 | VPN 密码明文 + DB 无加密 | 0.1d(短)+ 1.0d(中) | 明文受协议限制,DB 加密需设计 |
| 9 | 供应链无 GPG + commit pin | 0.5d | 影响 acme.sh gitee clone 路径 |
| 10 | DDoS / TCP MSS clamp | 0.8d | 跟 PR-7 部分相关 |

如果你想,这些可以单独写 `proposal-v2.85-pr9.md` ~ `proposal-v2.85-pr16.md`,**也可以推迟到 v2.86**。

---

## 执行顺序建议

**严格按 Week 1 → Week 2 顺序**:
- Week 1 每个 PR 独立可合
- Week 2 的 PR-8 是收尾,依赖 PR-2 的横幅 + PR-3 的时区(共用基础)

**反模式**:不要合并 Week 1 + Week 2 到 1 个 mega PR(评审无法聚焦,回滚代价高)

---

## 验证清单(v2.85 合入完毕前必须跑)

```bash
# 1. 单元测试
go test ./...

# 2. race detector(PR-1 加了 runner-test target 后)
docker build --target runner-test -t ikev2-panel-race:test .
docker run --rm ikev2-panel-race:test

# 3. 部署一致性(PR-1)
docker compose down
docker build --build-arg PANEL_VERSION=v2.85 -t ikev2-panel:v2.85 .
docker compose up -d
docker compose logs ikev2-panel 2>&1 | grep "version="
# 期望:version=v2.85

# 4. /healthz / /metrics(PR-8)
curl -s http://localhost:8443/healthz | jq .
curl -s http://localhost:8443/metrics | grep vpn_active_sas

# 5. DDNS IPv4/IPv6 限速(PR-5 + PR-7)
./scripts/up.sh
docker exec ikev2-panel tc -s qdisc show dev eth0
docker exec ikev2-panel tc -s filter show dev eth0
# 期望:看到 dual 模式 v4 + v6 各自 filter
```

---

## 关联文档

- [audit-2026-09-summary.md](file:///opt/ikev2-panel-v2-main/docs/audit-2026-09-summary.md) — Phase 1 Top-10
- [audit-2026-09-phase2-4.md](file:///opt/ikev2-panel-v2-main/docs/audit-2026-09-phase2-4.md) — Phase 2-4 汇总
- [audit-framework.md](file:///opt/ikev2-panel-v2-main/docs/audit-framework.md) — 5 维度框架
- [release-notes-v2.84.md](file:///opt/ikev2-panel-v2-main/docs/release-notes-v2.84.md) — 上一版本

---

> 创建日期:2026-09-19
> 状态:**8 份 proposal 已就绪,等待评审 + 执行决策**
