# 阿里云 DNS (alidns) API + DDNS 集成审计报告 — 2026-09-18

> 维护者:ikev2-panel-v2 项目组(第二轮 / 重新发起)
> 关联:[docs/audit-framework.md](file:///opt/ikev2-panel-v2-main/docs/audit-framework.md) (审计框架)
> [docs/audit-2026-09-security.md](file:///opt/ikev2-panel-v2-main/docs/audit-2026-09-security.md) (安全审计 — 已合并部分阿里云 RAM 权限)
> [docs/design.md §19](file:///opt/ikev2-panel-v2-main/docs/design.md) (DDNS 设计)

---

## 范围

第二轮,只审计阿里云 DNS (alidns) API 集成 + DDNS 同步链路。

| 模块 | 文件 | 行数 | 角色 |
|------|------|------|------|
| 签名 + HTTP | [internal/dns/aliyun.go](file:///opt/ikev2-panel-v2-main/internal/dns/aliyun.go) | 276 | 手写 alidns RPC v1 客户端 (HMAC-SHA1) |
| 同步器 | [internal/ddns/sync.go](file:///opt/ikev2-panel-v2-main/internal/ddns/sync.go) | 628 | 家庭感知 (v4/v6/dual) + 节流 + retry + 并发 upsert |
| 状态文件 | [internal/ddns/statefile.go](file:///opt/ikev2-panel-v2-main/internal/ddns/statefile.go) | 187 | INI-style runtime state, atomic write + v2-83 迁移 |
| IPv4 探测 | [internal/swanctl/ipv4watch.go](file:///opt/ikev2-panel-v2-main/internal/swanctl/ipv4watch.go) | 186 | `net.Dial("udp", target:80")` 拿 LocalAddr |
| IPv6 探测 | [internal/swanctl/ipv6watch.go](file:///opt/ikev2-panel-v2-main/internal/swanctl/ipv6watch.go) | 294 | 解析 `/proc/net/if_inet6` |
| 配置 | [internal/config/config.go](file:///opt/ikev2-panel-v2-main/internal/config/config.go) | 247 | env 读取 + 凭证 4 级优先级 |
| 凭证 | [internal/panelstate/state.go](file:///opt/ikev2-panel-v2-main/internal/panelstate/state.go) | 218 | `/data/panel-state/aliyun.creds` 读写 |

---

## 工具 / 参考

### 官方文档
- alidns API 入口:[DescribeDomainRecords](https://help.aliyun.com/zh/dns/api-alidns-2015-01-09-describedomainrecords) / [UpdateDomainRecord](https://help.aliyun.com/zh/dns/api-alidns-2015-01-09-updatedomainrecord)
- 签名机制:**[V2 RPC (HMAC-SHA1)](https://help.aliyun.com/zh/cmn/developer-reference/signature-mechanism)** (当前项目用) vs **[V3 ACS3-HMAC-SHA256](https://help.aliyun.com/zh/sdk/product-overview/v3-request-structure-and-signature)** (推荐)
- 错误码:**[alidns 公共错误码](https://help.aliyun.com/zh/dns/developer-reference/error-codes)** / **[全局错误码](https://next.api.aliyun.com/global-error-code)** (含 `SignatureNonceUsed`, `Throttling.User`, `Forbidden.RAM`)
- RAM 最佳实践:**[阿里云 RAM 用户最佳实践](https://help.aliyun.com/zh/ram/getting-started/best-practices-for-ram-users)**

### 标杆项目
- **[ddns-go (jeessy2)](https://github.com/jeessy2/ddns-go)** — Go,17.3k stars,多 provider + IPv6 + 双栈
- **[ddclient](https://github.com/ddclient/ddclient)** — Perl 老牌 DDNS,多 provider
- **[terraform-provider-alicloud](https://github.com/aliyun/terraform-provider-alicloud)** — 阿里云官方,生产级 RPC 调用实现
- **[alibaba-cloud-sdk-go](https://github.com/aliyun/alibaba-cloud-sdk-go)** — 官方 Go SDK,可读作"正确签名"参考

### 标准
- **[RFC 3986 — URI Generic Syntax](https://tools.ietf.org/html/rfc3986)** (签名 URL 编码规则)
- **[RFC 2104 — HMAC](https://www.ietf.org/rfc/rfc2104.txt)**
- **[RFC 2119 — MUST / SHOULD / MAY](https://www.ietf.org/rfc/rfc2119.txt)**
- **OWASP CVSS 3.1**(严重度评级)

---

## 严重度统计

| 等级 | 数量 |
|------|------|
| HIGH | **3** |
| MED | **7** |
| LOW | **5** |
| **合计** | **15** |

---

## 发现

### [SEVERITY-HIGH] ISSUE-001: 签名用 HMAC-SHA1 而非 HMAC-SHA256 (V2 而非 V3)

- **位置**: [internal/dns/aliyun.go:176](file:///opt/ikev2-panel-v2-main/internal/dns/aliyun.go#L176) (`SignatureMethod=HMAC-SHA1`) + [internal/dns/aliyun.go:260](file:///opt/ikev2-panel-v2-main/internal/dns/aliyun.go#L260) (`hmac.New(sha1.New, ...)`)
- **问题**: 代码使用阿里云 V2 RPC 签名 (`SignatureMethod=HMAC-SHA1` + `SignatureVersion=1.0`)。阿里云 2026 年起**推荐迁移到 V3 ACS3-HMAC-SHA256**。V2 仍兼容,但 SHA1 被 NIST SP 800-131A 标记为**可接受但不再推荐**(legacy use only),且 V3 还引入了 `x-acs-content-sha256`、`x-acs-date` 等更严格的 anti-replay 头。
- **参考**:
  - [阿里云 V3 签名](https://help.aliyun.com/zh/sdk/product-overview/v3-request-structure-and-signature) — "如果当前使用的是 V2 版本,推荐替换为 V3 版本"
  - [NIST SP 800-131A Rev-2](https://csrc.nist.gov/publications/detail/sp/800-131a/rev-2/final) — SHA1 用于 HMAC 仍可接受,但不应再用于新协议
  - [ddns-go util.HmacSignToB64](https://pkg.go.dev/github.com/jeessy2/ddns-go/v6/util) — 默认 `Algorithm = "SDK-HMAC-SHA256"`
  - [terraform-provider-alicloud](https://github.com/aliyun/terraform-provider-alicloud) — 内部 client 已切 V3
- **风险**:
  - SHA1 算法理论上可被 collision 攻击(MD/SHAttered $110K);虽然对 HMAC-SHA1 影响有限,但 V3 已经普及。
  - V2 的 `SignatureNonce` 是 query string 的一部分,URL 路径上任何中间缓存/CDN/访问日志都会记录这个 nonce,降低重放防护强度。V3 把 nonce 移到 header (`x-acs-signature-nonce`),服务器端可拒绝 15 分钟内重复 nonce。
  - 阿里云后续可能弃用 V2(已多次警告)。
- **修复方案**:
  1. 引入 `github.com/alibabacloud-go/openapi-util/service` (`GetAuthorization`),直接用阿里云官签。
  2. 或手写 V3:重写 `signRequest` 改用 SHA256 + `x-acs-*` header + `HashedCanonicalRequest` 模式。
  3. 改造 `call()` 让请求走 header 而不是 query。
- **工作量**: 2-3d (签名重写 + 全链路测试 + SignatureNonceUsed 错误码映射)

---

### [SEVERITY-HIGH] ISSUE-002: 错误码子串匹配 → RAM 误判 + 无 throttling 退避

- **位置**: [internal/ddns/sync.go:554-558](file:///opt/ikev2-panel-v2-main/internal/ddns/sync.go#L554-L558) (`strings.Contains(err.Error(), "RecordNotBelongToRAM") || strings.Contains(err.Error(), "Forbidden")`)
- **问题**:
  1. **`RecordNotBelongToRAM` 错误码并不存在**。阿里云 alidns 实际 RAM 相关错误码是 `Forbidden.RAM`(见官方错误码表)。代码用 `strings.Contains` 拼一个不存在的字符串做匹配,等价于"永远不命中",导致 RAM 权限错误也会被 retry 3 次(白白浪费 API 配额 + 让用户在控制台看到错误)。
  2. **`Forbidden` 子串匹配太宽**。`Forbidden.AccessKeyDisabled`、`Forbidden.RAM`、`Forced.RecordLocked` 等完全不同语义都被一并 catch 为"立即停 retry",而**真正的网络/Throttling 错误反而被 retry 3 次**:
     - `Throttling.User`(`请求频次达到阈值`)
     - `Throttling.Api`
     - `ServiceUnavailable`(临时)
     - `InternalError`(建议重试)
     这些都该按指数退避重试,但代码没有区分。
- **参考**:
  - [alidns 公共错误码](https://help.aliyun.com/zh/dns/developer-reference/error-codes) — 列了 `Forbidden.RAM` 而不是 `RecordNotBelongToRAM`
  - [全局错误码](https://next.api.aliyun.com/global-error-code) — `28 Throttling`,`29 Throttling.Api`,`30 Throttling.User`,`5 InternalError`(建议重试)
  - [alibaba-cloud-sdk-go client/retry](https://github.com/aliyun/alibaba-cloud-sdk-go) — 官方 SDK 把 error 分类为 `Retryable` / `Throttling` / `AccessDenied`,自动退避
- **风险**:
  - RAM 权限错误没有尽早报警,运维可能晚几小时才发现"凭证坏了"。
  - Throttling 错误被固定间隔 1s/4s/9s 重试,可能触发二次 throttling 雪崩(aliyun Throttling.User 通常要等 30-60s)。
- **修复方案**:
  1. 在 `dns/aliyun.go` 解析响应时同时回传 `Code` 字段(已解析),让上层按 code 分类。
  2. 维护一个分类表:
     ```go
     var retryableCodes = map[string]bool{
         "Throttling": true, "Throttling.User": true, "Throttling.Api": true,
         "ServiceUnavailable": true, "InternalError": true,
     }
     var fatalCodes = map[string]bool{
         "Forbidden.RAM": true, "InvalidAccessKeyId.NotFound": true,
         "InvalidAccessKeyId.Inactive": true,
         "Forbidden.AccessKeyDisabled": true, "IncompleteSignature": true,
         "InvalidParameter": true,
     }
     ```
  3. retry 退避改成 5s / 15s / 45s(对齐阿里云 SLA),并加入 jitter。
- **工作量**: 1-2d

---

### [SEVERITY-HIGH] ISSUE-003: DDNS 凭证优先级错配 + `/data/panel-state/aliyun.creds` 写入没清理 env

- **位置**:
  - 优先级:[internal/config/config.go:109-144](file:///opt/ikev2-panel-v2-main/internal/config/config.go#L109-L144) (`loadAliyunCreds` 4 级优先级)
  - DDNS 运行时:[internal/ddns/sync.go:327-333](file:///opt/ikev2-panel-v2-main/internal/ddns/sync.go#L327-L333) (`CredentialGetter` 优先于 env)
- **问题**:
  1. **启动时优先级和运行时优先级不一致**:
     - 启动:`/data/panel-state/aliyun.creds` > `IKEV2_ALIYUN_KEY_ID` > `ALIYUN_ACCESS_KEY_ID` > `Ali_Key` (panelstate 最高)
     - DDNS 运行时 tick:`CredentialGetter` > `cfg.AliyunAccessKeyID/Secret` (env fallback)
     - 但 `CredentialGetter` 是 main.go 注入,如果是返回 nil/false,**DDNS 不会 fallback 到 env** —— 直接 skip。这跟设计说明"运行时优先 getter"不一致,实际上是"getter 不存在 = 完全停摆"。
  2. **凭证切换路径不闭环**:
     - 面板 UI 写 `aliyun.creds` → `CredentialGetter` 读到 → DDNS 下次 tick 用新凭证 ✓
     - 面板 UI 清除凭证(`ClearAliyun`)→ `CredentialGetter` 读 panelstate 内存缓存 → 返回 nil → DDNS skip ✓
     - **但如果凭证文件被外部修改**(运维手工改、备份恢复),`CredentialGetter` 不会重新加载 —— 缓存只在 `LoadAliyun` / `WriteAliyun` / `ClearAliyun` 时更新,文件 mtime 改变无法感知。
  3. **凭证冲突时启动顺序不报错**:
     - 如果 panelstate 和 env 都设了,启动日志只输出 `panelstate`,但用户以为 env 是真正的凭证 —— 调试时容易踩坑。
- **参考**:
  - [RAM 最佳实践 — 单一凭证来源](https://help.aliyun.com/zh/ram/getting-started/best-practices-for-ram-users) — 避免凭证分散在多处
  - [12-Factor App — Config](https://12factor.net/config) — 单一可信源
- **风险**:
  - 运维换凭证后,DDNS 还在用旧凭证(凭证优先级不明确)。
  - 外部 `rm` 凭证文件后,内存缓存仍持有明文 secret,内存 dump 后泄露。
  - panelstate 凭证与 env 凭证同时存在时,谁生效没法从代码看出来。
- **修复方案**:
  1. 把 4 级来源做成"凭证源"对象,统一面板展示。
  2. `CredentialGetter` 加 fallback:返回 `(nil, nil, false)` 时读 env,但打 WARN 日志"panelstate 缺失,fallback 到 env"。
  3. panelstate 文件加 mtime 监听(可选,low-priority)。
  4. 启动日志明确"aliyun creds source: panelstate (overriding env)" 或 "env-new"。
- **工作量**: 1d

---

### [SEVERITY-MED] ISSUE-004: URL 编码手动补丁链脆弱(空格 → %20, 星号 → %2A, %7E → ~)

- **位置**: [internal/dns/aliyun.go:246-249](file:///opt/ikev2-panel-v2-main/internal/dns/aliyun.go#L246-L249)
- **问题**: 注释说"阿里云要求特殊字符按 RFC 3986 编码",实现方式是先 `url.QueryEscape` 再 `ReplaceAll` 三次。这个 hack 在大多数字符上能工作,但有几个 corner case:
  - `url.QueryEscape` 默认对 `~` 不编码(Go 1.17+ 行为变化,Go 早期版本会编码为 `%7E`),ReplaceAll `%7E→~` 偶尔会**重复执行**(无影响,但表明作者对 Go stdlib 行为不清楚)。
  - `+`(空格→加号)在 RFC 3986 严格模式下应该 `%20`,但 `url.QueryEscape` 把空格编码为 `+`,后用 `+→%20` 替换 — OK。
  - 但 **`*` 被错误地替换为 `%2A`**,RFC 3986 实际上 *未保留* `*`,但阿里云文档明确要求编码为 `%2A`。代码这样做是对的,但这不是 `url.QueryEscape` 默认行为(Go 会保留 `*`)。这意味着如果以后有人改用 `PathEscape` 或别的 escape 函数,这个补丁链会失效。
  - **关键弱点**:`RegionId` 等公共参数里有中文/扩展 UTF-8 字符时,`url.QueryEscape` 按字节编码,**正确**;但 canonical 字符串里**参数名也要编码**,代码用的是 `url.QueryEscape(k)`,参数名是 ASCII(AccessKeyId、Action 等),所以 OK。这块的脆弱点更在于"未来如果参数值是中文域名(国际化域名 IDN)",需要走 punycode → xn-- 前缀,不是这里的问题。
- **参考**:
  - [RFC 3986](https://tools.ietf.org/html/rfc3986) — `percentEncode` 规则
  - [Go net/url — url.go](https://go.dev/src/net/url/url.go) — `QueryEscape` 行为
  - [阿里云签名机制 — 编码规则](https://help.aliyun.com/zh/cmn/developer-reference/signature-mechanism) — 明确要求 `+→%20`, `*→%2A`, `%7E→~`
- **风险**:
  - 当前字符集下实际工作。但**注释里说"空格变 +, 但阿里云文档示例里空格用的是 %20"** — 暗示作者不确定。如果后续加字符串含 emoji / 控制字符,可能签名错误(`IncompleteSignature`)。
  - 缺乏单元测试覆盖 unicode 字符(目前测试可能在 ASCII 内)。
- **修复方案**:
  1. 抽出 `percentEncode(s)` 工具函数(对齐官方文档伪代码)。
  2. 写测试用例:中文域名 IDN(punycode 化后)、`!@#$%^&*()`、emoji `🌍`。
  3. 用阿里云官方 Go SDK 的 `sign()` 方法(已 battle-tested)替换。
- **工作量**: 0.5d

---

### [SEVERITY-MED] ISSUE-005: retry 退避硬编码 1s/4s/9s,不区分错误码,缺 jitter

- **位置**: [internal/ddns/sync.go:543-565](file:///opt/ikev2-panel-v2-main/internal/ddns/sync.go#L543-L565)
- **问题**: retry 循环 `time.Sleep(time.Duration(attempt*attempt) * time.Second)` — 1s, 4s, 9s,固定间隔。
  1. **不区分错误类型**:网络瞬断、Throttling、参数错误都按同样间隔重试。
  2. **无 jitter**:多容器同时 DDNS 失败(罕见但可能),同步重试会形成 thundering herd。
  3. **Throttling 间隔不够**:阿里云 `Throttling.User` 通常建议 30-60s 后重试,9s 远不够。
- **参考**:
  - [阿里云 Throttling 错误](https://next.api.aliyun.com/global-error-code) — 28, 29, 30 都需要"稍后重试"
  - [AWS Architecture Blog — Exponential Backoff and Jitter](https://aws.amazon.com/blogs/architecture/exponential-backoff-and-jitter/) — 业界标准
  - [alibaba-cloud-sdk-go DefaultRetryPolicy](https://github.com/aliyun/alibaba-cloud-sdk-go) — 默认 5s → 10s → 20s + jitter
- **风险**:
  - Throttling 重试雪崩(虽然单容器小,概率低,但 multi-tenant 部署可能)
  - 真正的"参数错误"(永远会失败)在 14s 后才放弃
- **修复方案**:
  1. retry policy:网络错误 / Throttling → 5s/15s/45s + 0-30% jitter;其他 → 1s/4s/9s + jitter
  2. 抽 `retryWithBackoff(ctx, maxAttempts int, base, max time.Duration, isRetryable func(error) bool)`
- **工作量**: 0.5d

---

### [SEVERITY-MED] ISSUE-006: IP 探测用 UDP dial,无法覆盖 IPv6,且 blackhole 时不稳定

- **位置**: [internal/swanctl/ipv4watch.go:124-185](file:///opt/ikev2-panel-v2-main/internal/swanctl/ipv4watch.go#L124-L185)
- **问题**: `net.Dial("udp", target+":80")` 拿 `LocalAddr()` 这个方法是 ddclient / ddns-go 的通用做法,**工作原理**(对 UDP,内核只选路由,**不实际发送 SYN**)是对的,但有几个边界:
  1. **UDP blackhole 时不超时**:正常网络下 3s timeout 足够,但**运营商 UDP 黑洞**(很多 ISP 对 UDP 53/80 不直接 RST,内核会重传到 timeout)下,3s 不一定够,且每次都会卡 3s 才知道"探不出来"。Retry 应该更长(如 6s)+ 多个探测目标(8.8.8.8 + 1.1.1.1 + 208.67.222.222 任一成功即可)。
  2. **IPv6 探测走 `/proc/net/if_inet6` 解析,跟 IPv4 探测不同路径**:`ipv6watch.go` 是"读 proc" 而 IPv4 是"dial UDP"。架构不对称,后续要加 "fallback IPv4 / IPv6" 复杂。
  3. **UDP dial 在 IPv6-only 网络返回 IPv4 地址**:`LocalAddr()` 可能是 `::ffff:8.8.8.8`,代码没处理这种映射地址。
  4. **interface 校验逻辑 bug**:如果 dial 出的 IP 是 `100.64.x.x`(CGNAT),`isGlobalV4` 会过滤掉;但如果用户机器**就在 CGNAT 内**,`iface=""` 时根本拿不到公网 IP,代码会反复 WARN 但不告诉用户"考虑用 IKEV2_DDNS_PROBE_TARGET 走内网 IP"。
- **参考**:
  - [ddns-go util.GetRequestIP](https://github.com/jeessy2/ddns-go) — 用 HTTP 接口(`https://api.ipify.org?format=json`)拿 IP,绕过 blackhole
  - [ddclient get_ip_from_cmd](https://github.com/ddclient/ddclient) — 多接口 fallback
  - [Linux ip-route — UDP route selection](https://man7.org/linux/man-pages/man8/ip-route.8.html) — 内核 UDP 路由规则
- **风险**:
  - 某些 ISP(VPN 友好但 UDP 不友好)下 IPv4 探测稳定失败 → DDNS 永远拿不到 IPv4 → 解析记录停在旧值 → 用户连不上
- **修复方案**:
  1. 双探测:UDP dial + HTTP GET `https://api.ipify.org`(超时 5s),任一成功即可
  2. IPv6 也用 HTTP 接口:`https://api64.ipify.org`
  3. dialer 上加 `LocalAddr` 显式指定,避免 `::ffff:` 映射
- **工作量**: 1d

---

### [SEVERITY-MED] ISSUE-007: IPv6 探测假定第一行为目标,多接口场景选择不可预测

- **位置**: [internal/swanctl/ipv6watch.go:147-205](file:///opt/ikev2-panel-v2-main/internal/swanctl/ipv6watch.go#L147-L205)
- **问题**:
  1. **顺序依赖**:遍历 `/proc/net/if_inet6` 取第一条匹配 global v6 的,**接口顺序由内核决定**(一般按 ifindex 升序),不同机器不保证是"出口接口"。
  2. **`iface=""` 时取第一个 global v6**:如果机器有 docker0/lo/eth0 多个,可能取错(虽然过滤了 fe80/fc/fd,但 2000::/3 在多个接口都可能存在,如 VPN 接口 / docker bridge / 6in4 tunnel)。
  3. **隐私扩展地址 vs stable**:注释说"flags: 0x01 = temporary, 0x80 = deprecated"。但 **flags == "10" 检查只跳 deprecated**,**没过滤 temporary**。RFC 7217 隐私扩展地址每个连接会变,如果 DDNS 用了临时地址,会触发频繁更新,被 API 限流。
- **参考**:
  - [RFC 7217 — IPv6 SLAAC Privacy Extensions](https://www.rfc-editor.org/rfc/rfc7217.html)
  - [Linux proc/net/if_inet6 format](https://www.kernel.org/doc/Documentation/networking/ip-sysctl.txt)
- **风险**:
  - DDNS 频繁更新临时地址 → 触发阿里云 Throttling
  - 多接口机器选错 IP → 解析到 docker bridge 内网地址
- **修复方案**:
  1. 显式过滤 temporary flags(`ifaddr6_flags & IFA_F_TEMPORARY`)
  2. `iface` 参数必填(或者有合理默认值);不提供 default 让用户配置
  3. 取第一个 global v6 后,**校验其 prefix 长度 == 64**(SLAAC 特征),排除 /128 这种
- **工作量**: 0.5d

---

### [SEVERITY-MED] ISSUE-008: 并发安全:DDNS tick 共享 `client` 通过 NewAliyunClient 重建,但 AliyunClient.HTTPClient 共享

- **位置**:
  - [internal/ddns/sync.go:346](file:///opt/ikev2-panel-v2-main/internal/ddns/sync.go#L346) (`client := dns.NewAliyunClient(...)` 每 tick 新建)
  - [internal/dns/aliyun.go:75-81](file:///opt/ikev2-panel-v2-main/internal/dns/aliyun.go#L75-L81) (`HTTPClient: &http.Client{Timeout: 10s}`)
- **问题**:
  1. **每次 tick 重建 client**:代码注释把它当优点("支持运行时凭证变更"),但实际上 `http.Client` 内部有 **连接池**(长连接),每 tick 重建 → 每个 tick 都新 TCP 握手 → 浪费资源 + 增加延迟。
  2. **`AliyunClient` 不是 immutable**:`AccessKeyID` / `AccessKeySecret` 字段是 public(导出),且没加锁。如果未来有人加 `SetCredentials` 之类的 API,**并发读写会 data race**。
  3. **`Sync` struct 字段混合**:`client` 在 NewSync 时设成 nil,运行时动态赋值;`detectV6` / `detectV4` 通过 setter 加锁写入,**但 `client` 字段没锁**。目前 `client` 只在 `tick()` 内部局部变量用,看似安全,但 `Sync.client` 这个字段名误导。
- **参考**:
  - [Go net/http Transport — connection pooling](https://pkg.go.dev/net/http#Client) — Transport 持有连接池
  - [Go Concurrency Patterns — Context](https://go.dev/blog/context)
- **风险**:
  - 当前没有并发 bug,但代码风格会让下一个修改者写出 race
  - 高频 DDNS tick(period=10s)会持续吃连接池
- **修复方案**:
  1. 把 `AliyunClient.HTTPClient` 提到外面,做包级共享(`var defaultHTTPClient = &http.Client{Timeout: 10s}`)
  2. 凭证变更走 `SetCredentials(id, secret)` 方法,内部加 RWMutex
  3. `Sync.client` 字段删除,改为方法局部变量
- **工作量**: 0.5d

---

### [SEVERITY-MED] ISSUE-009: 观测性不足:面板只能看到成功/失败,看不到"为什么失败"

- **位置**: [internal/ddns/sync.go:85-105](file:///opt/ikev2-panel-v2-main/internal/ddns/sync.go#L85-L105) (`LastSync` struct)
- **问题**:
  1. `LastSync.V4Error` / `V6Error` 只存**第一个 error 的 Error() 字符串**,但 `Error()` 把所有上下文(`DescribeDomainRecords:` + `HTTP 500: <body>`)压成一个字符串,**面板前端无法结构化展示**(例如"Throttling" 跟 "Forbidden.RAM" 在 UI 上都是红的)。
  2. **没有 error category**:从字符串里 grep `Throttling` 还是 `Forbidden`,取决于调用方。
  3. **没有 RequestID**:阿里云响应里有 `RequestId`(用于工单关联),代码**完全没解析**(见 [aliyun.go:202-213](file:///opt/ikev2-panel-v2-main/internal/dns/aliyun.go#L202-L213) 的 `apiResp` struct 只取 `Code`/`Message`)。
  4. **检测阶段 vs API 阶段混在一起**:面板显示"DDNS failed",用户不知道是探测失败(本地网络)还是 API 失败(凭证/阿里云)。
- **参考**:
  - [ddns-go qp.Display](https://github.com/jeessy2/ddns-go) — UI 显示 last success time + last error
  - [opentelemetry-go — Trace ID propagation](https://opentelemetry.io/docs/languages/go/)
- **风险**:
  - 用户看到 "DDNS failed" 但不知道是网络问题还是阿里云挂了,debug 困难
  - 提工单没有 RequestID,阿里云无法定位
- **修复方案**:
  1. `LastSync` 加结构化字段:`Stage string` (`detect` / `find` / `update`),`APICode string` (原始 error code),`RequestID string`
  2. `dns/aliyun.go` 解析响应时同时回传 `Code`、`Message`、`RequestId`(阿里云响应顶层有)
  3. 面板 UI 按 code 分类显示(绿色=成功 / 黄色=Throttling 等会自愈 / 红色=Forbidden 永久)
- **工作量**: 1d

---

### [SEVERITY-MED] ISSUE-010: 状态文件 INI 解析容错不足,手动注释/空格会被丢弃

- **位置**: [internal/ddns/statefile.go:64-119](file:///opt/ikev2-panel-v2-main/internal/ddns/statefile.go#L64-L119) (`parseStateFile`)
- **问题**:
  1. **v2-83 裸 `true/false` 和 INI-style 都支持**:但**注释行 `#` 之后到行尾的内容会被保留**(`TrimSpace` 只去首尾空白,不去 `#` 后内容)。这通常无害,但运维如果写 `# enabled=true`(注释里写了 enabled),解析会忽略(因为以 `#` 开头就 continue)。
  2. **`enabled=true` / `1` / `on` 三种合法值**,但 `false` / `0` / `off` 没显式允许 —— `val == "true" || val == "1" || val == "on"` 之外都视为 false。**这导致 `enabled=False`(Python 风格)被静默接受**。
  3. **TTL 等数值字段无法表达**:`stateRecord` 只有 `Enabled` + `Family`,未来想加 `TTL` / `ProbeTarget` 时,需要重新设计 schema,老用户升级会丢配置。
  4. **没有 schema 版本号**:文件里只有 `# header`,没有 `version=2`,导致未来字段加减时无法迁移。
- **参考**:
  - [ini 文件规范 — Python configparser](https://docs.python.org/3/library/configparser.html)
  - [TOML spec](https://toml.io/en/) — 现代替代
- **风险**:
  - 配置文件无声修改 → 用户下次启动发现开关变了
  - 升级到 v3 加新字段时,老配置不会迁移
- **修复方案**:
  1. 用 `[section]` 段格式(`[ddns]` + `enabled=true`),或干脆换 TOML(`github.com/pelletier/go-toml/v2` 4KB)
  2. 加 `version=1` 行
  3. 写入时显式覆盖(每次 SetEnabled 都重写整个文件,而不是 append)
- **工作量**: 1d

---

### [SEVERITY-MED] ISSUE-011: 凭证文件权限 / 日志脱敏审计

- **位置**:
  - [internal/panelstate/state.go:127-182](file:///opt/ikev2-panel-v2-main/internal/panelstate/state.go#L127-L182) (`WriteAliyun` 0o700 目录 + 0o600 文件)
  - [internal/config/config.go:117](file:///opt/ikev2-panel-v2-main/internal/config/config.go#L117) (stderr 日志 "panelstate aliyun.creds parse failed")
  - [internal/ddns/sync.go:541](file:///opt/ikev2-panel-v2-main/internal/ddns/sync.go#L541) (log "record outdated, updating" — 没打 secret,但 error 字符串里可能含)
- **问题**:
  1. **凭证写入权限正确**(0o600),但**读取时没校验文件权限**:如果运维误改 `chmod 644`,代码不告警。应该 `os.Stat` 检查后跟 RAM 最佳实践告警。
  2. **`stderr` 直接打 credential parse 错误**(行 117-118):错误本身不包含 secret,但如果未来错误信息增强(例如 "missing field key_secret in panelstate creds"),可能泄露文件名。
  3. **DDNS error 日志打印完整 error message**:API error 包含 `Code` + `Message`,Message 一般不含 secret,但**调试日志**(slog.DebugLevel)可能含完整 HTTP body,body 里**有可能含 AccessKeyId**(虽然 query string,不是 secret,但泄露 AK ID 也是信息泄露)。
  4. **MaskKeyID 函数**(行 213-218)在错误日志里没用 —— 凭证相关 error 仍可能 print 完整 AK ID。
- **参考**:
  - [OWASP Logging Cheat Sheet](https://cheatsheetseries.owasp.org/cheatsheets/Logging_Cheat_Sheet.html)
  - [阿里云 RAM 最佳实践 — 凭证保护](https://help.aliyun.com/zh/ram/getting-started/best-practices-for-ram-users)
- **风险**:
  - 操作员误改权限后,凭证泄露
  - 日志聚合(Graylog/ELK)能搜到 AK ID,合规问题
- **修复方案**:
  1. `LoadAliyun` 加权限校验:`if mode&0o077 != 0 { WARN }`
  2. 自定义 slog Handler,AK ID 字段自动 mask(类似 `MaskKeyID`)
  3. DDNS error 不打印 raw API body,只 print Code + Message
- **工作量**: 1d

---

### [SEVERITY-LOW] ISSUE-012: 多 provider 架构缺失 — AliyunClient 硬编码

- **位置**: 整个 `internal/dns/aliyun.go` 都是 `AliyunClient` 具体类型
- **问题**: 当前设计是 "DDNS Sync 直接依赖 AliyunClient"。如果未来要加 Cloudflare / 腾讯云 DNSPod / GoDaddy(参见 [ddns-go README 支持的 provider 列表](https://github.com/jeessy2/ddns-go)),需要重构整个调用链。
- **参考**:
  - [ddns-go provider 抽象](https://github.com/jeessy2/ddns-go) — `DNSProvider` interface
  - [ddclient plugin 模式](https://github.com/ddclient/ddclient)
  - [Go interface segregation](https://go.dev/blog/standard-library-complexity)
- **风险**:
  - 加新 provider 必须改 Sync 内部代码,工作量大
  - 当前用户(中国 VPS 阿里云为主)迁移其他云时被卡
- **修复方案**:
  1. 抽 `interface DNSProvider { FindRecord(...); UpdateRecordValue(...) }`
  2. `Sync` 持有 `[]DNSProvider`(per-domain),而不是具体 client
  3. 现在只实现 `AliyunProvider`,未来加 `CloudflareProvider` 不动 Sync
- **工作量**: 2d(单 provider 重构 + 1 个新 provider 落地)

---

### [SEVERITY-LOW] ISSUE-013: IPv6 RRKeyWord 搜索 vs 精确匹配 bug 风险

- **位置**: [internal/dns/aliyun.go:104-138](file:///opt/ikev2-panel-v2-main/internal/dns/aliyun.go#L104-L138) (`FindRecord`)
- **问题**:
  1. 传 `RRKeyWord=vpn` + `SearchMode=EXACT`,阿里云返回的 `Record.RR` 字段**就是 "vpn"**(精确)。代码再 `r.RR == rr` 过滤,这一步冗余但无害。
  2. **但如果 RR 含通配符**(`_acme-challenge`),EXACT 模式下阿里云返回的 RR 可能保留通配符;代码按 `r.RR == rr` 严格比,如果配置文件里写 `RR=_acme-challenge`,而阿里云返回 `_acme-challenge.example.com.`,**就 miss 掉**。
  3. **没处理 `RR=@`**:阿里云 API 对 `@`(根域名)传 "RR=@" 还是 "RR=" 不一致,代码用 `cfg.RR` 默认 `"@"`,可能命中失败。
- **参考**:
  - [alidns DescribeDomainRecords 参数文档](https://help.aliyun.com/zh/dns/api-alidns-2015-01-09-describedomainrecords)
  - [DNSPod 类似问题](https://github.com/TencentCloud/tencentcloud-sdk-go)
- **风险**:
  - 根域名 RR=@ 不会更新
  - 通配符场景失效
- **修复方案**:
  1. 不用 `RRKeyWord`,改用 `PageNumber=1&PageSize=100` 全量拉,客户端过滤(`strings.HasSuffix(r.RR+"."+r.DomainName, fullDomain)`)
  2. 或阿里云 [DescribeDomainRecords](https://help.aliyun.com/zh/dns/api-alidns-2015-01-09-describedomainrecords) 实际有 `RR` 参数(单值,精确),用 `RR` 不用 `RRKeyWord`
- **工作量**: 0.5d

---

### [SEVERITY-LOW] ISSUE-014: SignatureNonce 退化为时间戳 — 重放窗口扩大

- **位置**: [internal/dns/aliyun.go:269-275](file:///opt/ikev2-panel-v2-main/internal/dns/aliyun.go#L269-L275) (`randomNonce`)
- **问题**: 注释说"极端情况:用时间戳做 nonce"。`crypto/rand.Read` 在 Linux 上几乎不会失败(除非 /dev/urandom 损坏),但代码 fallback 到 `time.Now().UnixNano()` 是**确定性问题**:
  1. 如果 `crypto/rand` 真的失败,fallback 到 `UnixNano`,同一毫秒内多次调用会**nonce 重复** → 触发阿里云 `SignatureNonceUsed` 错误(`15 分钟内不能重复`)
  2. 即便 `crypto/rand` 正常,nonce 是 32 hex 字符(16 字节 = 128 bit)—— 足够长,但如果**多个 DDNS 实例同时运行**(虽然目前没这设计),各自产生 nonce 也 OK
  3. **nonce 没跟 credential 绑定**:理论上同一个 AK 在 15 分钟内发了 1 个请求,nonce 用了 32 字符 hex,碰撞概率 ≈ 2^-128,实际不会撞,但应该用 UUID v4 / v7(基于时间,可排序) 标准化
- **参考**:
  - [RFC 4122 — UUID](https://www.rfc-editor.org/rfc/rfc4122)
  - [阿里云 SignatureNonceUsed 错误](https://next.api.aliyun.com/global-error-code) — `27 SignatureNonceUsed`
  - [ddns-go random.Nonce](https://github.com/jeessy2/ddns-go) — 用 `uuid.New().String()`
- **风险**:
  - crypto/rand 失败极小概率,但 fallback 到 UnixNano 增加 nonce 重复概率
- **修复方案**:
  1. 用 `github.com/google/uuid`(`uuid.New().String()`)
  2. 或手写 `crypto/rand` + retry,失败时**直接 return error**,不 fallback
- **工作量**: 0.1d

---

### [SEVERITY-LOW] ISSUE-015: StateFile 写竞争 — DDNS tick 与面板 UI 写并发没原子协调

- **位置**:
  - [internal/ddns/sync.go:218-249](file:///opt/ikev2-panel-v2-main/internal/ddns/sync.go#L218-L249) (`SetEnabled` / `SetFamily` 调 `writeStateFile`)
  - [internal/ddns/sync.go:614-620](file:///opt/ikev2-panel-v2-main/internal/ddns/sync.go#L614-L620) (`readStateFile` 单独暴露但 NewSync 已处理)
- **问题**:
  1. `SetEnabled` / `SetFamily` 加锁后,释放锁才 `writeStateFile`:**两个 goroutine 同时调用 SetEnabled(true) 和 SetFamily("v4"),可能产生 INI 文件写入 race**。两次都先 lock、写各自字段到 in-memory map、再 unlock,写文件时各拿一份 snapshot,后者覆盖前者,**部分写入丢失**。
  2. `writeStateFile` 调 `swanctl.AtomicWriteFile`(write+rename)是原子的,但**两个 Set 调用各自原子写,可能写一前一后丢失中间状态**。
  3. **DDNS Run() 启动时** `NewSync` 已经读取 state file,但 `Run()` 期间 SetEnabled 又写,**Run 读不到新值**(因为 `s.cfg.Enabled` 是 in-memory copy,SetEnabled 改的是同一个字段,所以实际 OK,但 `parseStateFile` 没在 Run 内重读)。
- **参考**:
  - [Go sync.Mutex vs RWMutex](https://go.dev/blog/race-detector)
  - [swanctl.AtomicWriteFile 源码](file:///opt/ikev2-panel-v2-main/internal/swanctl) — write-tmp + rename
- **风险**:
  - 面板 UI 频繁切 family,丢失中间状态
  - 不致命,但行为不直观
- **修复方案**:
  1. 抽 `Sync.mu` 包住整个 read-modify-write 流程(包括 writeStateFile)
  2. 或用 channel 串行化所有 set 操作
- **工作量**: 0.5d

---

## 不在范围内

明确**没看**的部分(避免 scope creep):

- ❌ strongSwan VICI 协议正确性(留作后续 cert 审计)
- ❌ panelstate 文件加密(KMS / age)—— 评估见 audit-security.md
- ❌ multi-tenant credential scoping(单租户场景)
- ❌ IPv6 prefix delegation / DHCPv6-PD 探测
- ❌ DDNS API 调用频控策略(用户运营),只审计 retry/throttle 技术正确性
- ❌ v3-83 → v3-85 迁移(等 v2-85 实际发布后再审计)

---

## 建议优先级

### 本周(Sprint 0 / P0)
1. **ISSUE-002**(error code 分类 + Throttling 退避)— HIGH,可被阿里云临时故障触发
2. **ISSUE-005**(retry 退避加 jitter)— MED,Throttling 雪崩防御
3. **ISSUE-006**(双探测 fallback)— MED,某些 ISP 下完全不可用

### 下周(Sprint 1 / P1)
4. **ISSUE-001**(HMAC-SHA1 → SHA256)— HIGH,但工程量大(2-3d),需要完整回归测试
5. **ISSUE-009**(观测性:stage / RequestID)— MED,debug 友好度
6. **ISSUE-003**(凭证优先级对齐)— HIGH,但行为清晰,小工程

### P2(本月)
7. **ISSUE-011**(权限校验 + 日志脱敏)— MED
8. **ISSUE-008**(AliyunClient 不可变 / http.Client 共享)— MED
9. **ISSUE-010**(state file schema 版本)— MED
10. **ISSUE-007**(IPv6 temporary address 过滤)— MED
11. **ISSUE-004**(URL 编码抽工具函数)— MED

### P3 / Backlog
12. **ISSUE-012**(多 provider 抽象)— LOW,但**只在用户明确要求时启动**(当前用户都是阿里云)
13. **ISSUE-013**(RR 搜索 bug)— LOW
14. **ISSUE-014**(UUID nonce)— LOW,1 小时可改
15. **ISSUE-015**(state file 写竞争)— LOW,边缘场景

### 永不修(明确不接受)
- ❌ HMAC-SHA1 替换 SHA256 之外的"V3 必须走 header"重构(见 ISSUE-001 修复方案 1 vs 2):改 header 后阿里云 SDK 自动注入 `x-acs-content-sha256`,代码耦合更深,**保留 query 签名反而简单**。P1 阶段可以小步切换 SHA256 算法但保留 query。

---

## 参考链接(直接引用)

### 阿里云官方
- [alidns API DescribeDomainRecords](https://help.aliyun.com/zh/dns/api-alidns-2015-01-09-describedomainrecords)
- [alidns API UpdateDomainRecord](https://help.aliyun.com/zh/dns/api-alidns-2015-01-09-updatedomainrecord)
- [alidns 公共错误码](https://help.aliyun.com/zh/dns/developer-reference/error-codes)
- [V2 签名机制](https://help.aliyun.com/zh/cmn/developer-reference/signature-mechanism)
- [V3 签名机制](https://help.aliyun.com/zh/sdk/product-overview/v3-request-structure-and-signature)
- [全局错误码](https://next.api.aliyun.com/global-error-code)
- [RAM 用户最佳实践](https://help.aliyun.com/zh/ram/getting-started/best-practices-for-ram-users)

### 标杆项目
- [ddns-go](https://github.com/jeessy2/ddns-go) — 多 provider Go DDNS
- [ddclient](https://github.com/ddclient/ddclient) — 老牌 Perl DDNS
- [terraform-provider-alicloud](https://github.com/aliyun/terraform-provider-alicloud) — 阿里云官方 IaC
- [alibaba-cloud-sdk-go](https://github.com/aliyun/alibaba-cloud-sdk-go) — 官方 Go SDK

### 标准 / RFC
- [RFC 2104 — HMAC](https://www.ietf.org/rfc/rfc2104.txt)
- [RFC 3986 — URI Generic Syntax](https://tools.ietf.org/html/rfc3986)
- [RFC 7217 — IPv6 SLAAC Privacy Extensions](https://www.rfc-editor.org/rfc/rfc7217.html)
- [NIST SP 800-131A Rev-2](https://csrc.nist.gov/publications/detail/sp/800-131a/rev-2/final)
- [OWASP Cheat Sheet Series](https://cheatsheetseries.owasp.org/)

### 内部链接
- [internal/dns/aliyun.go](file:///opt/ikev2-panel-v2-main/internal/dns/aliyun.go)
- [internal/ddns/sync.go](file:///opt/ikev2-panel-v2-main/internal/ddns/sync.go)
- [internal/ddns/statefile.go](file:///opt/ikev2-panel-v2-main/internal/ddns/statefile.go)
- [internal/swanctl/ipv4watch.go](file:///opt/ikev2-panel-v2-main/internal/swanctl/ipv4watch.go)
- [internal/swanctl/ipv6watch.go](file:///opt/ikev2-panel-v2-main/internal/swanctl/ipv6watch.go)
- [internal/config/config.go](file:///opt/ikev2-panel-v2-main/internal/config/config.go)
- [internal/panelstate/state.go](file:///opt/ikev2-panel-v2-main/internal/panelstate/state.go)

---

**汇总:N=15 issue(3 HIGH / 7 MED / 5 LOW)。HIGH 集中在签名算法迁移、错误码分类、凭证优先级,均是可独立修复的小工作量。建议先打 P0 三件套,再排 P1。**