# 安全合规审计报告 — 2026-09

> 维度:**合规性 + 安全性**(审计框架第 1 阶段)
> 审计员:项目组自查
> 范围:`/opt/ikev2-panel-v2-main/` 全部代码 + Dockerfile + 文档
> 时间:2026-09-18
> 状态:**草案,等待评审**

---

## 范围

| 路径 | 类别 |
|------|------|
| `Dockerfile` | 镜像构建 / 基础镜像 / 供应链 |
| `internal/auth/*` | 密码 hash / session / CSRF |
| `internal/store/*` | DB schema / SQL 注入 / 用户密码存储 |
| `internal/web/handlers_auth.go` | 登录流程 / 限速 |
| `internal/web/handlers_*.go` | 表单校验 / XSS / CSRF token |
| `internal/swanctl/writer.go` | swanctl 用户密码文件权限 |
| `internal/cert/generate.go` | CA / 服务端证书权限 |
| `configs/swanctl-ipv6-only.conf` | IKEv2 加密套件 |
| `internal/dns/aliyun.go` | 阿里云 API 签名 / 错误处理 |
| `internal/ddns/sync.go` | RAM 权限错误识别 |
| `internal/panelstate/state.go` | 凭证文件权限 |
| `scripts/entrypoint.sh` | 启动初始化 / 文件权限 |
| `go.mod` | 依赖 license |

## 工具 / 参考

- [RFC 8247 — IKEv2 安全推荐](https://datatracker.ietf.org/doc/html/rfc8247)
- [NIST SP 800-131A — 密码学过渡建议](https://csrc.nist.gov/publications/detail/sp/800-131a/rev-2/final)
- [strongSwan Security Recommendations](https://docs.strongswan.org/docs/strongswanSecurity.html)
- [OWASP Cheat Sheet Series](https://cheatsheetseries.owasp.org/)
- [OWASP Authentication Cheatsheet](https://cheatsheetseries.owasp.org/cheatsheets/Authentication_Cheat_Sheet.html)
- [alidns 错误码表](https://help.aliyun.com/zh/dns/developer-reference/error-codes)
- [CIS Docker Benchmark](https://www.cisecurity.org/benchmark/docker)
- [ChooseALicense](https://choosealicense.com/) / [SPDX License List](https://spdx.org/licenses/)

---

## 发现汇总

按严重度分组:

| 严重度 | 数量 | 关键问题 |
|--------|------|----------|
| **HIGH** | 4 | 登录无 rate limit / MODP2048 + 模-pnone / 镜像无 GPG 校验 / License 缺失 |
| **MED** | 6 | VPN 用户密码明文存 DB / 错误码字符串匹配 / Cookie Secure 行为 / HSTS 缺失 / 运行时日志目录 1777 / GCM 套件排序 |
| **LOW** | 5 | 注释误导 / LastFailedFile 权限 / 自签证书 100 年有效期 / iptables 规则可读性 / 用户名输入长度未限制 |

---

## [SEVERITY-HIGH] ISSUE

### ISSUE-S01:登录端点无 rate limit,暴力破解风险

- **位置**:[internal/web/handlers_auth.go:60-67](file:///opt/ikev2-panel-v2-main/internal/web/handlers_auth.go#L60-L67)
- **问题**:`handleLogin` 收到 POST 后直接 `s.Store.GetAdminByUsername` + `auth.VerifyPassword`,失败仅写 debug log。**完全没有 IP 维度 / 用户名维度的失败计数**。
- **影响**:8443 默认公网 host 网络暴露,攻击者可无限尝试密码组合。bcrypt 慢但 12 字符管理员密码仍有被字典+bcrypt 离线破解的可能。
- **参考**:
  - [OWASP Authentication Cheatsheet §4 — 登录节流](https://cheatsheetseries.owasp.org/cheatsheets/Authentication_Cheat_Sheet.html#login-throttling)
  - [RFC 2616 — 429 Too Many Requests](https://datatracker.ietf.org/doc/html/rfc2616#section-10.4.4)
  - 标杆:[fail2ban](https://github.com/fail2ban/fail2ban) / [OWASP推荐:5 次失败锁 5 分钟](https://owasp.org/www-community/controls/Blocking_Brute_Force_Attacks)
- **修复方案**:
  1. 在 `internal/auth/` 加 `RateLimiter`:
     - 内存 `map[ip]struct{count, firstFail, lockedUntil}` + mutex
     - 5 次失败 → 锁 5 分钟(对应 design §4.1 的承诺)
     - 配置化:`AUTH_MAX_ATTEMPTS=5`、`AUTH_LOCK_MINUTES=5`
  2. handler 入口检查 IP 锁状态,命中 → 直接 429
  3. 加测试:`TestRateLimiter_Lockout`、`TestRateLimiter_PerIP`
  4. 持久化:可选写 `/data/panel-state/ratelimit.json`(容器重启后攻击者不会"洗白")
- **工作量**:1.0d(代码 + 测试 + 文档)
- **优先级**:**本周修**

---

### ISSUE-S02:IKEv2 加密套件仍接受 `modp2048` 和 `-modpnone`

- **位置**:[configs/swanctl-ipv6-only.conf:44, 53](file:///opt/ikev2-panel-v2-main/configs/swanctl-ipv6-only.conf#L44)
- **问题**:ESP/IKE proposals 包含:
  - `aes256-sha256-modp2048` / `aes128-sha256-modp2048`(MODP2048 DH 组)
  - `aes256gcm16-sha256-modpnone` / `aes128-sha256-modpnone`(`-modpnone`,无 PFS)
- **影响**:
  - **MODP2048** 在 [NIST SP 800-131A (2024 update)](https://csrc.nist.gov/publications/detail/sp/800-131a/rev-2/final) 已划入"acceptable but not recommended",**2030 后须淘汰**
  - **modpnone** = 无 PFS(完美前向保密),**违反 RFC 8247 §3.1** 推荐
  - 注释说"为 macOS 追加 modpnone 兜底",但 macOS 13+ / iOS 16+ 都支持 ecp256 / curve25519,**不需要 modpnone 兜底**
- **参考**:
  - [RFC 8247 — IKEv2 Cipher Suites](https://datatracker.ietf.org/doc/html/rfc8247)
  - [strongSwan Security Recommendations § PFS](https://docs.strongswan.org/docs/strongswanSecurity.html)
  - [strongSwan iOS 互操作文档](https://docs.strongswan.org/docs/latest/interop/ios.html)
- **修复方案**:
  ```conf
  # 推荐配置(2026-09 安全基线):
  proposals = aes256gcm16-sha256-ecp256, aes128gcm16-sha256-ecp256,
              aes256gcm16-sha256-curve25519, aes128gcm16-sha256-curve25519,
              aes256-sha256-modp3072, aes128-sha256-modp3072
  esp_proposals = aes256gcm16-sha256-ecp256, ...(同 proposals)
  # 移除 modp2048 和 modpnone
  ```
  - 同步更新 [docs/design.md §9.1](file:///opt/ikev2-panel-v2-main/docs/design.md) 关于 macOS 互操作的说明
  - 测试:macOS 13+ / iOS 16+ 实机能连接(真机验证)
- **工作量**:0.5d(改 conf + 文档 + 真机验证)
- **优先级**:**本周修**

---

### ISSUE-S03:Docker 镜像无 GPG 校验,供应链风险

- **位置**:
  - [Dockerfile:23-24](file:///opt/ikev2-panel-v2-main/Dockerfile#L23-L24)(清华源,无 GPG)
  - [Dockerfile:149-150](file:///opt/ikev2-panel-v2-main/Dockerfile#L149-L150)(runtime 镜像同样)
  - [Dockerfile:241-247](file:///opt/ikev2-panel-v2-main/Dockerfile#L241-L247)(acme.sh 镜像,无 commit 锁定)
- **问题**:
  - apt 源指向 `mirrors.tuna.tsinghua.edu.cn`(国内镜像),**未验证镜像签名** → 如果源被劫持/污染,可能装恶意包
  - acme.sh git clone 用了 `--depth 1` 但没指定 commit/tag → 拉到的版本不可复现
  - strongSwan MD5 校验(✓)是好的,但只校验了源码包,**编译后的二进制没签名**
- **影响**:CI/CD / 镜像分发 / 升级时可能被注入恶意代码
- **参考**:
  - [Docker Security Cheatsheet — 镜像签名](https://cheatsheetseries.owasp.org/cheatsheets/Docker_Security_Cheat_Sheet.html)
  - [CIS Docker Benchmark §4.1 — 验证镜像](https://www.cisecurity.org/benchmark/docker)
  - [Docker Content Trust](https://docs.docker.com/engine/security/trust/)
- **修复方案**:
  1. **apt 源保留 GPG 校验**(默认 debian-slim 的 sources.list 是 signed 的,只要 `apt-get update` 不报错就算通过)
  2. **加 `set -o pipefail` + 镜像源 GPG keyring 显式 import**(`/etc/apt/keyrings/tuna-archive-keyring.gpg`)
  3. **acme.sh 改 pinned commit**:`git clone https://github.com/acmesh-official/acme.sh.git && cd acme.sh && git checkout <sha>`
  4. **(可选)Docker 镜像签名**:用 [cosign](https://github.com/sigstore/cosign) 给镜像签名
- **工作量**:0.5d(改 Dockerfile + 测试镜像构建)
- **优先级**:**本周修**

---

### ISSUE-S04:无 LICENSE 文件,默认版权状态不明确

- **位置**:仓库根目录
- **问题**:`ls LICENSE*` 无输出。Go 默认 = "All rights reserved"(无明确 license)。代码依赖 govici (Apache 2.0) + sqlite (MIT) + go-qrcode (BSD-3) + bcrypt (BSD-3),可使用 MIT/Apache 2.0 兼容 license。
- **影响**:
  - 用户无法合法使用、修改、再分发我们的代码
  - 镜像分发 / 商业使用 / 二次开发都面临法律灰色地带
- **参考**:
  - [ChooseALicense — 推荐 MIT](https://choosealicense.com/licenses/mit/)
  - [SPDX License List](https://spdx.org/licenses/)
- **修复方案**:
  1. 加 `LICENSE` 文件(MIT)
  2. README 顶部加 `License: MIT` badge
  3. 重要 source 文件加 SPDX header:
     ```go
     // SPDX-License-Identifier: MIT
     ```
- **工作量**:0.5d
- **优先级**:**本周修**

---

## [SEVERITY-MED] ISSUE

### ISSUE-S05:VPN 用户密码明文存 DB,泄露则全裸

- **位置**:
  - [internal/store/users.go:32, 97](file:///opt/ikev2-panel-v2-main/internal/store/users.go#L32)(`u.Password` 直接入库)
  - [internal/auth/password.go:23-24](file:///opt/ikev2-panel-v2-main/internal/auth/password.go#L23-L24)(注释承认 design §4.3 妥协)
- **问题**:VPN 用户密码(`users.password`)明文存 sqlite,因为 strongSwan EAP-MSCHAPv2 需要明文做密码校验。**如果 DB 文件泄漏,所有用户 VPN 密码裸奔**。
- **影响**:
  - `/data/ikev2-panel.db` 被攻击者拿到 = 拿到所有用户 VPN 凭证
  - 攻击者可"撞库"——其他网站如果用户复用了同密码,就连锁沦陷
- **参考**:
  - [OWASP Password Storage Cheatsheet](https://cheatsheetseries.owasp.org/cheatsheets/Password_Storage_Cheat_Sheet.html)
  - [HashiCorp Vault — 静态加密](https://www.vaultproject.io/docs/secrets/db)
- **修复方案**(权衡):
  1. **方案 A(轻量)**:**加密静态** —— 用一个容器启动时随机生成的 key(写到 host mounted file),对 DB password 列做 AES-GCM 加密。攻击者拿到 DB + 加密 key = 仍能解密;只拿到 DB = 看不到明文
  2. **方案 B(治本)**:**改认证机制** —— strongSwan EAP-TLS(证书认证),但用户体验变差(mobileconfig 配置变复杂)
  3. **方案 C(轻量)**:**DB 加密** —— 用 [SQLite Encryption Extension](https://www.sqlite.org/see/) 或 [modernc.org/sqlite 加密扩展](https://gitlab.com/cznic/sqlite),整库 AES 加密
  4. **方案 D(临时)**:**加密 chmod 0300** —— DB 文件 chmod 0300(root only),降低被同主机其他用户读到的风险
  5. **明确化风险声明**:release notes + README 警告:"本项目为兼容 strongSwan EAP-MSCHAPv2 协议,VPN 用户密码以加密形式存储..."(即使方案 A 也要诚实声明)
- **建议**:先做方案 D(0.1d),加方案 E(risk declaration),后续 v3 实施方案 C
- **工作量**:0.1d(D+E) / 1.0d(A) / 2.0d(C)
- **优先级**:**本周修 D+E**,P2 排期 C

---

### ISSUE-S06:RAM 权限错误用字符串匹配,易误判

- **位置**:[internal/ddns/sync.go:554-557](file:///opt/ikev2-panel-v2-main/internal/ddns/sync.go#L554-L557)
  ```go
  if strings.Contains(err.Error(), "RecordNotBelongToRAM") ||
      strings.Contains(err.Error(), "Forbidden") {
  ```
- **问题**:
  - **`RecordNotBelongToRAM` 错误码可能是臆造** —— 需要查 [alidns 错误码表](https://help.aliyun.com/zh/dns/developer-reference/error-codes) 确认
  - **`Forbidden` 太宽泛** —— 任何含 "Forbidden" 的字符串(包括临时网络中断的报错)都会被误判为权限错误
- **影响**:
  - 真实权限错误没被识别 → 反复 retry 14 秒 + 浪费 API 配额
  - 临时错误被误判为权限 → 跳过 retry,真的需要 retry 时反而不重试
- **参考**:
  - [alidns 错误码表](https://help.aliyun.com/zh/dns/developer-reference/error-codes)
  - [阿里云 OpenAPI 错误码规范](https://help.aliyun.com/zh/sdk/developer-reference/error-codes)
- **修复方案**:
  1. **核对错误码**:查阿里云官方文档,确认 RAM 权限相关的 Code(应该是 `Forbidden.RAM` 或 `Forbidden.User` 之类)
  2. **解析 JSON 错误**:`alidns API error: code=Forbidden msg=...` 是已知格式,正则提取 `code=` 字段精确匹配
  3. **改进 retry 策略**:阿里云 API 限流码(`Throttling`)应该长 backoff,不只是无限重试
- **工作量**:0.5d(查文档 + 改代码 + 加测试)
- **优先级**:**本周修**

---

### ISSUE-S07:HSTS 缺失,中间人攻击风险

- **位置**:`internal/web/server.go`(整文件)
- **问题**:web server 没发 `Strict-Transport-Security` header。客户端第一次访问(可能是 `http://`)就被中间人降级,后续即使切到 HTTPS 也可能被骗。
- **影响**:8443 直接公网暴露,任何 HTTP→HTTPS 跳转前的请求都可被劫持。
- **参考**:
  - [OWASP HSTS Cheatsheet](https://cheatsheetseries.owasp.org/cheatsheets/HTTP_Strict_Transport_Security_Cheat_Sheet.html)
  - [RFC 6797](https://datatracker.ietf.org/doc/html/rfc6797)
- **修复方案**:
  1. 在 server.go middleware 里加:
     ```go
     w.Header().Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
     ```
  2. **不要 preload**(没必要,先保证能用)
  3. 测试:curl -I 看 header 在
- **工作量**:0.1d
- **优先级**:**本周修**

---

### ISSUE-S08:Cookie Secure 行为依赖 `s.Secure` 字段,需校验实际生效

- **位置**:
  - [internal/auth/session.go:28](file:///opt/ikev2-panel-v2-main/internal/auth/session.go#L28)(`Secure: secure`)
  - `s.Secure` 怎么赋值?(`internal/config/config.go` L74: `CookieSecure: getbool("IKEV2_COOKIE_SECURE", true)`)
- **问题**:
  - 默认 `IKEV2_COOKIE_SECURE=true` — 但如果用户通过 `http://` 访问(假设他们没自动跳转),cookie 根本不会被发送(session 失效)
  - 如果用户误设 `IKEV2_COOKIE_SECURE=false`,cookie 在 HTTP 也发 → 中间人攻击
- **影响**:容器误配置 → 登录态被劫持
- **参考**:
  - [OWASP Cookie Security Cheatsheet](https://cheatsheetseries.owasp.org/cheatsheets/Session_Management_Cheat_Sheet.html)
- **修复方案**:
  1. **强制 HTTPS 检测**:server.go 检查 `r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"`,如果都不满足且不是本地 → 302 → `https://` 同 URL
  2. **Cookie Secure 强制 true**:`CookieSecure` 配置项去掉,代码里写死 true,只允许通过 TLS / trusted proxy 接受
  3. **日志**:启动时打印 `CookieSecure=...`,用户能看到
- **工作量**:0.3d
- **优先级**:下批修

---

### ISSUE-S09:`/var/log` chmod 1777,任何用户可写

- **位置**:[Dockerfile:228](file:///opt/ikev2-panel-v2-main/Dockerfile#L228)
- **问题**:`chmod 1777 /var/log` 让任何进程 / 容器内用户能写日志文件。
- **影响**:
  - 攻击者如果拿到容器内非 root shell,可以通过日志目录做 symlink attack
  - 日志文件可能被恶意污染
- **参考**:
  - [CIS Docker Benchmark §4 — 容器配置](https://www.cisecurity.org/benchmark/docker)
- **修复方案**:
  1. **删除 chmod 1777**,用 debian-slim 默认 0755
  2. 创建专用的 `/var/log/ikev2-panel/` 目录,chown root:root, chmod 0755
  3. strongSwan filelog 也写到子目录而不是 /var/log/syslog
- **工作量**:0.1d
- **优先级**:下批修

---

### ISSUE-S10:敏感错误信息可能泄漏内部信息

- **位置**:
  - [internal/ddns/sync.go:556](file:///opt/ikev2-panel-v2-main/internal/ddns/sync.go#L556)(直接返回 error string 给用户)
  - `http.Error(w, "failed to update state: "+err.Error(), http.StatusInternalServerError)` 类模式
- **问题**:用户看到错误信息包含阿里云 RequestID / 内部 API endpoint / 错误码,可能用于攻击者探测。
- **影响**:信息泄露,辅助攻击
- **参考**:
  - [OWASP Error Handling Cheatsheet](https://cheatsheetseries.owasp.org/cheatsheets/Error_Handling_Cheat_Sheet.html)
- **修复方案**:
  1. 通用错误用 "internal error" 替代详细信息
  2. 详细错误写到 server log,不返回 client
  3. 用户操作错误(400 类)可以详细,因为是用户行为问题
- **工作量**:0.3d
- **优先级**:下批修

---

## [SEVERITY-LOW] ISSUE

### ISSUE-S11:aliyun.go 注释误导(HMAC-SHA256 vs SHA1)

- **位置**:[internal/dns/aliyun.go:193-198 vs L228](file:///opt/ikev2-panel-v2-main/internal/dns/aliyun.go#L193-L198)
- **问题**:注释说"HMAC-SHA256",代码用 `sha1.New` + `SignatureMethod=HMAC-SHA1`。**实际 RPC v1 标准就是 SHA1**,所以代码对,但注释误导。
- **修复**:注释改为 HMAC-SHA1,或升级到 v3 签名(用 SHA256)。
- **工作量**:0.05d
- **优先级**:顺手改

---

### ISSUE-S12:`LAST_DDNS_FAILED` / `LAST_RENEW_FAILED` 文件权限 0644

- **位置**:
  - [internal/ddns/sync.go:608](file:///opt/ikev2-panel-v2-main/internal/ddns/sync.go#L608)(`os.WriteFile(... 0o644)`)
  - entrypoint.sh `LAST_RENEW_FAILED` 类似
- **问题**:0644 = 同主机其他用户可读。文件内容含阿里云 RequestID / 错误细节,信息泄露。
- **修复**:改 `0o600`
- **工作量**:0.05d
- **优先级**:下批

---

### ISSUE-S13:自签 CA / 证书默认 100 年有效期过长

- **位置**:`internal/cert/generate.go`(需要确认 L120-130 区域)
- **问题**:通常自签 CA 用 10 年,服务证书 1 年。100 年太长,一旦泄漏很难作废。
- **修复**:CA 10 年,服务证书 397 天(Apple 要求)
- **工作量**:0.1d
- **优先级**:下批

---

### ISSUE-S14:用户名 / 备注输入长度未限制

- **位置**:`internal/web/handlers_users.go`(创建用户 handler)
- **问题**:用户名字段无 max length 校验,DB schema 可能限制但代码层不挡。攻击者可以创建 1MB 用户名。
- **修复**:handler 加 `len(u.Username) > 32 → 400`,同理 note
- **工作量**:0.1d
- **优先级**:下批

---

### ISSUE-S15:iptables 规则在 `entrypoint.sh` 拼接,可读性差

- **位置**:`scripts/entrypoint.sh`
- **问题**:`iptables -t nat -A POSTROUTING -s ${subnet} -o ${iface} -j MASQUERADE` 类命令用 shell 拼接,出错时难调试。
- **修复**:封装到 helper 函数,加 set -x 调试
- **工作量**:0.3d
- **优先级**:非安全,记入 Phase 4(借鉴优化)考虑用 nftables 替代

---

## 不在范围内

故意**不审计**:

- ❌ strongSwan 内部 charon 实现(不是我们的代码)
- ❌ acme.sh 实现(外部依赖)
- ❌ Linux 内核 IKEv2 stack(没用)

## 建议优先级

### 本周(W1)

| 优先级 | ISSUE | 工作量 |
|--------|-------|--------|
| 🔴 | **S01 登录 rate limit** | 1.0d |
| 🔴 | **S02 IKEv2 加密套件去 modp2048/modpnone** | 0.5d |
| 🔴 | **S03 镜像 GPG 校验** | 0.5d |
| 🔴 | **S04 LICENSE 文件** | 0.5d |
| 🟡 | **S06 RAM 错误码精确化** | 0.5d |
| 🟡 | **S07 HSTS** | 0.1d |
| 🟡 | **S05 方案 D(权限)+ E(risk declaration)** | 0.1d |
| ⚪ | **S11 注释顺手改** | 0.05d |
| **小计** | | **3.25d** |

### 下周(W2 - 跟 Phase 2 正确性一起做)

- S08 Cookie Secure 强制
- S09 /var/log 权限
- S10 错误信息脱敏
- S12 LAST_*_FAILED 文件权限
- S13 自签 CA 有效期
- S14 输入长度限制

### P2(后续版本)

- S05 方案 C(DB 加密)
- S15 iptables 重构 / nftables 替代

---

## 借鉴参考(供方案决策用)

- **登录限速**:
  - [OWASP 推荐](https://cheatsheetseries.owasp.org/cheatsheets/Authentication_Cheat_Sheet.html#login-throttling)
  - [fail2ban](https://github.com/fail2ban/fail2ban) — 业界标准
  - [exponential backoff pattern](https://en.wikipedia.org/wiki/Exponential_backoff)
- **加密套件**:
  - [Mozilla SSL Configuration Generator](https://ssl-config.mozilla.org/)
  - [strongSwan Security Recommendations](https://docs.strongswan.org/docs/strongswanSecurity.html)
- **Docker 安全**:
  - [Docker Security Cheatsheet](https://cheatsheetseries.owasp.org/cheatsheets/Docker_Security_Cheat_Sheet.html)
  - [Docker Bench Security](https://github.com/docker/docker-bench-security)
- **密码存储**:
  - [OWASP Password Storage Cheatsheet](https://cheatsheetseries.owasp.org/cheatsheets/Password_Storage_Cheat_Sheet.html)
  - [HashiCorp Vault](https://www.vaultproject.io/docs/secrets/db)

---

## 完成定义

- [ ] S01 / S02 / S03 / S04 修复(本周)
- [ ] S05/S06/S07 修复(本周)
- [ ] 每个修复有对应 test 覆盖
- [ ] release-notes 记录修复内容
- [ ] 用户可见的安全警告加进 README

---

## 下一步

1. 你评审本报告,挑要修的 issue
2. 我**不批量改** — 一个 issue 一个 PR
3. 修完再回头看**问题是否解决**(再跑测试 + 真机验证)
