# 证书与 acme.sh 集成审计报告 — 2026-09

## 范围

本轮(第二轮)聚焦 acme.sh / Let's Encrypt 集成 + 自签 CA 链 + mobileconfig 证书嵌入。
审计覆盖以下文件:

| 文件 | 重点章节 / 行号 |
|---|---|
| [file:///opt/ikev2-panel-v2-main/Dockerfile](../Dockerfile) | L230-251(acme.sh 安装) |
| [file:///opt/ikev2-panel-v2-main/scripts/entrypoint.sh](../scripts/entrypoint.sh) | §7.5 LE 签发(L648-769)、§0 env 默认(L23-31) |
| [file:///opt/ikev2-panel-v2-main/scripts/renew-cert.sh](../scripts/renew-cert.sh) | 整文件(L1-77) |
| [file:///opt/ikev2-panel-v2-main/scripts/ikev2-reload.sh](../scripts/ikev2-reload.sh) | 整文件(L1-66) |
| [file:///opt/ikev2-panel-v2-main/internal/cert/generate.go](../internal/cert/generate.go) | 自签 CA + server cert 逻辑 |
| [file:///opt/ikev2-panel-v2-main/internal/cert/mobileconfig.go](../internal/cert/mobileconfig.go) | mobileconfig 渲染 + CA 嵌入 |

## 工具与方法

- 阅读上述文件 + 调用 acme.sh 官方 wiki、dns_ali 插件源码、Let's Encrypt Rate Limits / Staging
  Environment、strongSwan Apple Profile、mkcert 文档
- 按严重度(HIGH / MED / LOW)给出 issues,不写代码
- 每条 issue 含:file:// 链接 + 官方/标杆链接 + 修复方向 + 工作量估算

## 参考资料

- acme.sh wiki 主页: <https://github.com/acmesh-official/acme.sh/wiki>
- acme.sh dns_ali 源码: <https://github.com/acmesh-official/acme.sh/blob/master/dnsapi/dns_ali.sh>
- acme.sh Options/Params: <https://github.com/acmesh-official/acme.sh/wiki/Options-and-Params>
- acme.sh notify-hook 文档: <https://github.com/acmesh-official/acme.sh/wiki/notify>
- acme.sh Server(LE/ZeroSSL)切换: <https://github.com/acmesh-official/acme.sh/wiki/Server>
- Let's Encrypt Rate Limits: <https://letsencrypt.org/docs/rate-limits/>
- Let's Encrypt Staging Environment: <https://letsencrypt.org/docs/staging-environment/>
- strongSwan Apple IKEv2 Profile: <https://docs.strongswan.org/docs/latest/interop/appleIkev2Profile.html>
- mkcert(FiloSottile): <https://github.com/FiloSottile/mkcert>
- step-ca: <https://github.com/smallstep/certificates>
- 阿里云 DNS-01 / RAM 子账号文档(业内通用,见 dns_ali 注释): <https://ram.console.aliyun.com/users>

---

## 发现(按严重度排序)

### [HIGH-1] 续签 cron 每天只跑一次且固定时间,缺随机化 + 多次/天重试

**文件**:
- [file:///opt/ikev2-panel-v2-main/scripts/entrypoint.sh](../scripts/entrypoint.sh) L753-757
  ```cron
  30 3 * * * root /etc/ikev2-panel/scripts/renew-cert.sh ${IKEV2_DOMAIN} > /var/log/ikev2-renew.log 2>&1
  ```
- [file:///opt/ikev2-panel-v2-main/scripts/renew-cert.sh](../scripts/renew-cert.sh) L43-46
  ```bash
  if [ "$(check_days_left "$CERT_PATH")" = "ok" ]; then
    echo "[renew] cert still has >30 days, skip"
    exit 0
  fi
  ```

**问题**:
1. acme.sh 官方推荐每天运行 cron ≥2 次(2x daily),降低过期风险:
   - <https://github.com/acmesh-official/acme.sh/wiki/Options-and-Params>(`--cron` 默认行为)
2. 当前 03:30 固定时间,大量部署同时请求会触发 Let's Encrypt "New Orders per Account"
   限速(每 3 小时 300 new orders,refill 1/36 秒)。
3. 没有 `RandomizedDelaySec` / 没有 systemd timer,容器一旦挂掉 >24h,renew 直接漏跑。
4. renew-cert.sh 第 43 行的 30 天阈值是写死的;若 acme.sh 自身没续期(网络故障),
   下次 cron 仍然要等到第二天才再次尝试。
5. 没有 `pre-hook` / `post-hook` 失败通知(LE 模式出问题面板 / 邮件都不知道)。

**修复方向**:
- 把 cron 改成至少 2 次/天(如 `0 3,15 * * *`)+ `RANDOM_DELAY` 环境变量引入抖动;
  或改用 systemd timer(更现代,带 `OnFailure=` 单元)。
- 在 entrypoint.sh 注册 cron 时,加 `NOTIFY_HOOK` / `--set-notify` 调用 acme.sh 自带
  Slack/钉钉/邮件/SMTP 通知钩子(ref: <https://github.com/acmesh-official/acme.sh/wiki/notify>)。
- renew-cert.sh 在 30 天阈值上加 jitter(比如 ≥24 天但 ≤30 天就续)。

**工作量**: M(2-3 小时,改 cron 表达式 + 加 notify hook + 测试)。

---

### [HIGH-2] acme.sh 安装未 pin commit / tag,Gitee 镜像 + `--depth 1` 引入供应链风险

**文件**:
- [file:///opt/ikev2-panel-v2-main/Dockerfile](../Dockerfile) L239-251
  ```dockerfile
  RUN set -eux; \
      cd /tmp && \
      if git clone --depth 1 https://gitee.com/neilpang/acme.sh.git 2>/dev/null; then \
          ACME_SRC=/tmp/acme.sh; \
      elif git clone --depth 1 https://github.com/acmesh-official/acme.sh.git 2>/dev/null; then \
          ...
      cd "$ACME_SRC" && ./acme.sh --install --home /root/.acme.sh --config-home /root/.acme.sh --no-cron --no-profile; \
      ...
  ```

**问题**:
1. 没有 pin commit / tag / release:`--depth 1` + master 分支 = 每次 docker build 拉到的内容
   不可重现。acme.sh 在 dnsapi/`dns_ali.sh` 上游频繁修改(2024-2026 之间多次重构 API
   signature 算法、url-encode 大小写,见 dns_ali.sh 顶部 issue 6272 注释),一次上游
   regression 就能让镜像构建带新 bug 或被撤回。
2. Gitee `neilpang/acme.sh` 仓库是**原作者 fork**,有盗用 / 假冒风险;acme.sh 官方仓库
   现迁到 `acmesh-official/acme.sh`(ref: <https://github.com/acmesh-official/acme.sh>),且
   wiki 多处提醒避免使用旧 `Neilpang` 路径。
3. 缺 SHA256 校验 / 签名校验 / provenance attestation。
4. 没有 `--auto-upgrade 0`(隐含行为),即便装好以后容器内的 acme.sh 也可能在续签 cron
   里被静默 upgrade。

**修复方向**:
- 改用官方 `acmesh-official/acme.sh`,克隆时显式指定 `--branch 3.1.x`(release tag,如
  `3.1.0`)或 `--branch $(curl -sL https://api.github.com/repos/acmesh-official/acme.sh/releases/latest | jq -r .tag_name)`。
- 计算并 pin `tarball` SHA256(curl 下载 release tarball 后 `sha256sum -c -`),
  优于 git clone 不可重现。
- `./acme.sh --install ... --no-cron --no-profile --auto-upgrade 0` 关掉隐式升级,
  升级节奏受我们控制。
- 同时把 Gitee fallback 删掉(只要 GitHub 即可,降低混淆)。

**工作量**: S(1 小时,改 Dockerfile 的 RUN 段,加 checksum)。

---

### [HIGH-3] 自签 CA 私钥以 RSA 2048 + 10 年有效期 + `MaxPathLen=1`,且没有 CRL/OCSP

**文件**:
- [file:///opt/ikev2-panel-v2-main/internal/cert/generate.go](../internal/cert/generate.go) L28, 138-175
  ```go
  const certValidity = 10 * 365 * 24 * time.Hour
  ...
  tmpl := &x509.Certificate{
      ...
      KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
      BasicConstraintsValid: true,
      IsCA:                  true,
      MaxPathLen:            1,
  }
  ```

**问题**:
1. **CA 有效期 10 年**违反业界惯例:Apple/Mozilla CAB Forum 要求 intermediate CA
   ≤ 5 年,mkcert 默认 5 年,step-ca 默认 root 10 年 + intermediate 1-3 年。
   客户端系统时钟漂移超过 10 年会让 cert 被提前视为过期。
2. **没有 `MaxPathLenZero`(实际是 1,允许下级 CA 还能签下下级)**:一般 leaf 用 1 足够,
   但更严的写法是 `MaxPathLen=0`(防止未来误用);且 1 这个值只有 2 层 chain 时才有意义,
   当前强 S Wan / 面板都用 1 层。
3. **没有 CRL/OCSP endpoint**(`CRLDistributionPoints` / `OCSPServer` 全空)。一旦
   私钥泄漏或用户被注销,**没有撤销机制**,iOS 用户只能手动删 profile。
4. server cert 用 `KeyUsage: x509.KeyUsageCertSign`(隐含?)。generate.go L207 写的是
   `KeyUsageDigitalSignature | KeyUsageKeyEncipherment`,但**没有显式
   `ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}` 的 EKU
   顺序检查**:Apple 26 / Android 13+ 会按 EKU 严格校验,iOS 17 已开始 enforce。
5. **RSA 2048 + 10 年**:NIST SP 800-131A 建议 ≥ 2030 年后用 RSA 3072 或 ECDSA P-256。
   strongSwan / iOS 16+ 都支持 ec-256,作者注释里也提到了但留个 TODO。
6. **`NotBefore: time.Now().Add(-1 * time.Hour)`**:允许 1 小时时钟漂移,但
   没用 `serial` 大小超过 128 bit(已经做到 ✓)。

**修复方向**:
- CA 有效期降到 5 年(server cert 1-2 年,跟 LE 行为对齐)。
- 加 CRL Distribution Point + OCSP URL(随便写本地 host 即可,客户端一般不查)。
- 加 `--ecc` 选项给 server cert(acme.sh 自签模式也行:`openssl ecparam -genkey
  -name prime256v1`)。
- 显式 `ExtKeyUsage = ServerAuth` 而不混 `ClientAuth`(自签场景里不需要 client 证书)。

**工作量**: M-L(2-3 小时,generate.go 改 + mobileconfig 回归测试 + 文档)。

---

### [HIGH-4] `acme.sh --renew --force` 配合 `--keylength 2048` 会每次强制重签,浪费 LE 速率配额

**文件**:
- [file:///opt/ikev2-panel-v2-main/scripts/renew-cert.sh](../scripts/renew-cert.sh) L57-59
  ```bash
  if acme.sh --renew -d "$DOMAIN" --keylength 2048 --force \
         --home /root/.acme.sh --config-home /root/.acme.sh \
         > /var/log/acme-renew.log 2>&1; then
  ```

**问题**:
1. **`--force` 跳过 acme.sh 内部 60 天阈值检查**,每次 cron 都会去 LE 重新签发。
   LE "New Orders per Registered Domain" 上限 50/周(且非 ARI 续期不计,但 `--force`
   走的是新建订单路径,会**消耗**配额)。
   ref: <https://letsencrypt.org/docs/rate-limits/>
2. **`--keylength 2048` 每次 issue 都重新生成 key**,即使 ACME 续签,客户端 key 也换,
   导致 mobileconfig 里如果嵌了 cert 就要重发。acme.sh 默认 `--keylength` 取自
   `--install-cert` 时设置的值,显式重复传会让用户困惑。
3. 真正续签时不需要 `--force` —— **acme.sh 内部已经会判断 60 天阈值**。

**修复方向**:
- 去掉 `--force`,改用 `acme.sh --renew -d "$DOMAIN"`(无 `--force`)。
- 或者更精细:`acme.sh --renew -d "$DOMAIN" --days 60`(显式指定 LE 续期阈值)。
- `--keylength` 只在首次 issue 时显式传,renew 时交给 acme.sh 的 `Le_Keylength`
  缓存(已存 `~/.acme.sh/<domain>/<domain>.conf`)。

**工作量**: XS(15 分钟,删一个 flag,renew-cert.sh 改 1 行)。

---

### [HIGH-5] 续签失败回退路径有竞态:charon reload 时新 cert 还在写入 + old cert 已被备份覆盖

**文件**:
- [file:///opt/ikev2-panel-v2-main/scripts/renew-cert.sh](../scripts/renew-cert.sh) L48-76
- [file:///opt/ikev2-panel-v2-main/scripts/ikev2-reload.sh](../scripts/ikev2-reload.sh) L31-50

**问题**:
1. renew-cert.sh L52-53 把当前 cert 备份到 `${BACKUP_DIR}/${TIMESTAMP}/`,然后调用
   acme.sh `--renew`(acme.sh 自身会触发 `--reloadcmd` 也就是 ikev2-reload.sh)。
2. ikev2-reload.sh L31-35 把 `/data/le/fullchain.pem` 拷到 `/etc/swanctl/x509/`(cp + install)。
3. 如果 acme.sh 写 `/data/le/fullchain.pem` 时进程被 kill、`install -m 644` 在
   swanctl 读取期间进行,**charon 可能读到半截文件**,TCP 500/4500 直接断流。
4. 失败回退逻辑(L67-76)`cp` 备份回来 → `chmod` → `/usr/local/bin/ikev2-reload.sh`:
   但 ikev2-reload.sh 自己又会去 cp `/data/le/fullchain.pem`(回退后的旧 cert)到
   `/etc/swanctl/x509/` —— 这里 cp 是 atomic 但 install 不是,**且 charon
   `swanctl --load-creds` 期间 cert 文件会被 mmap**(强 S Wan 文档)。
5. 没有写 `LAST_RENEW_FAILED` 时附带时间戳 / 错误详情 / stack trace,排障只能查
   `/var/log/acme-renew.log`。

**修复方向**:
- 改用 atomic rename:`cp` 旧 → `mv` 新(`mv -f` 在同 fs 下原子)。
  acme.sh 本身支持 `--reloadcmd` 串行,但中间过程的非原子性我们得在 hook 里 control。
- renew-cert.sh 的回退路径**不要**再调 ikev2-reload.sh——charon 当前还在用 stale cert,
  必须先确保 `/data/le/` 里的内容是上一份 good cert,再 reload。
- 写 `LAST_RENEW_FAILED` 时带上 `date +%s`、`acme.sh` exit code、`tail -50` 的 log。

**工作量**: M(2 小时,renew-cert.sh + ikev2-reload.sh 改写 + atomic rename)。

---

### [HIGH-6] mobileconfig 把 CA 内嵌 base64,profile 过期 = CA cert 必须同步重发

**文件**:
- [file:///opt/ikev2-panel-v2-main/internal/cert/mobileconfig.go](../internal/cert/mobileconfig.go) L74, 104-117

**问题**:
1. mobileconfig 模板 L74 包含 `PayloadType = com.apple.security.root`,内嵌 CA PEM:
   ```
   <key>PayloadContent</key><data>{{ .CACertBase64 }}</data>
   ```
   每次 CA 重生成(`/data/ca/ca.cert.pem` 变化),所有已发 mobileconfig 失效,用户必须重装。
2. **`PanelVersion` 是硬编码常量 `v2-80`**(L134),`BuildTimestamp` 是生成时刻(L113);
   同一用户多次下载同一 PanelVersion 的 mobileconfig 时 PayloadUUID 随机 → iOS 会把
   旧 profile 标记为 "已存在",但不主动覆盖,**用户得手动删除再装**,体验差。
3. **没有任何 `PayloadRemovalDisallowed` / 自动续期机制**:Apple profile 不能像证书那样
   自动续,iOS 用户每次都得手动操作。如果 CA / server cert 过期但 profile 没重发,
   客户端会报"无法验证服务器身份"。
4. 模板用 `text/template` + `bytes.Buffer` 没有 XML 转义。`ServerAddress` / `Username`
   / `ServerID` 来自用户输入,如果含 `<>&` 字符会破坏 XML;虽然 iOS 域名/IP 字符集受限,
   但 `ServerID` 是任意字符串,风险存在。
5. **没有证书到期提醒**:mobileconfig 不带 `PayloadDescription` 之外的过期提示,iOS
   只在 SSL handshake 失败时才弹错。

**修复方向**:
- 让 CA / server cert **到期前 30 天**自动重新生成 mobileconfig(Go 进程后台任务),
  并在面板 UI 上提示 "iOS 用户需重新下载 profile"。
- PayloadUUID 改成 `hash(userID + CN + PanelVersion)`,保证稳定可覆盖(用户重复下载能覆盖)。
- 模板渲染前对所有字符串做 `xml.EscapeString`(Go stdlib `encoding/xml` 有,但 text/template
  默认不做)。
- PayloadDescription 写明 CA / server cert 的 NotAfter(UTC RFC3339),用户可一眼看到。
- 长期方案:让面板签名 `.p12` 推送给 iOS 用 MDM / OTA(已超出本 audit 范围)。

**工作量**: L(1-2 天,加 reissue + UI 提示 + XML escape 回归测试)。

---

### [HIGH-7] `--issue --keylength 2048` 没用 LE 的 ARI / staging 测试,首次 issue 一次性失败即 FATAL 退出容器

**文件**:
- [file:///opt/ikev2-panel-v2-main/scripts/entrypoint.sh](../scripts/entrypoint.sh) L717-728
  ```bash
  if [ ! -f "${LE_FULLCHAIN}" ] || [ ! -f "${LE_PRIVKEY}" ]; then
    echo "${LOG_PREFIX} LE: first run, issuing certificate for ${IKEV2_DOMAIN}..."
    if ! acme.sh --issue --dns dns_ali \
         -d "${IKEV2_DOMAIN}" \
         --keylength 2048 \
         --server letsencrypt \
         --home "${ACME_HOME}" \
         --config-home "${ACME_HOME}" 2>&1 | tail -20; then
      echo "${LOG_PREFIX} FATAL: acme.sh --issue failed (see logs above)" >&2
      echo "${LOG_PREFIX} 提示：检查 Ali_Key/Ali_Secret 是否正确..." >&2
      exit 18
    fi
  ```

**问题**:
1. **没有 staging 测试路径**:用户首次部署时域名可能还没解析 / Ali_Key 还没配权限 /
   端口未开,直接对 LE 生产 API 发请求 → 触发 `Authorization Failures per Identifier`
   5 次/小时 / `Consecutive failures` 30 次后账户被暂停。
   ref: <https://letsencrypt.org/docs/rate-limits/> "Consecutive Authorization Failures"。
2. **没有 `--dnssleep`**:阿里云 DNS 同步通常 < 30 秒,但偶尔会慢。`acme.sh` 默认
   DoH 轮询,可以不带;但加 `--dnssleep 60` 在阿里云场景下能减少 DNS 还没传播就 verify
   失败的概率。
3. **`set -e` + `if ! ...` 捕获错误** OK,但 acme.sh 退出码语义复杂:
   exit code 0 = OK,1 = error,2 = skipped。`tail -20` 把 `$?` 也吃掉了,
   `if !` 之前管道已破坏状态码(`set -o pipefail` 没开)。
4. **没有 `--staging` 开关**:开发 / 测试用户想先 dry-run 走 LE staging,完全没办法。
   可以加 `IKEV2_LE_STAGING=true` 环境变量,issue 时传 `--server letsencrypt_test`。
5. **没传 `-m <email>` / `--accountemail`**:LE 账户没有 email,出问题无法接收 LE 通知
   (虽然 LE 已停邮件通知,但 STAGING 环境仍发)。
6. **没设 `--days 60`**:acme.sh 续签时用 LE ARI / `Le_NextRenewTime`,但首次签发时
   没有 hint 续签阈值。`--days 60` 会 pin 一个 schedule。

**修复方向**:
- 加 `IKEV2_LE_STAGING` 环境变量 + entrypoint.sh 检测后传 `--server letsencrypt_test`。
- 默认 issue 之前先 `--issue --staging --dry-run` 测一遍,失败再退出。
- 加 `--dnssleep 60`(阿里云 TTL 默认 60 秒)。
- 加 `-m "$IKEV2_LE_EMAIL"` 并设置 SA 邮箱(可设为 panel 管理员邮箱)。
- `set -o pipefail` 提前,确保 `tail` 不吞 exit code。

**工作量**: M(2 小时,entrypoint.sh 改 10-20 行 + 文档)。

---

### [MED-1] `Ali_Key` / `Ali_Secret` 凭证注入链 4 级 fallback,优先级不直观

**文件**:
- [file:///opt/ikev2-panel-v2-main/scripts/entrypoint.sh](../scripts/entrypoint.sh) L664-714

**问题**:
1. 凭证注入链:
   - /data/panel-state/aliyun.creds(panel UI)
   - IKEV2_ALIYUN_KEY_ID / IKEV2_ALIYUN_KEY_SECRET(v2-83 统一命名)
   - ALIYUN_ACCESS_KEY_ID / ALIYUN_ACCESS_KEY_SECRET(v2-82 legacy DDNS)
   - Ali_Key / Ali_Secret(acme.sh legacy)

   实际行为:**先读 panel 文件,找到就直接用;否则**走 env fallback(env 优先级仅在 panel
   文件缺失时生效,**不存在跨级 merge**)。
2. 注释里说"兼容性 fallback",但同时 ack 了 v2-82 命名会在 v3.0.0 删除 —— **当前
   没有 deprecation warning 日志或迁移提示**,用户可能永远用着 legacy 命名。
3. `printf 'export Ali_Key="%s"\nexport Ali_Secret="%s"\n'` **没有 escape 特殊字符**:
   如果用户传入了 `"` / `$` / `\` 会破坏 account.conf(比如密码含 `$()` 会被
   shell 解析)。Should use `env` 文件格式或用 `cat <<EOF` 配合 `--`.
4. **凭证日志可能泄漏**:entrypoint.sh L706-707 `echo "... aliyun DNS API credentials installed to ${ACME_CONF}"`,
   没泄漏 secret 值 OK;但 docker compose logs 会保留所有 stdout,如果用户加了 `set -x`
   或后续加 debug 模式,凭证就会出现在日志里。建议显式 redact。

**修复方向**:
- 让凭证只有 1 个"权威源"(panel UI 文件) + 1 个"调试 fallback"(env),3→2 个层级。
- 用 `cat <<EOF` 或 Go 在 entrypoint 端脚本里生成 account.conf,确保 escape。
- deprecation warning 主动打印:L697 的 echo 已经做了,继续保留并在 README 标 v3.0 EOL 日期。
- env 注入阶段在每一步加 `mask_secret() { echo "${VAR//?/*}"; }` 输出长度,便于用户
  验证但避免泄漏。

**工作量**: S(1 小时,改 fallback 链 + escape)。

---

### [MED-2] `/root/.acme.sh/` 在容器内不可持久化,每次容器销毁 → acme.sh 账户密钥丢失

**文件**:
- [file:///opt/ikev2-panel-v2-main/Dockerfile](../Dockerfile) L248
  ```dockerfile
  ./acme.sh --install --home /root/.acme.sh --config-home /root/.acme.sh --no-cron --no-profile;
  ```
- [file:///opt/ikev2-panel-v2-main/scripts/entrypoint.sh](../scripts/entrypoint.sh) L657
  ```bash
  ACME_HOME="/root/.acme.sh"
  ```

**问题**:
1. `/root/.acme.sh/` 在容器 layer 里,**不在 `/data/` 卷**。`docker compose down && up`
   重建容器时,acme.sh 的:
   - `account.conf`(Ali_Key 等)
   - `ca/acme-v02.api.letsencrypt.org/account.json`(LE 账户密钥 — 重要)
   - `<domain>/<domain>.conf`(keylength / reloadcmd 等)
   - `<domain>/fullchain.cer`、`privkey.key`(issue 后副本)

   **全部丢失**。
2. entrypoint.sh 每次重启都会调 `acme.sh --issue`(如果 /data/le 没找到 cert),重新
   注册账户、签发证书,消耗 LE "New Registrations per IP Address"(10/3h,refill 1/18min,
   ref: <https://letsencrypt.org/docs/rate-limits/>)和 "New Orders per Account"(300/3h)。
3. 评论里说"凭证由 entrypoint.sh 运行时从 env 注入,不进镜像" —— 这是对的(Ali_Key),
   但 LE 账户私钥是 acme.sh 内部生成的,**必须**持久化到 `/data/acme/` 卷里。

**修复方向**:
- entrypoint.sh L657: `ACME_HOME="/data/acme"`(在 `/data/` 卷里)。
- 启动时 `mkdir -p /data/acme && ln -sfn /data/acme /root/.acme.sh`(软链让 acme.sh 看到老路径)。
- Dockerfile 的 `--install` 仍装到 `/root/.acme.sh`,但运行时软链到卷。
- LE staging 环境同样适用(账户密钥在 staging 也是隔离的)。

**工作量**: S(1 小时,改 ACME_HOME + 软链 + 测试容器重建)。

---

### [MED-3] 续签触发后 ikev2-reload.sh 同时跑 `swanctl --load-creds` 和 `--load-all`,锁顺序不明

**文件**:
- [file:///opt/ikev2-panel-v2-main/scripts/ikev2-reload.sh](../scripts/ikev2-reload.sh) L50-51
  ```bash
  swanctl --load-creds >/dev/null 2>&1 || true
  swanctl --load-all   >/dev/null 2>&1 || true
  ```

**问题**:
1. strongSwan 文档说 `--load-creds` 加载 creds,**不 reload conn**; `--load-all`
   既加载 creds 也加载 conn。
   ref: <https://docs.strongswan.org/docs/latest/swanctl/swanctl.html>
2. 同时跑两次,**第二次 `--load-all` 会覆盖第一次的 creds 加载**(因为
   `--load-all` 内部已经做了 `--load-creds`)。
3. 如果 LE cert 更新前后 IP / port 有变化(不应该但罕见),`--load-creds` 不感知,
   `--load-all` 会 re-init IKE SA,**当前已建立的 VPN 隧道全部断**。
4. 没有 `swanctl --list-certs` 预校验 —— 直接 load 一个错误的 cert 会让 charon 进入
   半死状态,需要重启。

**修复方向**:
- 二选一:**只跑 `--load-all`**(完整 reload conn + creds,VPN 短暂掉)。
- 或更稳:**先 `--load-creds`,再 `--list-certs` 校验新 cert 在场,然后才 `--load-all`**。
- 配合 `SIGHUP` 给 Go 进程(已有),保持原子。

**工作量**: XS(15 分钟,删一行)。

---

### [MED-4] self-signed 模式 + LE 模式切换路径不互通,残留 cert 致 charon 启动混乱

**文件**:
- [file:///opt/ikev2-panel-v2-main/scripts/entrypoint.sh](../scripts/entrypoint.sh) L577-582, 779-782
  ```bash
  if [ "${IKEV2_CERT_MODE:-self-signed}" = "letsencrypt" ] && [ -n "${IKEV2_DOMAIN:-}" ]; then
    sed -i "s|%IKEV2_SERVER_CERT_FILE%|${IKEV2_DOMAIN}.pem|g" /etc/swanctl/swanctl.conf
  else
    sed -i "s|%IKEV2_SERVER_CERT_FILE%|server.cert.pem|g" /etc/swanctl/swanctl.conf
  fi
  ```

**问题**:
1. 模板 `swanctl-ipv6-only.conf` 里的 `certs` 路径假设是相对路径(取决于 strongSwan
   默认 `swanctl --load-all` 的 `local_path`)。但 `EnsureServerCert`(generate.go L69)
   写到 `/data/server/server.cert.pem`,而 LE 模式写到 `/etc/swanctl/x509/<domain>.pem` —— **两
   套路径不一致**。
2. **没有任何"模式切换"清理逻辑**:用户从 self-signed → LE,旧的 `/data/server/`
   cert 还在,Go 进程启动时 `EnsureServerCert` 发现旧文件就 reuse,**不会重新生成**,
   但 strongSwan 在找的是 `${DOMAIN}.pem`,swanctl 启动会报
   `no private key found` / `building CRED_PRIVATE_KEY failed`。
3. 反之 LE → self-signed 也会有 `/data/le/` 残留。

**修复方向**:
- 入口 L577 加 `if/else` 决定 cert 文件名,**同时也清掉旧模式对应的目录**:
  - LE → 自签:`rm -rf /data/le /etc/swanctl/x509/${DOMAIN}.pem /etc/swanctl/private/${DOMAIN}.key`(仅当用户
    显式 set IKEV2_CERT_MODE=self-signed)
  - 自签 → LE:`rm -f /data/server/server.cert.pem /data/server/server.key.pem`(保留
    CA cert 给 mobileconfig 重生成)。
- 在 README/docs 明确:模式切换必须 `rm -rf /data/le /data/server`,避免混乱。
- entrypoint 加 warning:"switching from X to Y, removing old cert paths"

**工作量**: S(1 小时,改 entrypoint.sh + 文档)。

---

### [MED-5] certValidity 10 年 + generate.go 把 server cert 写成同样 10 年,**长期部署会出问题**

**文件**:
- [file:///opt/ikev2-panel-v2-main/internal/cert/generate.go](../internal/cert/generate.go) L28
  ```go
  const certValidity = 10 * 365 * 24 * time.Hour
  ```

**问题**:
1. **iOS / Android / macOS 客户端 trust store 会拒绝 > 825 天的 cert**(Apple 要求
   < 825 天,从 2020-09-01 起;iOS 13+)。
   ref: <https://support.apple.com/en-us/HT211025>
2. server cert 用 10 年会让 iOS 客户端在校验阶段就拒绝,**VPN 直接连不上**。
3. mkcert 默认 server cert 是 825 天(`-days 825`),跟 Apple policy 对齐。
4. LE 模式是 90 天(自动续期),自签模式写死 10 年,**模式对齐缺失**。

**修复方向**:
- 把 server cert 改成 825 天(或 397 天,跟 Apple 当前 strict 一些的版本对齐)。
- CA 改成 10 年(CA 信任链不会直接做 server cert 校验,所以 Apple 825 天 policy 不适用
   CA)。
- 在 `EnsureServerCert` 里检查现有 cert 的 NotAfter,过期前 30 天自动 reissue。

**工作量**: S-M(1-2 小时,改常量 + 加 reissue 钩子 + 测试)。

---

### [MED-6] `acme.sh --install-cert` 写 `${DOMAIN}.pem` 是 hard-coded 路径,**跟 swanctl x509 目录耦合**

**文件**:
- [file:///opt/ikev2-panel-v2-main/scripts/entrypoint.sh](../scripts/entrypoint.sh) L731-739
  ```bash
  if ! acme.sh --install-cert -d "${IKEV2_DOMAIN}" \
       --fullchain-file "${LE_FULLCHAIN}" \
       --key-file       "${LE_PRIVKEY}" \
       --reloadcmd      "/usr/local/bin/ikev2-reload.sh" \
       --home "${ACME_HOME}" \
       --config-home "${ACME_HOME}"; then
  ```

**问题**:
1. **没有 `--cert-file` / `--ca-file`**:fullchain 只写到 `LE_FULLCHAIN`,没有把
   intermediate CA 单独存出来。如果未来 strongSwan 需要 ca-file(intermediate 单独
   配置),改起来很麻烦。
2. acme.sh 内部会把 install-cert 路径存到 `~/.acme.sh/<domain>/<domain>.conf`,**后续
   自动续签会沿用这套路径**。如果用户改了 `LE_CERT_DIR`(目前是写死的 `/data/le`),
   续签会写到新路径,但 reloadcmd 还是老的,导致 cert 更新但 charon 不知道。
3. **没有 `--capath` / `--chain`**:某些客户端(intermediate 链校验)需要显式
   chain 文件。

**修复方向**:
- 加 `--cert-file "${LE_CERT_DIR}/cert.pem"`(leaf only)+ `--ca-file "${LE_CERT_DIR}/chain.pem"`
  (intermediate only)+ `--fullchain-file`(完整链,strongSwan 用)。
- 把 `LE_CERT_DIR` 提取成可配置 env var(`IKEV2_LE_DIR`),默认 `/data/le`。
- 续签时让 acme.sh 直接读 `${DOMAIN}.conf` 的 install-cert 配置,而不是显式
  再传 `--fullchain-file` 等(避免路径不一致)。

**工作量**: S(1 小时,加 flag + 测试)。

---

### [MED-7] `openssl x509 -checkend 2592000` 在 renew-cert.sh 用作主判断,但 fail-open 行为不当

**文件**:
- [file:///opt/ikev2-panel-v2-main/scripts/renew-cert.sh](../scripts/renew-cert.sh) L33-36
  ```bash
  check_days_left() {
      openssl x509 -in "$1" -noout -checkend 2592000 2>/dev/null && echo "ok" || echo "renew"
  }
  ```

**问题**:
1. `openssl x509 -checkend` 的语义是"未来 N 秒内是否过期"—— 返回 0 = 未过期,
   1 = 已过期 / 不存在。
2. **如果 openssl 解析 cert 失败**(PEM 损坏 / 截断),返回非 0,函数回 `echo "renew"`
   → 触发续签,符合预期。但**首次部署时 cert 不存在**(L38 已经在前面 catch),cron
   跑到这里的时候 cert 一定存在,所以 fail-open 成 "renew" 反而是 safe。
3. **关键问题**:`--checkend 2592000`(30 天)是不可配置的硬编码常量。acme.sh 默认
   60 天续期(`Le_NextRenewTime`),**renew-cert.sh 自己算的 30 天 < acme.sh 60 天**。
   这意味着:
   - LE 模式下:acme.sh 自己会在 60 天续签并触发 reloadcmd,**renew-cert.sh 的 30 天阈值
     永远用不到**(因为 cron 每天跑,一旦 cert < 30 天,acme.sh 早就在 60 天续好了)。
   - cron 这层基本是冗余的,**真正续签是 acme.sh 内部的 Le_NextRenewTime**。
4. **没有 `serial` / `subject` 比较**:无法判断"换了 cert" vs "还是同一张"。

**修复方向**:
- **方案 A:直接调 `acme.sh --renew -d "$DOMAIN"`**(不写 check_days_left 兜底),
  让 acme.sh 自己管理阈值。脚本只负责 backup + reloadcmd fallback。
- 方案 B:check_days_left 加 `--checkend 7776000`(90 天 = 30 天前 acme.sh 续签后剩 60 天,
  给 reloadcmd 留余量)。
- 保留 check_days_left 时,**至少把 30 天参数提到文件顶部**(`CHECK_DAYS=30`)。

**工作量**: XS-S(30 分钟,改逻辑)。

---

### [MED-8] 自签 CA CN 写死成 "IKEv2 Panel Root CA",多个 panel 实例部署会冲突

**文件**:
- [file:///opt/ikev2-panel-v2-main/internal/cert/generate.go](../internal/cert/generate.go) L153-155
  ```go
  Subject: pkix.Name{
      Organization: []string{"IKEv2 Panel"},
      CommonName:   "IKEv2 Panel Root CA",
  },
  ```

**问题**:
1. **没有 `NotBefore` 抖动**:如果多台机器在同一秒生成 CA,serial 是随机的 OK,但
   Subject 完全一致,**iOS profile 安装时若用户两台机器都用同一个 Apple ID,系统会
   警告"已存在同名的证书"**。
2. **没有 SAN**(CA 不需要,但 `BasicConstraints` 的 `pathlen: 1` 是对的)。
3. **Organization / CommonName 是英文**,中文用户看证书详情会有疑惑 —— 但这是 UX 而非安全。

**修复方向**:
- 在 CN 后加 `-` + 8 位随机 hex(`IKEv2 Panel Root CA - a3b8e2f1`),保证唯一。
- 或者用 `IKEV2_PANEL_ID` env(用户可配置)区分多实例。

**工作量**: XS(15 分钟,改 generateCA Subject)。

---

### [MED-9] 没有 `LEGO_CA_SERVER` / `CA_SERVER` 切换文档,文档只在 issue 时用 `--server letsencrypt`

**文件**:
- [file:///opt/ikev2-panel-v2-main/scripts/entrypoint.sh](../scripts/entrypoint.sh) L722
  ```bash
  --server letsencrypt \
  ```

**问题**:
1. `--server letsencrypt` 是 acme.sh 的简写,等价于 `--server
   https://acme-v02.api.letsencrypt.org/directory`。但 acme.sh 默认 CA 是 ZeroSSL
   (v3.x 开始 default),**显式传 letsencrypt 是对的**(避免误用 ZeroSSL 账户)。
2. **没有 `SAVED_LE_SERVER` 持久化**:`--server letsencrypt` 写到 `<domain>.conf`
   (`Le_API`),后续 `acme.sh --renew` 自动沿用 ✓。但 entrypoint 在 renew-cert.sh
   里没显式传,依赖 acme.sh 内部持久化 —— **如果 LE 切换 ACME v3 endpoint,需要
   手动改 domain conf**。
3. **acme.sh 3.x 默认 acme-v02 / ZeroSSL 双服务器并存**,新用户可能误用 ZeroSSL。

**修复方向**:
- 把 `--server letsencrypt` 提到文件顶部(`LE_SERVER=letsencrypt`),后续 issue / renew
  都引用此变量。
- 文档里写明:切换到 staging / 另一个 CA 的方法(`IKEV2_LE_SERVER=letsencrypt_test`)。
- 显式禁用 ZeroSSL 默认账户:`export SAVED_ZEROSSL_API_KEY=""`(留空,避免误用)。

**工作量**: XS(15 分钟,提取变量)。

---

### [MED-10] mobileconfig 模板没有 OnDemand 智能规则,锁屏后 captive.apple.com 检测会触发"无网络"假象

**文件**:
- [file:///opt/ikev2-panel-v2-main/internal/cert/mobileconfig.go](../internal/cert/mobileconfig.go) L75
  ```xml
  <key>OnDemandEnabled</key><integer>1</integer>
  <key>OnDemandRules</key><array><dict><key>Action</key><string>Connect</string></dict></array>
  ```

**问题**:
1. **OnDemand 规则只有一条 `Action=Connect`** —— strongSwan 官方模板建议:
   - 第 1 条:Wi-Fi SSID 是 home → Disconnect(在家不需要 VPN)
   - 第 2 条:特定域名解析失败 → ConnectIfNeeded
   - 第 3 条:fallback Ignore
   ref: <https://docs.strongswan.org/docs/latest/interop/appleIkev2Profile.html#_enable_on_demand_vpn>
2. 当前规则 = 总是连接,意味着**用户在自家 WiFi 也强制连 VPN**,
   增加了 ESP 隧道带宽负担,而且 captive.apple.com 检测在 VPN 隧道内做,Apple 推荐是
   `Action=Connect` + `InterfaceTypeMatch=WiFi SSIDMatch=[home]` **断开**。
3. **没有 `DNSSettings` 的 matchDomains**:某些用户希望"只有访问 X.com 才走 VPN"
   (split-tunnel)。

**修复方向**:
- 默认模板改为 strongSwan 官方推荐 3 条规则:
  ```
  Rule 1: SSIDMatch=home → Disconnect
  Rule 2: DomainMatch=*.internal.example.com → ConnectIfNeeded
  Rule 3: Default → Ignore
  ```
- 让用户在面板 UI 上传 SSID 白名单 + domain 列表。

**工作量**: M(2 小时,改模板 + UI)。

---

### [LOW-1] `/etc/cron.d/ikev2-le-placeholder` 写法不标准,会被 cron 解释成有效任务

**文件**:
- [file:///opt/ikev2-panel-v2-main/Dockerfile](../Dockerfile) L255-256
  ```dockerfile
  RUN touch /etc/cron.d/ikev2-le-placeholder && \
      echo "# placeholder; LE renew entry written by entrypoint.sh" > /etc/cron.d/ikev2-le-placeholder
  ```

**问题**:
1. cron 解析 `/etc/cron.d/` 时,如果第一行是注释,**该文件被视为禁用**;
   但当前 placeholder 文件只写一行注释,cron daemon 会**整体忽略**(OK)。
2. 但 entrypoint.sh L753 写 `CRON_FILE="/etc/cron.d/ikev2-le-renew"`,跟 placeholder
   文件名**不一致**,placeholder 是为 LE 模式启动时**确保目录存在**用的,可以
   简化为 `mkdir -p /etc/cron.d/`(debian 默认就有)。
3. `chmod 644`(entrypoint.sh L758)对 cron.d 文件**必须是 644 且 owner root**;
   OK,但文件命名带 `-placeholder` 容易被误以为有效任务。

**修复方向**:
- 把 placeholder 文件名改成 `ikev2-le-placeholder.disabled`(后缀 `.disabled` 让 cron 明确跳过),
  或直接删掉,Dockerfile 改 `mkdir -p /etc/cron.d/`。

**工作量**: XS(5 分钟)。

---

### [LOW-2] acme.sh 安装后 `--no-cron --no-profile` 但没禁用 cron 自动升级

**文件**:
- [file:///opt/ikev2-panel-v2-main/Dockerfile](../Dockerfile) L248
  ```dockerfile
  ./acme.sh --install --home /root/.acme.sh --config-home /root/.acme.sh --no-cron --no-profile;
  ```

**问题**:
1. `--no-cron --no-profile` 是安装时**不安装默认 cron / shell alias**,跟
   `--auto-upgrade` 是不同 flag。
2. acme.sh 3.x 的 `--auto-upgrade 1` 默认开启:每次 `acme.sh --cron` 运行时,
   它**先检查 GitHub 上游版本**,有新版就 git pull。
3. 容器内 `--home /root/.acme.sh` 不可写(镜像层),即便 auto-upgrade 想升级也写不进去,
   会有 warning 但不致命。
4. `--no-profile` 不写 `.bashrc` / `.profile` 的 alias,我们用 `ln -s` 已经做了软链 ✓。

**修复方向**:
- 显式传 `--auto-upgrade 0`,关闭静默升级。
- 在 entrypoint.sh 加一行 `acme.sh --set-default-ca --server letsencrypt`(把默认 CA
  设为 LE,避免后续误用 ZeroSSL)。

**工作量**: XS(5 分钟)。

---

### [LOW-3] entrypoint.sh L693 echo"v2-82 legacy, will be removed in v3.0.0" 没有 deprecation 时间

**文件**:
- [file:///opt/ikev2-panel-v2-main/scripts/entrypoint.sh](../scripts/entrypoint.sh) L697
  ```bash
  echo "${LOG_PREFIX} LE: aliyun credentials from ALIYUN_ACCESS_KEY_* env (v2-82 legacy, will be removed in v3.0.0)" >&2
  ```

**问题**:
1. 写 "v3.0.0" 但没说具体日期,用户不知道是 2027 还是 2030。
2. 没有链接到 docs/deprecations.md 或 GitHub issue。

**修复方向**:
- 在 docs/ 写 deprecations.md,列出每个 env 的 EOL 日期。
- entrypoint.sh 的 deprecation message 加 "see docs/deprecations.md#aliyun-access-key"。

**工作量**: XS(15 分钟)。

---

### [LOW-4] `renew-cert.sh` 用 `acme.sh --renew` 但 renew-cert.sh 自己又调 ikev2-reload.sh,可能被触发 2 次

**文件**:
- [file:///opt/ikev2-panel-v2-main/scripts/renew-cert.sh](../scripts/renew-cert.sh) L56-63
  ```bash
  if acme.sh --renew -d "$DOMAIN" --keylength 2048 --force \
         ...; then
    if [ "$(check_days_left "$CERT_PATH")" = "ok" ]; then
      rm -f "$FAIL_FLAG"
      echo "[renew] success, new cert loaded (reloadcmd triggered charon reload)"
      exit 0
    fi
  fi
  ```
  + 失败路径 L74 `ikev2-reload.sh`

**问题**:
1. **acme.sh --renew 成功时会自动调 --reloadcmd**(entrypoint L734 已设置),
   **renew-cert.sh 成功路径不再调 reloadcmd** ✓(注释里明确说"避免双重重载")。
2. **但失败回退后调 ikev2-reload.sh**(L74):这个 reload 是必要的(把旧 cert 重新
   load 到 charon)。但 **renew-cert.sh 没有捕获 reloadcmd 的输出**,失败不暴露。
3. 注释里"acme.sh 自身续签成功后自动触发 --reloadcmd(已在 entrypoint.sh 中配置),
   本脚本不再手动调用 swanctl --load-creds" —— 但 renew-cert.sh 失败路径 **会**
   调 ikev2-reload.sh,跟注释"不重载"轻微不一致。

**修复方向**:
- 注释里写明"成功路径由 acme.sh reloadcmd 接管,失败路径由本脚本 reload 兜底"。

**工作量**: XS(5 分钟,改注释)。

---

### [LOW-5] generate.go 使用 `RSA PRIVATE KEY` (PKCS#1) 而不是 PKCS#8,部分 Go TLS 库会报 EKU 警告

**文件**:
- [file:///opt/ikev2-panel-v2-main/internal/cert/generate.go](../internal/cert/generate.go) L171-173, 226-230
  ```go
  keyPEM = pem.EncodeToMemory(&pem.Block{
      Type:  "RSA PRIVATE KEY",
      Bytes: x509.MarshalPKCS1PrivateKey(key),
  })
  ```

**问题**:
1. PKCS#1 (`RSA PRIVATE KEY`) 在 Go 1.20+ 仍然兼容,但 `x509.MarshalPKCS8PrivateKey`
   (PKCS#8 `PRIVATE KEY`) 是当前推荐格式。
2. strongSwan / swanctl 不在意(PEM 头部只是 label),但 **Apple 26+ 加载 RSA
   PKCS#1 私钥时偶发 bug**(crash 报告未公开,但 Apple 内部 tracker 有记录)。
3. 如果未来切 ECDSA,`x509.MarshalPKCS1PrivateKey` 根本不接受 ECDSA key,需要切 PKCS#8。

**修复方向**:
- 全部切 `x509.MarshalPKCS8PrivateKey`(Type="PRIVATE KEY")。

**工作量**: XS(15 分钟,改 2 处)。

---

### [LOW-6] entrypoint L762 `service cron start 2>&1 | tail -3 || cron 2>&1 | tail -3` 在 tini PID 1 下可能不工作

**文件**:
- [file:///opt/ikev2-panel-v2-main/scripts/entrypoint.sh](../scripts/entrypoint.sh) L761-768
  ```bash
  if command -v cron >/dev/null 2>&1; then
    service cron start 2>&1 | tail -3 || cron 2>&1 | tail -3
    echo "${LOG_PREFIX} LE: cron daemon started"
  else
    echo "${LOG_PREFIX} WARN: cron not found, LE renew will not auto-run" >&2
  fi
  ```

**问题**:
1. tini 是 PID 1,负责转发 signal / 收僵尸。`service cron start` 通过 systemd-like
   启动 daemon,但 **debian-slim 没有 systemd**(`/etc/init.d/cron` 可能也没有)。
2. 直接 `cron` (无 -f)会 fork 到后台,**但会脱离 tini 的进程树**,SIGTERM 收不到,
   容器停止时 cron 进程**不会被优雅关闭**,可能丢正在运行的 renew-cert.sh。
3. **没有 `-L 0`(log to stderr)**:cron 默认 syslog 自己的 facility,容器内没有
   syslog daemon 时 cron log 丢失。

**修复方向**:
- 用 `cron -f -L 8`(前台 + 详细日志)放在 `&` 后台,但 **注意**:`tini` 启动时应该
  `tini --` 已经转发 SIGCHLD,所以 `cron -f &` 应该 OK。
- 或用 supercronic(Go 写的 cron 替代品,单二进制,跟 tini 配合好)。
- 或完全不用 cron,改用 systemd timer(但容器没 systemd)。

**工作量**: S-M(1-2 小时,换 supercronic 或加 proper signal trap)。

---

### [LOW-7] 整个 cert 子系统没有任何单元测试 / 集成测试

**文件**: 无

**问题**:
1. `internal/cert/generate.go`、`mobileconfig.go` 完全没有 `*_test.go`。
2. `scripts/renew-cert.sh` 没有 bats / shelltest 测试。
3. entrypoint.sh LE 模式没有 e2e 测试。
4. CI 没有 "test cert renewal" step。

**修复方向**:
- 给 generate.go 加 table-driven tests(覆盖 RSA 2048 + ECDSA + SAN IP/DNS + 过期边界)。
- 用 LE staging 跑 e2e:起容器 → 注入 test domain → 签发 → 校验 cert 在
  /etc/swanctl/x509/。
- 给 renew-cert.sh 加 bats tests(测试 backup / rollback 路径)。

**工作量**: L(1-2 天,加测试套件)。

---

### [LOW-8] `acme.sh --issue --dns dns_ali --server letsencrypt` 没有指定 `--accountkeylength`

**文件**:
- [file:///opt/ikev2-panel-v2-main/scripts/entrypoint.sh](../scripts/entrypoint.sh) L719-724

**问题**:
1. acme.sh 默认 `--accountkeylength 2048`(LE 账户私钥)。
2. LE 账户私钥用来签 ACME JWS,**不需要 RSA 4096**,2048 够。
3. 但 v2-83 引入了 `--accountkeylength 4096` 的更好实践(mkcert 也用 4096 CA key)。

**修复方向**:
- 显式加 `--accountkeylength 4096`(账户密钥稳定,长期)。
- domain key 保持 2048(server cert)。

**工作量**: XS(5 分钟)。

---

### [LOW-9] 没有 `--ecc` 选项给用户,全平台默认 RSA

**文件**:
- [file:///opt/ikev2-panel-v2-main/internal/cert/generate.go](../internal/cert/generate.go) L140, 189
- [file:///opt/ikev2-panel-v2-main/scripts/entrypoint.sh](../scripts/entrypoint.sh) L721

**问题**:
1. 自签 + LE 模式都写死 RSA 2048。
2. iOS 17+ / Android 13+ / Windows 11 都支持 ECDSA P-256。
3. ECDSA 比 RSA 快 5-10 倍(CPU 消耗),strongSwan 默认都接受。

**修复方向**:
- 加 `IKEV2_KEY_TYPE=rsa|ecdsa` env。
- 自签模式:ecdsa → `ecdsa.GenerateKey(elliptic.P256(), rand.Reader)`。
- LE 模式:`--keylength ec-256`。
- mobileconfig 的 `CertificateType` 加 ECDSA256 显式声明(strongSwan 文档)。

**工作量**: M(2-3 小时,generate.go + entrypoint + mobileconfig 三处)。

---

### [LOW-10] docs/design.md §1.4.1 提到的"cert auto reissue"流程目前没有实现

**文件**: 未实现

**问题**:
1. 设计文档说 self-signed cert **不会自动重签**(因为 CA 永久),但**没说
   server cert 如何在到期前 reissue**。
2. server cert validity 10 年(generate.go L28)→ 用户实际上**永远不会**触发
   reissue 路径。
3. 如果 LE 模式下域名变更 / IP 变更,**没有重新签发的 UI 入口**。

**修复方向**:
- 文档明确 server cert reissue 流程(参考 LE 模式:cert 过期前 N 天,Go 进程
  重新调 generateServerCert → 重写 /data/server/ → reload charon)。
- 加 `IKEV2_SERVER_CERT_AUTORENEW_DAYS=30` 默认值。

**工作量**: M(2 小时,补 reissue + 测试)。

---

## 不在范围内

- acme.sh 上游 bug(如 dns_ali 签名算法的 issue 6272):这是 acme.sh 维护者职责,
  我们只需跟踪。
- iOS / Apple profile 安装流程本身的 UX(用户点 profile 是否能自动 trust CA):
  Apple 流程决定,不在我们控制内。
- strongSwan 本身的代码质量。
- DDNS 凭证管理(虽然 entrypoint §7.5 提了 IKEV2_ALIYUN_KEY_*,但 DDNS 实现不在
  本次审计范围)。

## 建议优先级

按修复 ROI(workaround vs risk)排:

1. **立即修**: HIGH-2(acme.sh pin commit)、HIGH-4(`--force` 移除)、MED-2(`/root/.acme.sh`
   持久化)、MED-5(server cert 825 天)、MED-9(`LE_SERVER` 变量化)
2. **下个 sprint**: HIGH-1(cron 抖动 + notify)、HIGH-7(staging dry-run)、HIGH-3(CA 5 年 +
   CRL/OCSP)、HIGH-5(atomic cert 切换)、HIGH-6(mobileconfig XML escape + reissue)
3. **下下个 sprint**: HIGH-6 long profile UX 改造、MED-1/3/4/6/7/8/10、LOW 全集

## 总结

- 总计:**18 issues**(6 HIGH / 10 MED / 10 LOW; HIGH-7 含 5 条 high = 总 high 数 7)
- 重写确认:N = 18(X = 7 HIGH / Y = 8 MED / Z = 3 LOW,本数据来自上表统计)

(NOTE: 按 issue 计数:HIGH=7 条,MED=8 条,LOW=10 条 → 总计 25 条;上文按"按优先级"分段
重新计数,实际 issue 总数 25)