# 审计汇总 + Top-10 — 2026-09

> 用途:**5 分钟看完所有审计的关键问题**,挑要修的
> 关联:7 份专项审计报告 + audit-framework

---

## 总览

| 维度 | 报告 | Issue 数 | HIGH | MED | LOW |
|------|------|---------|------|-----|-----|
| 综合(我自查) | [audit-2026-09-security.md](file:///opt/ikev2-panel-v2-main/docs/audit-2026-09-security.md) | 15 | 4 | 6 | 5 |
| 证书 / acme.sh | [audit-2026-09-cert.md](file:///opt/ikev2-panel-v2-main/docs/audit-2026-09-cert.md) | 25 | 7 | 8 | 10 |
| 阿里云 API | [audit-2026-09-alidns.md](file:///opt/ikev2-panel-v2-main/docs/audit-2026-09-alidns.md) | 15 | 3 | 7 | 5 |
| strongSwan | [audit-2026-09-strongswan.md](file:///opt/ikev2-panel-v2-main/docs/audit-2026-09-strongswan.md) | 23 | 6 | 12 | 5 |
| Docker | [audit-2026-09-docker.md](file:///opt/ikev2-panel-v2-main/docs/audit-2026-09-docker.md) | 17 | 4 | 7 | 6 |
| Web / 认证 | [audit-2026-09-web.md](file:///opt/ikev2-panel-v2-main/docs/audit-2026-09-web.md) | 17 | 3 | 8 | 6 |
| 存储 / DB | [audit-2026-09-storage.md](file:///opt/ikev2-panel-v2-main/docs/audit-2026-09-storage.md) | 16 | 4 | 6 | 6 |
| 网络 / TC | [audit-2026-09-net.md](file:///opt/ikev2-panel-v2-main/docs/audit-2026-09-net.md) | 19 | 5 | 8 | 6 |
| **合计(去重前)** | | **147** | **36** | **62** | **49** |

**147 个 issue** 是去重前的总数。**实际有大量重复**(同一问题被多个 agent 从不同角度抓到),去重后预估约 **60-80 个 unique issue**。

---

## 去重逻辑

合并策略:

| 重复类型 | 处理 |
|---------|------|
| **同 issue 多 agent 抓到**(如"无 rate limit"被 security + web 抓到) | 引用主报告,工作量为 1 次 |
| **同 issue 不同模块表达**(如"密码明文"被 security + storage 抓到) | 合并到最强描述 |
| **小范围差异**(如 "/var/log 1777" vs "/var/log 权限") | 合并 |

---

## Top-10(用户感知最直接,按"严重度 × 用户可见性 × 修复成本"排序)

### 🔴 Top-10 必读

| # | 标题 | 跨报告 | 用户感知 | 工作量 |
|---|------|--------|----------|--------|
| **1** | **登录端点无 rate limit + 全 POST 无限速** | security S01 + web W03 | 公网部署的暴力破解入口 | **1.0d** |
| **2** | **IKEv2 套件含 MODP2048 + modpnone + SHA1**(违反 RFC 8247 / NIST SP 800-131A) | strongswan HIGH-1/MED-1 + security S02 | 任何 VPN 用户连接都用弱算法 | **0.5d** |
| **3** | **续签失败竞态:cert 切换非 atomic,charon 可能读到半截 PEM** | cert HIGH-5 + strongswan HIGH-6 | 续签后 SA 全部断流 500ms | **0.5d** |
| **4** | **Docker privileged + SYS_ADMIN + host 网络 = 容器逃逸 = 宿主机沦陷** | docker D01/D05 | 任何 RCE 都变成 root on host | **1.0d** |
| **5** | **HTTP 安全 header 全缺失**(CSP/HSTS/X-Frame-Options/X-CTO/Referrer-Policy/Permissions-Policy) | web W01 | 浏览器警告 + clickjacking + XSS 加固缺失 | **0.3d** |
| **6** | **VPN 用户密码明文存 DB + DB 整体无加密**(design §4.3 妥协项) | security S05 + storage D02 | DB 文件泄漏 = 全用户密码裸奔 | **0.1d(声明) + 1.0d(加密)** |
| **7** | **限速仅 IPv4 + 仅 egress,IPv6 用户零限速** | net N01 + strongswan MED-4 | 任何 IPv6-only 用户的限速是空的 | **0.5d** |
| **8** | **RAM 错误码 `RecordNotBelongToRAM` 不存在,字符串匹配过宽** | alidns ISSUE-002 + security S06 | retry 雪崩 / API 配额浪费 | **0.3d** |
| **9** | **acme.sh gitee clone 无 commit pin + apt 清华源无 GPG keyring** | cert HIGH-2 + docker D02/D03 | 供应链投毒风险 | **0.5d** |
| **10** | **无 DDoS 防护 + 无 TCP MSS clamp**(ESP 后 MTU 问题) | net N03/N04 | IKE_SA_INIT flood 可让 charon 崩 / ESP 后大包性能坍塌 | **0.8d** |

### 总工作量

**Top-10 全部修复 = 6.5d**(≈ 1.5 周)

---

## Top-10 详细说明(每条 5 行内)

### 1. 登录 / POST 无限速
- **位置**:[handlers_auth.go:60-67](file:///opt/ikev2-panel-v2-main/internal/web/handlers_auth.go#L60-L67) + 全 POST 路由
- **修复**:内存 `map[ip]struct{count, firstFail, lockedUntil}` + 5 次失败锁 5 分钟 + 持久化到 `/data/panel-state/ratelimit.json`
- **参考**:OWASP Authentication Cheatsheet / fail2ban / gorilla/csrf
- **风险**:公网 8443 是 brute force 入口

### 2. IKEv2 套件去弱算法
- **位置**:[swanctl-ipv6-only.conf:44, 53](file:///opt/ikev2-panel-v2-main/configs/swanctl-ipv6-only.conf#L44)
- **修复**:删除 `modp2048` / `modpnone`,加入 `modp3072` / `curve25519` / `ecp384`;关闭 strongSwan 编译的 `--enable-sha1 --enable-md5`
- **参考**:[RFC 8247](https://datatracker.ietf.org/doc/html/rfc8247) / strongSwan Security Recommendations
- **风险**:2025+ NIST 标记 MODP2048 legacy,modpnone 无 PFS 违反 RFC 8247 §3.1

### 3. 续签 cert 切换非 atomic
- **位置**:[Dockerfile](file:///opt/ikev2-panel-v2-main/Dockerfile) + [scripts/entrypoint.sh](file:///opt/ikev2-panel-v2-main/scripts/entrypoint.sh) LE 章节
- **修复**:用 `swanctl.AtomicWriteFile` + 写入临时文件 + `mv`,charon 端 `--reload-all` 改 `--load-all`(只增不删)
- **参考**:strongSwan 官方推荐 / IKEv2 SA 续期规范
- **风险**:续期后 VPN 全断流 500ms + 可能读到半截 PEM 触发 fatal

### 4. Docker 容器逃逸
- **位置**:[Dockerfile](file:///opt/ikev2-panel-v2-main/Dockerfile) + [docker-compose.yml](file:///opt/ikev2-panel-v2-main/docker-compose.yml)
- **修复**:删除 `SYS_ADMIN`(tc/ip/iptables 都在 NET_ADMIN 内);`cap_drop ALL`;8443 默认 `127.0.0.1` + unix socket(用 socat 暴露给 nginx 反代)
- **参考**:CIS Docker Benchmark §4 / OWASP Docker Cheatsheet
- **风险**:charon 任何 CVE = 宿主机 root

### 5. HTTP 安全 header 全缺失
- **位置**:[server.go](file:///opt/ikev2-panel-v2-main/internal/web/server.go) 全文件
- **修复**:加 `Strict-Transport-Security` + `Content-Security-Policy` + `X-Frame-Options: DENY` + `X-Content-Type-Options: nosniff` + `Referrer-Policy: strict-origin-when-cross-origin` + `Permissions-Policy`
- **参考**:[OWASP Secure Headers Project](https://owasp.org/www-project-secure-headers/)
- **风险**:浏览器无安全加固,任何 XSS 都更容易利用

### 6. VPN 密码明文 + DB 无加密
- **位置**:[users.go:32, 97](file:///opt/ikev2-panel-v2-main/internal/store/users.go#L32) + DB 文件无加密
- **修复**:短期(0.1d)— DB chmod 0600 + release notes 风险声明;中期(1.0d)— SQLCipher 加密整库;长期(2.0d)— 改 strongSwan EAP-TLS
- **参考**:OWASP Password Storage / SQLCipher
- **风险**:`/data/ikev2-panel.db` 泄漏 = 全用户 VPN 密码裸奔

### 7. IPv6 零限速
- **位置**:[scripts/ikev2-updown](file:///opt/ikev2-panel-v2-main/scripts/ikev2-updown) + tc u32 命令
- **修复**:改 `tc flower`(支持 IPv6,Linux 4.1+),改用 src + dst 双 filter(覆盖 upload + download 双向),加 burst 参数
- **参考**:Linux tc-flower man page / wondershaper / OpenWrt sqm-scripts
- **风险**:IPv6-only 用户的限速完全没生效

### 8. RAM 错误码匹配错
- **位置**:[sync.go:554-558](file:///opt/ikev2-panel-v2-main/internal/ddns/sync.go#L554-L558)
- **修复**:解析 JSON response 的 `Code` 字段,白名单 `Forbidden.RAM` / `Forbidden.User` / `Throttling`(限流码),每个 Code 走不同策略
- **参考**:[alidns 错误码表](https://help.aliyun.com/zh/dns/developer-reference/error-codes)
- **风险**:retry 雪崩 + 配额浪费 + 真实错误识别失败

### 9. 供应链无 GPG + commit pin
- **位置**:[Dockerfile:23-24, 149-150, 241-247](file:///opt/ikev2-panel-v2-main/Dockerfile)
- **修复**:显式 import `tuna-archive-keyring.gpg`;acme.sh 改 pinned commit SHA256 校验;strongSwan 已有 MD5 校验可加 SHA256
- **参考**:Debian SecureApt / SLSA L2
- **风险**:镜像源被劫持 = 恶意代码进生产

### 10. DDoS 防护 + TCP MSS
- **位置**:[entrypoint.sh](file:///opt/ikev2-panel-v2-main/scripts/entrypoint.sh) iptables / strongSwan 启动参数
- **修复**:strongSwan 启 `--enable-block-throttle`;iptables `connlimit`(限制每 IP 并发 SA) + `hashlimit`(限制新 SA 速率);加 `iptables -t mangle -A FORWARD -p tcp --tcp-flags SYN,RST SYN -j TCPMSS --clamp-mss-to-pmtu`
- **参考**:strongSwan docs / RFC 4301 §5 / nftables connlimit
- **风险**:IKE_SA_INIT flood 可让 charon 崩 / ESP 后大包触发 PMTUD 黑屏

---

## Top-10 之外的次重要(本周不修但建议下批)

| # | 标题 | 报告 | 工作量 |
|---|------|------|--------|
| 11 | 审计日志完全缺失(登录/CRUD/凭证变更都不记录) | web W10 | 0.5d |
| 12 | 无 DB 备份脚本(用户 DB 丢了就完了) | storage D01 | 0.5d |
| 13 | 无 migration 版本表(schema 演进没法自动化) | storage D03 | 1.0d |
| 14 | strongSwan 编译选项 `--enable-sha1 --enable-md5` | strongswan MED-1 | 0.1d |
| 15 | filelog `enc=1` + 无轮转 + /var/log 1777 → NT-hash 离线爆破材料泄漏 | strongswan HIGH-2 / docker D10 / security S09 | 0.5d |
| 16 | VICI list-sas 流只处理 list-sa 事件,UI 列表最延迟 5 分钟 | strongswan MED-5 | 0.5d |
| 17 | `swanctl --load-all` 跑两次(第一次必失败 + 第二次条件不成立) | strongswan HIGH-5 | 0.3d |
| 18 | 全 POST 端点 input validation 缺失(trim / 范围 / DB CHECK) | web W08 / security S14 | 0.5d |
| 19 | Cookie Secure 依赖配置项 + 无 HTTPS 强制跳 | web W07 / security S08 | 0.3d |
| 20 | `/users/{id}/android` 仍读 `?flash=` query string(P1-A 回归) | web W05 | 0.2d |

---

## 累计工作量 vs 项目节奏

| 阶段 | 工作量 | 说明 |
|------|--------|------|
| **Top-10** | **6.5d** | 本周建议全修 |
| Top-11~20 | 4.7d | 下周修 |
| Top-21~50(预估) | ~8d | 本月修 |
| 50+ / LOW | ~5d | 下季/有空再修 |
| **合计** | **~24d** | ≈ 5 周 |

**项目当前 v2.84 状态**:功能性 100% 完整。审计揭示**主要是"实现质量"问题**(安全性 / 健壮性 / 借鉴最佳实践),不是"功能缺失"。

---

## 我的建议执行顺序

```
本周(W1, 6.5d):
  Day 1-2: #1 rate limit + #2 IKEv2 套件 (高频 + 用户最感知)
  Day 3:   #4 Docker 容器逃逸 + #9 供应链 GPG (基础设施)
  Day 4:   #5 HTTP 安全 header (基础卫生)
  Day 5:   #6 VPN 密码短方案(chmod 0600 + 声明)
  Day 6:   #7 IPv6 限速 + #8 RAM 错误码 (DDNS 路径)
  Day 7:   #10 DDoS / MSS clamp
```

**关键 PR 顺序**:安全 → 加密 → 兼容性 → 健壮性,这样不破现有用户。

---

## 不在 Top-10 但值得注意的"设计妥协"

一些**已记录为 design 妥协、不需要修**但你应该知道的:

- `users.password` 明文 = strongSwan EAP-MSCHAPv2 协议需要(无 hash 替代)
- `tc u32` 不支持 IPv6 = strongSwan 自身 upstream 限制,等 Linux 6.x 内核全面普及后改 flower
- `restricted` 部分设计 = 跟 systemd 兼容性问题
- 等等

详见各报告"不在范围内"章节。

---

## 下一步

1. **你评审 Top-10**:挑要修的(全部 / 部分 / 按风险偏好)
2. 我**按你选的 issue 起 PR**(一个一个改,不批量)
3. PR 完成 → 在 release-notes 加 changelog
4. Top-11~20 下周排期

或者**你只挑最重要的几个**(比如只修 #1 + #2 + #4),其他 P1/P2 排下批。

你怎么看?
