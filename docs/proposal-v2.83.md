# v2-83 提案(待评审):统一阿里云凭证 + 面板 LE 证书 + HTTPS 热重载

> 状态:**草案**,等待多 agent 评审。
> 评审目标:在动手前识别安全/正确性/兼容性风险。
> 关联文档:docs/design.md §1.4.1 §19 §21,docs/release-notes-v2.82.md。

---

## 背景

### 来自 v2-82 复盘 + 用户近两轮对话

1. **凭证分散**:DDNS(v2-82)用 `ALIYUN_ACCESS_KEY_ID`/`ALIYUN_ACCESS_KEY_SECRET`,acme.sh(`dns_ali` 插件)用 `Ali_Key`/`Ali_Secret`。两套 env 名,同一对事实上的凭证,容易写错。
2. **面板 HTTPS 警告**:用户报告 `https://域名:8443` 报"不安全"。经核实,在 `IKEV2_CERT_MODE=self-signed`(默认)模式下确实是自签 CA → 浏览器不信任 → 警告。LE 模式下证书是 LE CA 签的,本应无警告。
3. **HTTPS 不热重载隐患**:Go `http.Server.ListenAndServeTLS` 启动时读一次 cert 文件,之后不再重读。acme.sh `--reloadcmd` 触发的是 `ikev2-reload.sh`,只 reload charon,**面板 HTTPS 不会 reload**。续期后 ~60 天内用户看到的是"过期证书",实际是旧但仍有效的证书。

---

## 目标

| ID | 目标 | 验收标准 |
|----|------|----------|
| G1 | **凭证一处配置**:面板 "Aliyun API" 卡片一处填,同时驱动 acme.sh + DDNS | `.env` 里不出现 `Ali_Key`/`Ali_Secret`/`ALIYUN_ACCESS_KEY_*` |
| G2 | **8443 浏览器无警告**:LE 模式下面板 HTTPS 使用 LE 证书 | `curl -vI https://域名:8443` 显示 issuer = Let's Encrypt |
| G3 | **续期后 HTTPS 自动 reload**:Go server 在续期后 ~1s 内切到新证书 | 模拟续期后,新 TLS handshake 返回新证书 fingerprint |
| G4 | **向后兼容**:v2-82 用户升级无感 | 现有 `Ali_Key` env 仍生效(标 deprecated) |

---

## 设计方案

### D1. 凭证统一:env + 运行时文件双轨

**现状**:
- `.env`:`Ali_Key`, `Ali_Secret`(给 acme.sh),`ALIYUN_ACCESS_KEY_ID`, `ALIYUN_ACCESS_KEY_SECRET`(给 DDNS)
- 实际是同一对凭证

**改动**:
- **新建**:`IKEV2_ALIYUN_KEY_ID` / `IKEV2_ALIYUN_KEY_SECRET`(统一命名)
- **兼容**:config 读取时按优先级 `IKEV2_ALIYUN_KEY_ID` → `ALIYUN_ACCESS_KEY_ID` → `Ali_Key`
- **运行时**:面板 "Aliyun API" 卡片(写 `/data/panel-state/aliyun.creds`,格式 JSON,0600)
  - 卡片保存 → 触发 `os.WriteFile(..., 0600)`
  - **不**运行时重载 acme.sh(避免复杂);重启生效,文档说明
- **entrypoint.sh 读取顺序**:
  1. 读 `/data/panel-state/aliyun.creds`(面板填的优先)
  2. fallback 到 env(`IKEV2_ALIYUN_KEY_ID`)
  3. 注入到 `/root/.acme.sh/account.conf`(`export Ali_Key=... Ali_Secret=...`)
  4. 注入到 Go 进程 env(供 DDNS 用)

**为什么不运行时重载 acme.sh**:
- acme.sh 是 entrypoint 阶段调用的子进程,运行时改 account.conf 不影响已签发的证书
- 真正生效要重启容器,简单且符合容器化习惯
- 留作未来 backlog(P2+)

### D2. 面板 HTTPS 默认走 LE 证书

**现状**:
- `IKEV2_CERT_MODE=self-signed`(默认):用 `cert.EnsureServerCert` 生成自签证书,`EnsurePanelCert` 复用同一证书 → 浏览器警告
- `IKEV2_CERT_MODE=letsencrypt`:entrypoint 调 acme.sh --issue(走 `dns_ali`),证书装到 `/data/le/fullchain.pem`,`writePanelTLSCerts` 复制到 `/data/panel-tls/`

**改动**:
- **默认改为 `letsencrypt`**:`IKEV2_CERT_MODE` 默认值 `self-signed` → `letsencrypt`
- **引导**:`.env.example` 把 `IKEV2_CERT_MODE=letsencrypt` 提到注释首行,加 "必填 `IKEV2_DOMAIN` + Aliyun 凭证"
- **降级**:如果 LE 模式启动失败(凭证空、域名解析不到、`acme.sh --issue` 失败),**降级到 self-signed** 而不是直接 exit —— 因为用户场景很多是"先跑起来后改凭证",容器不能起不来
  - entrypoint.sh 捕获 acme.sh 失败 → log WARN → 写 `/data/le/LAST_LE_FALLBACK` → Go 端面板顶部加横幅"当前为自签证书,8443 浏览器会警告"
- **首页横幅**:面板读 `LAST_LE_FALLBACK` 显示提示 + 引导去填凭证

**为什么降级而不是 hard fail**:
- 用户场景:首次部署时可能没填凭证,先 self-signed 跑起来面板能访问,再去配 → 更友好
- 已经实现的 self-signed 路径不动,只是兜底
- 如果用户明确想要 LE 且失败,日志写清楚了,横幅显示引导

### D3. HTTPS 热重载(SIGHUP)

**现状**:
- `httpSrv := &http.Server{...}` 启动时 cert 加载一次
- `acme.sh --reloadcmd /usr/local/bin/ikev2-reload.sh` 触发 charon 重载,**不 reload http server**

**改动**:
- Go 端监听 `SIGHUP` 信号 → 重新读 `/data/panel-tls/cert.pem` + `key.pem` → 重新 `tls.X509KeyPair` → 替换 `httpSrv.TLSConfig.GetCertificate`
- 用 `tls.Config.GetCertificate` 回调(而不是直接替换 server),这样每个新连接握手时拿最新 cert,无需重启 server
- `ikev2-reload.sh` 增加一行:`kill -HUP $(pidof ikev2-panel)`(放在 reload charon 之后)
- 验证:跑测试脚本,renew-cert.sh 后 curl 看证书 fingerprint 变了

**关键设计点**:
- 用 `GetCertificate` 回调而非 `srv.TLSConfig.Certificates` 列表 → 热更新更安全
- 读文件失败保留旧证书(降级,不停服)
- SIGHUP 监听用 `signal.Notify(sighup, syscall.SIGHUP)`,跟现有 SIGINT/SIGTERM 不冲突

### D4. 兼容性矩阵

| 升级路径 | 行为 |
|----------|------|
| v2-79 → v2-83 | `IKEV2_CERT_MODE=self-signed`(未改),`Ali_Key` 不再读但不强删(给 warn),面板走老路径 |
| v2-82 → v2-83 | DDNS 仍能跑(`ALIYUN_ACCESS_KEY_*` 兼容),面板 HTTPS 默认改 LE,**首次启动会触发 acme.sh** |
| 新装 | `.env.example` 默认 LE,引导填凭证 |

---

## 文件清单(待评审)

| 路径 | 操作 | 说明 |
|------|------|------|
| `internal/config/config.go` | 改 | 加 `AliyunKeyID`/`AliyunKeySecret`,读取优先级链 |
| `internal/web/handlers_aliyun.go` | 新 | 凭证卡 POST handler |
| `internal/web/handlers_home.go` | 改 | 加 `LEFallback` / `LEConfigured` 字段 |
| `internal/web/server.go` | 改 | SIGHUP 监听 + GetCertificate 回调 |
| `cmd/ikev2-panel/main.go` | 改 | 装 GetCertificate + 启动 SIGHUP goroutine |
| `scripts/entrypoint.sh` | 改 | 凭证读 `/data/panel-state/aliyun.creds` 优先 + LE 失败降级 |
| `scripts/ikev2-reload.sh` | 改 | 加 `kill -HUP` |
| `web/templates/home_content.html` | 改 | 加 Aliyun 凭证卡 + LE 降级横幅 |
| `docker-compose.yml` | 改 | 默认 `IKEV2_CERT_MODE=letsencrypt` |
| `.env.example` | 改 | 默认 LE + 凭证引导 |
| `docs/design.md` | 改 | §19.6 §19.7 v2-83 设计 + §21 路线图 |
| `docs/release-notes-v2.83.md` | 新 | 升级步骤 + 兼容性说明 |
| `README.md` | 改 | M11 + 凭证说明 |
| 测试 | 新 | `internal/web/handlers_aliyun_test.go`、`internal/web/server_sighup_test.go` |

---

## 评审请回答

1. **D1 凭证优先级链**是否合理?是否需要再支持"env + 凭证卡并存时,凭证卡覆盖"?
2. **D2 LE 降级到 self-signed**是否符合预期?还是应该 hard fail 让用户必须先配?
3. **D3 SIGHUP 热重载**用 `GetCertificate` 回调 vs 直接 `srv.TLSConfig = newCfg` 哪种更安全?
4. **D4 兼容性**v2-82 用户升级到 v2-83,**面板 HTTPS 默认从 self-signed 切到 LE 是个行为变更**,是否需要 opt-in(`IKEV2_CERT_MODE=letsencrypt` 显式开)而不是改默认?
5. **凭证存储安全**:`/data/panel-state/aliyun.creds` JSON + 0600,够用?还是需要进一步加密?
6. **测试覆盖**:哪些是必测的?哪些可以留作 P2?
