# 自我审计框架

> 起始日期:2026-09-18
> 维护者:ikev2-panel-v2 项目组
> 关联:[docs/design.md](file:///opt/ikev2-panel-v2-main/docs/design.md)(设计文档)、[docs/release-notes-v2.84.md](file:///opt/ikev2-panel-v2-main/docs/release-notes-v2.84.md)(最新发布说明)

---

## 为什么做这件事

自 v2-79 起,本项目几乎所有功能都是**自研**:从 IKEv2 服务编排到 DDNS 同步、从证书签发到 web 面板。自研给我们带来灵活性和可控性,但也意味着:

1. **没有"社区共识"兜底**——strongSwan / acme.sh / 阿里云 API 这些生态里的最佳实践,我们可能没意识到
2. **代码质量靠开发者经验**——我们自评没有 bug,不代表真的没 bug
3. **借力机会成本高**——可能有更好的开源方案能替代我们几十行手写代码

**预防性审计 > 修 bug**。这是本框架的核心动机。

---

## 5 个审计维度

每个维度独立成一份报告(`docs/audit-YYYY-MM-<dim>.md`),独立评审,独立排期。

### 维度 1:合规性(Compliance)🟦

**关注**:license、法律、标准符合度

| 子项 | 内容 |
|------|------|
| C1 | **依赖 license** —— go.mod 里所有包的 license,跟项目 license 的兼容性 |
| C2 | **strongSwan GPL 传染性** —— 我们打包 strongSwan 进 Docker 镜像分发,镜像整体的 license 影响 |
| C3 | **阿里云 RAM 最小权限** —— 我们的 minimal policy 模板是否过宽 |
| C4 | **隐私 / 数据保护** —— 用户凭证、session、密码的 retention + encryption-at-rest |

**参考**:
- [ChooseALicense](https://choosealicense.com/)
- [SPDX License List](https://spdx.org/licenses/)
- [阿里云 RAM 最佳实践](https://help.aliyun.com/zh/ram/getting-started/best-practices-for-ram-users)

---

### 维度 2:安全性(Security)🔴

**关注**:漏洞、攻击面、加密强度

| 子项 | 内容 |
|------|------|
| S1 | **strongSwan 加密套件** —— IKE/AH/ESP 用的 cipher / DH 组 / PRF,是否对齐 [RFC 8247](https://datatracker.ietf.org/doc/html/rfc8247) |
| S2 | **登录端点限速** —— 暴力破解防护(design §4.1 声称但未实现) |
| S3 | **私钥 / 凭证文件权限** —— `/data/le/`、`/data/panel-state/aliyun.creds`、DB 文件的实际权限位 |
| S4 | **HTTPS 强制 + HSTS** —— 8443 是否强制 HTTPS、Cookie Secure / HSTS / SameSite 设置 |
| S5 | **SQL 注入 / XSS / CSRF** —— 用户输入校验、模板 escape、CSRF token 实现 |
| S6 | **会话安全** —— session ID 强度、过期、刷新、登出 |
| S7 | **敏感信息日志** —— 密码 / secret / token 是否会被 log 打印 |
| S8 | **Docker 镜像安全** —— 基础镜像选型、多阶段构建减少 attack surface |

**参考**:
- [OWASP Cheat Sheet Series](https://cheatsheetseries.owasp.org/)
- [strongSwan Security Recommendations](https://docs.strongswan.org/docs/strongswanSecurity.html)
- [NIST SP 800-131A](https://csrc.nist.gov/publications/detail/sp/800-131a/rev-2/final)
- [CIS Docker Benchmark](https://www.cisecurity.org/benchmark/docker)

---

### 维度 3:正确性(Correctness)🟡

**关注**:功能对不对、并发安全、资源管理

| 子项 | 内容 |
|------|------|
| Q1 | **并发安全** —— `go test -race` 跑全包;哪些 struct 没加锁;channel 是否会泄漏 |
| Q2 | **资源泄漏** —— 文件句柄、DB 连接、goroutine 退出路径 |
| Q3 | **错误处理** —— `_ = xxx` 吞掉多少 error?哪些是真不该 log,哪些该 log |
| Q4 | **时区 / 时间** —— UTC vs local、证书过期计算、cron 触发时机 |
| Q5 | **重启恢复** —— 容器重启后:charon reload?DDNS 状态保留?用户密码 hash 一致? |
| Q6 | **边界条件** —— 空配置、IPv4-only、IPv6-only、双栈、证书过期、domain 未解析、key 错 |
| Q7 | **acme.sh 集成** —— `--issue --dns dns_ali --reloadcmd` 调用时机、env 注入、`--renew --force` 副作用 |
| Q8 | **阿里云 API 调用** —— HMAC-SHA1 vs SHA256、错误码、限流处理、SignatureNonce |
| Q9 | **strongSwan VICI 协议** —— govici 版本兼容、SA 列表解析完整性 |
| Q10 | **DDNS IPv4 探测** —— `net.Dial("udp","8.8.8.8:80")` 可靠性、跟 ddns-go / ddclient 对比 |

**参考**:
- [Go Concurrency Patterns](https://go.dev/blog/pipelines)
- [acme.sh 文档](https://github.com/acmesh-official/acme.sh/wiki)
- [alidns API 错误码](https://help.aliyun.com/zh/dns/developer-reference/error-codes)

---

### 维度 4:易用性(Usability)🟢

**关注**:用户体验、首启动、文档组织

| 子项 | 内容 |
|------|------|
| U1 | **首启动体验** —— `git clone → docker compose up` 用户看到什么?引导清晰吗? |
| U2 | **错误信息友好度** —— "DDNS sync failed: stage=detect v6 err=..." 用户能知道下一步做什么吗? |
| U3 | **面板 UI 一致性** —— 卡片风格、按钮、错误展示是否统一 |
| U4 | **mobileconfig 流程** —— 用户 iPhone 扫码 → 装完 → 拨 VPN,任何一步失败有提示吗 |
| U5 | **文档组织** —— README / design / release-notes / proposal 之间的关系,新人 1 小时能上手吗 |
| U6 | **可观测性** —— 容器出问题怎么 debug?日志在哪?能 grep 吗 |
| U7 | **国际化** —— 面板中英文 / 多用户场景 / 时区显示 |
| U8 | **辅助功能(a11y)** —— 屏幕阅读器 / 键盘导航 / 颜色对比 |

**参考**:
- [Nielsen 10 Usability Heuristics](https://www.nngroup.com/articles/ten-usability-heuristics/)
- [Web Content Accessibility Guidelines (WCAG)](https://www.w3.org/WAI/standards-guidelines/wcag/)

---

### 维度 5:借鉴优化(Borrow & Improve)🌟

**关注**:参考领域标杆,看我们能不能用更少代码做更好的事

**广度 vs 深度**:本维度默认**深度优先**——挑 2-3 个用户最感知的核心组件深入。

| 组件 | 我们当前实现 | 领域标杆 | 借鉴方向 |
|------|------------|---------|---------|
| **Web 框架** | stdlib `net/http` + 手写 mux | [chi](https://github.com/go-chi/chi) / [gin](https://github.com/gin-gonic/gin) | 路由分组 / middleware 生态 |
| **配置管理** | env + 手写 Load() | [spf13/viper](https://github.com/spf13/viper) / [caarlos0/env](https://github.com/caarlos0/env) | struct tag / 自动校验 |
| **结构化日志** | log/slog | [uber-go/zap](https://github.com/uber-go/zap) / [rs/zerolog](https://github.com/rs/zerolog) | context 透传 / 字段追踪 |
| **DDNS 客户端** | 自写 | [ddns-go](https://github.com/jeessy2/ddns-go) / [ddclient](https://github.com/ddclient/ddclient) | 多 provider / IPv6 / 探测策略 |
| **证书管理** | entrypoint 调 acme.sh | [caddy](https://github.com/caddyserver/caddy) / [traefik](https://github.com/traefik/traefik) | hot reload / 多 CA |
| **用户/凭证存储** | sqlite + 手写 | [dex](https://github.com/dexidp/dex) / [authentik](https://github.com/goauthentik/authentik) | 密码 hash 算法 / RBAC |
| **tc 限速** | 手写 tc 命令 | nftables examples | IPv6 支持 / 多用户公平 |
| **HTML 模板** | html/template | [templ](https://github.com/a-h/templ) / [jet](https://github.com/CloudyKit/jet) | 类型安全 |
| **测试** | stdlib testing | [testify](https://github.com/stretchr/testify) / [gomock](https://github.com/uber-go/mock) | assertion / mock |
| **migration** | ?(没看到) | [golang-migrate/migrate](https://github.com/golang-migrate/migrate) | schema 版本管理 |

**借鉴原则**:
1. **不照搬**——我们的需求可能跟标杆不同,先理解 why,再决定是否用
2. **不堆依赖**——每加一个 dep 是新的维护成本,小项目慎重
3. **保留简化**——如果自研已经够用,**不换**;只在自研有明显缺陷或标杆明显更优时才换

---

## 审计节奏

| 周 | 维度 | 产出 | 状态 |
|----|------|------|------|
| W1 | 安全 + 合规 | `docs/audit-2026-09-security.md` | 🔴 进行中 |
| W2 | 正确性 | `docs/audit-2026-09-correctness.md` | ⏸️ |
| W3 | 易用性 | `docs/audit-2026-09-usability.md` | ⏸️ |
| W4+ | 借鉴优化 | `docs/audit-2026-09-borrow.md` | ⏸️ |

**节奏说明**:
- 每周一份独立报告,**可评审、可批驳、可调整**
- 不批量改代码 —— 一个 issue 一个 PR,可回滚
- 不强求"一次做完"——审计是持续过程,不是一次性工程

---

## 报告格式(统一模板)

每份报告 `docs/audit-YYYY-MM-<dim>.md`:

```markdown
# [维度] 审计报告 — YYYY-MM-DD

## 范围
(列出审计的代码路径、配置文件、文档)

## 工具 / 参考
(列出参考的官方文档、标杆项目、RFC 编号)

## 发现

### [SEVERITY-HIGH] ISSUE-001: <title>
- **位置**: `file://path/to/file:L10-L20`
- **问题**: <详细描述>
- **参考**: <官方文档 / RFC / 标杆项目链接>
- **风险**: <实际场景下的危害>
- **修复方案**: <具体改法 / diff 思路>
- **工作量**: 0.5d

### [SEVERITY-MED] ISSUE-002: ...

### [SEVERITY-LOW] ISSUE-003: ...

## 不在范围内
(故意没看的部分,留作下一批或 v3)

## 建议优先级
1. 先修什么(本周 / 下周)
2. 延后什么(P2 / P3)
3. 永不修(明确不接受 / 设计妥协)
```

**严重度定义**:
- **HIGH**:有可被利用的安全漏洞 / 用户实际损失(数据泄露、服务不可用)
- **MED**:有 bug 但难触发 / 性能问题 / 用户体验差但不致命
- **LOW**:代码味道 / 可读性 / 跟最佳实践不符但不影响功能

---

## 借鉴工作流(每个组件)

1. **静态读标杆** —— clone 下来读源码,理解核心架构
2. **差异列表** —— 标杆做了什么,我们没做
3. **判断 ROI** —— 借鉴的收益 vs 引入成本(学习曲线 / 依赖体积 / 行为变更)
4. **方案文档** —— 如果决定借鉴,产出"借鉴方案",列出代码改动 + 测试 + 回滚方案
5. **实施** —— 不批量,一个组件一个 PR

---

## 不在审计范围

明确**不做**的事,避免 scope creep:

- ❌ 重写整个项目用其他语言 / 框架
- ❌ 把所有 stdlib 换成第三方库
- ❌ "为了美观" 重写所有注释
- ❌ 跟 GitHub 上其他类似项目做"功能 PK"(我们定位是**轻量 IKEv2 面板**,不是 caddy / traefik)

---

## 后续动作

读完本框架后,**第一步是评审本框架本身**:
- 5 个维度划分是否合理?
- 借鉴优化维度挑的标杆项目是否恰当?
- 严重度定义是否符合项目期望?

如果框架 OK,下一步直接进 **W1 安全合规审计**,产出 `docs/audit-2026-09-security.md`。
