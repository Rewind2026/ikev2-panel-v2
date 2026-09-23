# v2-83 Release Notes

> 发布日期:2026-09-18
> 主要内容:**阿里云凭证统一** + **面板 HTTPS 证书热重载** + **顺手修 v2-82 告警 bug**

---

## 概要

v2-83 是 v2-82 的姊妹版本,补齐 v2-82 留下的两个 UX 问题:

1. **凭证两套**:DDNS 用 `ALIYUN_ACCESS_KEY_*`,acme.sh 用 `Ali_Key`/`Ali_Secret`,用户配置易混淆
2. **8443 报不安全**:LE 模式下应该用 LE 证书,但续期后面板 HTTPS 不会自动切到新证书

**核心改动**(6 项):

| # | 改动 | 用户感知 |
|---|------|----------|
| 1 | **面板 UI 阿里云凭证卡**(写 `/data/panel-state/aliyun.creds`) | 不用动 .env,UI 填完重启生效 |
| 2 | **凭证统一 4 级优先级链** | 老 env 名兼容,启动时 WARN 一次 |
| 3 | **DDNS 同步直读凭证文件** | 面板改凭证 → DDNS 下次 tick 自动用新凭证(无需重启) |
| 4 | **面板 HTTPS SIGHUP 热重载** | acme.sh 续期后 ~1s 内面板拿到新证书(不再依赖 container restart) |
| 5 | **证书模式默认仍是 self-signed** | v2-82 用户升级**无感**;LE 显式 opt-in |
| 6 | **顺手修 `LAST_RENEW_FAILED` 告警脱钩** | 续签失败立即告警,不用等 30 天证书过期才发现 |

---

## 升级说明

### ⚠️ 行为不变(升级无感)

- 默认 `IKEV2_CERT_MODE` 仍是 `self-signed`(不会破坏现有 mobileconfig 客户端的根 CA 信任)
- 老 env 名 `ALIYUN_ACCESS_KEY_*` / `Ali_Key`/`Ali_Secret` 仍能读(启动时 WARN 一次)
- DDNS 卡片功能不变

### 用户升级步骤(v2-82 → v2-83)

```bash
# 1. 拉取新代码(切到 v2-83 tag)
git pull

# 2. (可选) .env 里把凭证迁移到新名字(更简洁)
# 旧: ALIYUN_ACCESS_KEY_ID / ALIYUN_ACCESS_KEY_SECRET
# 新(推荐): IKEV2_ALIYUN_KEY_ID / IKEV2_ALIYUN_KEY_SECRET
# 不改也能跑(向后兼容),启动会有 WARN 一次

# 3. 重建并启动
./scripts/up.sh --build
```

升级后**凭证仍然生效**(从 env 读,启动日志会显示 `aliyun_creds_source=env-legacy-ddns`)。什么时候想搬到面板,见下节"凭证迁移到面板"。

### 凭证迁移到面板(从 .env 到 UI)

把已经写在 `.env` 里的阿里云凭证搬到面板 UI 卡的步骤:

```bash
# 1. 先别删 .env!启动前 entrypoint 还是从 env 读
# 2. 面板填凭证(留 .env 不动,临时并存):
#    面板 → "阿里云 API 凭证" 卡 → 填 ID + Secret + confirm=yes
# 3. 验证面板顶部"已配置" + KeyID 掩码 + 来源=panelstate
# 4. 重启容器让 entrypoint 从 panel-state 文件读(优先级 1)
docker compose restart
# 5. 启动日志看 "LE: aliyun DNS API credentials loaded from /data/panel-state/aliyun.creds"
# 6. 删 .env 里的 ALIYUN_ACCESS_KEY_* / IKEV2_ALIYUN_KEY_* / Ali_Key,避免双重配置
docker compose restart  # 让 env 变更生效
```

### LE 模式 + 凭证改动后需 restart

面板 UI 改凭证后,**DDNS 下次 tick(默认 60s)立即生效**,但** LE 模式要重启容器才生效**:

```text
为什么:
  DDNS sync 是 Go 进程内的 goroutine,通过 CredentialGetter 函数每次 tick 重新读 /data/panel-state/aliyun.creds
    → 改完凭证后下次 tick(默认 60s)自动用新凭证 → 无需重启 ✅
  acme.sh 是 entrypoint 启动时调用的,运行时改 account.conf 不影响已签发的证书
    → 重启 entrypoint 时重新把凭证注入 /root/.acme.sh/account.conf → 后续续签用新凭证 ⚠️
```

操作步骤:
1. 面板填新凭证(留 confirm=yes,自动用新凭证)
2. **重启容器**:`docker compose restart`
3. 启动日志确认凭证已加载
4. **LE 续签是 cron 驱动**——下次续签才会用新凭证,刚签发完的证书不会自动重签

### 凭证轮换 / 泄漏后处置

```text
场景:AccessKey 泄漏 / 定期轮换 / 阿里云 AccessKey 控制台显示异常活动
```

```bash
# 1. 阿里云控制台立即禁用/删除旧 AccessKey
#    注意:旧 Key 失效后,DDNS + LE 续签会失败
#    → 续签失败会触发面板顶部红色横幅"证书续签失败"(不是 DDNS 失败)
#    → DDNS 失败横幅只在连续 3 次同步失败才显示(刷 throttling)

# 2. 创建新 AccessKey + 最小权限(设计 §19.6 RAM 策略)

# 3. 面板"阿里云 API 凭证"卡填新 Key
#    → DDNS 下次 tick(默认 60s)用新 Key

# 4. 重启容器让 acme.sh 拿新 Key 续签
docker compose restart

# 5. 验证:
docker logs ikev2-panel | grep -i 'aliyun'
#    期望: aliyun_creds_source=panelstate, key_id_masked=LTAI...ab12(新 Key 末 4 位)

# 6. (可选) .env 里的旧 env 全部去掉,避免混淆
```

### 想用 LE 消除浏览器警告?(opt-in)

⚠️ **这一改动会让所有 mobileconfig 客户端**:
- 之前信任的是 mobileconfig 内嵌的**自签 CA**
- 切到 LE 后 server 用 Let's Encrypt 证书,**自签 CA 不再能验证 LE 证书链**
- iOS / macOS / Android 都会弹"无法验证服务器身份",需要**重新分发 mobileconfig**

```bash
# 1. .env 改 LE 模式
sed -i 's/^IKEV2_CERT_MODE=.*/IKEV2_CERT_MODE=letsencrypt/' .env

# 2. 域名解析必须指向 VPS 公网 IP(v6/v4 都行)
# 阿里云 DNS 控制台加 AAAA 记录(可由 DDNS 自动同步,见下)

# 3. 填阿里云凭证(任选一种方式):
#    a. 推荐:在面板 "阿里云 API 凭证" 卡片填(type=password + confirm=yes)
#    b. 命令行:在 .env 里写
#       IKEV2_ALIYUN_KEY_ID=LTAI5t...
#       IKEV2_ALIYUN_KEY_SECRET=...

# 4. 重启容器
docker compose restart

# 5. 等 ~30s,acme.sh 首次签发完成。验证:
curl -vI https://你的域名:8443
# 期望 issuer = "Let's Encrypt" 或 "R10/R12"

# 6. 重新生成所有 mobileconfig 分发给客户端!
# (面板 "用户详情" → 下载 mobileconfig,客户端必须**重新安装 profile**)
```

---

## 用户配置对比

### 之前(v2-82):两套凭证

```bash
# .env
ALIYUN_ACCESS_KEY_ID=LTAI5t...      # DDNS 用
ALIYUN_ACCESS_KEY_SECRET=...
Ali_Key=LTAI5t...                   # acme.sh 用(同一对!)
Ali_Secret=...
```

→ 两份 env,容易写错

### 之后(v2-83):一套

**方式 A:面板 UI 填(推荐)**

```
面板首页 → "阿里云 API 凭证" 卡片:
  AccessKey ID:     [LTAI5t...        ]
  AccessKey Secret: [********         ]
  ☑ 我已确认要保存(confirm=yes)
  [保存凭证]
```

→ 写 `/data/panel-state/aliyun.creds`,0600,atomic rename

**方式 B:.env 写**

```bash
# .env (新统一命名)
IKEV2_ALIYUN_KEY_ID=LTAI5t...
IKEV2_ALIYUN_KEY_SECRET=...
```

→ 老 `ALIYUN_ACCESS_KEY_*` / `Ali_Key` 仍能读(标 deprecated,启动 WARN 一次)

---

## 关键设计决策

### 1. 默认 cert mode 不改(评审 C 建议)

LE 是 opt-in,而不是默认改。理由:
- 切 LE = 客户端根 CA 切换 = 所有 mobileconfig 失效
- 静默改默认会"骗"用户多年发现证书没续(评审 A)
- 用户主动改 `.env` 是知情同意,行为可控

### 2. 凭证放独立目录 `/data/panel-state/`(评审 A 建议)

不放 `/data/le/` 是因为:
- `/data/le/` 已经有 LE 私钥 + 续期 backup
- backup 任务打包上云时 `aliyun.creds` 跟着外泄 = 拿到 RAM 全权限 key + LE 私钥
- 独立目录 + 0700 权限,把"风险资产"集中到一处

### 3. SIGHUP 热重载走 `atomic.Pointer`(评审 B 建议)

`GetCertificate` 回调每次 TLS 握手都调 → 不在握手路径做磁盘 I/O,只做 atomic.Load(O(1) 无锁)
SIGHUP goroutine 负责读 + 解析 + atomic.Store,只在续期(每天 1 次)触发

### 4. DDNS sync 直读凭证文件(评审 B 建议)

不用 env 启动固化,而是每次 tick 通过 CredentialGetter 函数读 `/data/panel-state/aliyun.creds`
→ 面板 UI 改凭证后,DDNS 下次 tick 自动用新凭证(无需重启容器)

### 5. 续期后复制 LE cert 到 panel-tls(评审 A/B 共同建议)

之前只有 `ikev2-reload.sh` 复制到 `/etc/swanctl/`,**没有复制到 `/data/panel-tls/`**
→ SIGHUP 重读的 cert 永远是启动时那份,续期失效
→ v2-83 改为 3 步:复制到 swanctl + 复制到 panel-tls + kill -HUP Go 进程

---

## 文件清单

### 新增

| 路径 | 行数 | 说明 |
|------|------|------|
| `internal/panelstate/state.go` | ~150 | 凭证 I/O + JSON 原子写 + 内存缓存 |
| `internal/panelstate/state_test.go` | ~100 | 6 个测试 |
| `internal/web/handlers_aliyun.go` | ~130 | 凭证卡 API (status/save/clear) |
| `docs/release-notes-v2.83.md` | (本文件) | 升级说明 |

### 修改

| 路径 | 改动 |
|------|------|
| `internal/config/config.go` | 加 `AliyunAccessKeySource` + `loadAliyunCreds()` 4 级优先级链 |
| `internal/ddns/sync.go` | 加 `CredentialGetter` 字段,tick 时按当前凭证构造 client |
| `internal/web/server.go` | 加 `PanelState` + `AliyunAccessKeySource` 字段;3 个新路由 |
| `internal/web/handlers_home.go` | 加 `loadAliyunStatus` + `LAST_RENEW_FAILED` 脱钩修复注释 |
| `web/templates/home_content.html` | 加阿里云凭证卡 + 顶部"未配置"横幅 |
| `cmd/ikev2-panel/main.go` | SIGHUP 监听 + atomic.Pointer 缓存 cert + TLSConfig 预构造 + 启动日志加 source |
| `scripts/entrypoint.sh` | 凭证优先级链(sed 解析 JSON,不依赖 jq) |
| `scripts/ikev2-reload.sh` | 复制 cert 到 panel-tls + kill -HUP Go 进程 |
| `docker-compose.yml` | image tag `v2-82` → `v2-83` |
| `.env.example` | 顶部加 LE opt-in 警告 + 凭证统一段 |
| `docs/design.md` | §19.6 §19.7 v2-83 设计 + §21 路线图 |
| `README.md` | M11 行 + 凭证统一段 |

---

## 测试覆盖

| 包 | 测试 | 状态 |
|----|------|------|
| `internal/panelstate` | 6 个 (MaskKeyID / JSONMarshal / WriteAliyun_Empty / AliyunPath / ClearAliyun / AliyunExists) | ✅ |
| `internal/ddns` | 8 个(已有 v2-82) | ✅ |
| `internal/dns` | 5 个(已有 v2-82) | ✅ |
| `internal/web` | 18 个(1 个调整 TestHome_LEWarning) | ✅ |
| 其他 7 包 | 各包原有测试 | ✅ |
| **总计** | **11 包全绿** | ✅ |

---

## 留作未来扩展(P2+)

- ❌ 凭证加密存储 / KMS / passphrase(过度工程)
- ❌ 运行时改凭证不重启 acme.sh(DNS-01 续期是 cron 驱动)
- ❌ Cloudflare / Godaddy / 其他 DNS provider DDNS(架构已留 AliyunClient 抽象)
- ❌ IPv4 DDNS(当前只动 AAAA)
- ❌ P2 测试覆盖补充(expiry cycle / ipv6watch update path)
- ❌ P3 cleanup 6 项

---

## 故障排查

### 面板 HTTPS 续期后没切到新证书

```bash
# 1. 看 SIGHUP 是否触发
docker logs ikev2-panel | grep -i 'SIGHUP reload'
# 期望: "SIGHUP reload: panel TLS cert updated" + not_after 时间

# 2. 看 ikev2-reload.sh 是否复制到 panel-tls
ls -la /data/panel-tls/
# cert.pem / key.pem 应存在,mtime 应在续期时间之后

# 3. 手动触发 SIGHUP 测试
pidof ikev2-panel  # 拿 PID
kill -HUP <pid>
curl -vI https://localhost:8443 2>&1 | grep -i 'subject\|issuer'
```

### acme.sh 签发失败

```bash
docker logs ikev2-panel | grep -E 'FATAL|acme.sh'
cat /var/log/acme-issue.log  # 详细错误
```

### DDNS 凭证改了但同步失败

```bash
# 1. 面板"阿里云 API 凭证"卡片看"当前生效来源"
#    应显示 panelstate(不是 env-legacy-*)

# 2. 看 /data/panel-state/aliyun.creds 是否存在 + 内容
cat /data/panel-state/aliyun.creds

# 3. 看 DDNS tick 日志
docker logs ikev2-panel | grep -i 'ddns'
```
