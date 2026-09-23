# Phase 6 总体计划 — 全面规范化 + strongSwan 深度借鉴

> 启动日期:2026-09-19
> 完成日期:TBD
> 总工作量:**9.0d**(9 个 PR,每个独立可交付)
> 总工作量(可选):**10.5d**(含 PR-18 ingress IFB)
> 关联:gap-analysis [audit-2026-09-post-v2.85-gap-analysis.md](audit-2026-09-post-v2.85-gap-analysis.md)

---

## 一、Phase 6 总体目标

把 v2.85 之后的**剩余 ~50-70 个 unique issue 全部修掉**,并把 strongSwan 没用上的高价值特性**全部借鉴**,目标:**v2.86 发布时项目达到 2026 OWASP / NIST / RFC 全合规水平 + strongSwan 官方 Security Recommendations 全对齐**。

完成后:
- ✅ 算法合规 RFC 8247 / NIST 800-131A Rev.2 / CNSA 2.0
- ✅ 容器隔离 OWASP Docker Top-10 全清
- ✅ 供应链 SLSA L2(acme.sh + 清华源 GPG)
- ✅ 抗量子 RFC 9370(ML-KEM-768 + ECDH hybrid)
- ✅ iOS / macOS / Android / Windows 全客户端稳定性
- ✅ strongSwan 6.1.0 最新特性全覆盖

---

## 二、Phase 6 全部 9 个 PR

### 总览

| PR | 主题 | 来源 | 工作量 | Sprint | 关键文件 |
|----|------|------|--------|--------|----------|
| **PR-9** | **登录 rate limit + DDoS 防护 + HTTP 安全 header** | web W03/W01 + security S01 | **1.0d** | Sprint 1 | handlers_auth / server / TLS |
| **PR-10** | **IKEv2 算法去弱(MODP2048/sha1/md5/stroke)+ RFC 7383 分片 + RFC 6023 childless** | strongswan HIGH-1/4 + MED-1/2/8/11 + 借鉴 §3.1/§3.3 | **1.2d** | Sprint 1 | swanctl-ipv6-only.conf + Dockerfile + entrypoint |
| **PR-11** | **Docker 减权(cap_drop + 删 SYS_ADMIN/privileged)+ 清华源 GPG + base image 瘦身** | docker D01/D02/D03/D05/D08 | **1.0d** | Sprint 1 | Dockerfile + docker-compose.yml + .dockerignore |
| **PR-12** | **续签 atomic rename + acme.sh pin SHA256 + GPG 校验 + renew 随机化** | cert HIGH-1/2/5/6 + strongswan HIGH-5/6 | **1.2d** | Sprint 2 | Dockerfile + scripts/renew-cert.sh + ikev2-reload.sh |
| **PR-13** | **filelog 脱敏 + logrotate + 0600 + 删除 sha1/md5 编译选项** | strongswan HIGH-2/MED-1/MED-6/LOW-3/4 | **0.5d** | Sprint 2 | strongswan-filelog.conf + Dockerfile + logrotate.d |
| **PR-14** | **DDNS 规范化(URL encoding + jitter retry + 错误码白名单 + 并发安全)** | alidns ISSUE-002/004/005/008/011/014/015 | **1.0d** | Sprint 2 | internal/dns/aliyun.go + state.go + sync.go |
| **PR-15** | **VICI Event Stream(替代 list-sas 轮询)+ IKE_SA_EXPIRED 提前 1h 提醒** | strongswan MED-5 + 借鉴 §3.6 | **0.8d** | Sprint 3 | internal/swanctl/collector.go + listener.go + handlers_home |
| **PR-16** | **Post-Quantum Hybrid(ML-KEM-768 + ECDH)+ kernel-libipsec 准备** | 借鉴 §3.2 + §3.7 | **1.0d** | Sprint 3 | Dockerfile + swanctl-ipv6-only.conf + conf.d |
| **PR-17** | **IKEv2 SA Redirect(RFC 5685)+ ESP TFC Padding(RFC 4306)+ VICI cert policy** | 借鉴 §3.4 + §3.5 | **0.8d** | Sprint 3 | swanctl-ipv6-only.conf + scripts/migrate-server.sh |
| **PR-18** | **ingress IFB(上传限速)+ tc sqm + dual 公平队列** | 借鉴 net N04 + wondershaper / sqm-scripts | **1.0d** | Sprint 4(可选) | ikev2-updown + limiter.go |

**Phase 6 总工作量**:
- 必做(PR-9 ~ PR-17):**8.5d**(约 1.7 周)
- 含 PR-18 ingress IFB:**9.5d**(约 1.9 周)

### Sprint 划分

| Sprint | 天数 | PR | 主题 |
|--------|------|----|----|
| **Sprint 1** | **3.2d** | PR-9 + PR-10 + PR-11 | **核心安全 + 算法合规 + 容器隔离** |
| **Sprint 2** | **2.7d** | PR-12 + PR-13 + PR-14 | **续签 atomic + 日志 + DDNS 规范化** |
| **Sprint 3** | **2.6d** | PR-15 + PR-16 + PR-17 | **VICI stream + PQC + strongSwan 借鉴** |
| **Sprint 4**(可选) | 1.0d | PR-18 | ingress IFB(上传限速) |

---

## 三、每个 PR 的详细范围

### PR-9 登录 rate limit + DDoS 防护 + HTTP 安全 header(1.0d)

**背景**:审计 Top-10 #1(登录无 rate limit)+ #5(HTTP 安全 header 全缺失)+ DDoS 防护。

**改动清单**:

#### 9.1 登录 rate limit(0.4d)
- 新建 [internal/auth/ratelimit.go](file:///opt/ikev2-panel-v2-main/internal/auth/ratelimit.go):
  - 内存 `map[ip]struct{count, firstFail, lockedUntil}` + `sync.RWMutex`
  - 阈值:**5 次失败 / 5 分钟锁 IP**
  - 持久化到 `/data/panel-state/ratelimit.json`(防重启清零)
- `handlers_auth.go`:登录失败 → `ratelimit.Record(ip, success)`
- 5 check 之一加 `ratelimit` healthz

#### 9.2 全 POST 限速(0.2d)
- `handlers_users.go` / `handlers_aliyun.go` / `handlers_ddns.go`:每个 POST 路由前面加 `ratelimit.Middleware(action, max=30/min)`
- 区分:"用户操作" / "凭证变更" / "DDNS 切换"三档阈值

#### 9.3 HTTP 安全 header(0.2d)
- `server.go` 加 middleware `secureHeaders(next)`:
  - `Strict-Transport-Security: max-age=31536000; includeSubDomains`
  - `Content-Security-Policy: default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; frame-ancestors 'none'`
  - `X-Frame-Options: DENY`
  - `X-Content-Type-Options: nosniff`
  - `Referrer-Policy: strict-origin-when-cross-origin`
  - `Permissions-Policy: geolocation=(), camera=(), microphone=()`
- LE 模式下加 `Public-Key-Pins` 头(可选)
- 测试:curl -I 验证所有头存在

#### 9.4 DDoS 防护(0.2d)
- `entrypoint.sh` §4.6 加 iptables 规则:
  - `connlimit --connlimit-above 50`(每 IP 最大 50 并发 SA)
  - `hashlimit --hashlimit-above 5/min --hashlimit-burst 10`(每 IP 新 SA 速率)
  - UDP 500 / 4500 端口单独限速

**用户感知**:
- 暴力破解入口关闭(5 次失败锁 IP 5min)
- K8s ingress / 云 WAF 友好(HSTS + CSP 标准头)
- 国家级 DDoS 不再一击毙命(50 SA 上限)

**测试**:
- `internal/auth/ratelimit_test.go`(5 个 unit test:阈值边界 / 持久化 / 并发安全 / IP 解锁)
- `internal/web/headers_test.go`(curl -I 验所有头)

---

### PR-10 IKEv2 算法去弱 + RFC 7383 分片 + RFC 6023 childless(1.2d)

**背景**:strongSwan 审计 HIGH-1/MED-1/2/8/11 + 借鉴 §3.1 + §3.3。

**改动清单**:

#### 10.1 算法提案重写(0.4d)
- [configs/swanctl-ipv6-only.conf](file:///opt/ikev2-panel-v2-main/configs/swanctl-ipv6-only.conf):
  - **删除** MODP2048 / MODP1536 / modpnone / SHA1 fallback
  - **新增** modp3072 / curve25519 / ecp384 / ecp256(已有)
  - **加注释**:"iOS 16.5 仅发 MODP2048,MODP3072/curve25519 fallback 协商失败时主动 accept 老算法"
  - `esp_proposals`:`-modpnone -sha1 -md5`
- `internal/cert/generate.go`:server cert 升级到 RSA 4096(可选,默认仍 2048 兼容老客户端)

#### 10.2 RFC 7383 消息分片(0.2d)
- `swanctl-ipv6-only.conf` 在 `charon { }` 块加:
  ```conf
  fragmentation {
      max_packet_size = 1280
  }
  ```
- `Dockerfile` `--enable-kernel-libipsec` 不变,**不强制用户态 ESP**(性能)

#### 10.3 RFC 6023 childless IKE SA(0.2d)
- `swanctl-ipv6-only.conf` connections 块:
  ```conf
  childless = yes
  ```
- rekey 行为变:旧 SA 在 rekey 期间继续承载流量 → 断流时间 < 100ms

#### 10.4 ikesa_lifetime + dpd_timeout 显式设(0.2d)
- `rekey_time = 24h`
- `ikelifetime = 26h`(rekey_time + 10%)
- `dpd_delay = 30s`
- `dpd_timeout = 120s`

#### 10.5 Dockerfile 编译选项清理(0.2d)
- 删除 `--enable-sha1 --enable-md5 --enable-stroke`
- 加 `--enable-gcm --enable-ccm --enable-aes --enable-chapoly`
- 镜像瘦身 ~8MB(stroke 移除)

**用户感知**:
- 算法合规:RFC 8247 / NIST 800-131A Rev.2 / CNSA 2.0 全对齐
- iOS 企业 NAT 拨号成功率 ↑(消息分片)
- rekey 零断流(childless)
- 镜像体积 -8MB

**测试**:
- `internal/limit/limiter_test.go` 加算法提案校验
- netns 真跑 swanctl.conf + charon 启动,验算法列表

---

### PR-11 Docker 减权 + 清华源 GPG + base image 瘦身(1.0d)

**背景**:docker 审计 D01/D02/D03/D05/D08,容器逃逸 + 供应链风险。

**改动清单**:

#### 11.1 capabilities 减权(0.4d)
- `docker-compose.yml`:
  - **删除** `privileged: true`
  - **删除** `cap_add: [SYS_ADMIN, NET_ADMIN, NET_RAW]`(tc 只需 NET_ADMIN)
  - 加 `cap_drop: [ALL]` + `cap_add: [NET_ADMIN, NET_RAW, CHOWN, SETUID, SETGID, DAC_OVERRIDE]`
  - 加 `security_opt: [no-new-privileges:true]`
  - 加 `read_only: true` + tmpfs 显式挂载

#### 11.2 apt 清华源 GPG(0.2d)
- `Dockerfile`:
  - 在 apt update 前加:
    ```dockerfile
    RUN curl -fsSL https://mirrors.tuna.tsinghua.edu.cn/debian/keyring.gpg | gpg --dearmor -o /usr/share/keyrings/tuna-archive-keyring.gpg
    ```
  - sources.list 改 `signed-by=` 形式

#### 11.3 base image 切 debian-slim → distroless(0.3d)
- 评估:distroless `static-debian12` + 自编译 strongSwan + libipsec + govici
- 如果可行,镜像体积 -200MB;如果不可行,保持 debian-slim 但加 `--no-install-recommends`
- **保守方案**:debian-slim + `--no-install-recommends` + 删 `/usr/share/doc` + `/usr/share/man`(省 100MB)

#### 11.4 user namespace 显式(0.1d)
- `docker-compose.yml` 加 `userns_mode: "host"`(默认 host network + host userns)
- 验证 tc / iptables 在减权后仍可执行(NET_ADMIN 足够)

**用户感知**:
- 容器逃逸风险从 root on host → 普通进程(必须二次提权)
- 清华源 GPG 防供应链投毒
- 镜像体积 -200MB(可选)

**测试**:
- `docker-compose.yml` 加 `healthcheck.test: ["CMD", "curl", "-f", "http://localhost:9000/healthz"]`
- 实跑 cap_drop 容器,验 tc / iptables / swanctl 都正常

---

### PR-12 续签 atomic rename + acme.sh pin SHA256 + renew 随机化(1.2d)

**背景**:cert 审计 HIGH-1/2/5/6 + strongSwan HIGH-5/6。

**改动清单**:

#### 12.1 cert 切换 atomic(0.4d)
- `internal/swanctl/writer.go`:`installCertsToSwanctl()` 改成:
  ```go
  // 1. 写到临时文件 server.cert.pem.tmp + server.key.pem.tmp
  // 2. atomic rename(.tmp → .pem)
  // 3. swanctl --load-creds(只换 creds,不 reload conns)
  // 4. 失败 → atomic rename .bak 恢复
  ```
- `scripts/ikev2-reload.sh`:`--load-all` 改 `--load-creds`(reload conns 用 `--reload-all`)

#### 12.2 acme.sh pinned commit SHA256(0.4d)
- `Dockerfile`:
  ```dockerfile
  # pinned to v3.0.7 commit a1b2c3d4...
  ARG ACMESH_COMMIT=a1b2c3d4...
  RUN git clone https://gitee.com/acmesh-official/acme.sh.git /root/.acme.sh \
      && cd /root/.acme.sh \
      && git checkout "${ACMESH_COMMIT}" \
      && echo "<sha256>  acme.sh.tar.gz" | sha256sum --check
  ```

#### 12.3 acme.sh GPG 校验(0.2d)
- 下载 acme.sh 维护者 GPG key(`https://github.com/acmesh-official.gpg`)
- 校验 release tarball 的 signature

#### 12.4 renew cron 随机化(0.2d)
- `scripts/entrypoint.sh` §7.5:
  ```bash
  # v2.86:renew cron 随机化
  RANDOM_DELAY_MIN=$((RANDOM % 3600))  # 0-60min 随机
  cat > /etc/cron.d/ikev2-le-renew <<EOF
  SHELL=/bin/bash
  $((RANDOM_DELAY_MIN / 60)) $((RANDOM_DELAY_MIN % 60)) * * * root /etc/ikev2-panel/scripts/renew-cert.sh
  EOF
  ```
- 多次重试:`RANDOM_DELAY=3600` + 3 次/天 `0 */8 * * *`

**用户感知**:
- LE 续签断流时间:500ms → < 100ms
- acme.sh 供应链攻击面消失(SHA256 + GPG)
- LE 速率配额公平分配(随机化 + 多重试)

**测试**:
- `internal/swanctl/writer_test.go`:atomic rename 模拟半截写
- 真跑 LE renew + 验 `--load-creds` 不影响 active SA

---

### PR-13 filelog 脱敏 + logrotate + 0600 + 删 sha1/md5(0.5d)

**背景**:strongSwan HIGH-2/MED-1/MED-6/LOW-3/4。

**改动清单**:

#### 13.1 filelog 脱敏(0.2d)
- [configs/strongswan-filelog.conf](file:///opt/ikev2-panel-v2-main/configs/strongswan-filelog.conf):
  - `enc = 0`(关闭 RAW 密钥材料)
  - `net = 0`(关闭网络协商每包)
  - `mgr = 1`(CONTROL 保留)
  - 加 `ike_name = yes`

#### 13.2 logrotate + 0600(0.2d)
- 新建 `/etc/logrotate.d/ikev2-charon`:
  ```
  /var/log/charon.log {
      daily
      rotate 7
      compress
      missingok
      notifempty
      postrotate
          killall -HUP charon 2>/dev/null || true
      endscript
  }
  ```
- `Dockerfile`:`/var/log/charon.log 0600 root root`

#### 13.3 SIGHUP 重载生效(0.1d)
- filelog 加 `append = yes`(`-SIGHUP` 重启日志时 append 不 truncate)
- `strongswan restart` 时 `charon --signal HUP`

**用户感知**:
- NT-hash 离线爆破风险消失(enc=1 关闭)
- 磁盘有界(7 天轮转)
- 日志 SIGHUP 重载生效

**测试**:
- `docker compose exec ikev2-panel logrotate -f /etc/logrotate.d/ikev2-charon`
- 验 charon.log 权限 0600

---

### PR-14 DDNS 规范化(URL + jitter retry + 错误码 + 并发)(1.0d)

**背景**:alidns ISSUE-002/004/005/008/011/014/015。

**改动清单**:

#### 14.1 URL 编码统一(0.2d)
- `internal/dns/aliyun.go`:删手动补丁链,改用 `net/url.QueryEscape` + 阿里云 V3 spec 编码规则
- 测试:空格 / 星号 / `~` / `+` / 中文 5 个 case

#### 14.2 jitter retry + 错误码分类(0.3d)
- 新建 `internal/dns/retry.go`:
  ```go
  type Backoff struct {
      Base time.Duration  // 1s
      Max  time.Duration  // 60s
  }
  func (b Backoff) Delay(attempt int) time.Duration { ... + jitter ... }
  ```
- `internal/dns/aliyun.go`:根据阿里云 `Code` 白名单分类重试:
  - `Throttling` / `ServiceUnavailable` → 指数退避
  - `InvalidAccessKeyId` / `Forbidden.RAM` / `RecordNotBelongToRAM` → 不重试
  - 其他 5xx → 最多 3 次
  - 4xx → 不重试(客户端错)

#### 14.3 SignatureNonce UUID v4(0.1d)
- `internal/dns/aliyun.go`:`nonce = uuid.New().String()`(16 字节随机)

#### 14.4 并发安全(0.2d)
- `internal/dns/sync.go`:每个 DDNS tick 新建 AliyunClient(避免共享 HTTPClient)
- `internal/dns/state.go`:`sync.Mutex` 保护 StateFile 写

#### 14.5 凭证文件权限(0.1d)
- `internal/dns/state.go`:写凭证到 `/data/panel-state/aliyun.creds` 用 `os.OpenFile(..., 0o600, 0o600)`
- 日志脱敏:KeyId 显示 `key_id_masked = "LTAI****XXXX"`

#### 14.6 错误码白名单(0.1d)
- `internal/dns/errors.go`(新):
  ```go
  var RetryableCodes = map[string]bool{
      "Throttling":            true,
      "ServiceUnavailable":   true,
      "InternalError":        true,
      "DomainRecordNotBelongToUser": false,
  }
  ```

**用户感知**:
- retry 雪崩消失(按错误码分类)
- 真实错误立刻显示(不浪费 9s)
- 凭证文件不能被普通用户读(0o600)

**测试**:
- `internal/dns/retry_test.go`:6 个 case(每错误码策略)
- `internal/dns/aliyun_test.go`:URL encoding 5 case

---

### PR-15 VICI Event Stream + IKE_SA_EXPIRED 提前 1h 提醒(0.8d)

**背景**:strongswan MED-5 + 借鉴 §3.6。

**改动清单**:

#### 15.1 VICI listen 订阅事件流(0.4d)
- 新建 [internal/swanctl/listener.go](file:///opt/ikev2-panel-v2-main/internal/swanctl/listener.go):
  ```go
  func ListenEvents(ctx context.Context, onEvent func(EventType, *Event)) error {
      // 用 govici Session.RegisterEvent()
      // 订阅:ike-updown / ike-rekey / ike-expire / child-updown / child-rekey
  }
  ```
- `internal/swanctl/collector.go`:替换 list-sas 5min 轮询为事件流

#### 15.2 IKE_SA_EXPIRED 提前 1h 提醒(0.2d)
- `internal/swanctl/listener.go`:监听到 `ike-expire`(提前 1h 触发)→ 写 audit log + 通过 SSE 推到 UI
- 新增 `/api/sa/expiring` SSE endpoint

#### 15.3 UI 实时更新(0.2d)
- `web/templates/home_content.html`:用 SSE / fetch 替换轮询
- `web/static/js/home.js`(新):SSE client

**用户感知**:
- "已连接用户"列表延迟 5min → 实时
- SA 过期前 1h 看到提醒,提前续签

**测试**:
- `internal/swanctl/listener_test.go`:netns 模拟 charon 事件
- `internal/web/handlers_home_test.go`:SSE 推送验

---

### PR-16 Post-Quantum Hybrid(ML-KEM-768 + ECDH)(1.0d)

**背景**:借鉴 §3.2(RFC 9370 + FIPS 203)。

**改动清单**:

#### 16.1 strongSwan 编译加 PQC(0.3d)
- `Dockerfile`:
  - 更新 strongSwan 到 **6.0.2+**(6.1.0 已 released 2026-09-07)
  - `--enable-pqc`(开 PQC plugin)
  - 验证 `swanctl --list-algs | grep ml-kem`

#### 16.2 swanctl.conf 双层 proposal(0.3d)
- `swanctl-ipv6-only.conf`:
  ```conf
  proposals = aes256gcm16-sha256-curve25519, \
              aes256gcm16-sha256-ml-kem-768, \
              aes256gcm16-sha256-ecp256, \
              ...
  ```
- 注释:"RFC 9370 hybrid(ml-kem-768 + ecp256 fallback),2027+ NIST 强制"

#### 16.3 kernel-libipsec 准备(0.2d)
- `configs/charon.conf`(新):
  ```conf
  charon {
      plugins {
          kernel-libipsec {
              use_netlink = no  # 用户态 ESP 支持所有 IKE 算法
          }
      }
  }
  ```
- 注释:"PQC 部署时启用,日常 kernel XFRM(性能)"

#### 16.4 文档说明(0.2d)
- `docs/design.md` 加 §PQC 章节:工作原理 / 性能影响(用户态 ESP ~30% 慢) / 启用方法

**用户感知**:
- 抗量子破解(2027+ NIST 标准)
- 加密 5 年有效期不会被量子破解
- 性能影响仅启用 kernel-libipsec 时,~30% 带宽

**测试**:
- `swanctl --list-algs | grep ml-kem-768` 必须有
- netns 真跑 hybrid 协商

**注意**:**默认不强制启用**,PQC 客户端支持要到 strongSwan 6.2+ / iOS 19+ 才完整。当前 PR 只**准备好**不**强推**。

---

### PR-17 IKEv2 SA Redirect + ESP TFC Padding + VICI cert policy(0.8d)

**背景**:借鉴 §3.4 + §3.5。

**改动清单**:

#### 17.1 RFC 5685 IKEv2 SA Redirect(0.4d)
- `configs/swanctl-ipv6-only.conf`:
  ```conf
  connections.<conn> {
      send_certreq = yes  # 接受 redirect
  }
  ```
- 新建 `scripts/migrate-server.sh`:用 `swanctl --redirect <oldgw> <newgw>` 通知所有 active SA
- `internal/swanctl/redirect.go`(新):Go 端调用

#### 17.2 ESP TFC Padding(0.2d)
- `swanctl-ipv6-only.conf` esp_proposals:
  ```conf
  esp_proposals = aes256gcm16, ..., tfc_padding
  ```
- 注释:"抗流量分析,带宽略损,家用可选"

#### 17.3 VICI cert policy(0.2d)
- `configs/strongswan.conf`:
  ```conf
  charon {
      filelog { ... }  # 已 PR-13 修
      vici {
          # 限制 vici socket 到 localhost + unix socket
          listen = unix:///var/run/charon.vici
      }
  }
  ```
- 注释:`vici` 默认监听 unix socket,不暴露 TCP 防远程利用

**用户感知**:
- server 迁移时客户端零感知(redirect)
- 抗流量分析(可选)
- VICI 不暴露网络(安全)

**测试**:
- `swanctl --redirect` 真发到 netns 客户端验接收

---

### PR-18 ingress IFB(上传限速)+ tc sqm(1.0d,可选)

**背景**:借鉴 net N04 + wondershaper / OpenWrt sqm-scripts。

**改动清单**:

#### 18.1 IFB(0.4d)
- `scripts/ikev2-updown`:
  ```bash
  # egress: tc qdisc add dev $IF handle 1: root htb
  # ingress: tc qdisc add dev $IF handle ffff: ingress
  #         + tc filter add dev $IF parent ffff: ... mirred egress redirect dev ifb0
  #         + tc qdisc add dev ifb0 root htb ...
  ```

#### 18.2 limiter.go 加 ingress(0.3d)
- [internal/limit/limiter.go](file:///opt/ikev2-panel-v2-main/internal/limit/limiter.go):`BuildTcCommands` 加 ingress family(IFB 镜像 ingress 到 egress 限速)

#### 18.3 dual 模式公平队列(0.2d)
- `BuildTcCommands` 加 SFQ per-user(避免单用户挤占带宽)
- `tc qdisc add ... sfq perturb 10`

#### 18.4 文档(0.1d)
- `docs/design.md` §限速章节更新

**用户感知**:
- IPv6-only VPS 上传也限速
- dual 模式 v4+v6 公平
- 单用户挤占 → 公平队列

**测试**:
- netns 真跑 ingress IFB,iperf3 上传限速生效

---

## 四、依赖关系图

```
PR-9 (rate limit)
  ↓ 引用 ratelimit 包
PR-15 (VICI stream)
  ↓ 引用 swanctl 包
PR-12 (续签 atomic)
  ↓ 依赖 PR-10 (swanctl.conf)
PR-14 (DDNS 规范化)
  ↓ 独立
PR-10 (算法去弱)
  ↓ 依赖 PR-11 (Docker 减权)
PR-11 (Docker 减权)
  ↓ 独立
PR-13 (filelog 脱敏)
  ↓ 依赖 PR-10
PR-16 (PQC)
  ↓ 依赖 PR-10 + PR-11
PR-17 (redirect/TFC)
  ↓ 依赖 PR-10
PR-18 (ingress IFB)
  ↓ 独立
```

**关键依赖**:
- PR-9 → PR-15(ratelimit 中间件被 SSE 路由使用)
- PR-10 → PR-11(算法改了 swanctl.conf,Docker 编译选项要跟着改)
- PR-10 → PR-13(算法改了,filelog 配置要反映新算法)
- PR-10 → PR-16(PQC 需要 swanctl.conf 改)
- PR-10 → PR-17(redirect/TFC 需要 swanctl.conf 改)

**推荐执行顺序**:PR-11 → PR-9 → PR-10 → PR-12 → PR-13 → PR-14 → PR-15 → PR-16 → PR-17 → PR-18

---

## 五、测试策略

### 单元测试
- 每个 PR 加 3-5 个 unit test
- 关键包:`internal/auth/ratelimit_test.go` / `internal/dns/retry_test.go` / `internal/swanctl/listener_test.go` / `internal/limit/limiter_test.go`

### 集成测试
- `internal/web/pr9_smoke_test.go` / `pr10_smoke_test.go` / ...
- httptest server 模拟完整路径

### Netns E2E
- 关键路径用 netns 真跑:
  - PR-10 算法变更 → netns 跑 swanctl + charon 验算法列表
  - PR-12 LE 续签 → 模拟 cert 切换
  - PR-16 PQC → 验 ml-kem-768 真协商
  - PR-18 ingress IFB → iperf3 上传限速

### 真实部署验证
- 每个 PR 完成后:rebuild image + `docker compose up` + curl /healthz 验

---

## 六、Release 节奏

| 版本 | 内容 | 时间 |
|------|------|------|
| **v2.86.0-alpha** | PR-9 + PR-11(核心安全) | 0.0d |
| **v2.86.0-beta** | + PR-10 + PR-12 + PR-13 + PR-14 | 0.0d |
| **v2.86.0-rc1** | + PR-15 + PR-16 + PR-17 | 0.0d |
| **v2.86.0** | 全部 PR + docs/release-notes-v2.86.md | 0.0d |

---

## 七、风险与回滚

### 每个 PR 单独打 git tag(便于回滚)
- `v2.86.0-pr9` / `v2.86.0-pr10` / ...

### 关键 PR 的回滚路径

| PR | 回滚命令 |
|----|---------|
| PR-10 算法去弱 | 改 swanctl.conf 回 `modp2048`,`docker compose restart` |
| PR-11 Docker 减权 | 改回 `privileged: true`,rebuild |
| PR-12 acme.sh pin | 改回 `git clone --depth 1`,rebuild |
| PR-14 DDNS 规范化 | 改回旧 sync.go |
| PR-16 PQC | 改回旧 swanctl.conf,删 `--enable-pqc` |

### 关键风险
- **PR-10 算法去弱**:可能影响老客户端(Win7,旧 Android),release notes 强提示
- **PR-11 Docker 减权**:tc / iptables / swanctl 任一缺 cap 会启动失败,需要严格测试
- **PR-16 PQC**:strongSwan 6.0.2 PQC 不稳定,可能编译失败
- **PR-15 VICI stream**:govici 的 Listen API 在新版可能改签名

---

## 八、Next Step

**Phase 6 已就位 9 个 PR + 总览计划**,总计 8.5-9.5d。

按你"深入去做"的方向,我建议:

**方案 A:Sprint 1(2.5-3.2d)优先**
- PR-11 Docker 减权 + 清华源 GPG(1.0d)
- PR-9 登录 rate limit + HTTP 安全 header(1.0d)
- PR-10 算法去弱 + 分片 + childless(1.2d)
- → v2.86.0-alpha 发布

**方案 B:Sprint 1 + 2(5.7d)**
- Sprint 1(3.2d) + Sprint 2(2.7d:续签 + 日志 + DDNS)
- → v2.86.0-beta 发布

**方案 C:全部 9 PR(9.5d)**
- 一次性发布 v2.86.0
- 包含 PQC(2027+ 标准)

你选哪个?或者你想先开某个特定 PR(比如"先做 PQC"或"先做 Docker 减权")?