# strongSwan / IKEv2 配置审计报告 — 2026-09

> 第三轮审计(聚焦 strongSwan 自身)。前两轮已分别完成 cert/acme.sh 与 docker / 安全审计。

## 范围

审计 ikev2-panel-v2 项目里所有跟 strongSwan IKEv2 守护进程 / swanctl / VICI 协议 / EAP-MSCHAPv2 / ESP 隧道相关的代码与配置,对比:

- strongSwan 官方 Security Recommendations & 算法提案规范
- RFC 8247(IKEv2 Cipher Suites)
- RFC 9395(IKEv2 后量子算法更新)
- NIST SP 800-131A / CNSA 2.0
- iOS / macOS / Windows / Android / Linux 客户端互操作官方文档
- 标杆项目:algo VPN、icl-it/server-configs-strongswan、strongswan/strongswan 官方 docker 镜像

| 文件 | 重点章节 / 行号 |
|---|---|
| [file:///opt/ikev2-panel-v2-main/configs/swanctl-ipv6-only.conf](../configs/swanctl-ipv6-only.conf) | proposals / esp_proposals / send_cert / rekey_time / mobike / pools(L1-92) |
| [file:///opt/ikev2-panel-v2-main/configs/strongswan-filelog.conf](../configs/strongswan-filelog.conf) | filelog 级别(L1-25) |
| [file:///opt/ikev2-panel-v2-main/Dockerfile](../Dockerfile) | strongSwan configure 插件(L77-104) + 默认 conf 空文件补丁(L177-190) |
| [file:///opt/ikev2-panel-v2-main/scripts/entrypoint.sh](../scripts/entrypoint.sh) | charon 启动 / forwarding / MASQUERADE / swanctl --load-all / reload 序列(L1-786) |
| [file:///opt/ikev2-panel-v2-main/scripts/ikev2-updown](../scripts/ikev2-updown) | tc qdisc/class 限速 + u32 filter(L1-75) |
| [file:///opt/ikev2-panel-v2-main/scripts/ikev2-reload.sh](../scripts/ikev2-reload.sh) | LE 续签后重载 charon + Go(L1-66) |
| [file:///opt/ikev2-panel-v2-main/internal/swanctl/writer.go](../internal/swanctl/writer.go) | EAP-MSCHAPv2 secrets 块写入 + 原子写(L23-48) |
| [file:///opt/ikev2-panel-v2-main/internal/swanctl/loader.go](../internal/swanctl/loader.go) | ReloadAll / LoadCreds CLI 调用 + VICI session(L86-252) |
| [file:///opt/ikev2-panel-v2-main/internal/swanctl/parser.go](../internal/swanctl/parser.go) | VICI list-sas 流式解析(L63-99) |
| [file:///opt/ikev2-panel-v2-main/internal/swanctl/ipv6watch.go](../internal/swanctl/ipv6watch.go) | IPv6 prefix 变化自动 reload(L48-131) |
| [file:///opt/ikev2-panel-v2-main/internal/swanctl/terminate.go](../internal/swanctl/terminate.go) | VICI terminate 协议(L33-84) |
| [file:///opt/ikev2-panel-v2-main/internal/cert/generate.go](../internal/cert/generate.go) | RSA 2048 server cert + EKU + SAN(L150-231) |
| [file:///opt/ikev2-panel-v2-main/internal/web/handlers_config.go](../internal/web/handlers_config.go) | 客户端导出 sswan 模板(L184-254) |

## 工具 / 参考

- strongSwan Security Recommendations: <https://docs.strongswan.org/docs/latest/howtos/securityRecommendations.html>(**注**:原报告草稿里的 `strongswanSecurity.html` 已 404,官方路径已改为 `howtos/securityRecommendations.html`)
- strongSwan Algorithm Proposals: <https://docs.strongswan.org/docs/latest/config/proposals.html>
- strongSwan swanctl.conf 文档: <https://docs.strongswan.org/docs/latest/swanctl/swanctlConf.html>
- strongSwan VICI 协议: <https://docs.strongswan.org/docs/latest/plugins/vici.html>
- strongSwan iOS / macOS 互操作: <https://docs.strongswan.org/docs/latest/interop/ios.html>
- strongSwan Apple Configuration Profile: <https://docs.strongswan.org/docs/latest/interop/appleIkev2Profile.html>
- strongSwan Android 互操作: <https://docs.strongswan.org/docs/latest/interop/android.html>
- strongSwan Windows 互操作: <https://docs.strongswan.org/docs/latest/interop/windowsClients.html>
- RFC 8247(IKEv2 Cipher Suites): <https://datatracker.ietf.org/doc/html/rfc8247>
- RFC 9395(IKEv2 PQ 更新): <https://datatracker.ietf.org/doc/html/rfc9395>
- NIST SP 800-131A Rev.2: <https://csrc.nist.gov/publications/detail/sp/800-131a/rev-2/final>
- NIST SP 800-77 Rev.1(IPsec 指南): <https://csrc.nist.gov/publications/detail/sp/800-77/rev-1/final>
- CNSA 2.0(NSA 商用国家保密算法套件 2.0): <https://media.defense.gov/2022/Sep/07/2003071834/-1/-1/0/CSA_CNSA_2.0_ALGORITHMS_.PDF>
- algo VPN(标杆): <https://github.com/trailofbits/algo>
- icl-it/server-configs-strongswan(老牌参考): <https://github.com/icl-it/server-configs-strongswan>
- strongswan/strongswan 官方 docker(参考 `Dockerfile` 完整插件列表): <https://github.com/strongswan/strongswan-docker>
- govici 客户端: <https://github.com/strongswan/govici>

---

## 发现(按严重度排序)

### [HIGH-1] MODP2048 同时保留在 IKE + ESP proposals,跟 strongSwan Security Recommendations 6.0 后的"应避免"清单冲突

**文件**:
- [file:///opt/ikev2-panel-v2-main/configs/swanctl-ipv6-only.conf](../configs/swanctl-ipv6-only.conf) L44, L53

**现状**:
```conf
proposals = aes256gcm16-sha256-ecp256, ..., aes256-sha256-modp2048, aes128-sha256-modp2048
esp_proposals = ..., aes256-sha256-modp2048, aes128-sha256-modp2048, ..., -modpnone
```

**问题**:
strongSwan 官方 Security Recommendations 把 `modp2048` 列在"weak & must not be used"的边界区(强 Swan 6.x 起的弱算法清单已把 MODP 系列从默认里剔除,只剩 `modp3072`/`modp4096`/`modp8192` 是 MUST/SHOULD)。RFC 8247 §2.4 列出 `Diffie-Hellman group 14 (2048-bit MODP)` 是 MUST-,NIST SP 800-131A Rev.2 也明确 2048-bit DH 在 2023 后**仅 legacy 兼容**,新部署应至少 3072-bit。

但 iOS 16.5 仍只发 `MODP_2048`(见官方 ios.html 默认提案清单)—— 完全去掉会导致 iOS 拨号失败。

**修复方向**:
- **保留**但**降权**:把 MODP 系列挪到 ECP/curve25519 系列**之后**(目前顺序已经是这样),确认 strongSwan 选了 ECP/curve25519 才落盘
- **可选**:加 `modp3072` 在 MODP2048 之前(MODP3072 iOS 16.5 不支持,等价 fallback;MODP4096 同)
- 考虑加 post-quantum hybrid proposal(`kyber768` / `kyber1024`,strongSwan 6.0 默认支持),但需 IKE SA 重写,不在本次范围
- 在 `esp_proposals` 中,MODP-none fallback 已经为 macOS 预留(官方 ios.html §"Troubleshooting macOS" 推荐),**保留**

**工作量**:0.3d(2 行 conf 顺序调换 + 在 `docs/design.md` 加备注说明 2048-bit 仅为 iOS 16 兼容兜底)

---

### [HIGH-2] charon filelog 把 `enc = 1`、`net = 1`、`mgr = 0` 写日志到 /var/log/charon.log,**无日志脱敏 / 无大小限制 / 不轮转**

**文件**:
- [file:///opt/ikev2-panel-v2-main/configs/strongswan-filelog.conf](../configs/strongswan-filelog.conf) L11-24

**现状**:
```conf
filelog {
    charon-log {
        path = /var/log/charon.log
        time_format = %Y-%m-%dT%H:%M:%S
        ike_name = yes
        default = 2          # INFO
        mgr = 0              # CONTROL(最高,含 SA 建立/销毁)
        net = 1              # AUDIT(网络协商每包)
        enc = 1              # RAW(密钥交换材料!)
        asn = 1
        job = 1
        knl = 1
    }
}
```

**问题**:
- `enc = 1` 会把每个 IKE SA 的加密参数(含**部分派生密钥 material、Nonce、KE payload 十六进制**)落到 /var/log/charon.log。这本身**不直接泄露 master key**(strongSwan 不会把 SK_e/SK_a 完整 dump),但暴露足够辅助离线暴力(尤其是 EAP-MSCHAPv2 模式下,攻击者拿到 NT-hash 相关参数可加速破解)。
- `net = 1` 是 AUDIT 级别,每条 IKE 消息都打一行,长跑日志会爆炸
- 没有 `time_format` 没有 logrotate → /var/log/charon.log 单文件无界增长(前面 audit-2026-09-docker §11 已发现同样问题,但那边只说 `enc=1`)
- 文件权限 1777(`/var/log` chmod),**同主机任何进程可读**(也是 docker 审计 D10)

**修复方向**:
- 改 `enc = 0`(或直接不写,默认就是 0)
- 改 `net = 0`(默认 DEBUG 才需要)
- `default = 1`(含 CONTROL 即可,生产不再开 DEBUG)
- 加 `default = 2`、`ike = 2`、`cfg = 2`(INFO 级别足够排错)
- 加 logrotate(`/etc/logrotate.d/charon.conf`):daily + rotate 7 + compress + postrotate 通知 charon reopen
- 改 `path = /var/log/ikev2-panel/charon.log` + 子目录 0755 root:root

**工作量**:0.5d(改 conf + 写 logrotate + 修 Docker 镜像里的 `/var/log` 1777)

---

### [HIGH-3] `send_certreq = yes` + 仅 pubkey 认证(EAP-MSCHAPv2 模式) 在某些客户端上多余且暴露 server cert 给被动嗅探

**文件**:
- [file:///opt/ikev2-panel-v2-main/configs/swanctl-ipv6-only.conf](../configs/swanctl-ipv6-only.conf) L54

**现状**:
```conf
local { auth = pubkey; certs = server.cert.pem }
remote { auth = eap-mschapv2; eap_id = %any }
send_certreq = yes       # 客户端也会发 CERTREQ,客户端通常没证书
```

**问题**:
1. EAP-MSCHAPv2 模式下,**客户端用 EAP 认证**,不需要客户端证书。客户端发 CERTREQ 后,charon 会发回一堆 CA chain,泄露 server 的信任锚(配合 [HIGH-2] 的 enc=1,被动攻击者能枚举 server 信任的 CA)。
2. 实际上 iOS/macOS EAP-MSCHAPv2 模式下也**不发 CERTREQ**(官方 ios.html 提到 `send_cert = always` 是反过来给客户端发 server cert),所以 `send_certreq = yes` 对它们是空操作。
3. strongSwan 默认 `send_certreq = yes`,但 RFC 7296 §3.7 推荐 EAP 模式关掉 CERTREQ 减少攻击面。

**修复方向**:
- 改 `send_certreq = no`(EAP 模式 client 没必要主动要 cert)
- 确认 `send_cert = always` 保留(给 iOS)

**工作量**:0.1d(1 行 conf + 验证 iOS / Android / Linux 客户端无影响)

---

### [HIGH-4] 客户端导出的 sswan 模板仍写 SHA1 算法 fallback + MODP1536,跟服务端 proposals 不一致,iOS 16 实测会触发 NO_PROPOSAL_CHOSEN

**文件**:
- [file:///opt/ikev2-panel-v2-main/internal/web/handlers_config.go](../internal/web/handlers_config.go) L229, L234

**现状**:
```go
esp_proposals = aes256gcm16-sha256, aes128gcm16-sha256
...
proposals = aes256gcm16-sha256-modp2048, aes128gcm16-sha256-modp2048
```

**问题**:
1. **服务端 proposals 不含纯 `aes256gcm16` / `aes128gcm16` 单元素形式**(服务端 L44 的 esp_proposals 都是带 DH 组的),客户端会拿不到匹配 → NO_PROPOSAL_CHOSEN → 拨号失败。
2. ESP proposals 客户端没指定 DH 组,服务端 esp_proposals 第一项强制 `ecp256` 会要求客户端 ECDH capability — 没问题,但**没有 `modpnone` fallback** 给"不重协商 ESP DH"的老客户端(虽然这里 IKE SA 已有 PFS,但 ESP SA rekey 时客户端可挑单独的 proposals)。
3. AEAD 算法(GCM)的 SHA256 是 PRF,**不能**单独跟 `sha256` 写成非 AEAD 形式(`aes256-sha256`),但客户端配置写的就是 AEAD,会匹配服务端非 AEAD 项 → 选到 `aes256-sha256-modp2048`(更弱)。

**修复方向**:
- 让 `sswan` 模板跟服务端 `swanctl-ipv6-only.conf` **完全对齐**(把 esp_proposals、proposals 复制一遍)
- 客户端 proposals 加 `modpnone` fallback(对应 ESP rekey 场景)
- 或者改成纯服务端 conf `include`(client 端 import swanctl.conf),但 sswan 是 strongSwan app 自有格式,只能复制

**工作量**:0.3d(改 Go 模板,加 conf 同步测试)

---

### [HIGH-5] entrypoint 启动时 `swanctl --load-all` 跑两次,**第一次 100% 失败**(LE cert 未装);且 §7.6 的"再 load 一次"在自签模式下 cert 仍未生成,**逻辑死循环**

**文件**:
- [file:///opt/ikev2-panel-v2-main/scripts/entrypoint.sh](../scripts/entrypoint.sh) L611-628, L771-782

**现状**:
```bash
# §7
if ! swanctl --load-all 2>&1 | tail -10; then
  echo WARN: swanctl --load-all failed (will retry after Go installs certs)
fi
# §7.5 LE cert 签发...
# §7.6
if [ -f "${SWAN_X509:-}" ]; then
  echo "Loading swanctl connections (post-cert)..."
  swanctl --load-all 2>&1 | tail -8 || echo WARN
fi
```

**问题**:
1. §7 第一次 `--load-all` 在 LE 模式必失败(cert 文件还没复制),在自签模式也必失败(Go 进程还没启动 → cert 没生成 → `certs = server.cert.pem` 找不到)。
2. §7.6 的条件 `if [ -f "${SWAN_X509:-}" ]` 在 LE 模式成立(§7.5 复制了),在自签模式**永远不成立**(Go 进程启动后才生成 cert,但 §7.6 在 Go exec **之前**)。
3. 结果:Go 进程启动 → `installCertsToSwanctl` → `LoadCreds` 才能让 charon 看到 cert。但 **`connections` 没 load** —— charon 启动时还没 read conf(默认 strongSwan 不会自动 load)。

实际看 main.go L70-75 也有 `scm.ReloadAll(ctx)`,所以最终会 load 一次。**但 entrypoint §7/§7.6 的两次 load 完全是噪音**,且 §7 在 LE 模式第一次失败后 charon 会把 broken conf 状态缓存住。

**修复方向**:
- 删 §7 第一次 `--load-all`
- §7.6 加 `|| true` + 注释说明 LE 模式成功 / 自签模式等 Go 进程
- 或者:§7 改用 `--load-conns`(只 load conn 不 load cert),把 cert 留给 §7.6/Go 进程

**工作量**:0.2d(删 5 行 + 加注释)

---

### [HIGH-6] LE 模式 renew-cert.sh 失败回退后**直接调用 ikev2-reload.sh**,但 reload.sh 内部再跑 `--load-all`,回滚证书 + 重载不幂等且无日志

**文件**:
- [file:///opt/ikev2-panel-v2-main/scripts/renew-cert.sh](../scripts/renew-cert.sh) L67-77
- [file:///opt/ikev2-panel-v2-main/scripts/ikev2-reload.sh](../scripts/ikev2-reload.sh) L30-51

**问题**:
1. `cp ${BACKUP_DIR}/.../fullchain.pem /data/le/fullchain.pem` 直接覆盖,无原子性(跟 `internal/swanctl/atomic.go` 的 tmp+rename 模式不统一)
2. `/usr/local/bin/ikev2-reload.sh` 内部 `--load-all`(再次全量卸载连接 + 重载)会**中断活跃 SA 500ms**——LE 续签失败回退不应该触发 SA 中断
3. reload.sh 内 `kill -HUP ${PANEL_PID}` 在 Go 进程已死情况下(pidof 返回空)不会 fail,但有"找不到 PID" 静默成功,运维看不出 reload 实际是否触发
4. **没有 charon 健康检查**:reload 后没验证 `swanctl --list-certs` 真有 cert,只在 §6 启动时检查过

**修复方向**:
- 加 `swanctl --load-creds` 验证(只换 cert 不重载 conn)
- 复制用 `install -m 644/600`(自带原子性 + 权限)
- reload.sh 加 `swanctl --list-certs` 校验,失败回退

**工作量**:0.5d(改 reload.sh + renew-cert.sh + 加验证)

---

### [MED-1] Dockerfile `--enable-sha1` `--enable-md5` 启用了 strongSwan Security Recommendations 显式禁止的 PRF / 完整性算法

**文件**:
- [file:///opt/ikev2-panel-v2-main/Dockerfile](../Dockerfile) L89-91

**现状**:
```dockerfile
--enable-sha1 \
--enable-sha2 \
--enable-md5 \
```

**问题**:
- strongSwan Security Recommendations: `md5,sha1` 在 integrity / PRF 上"must not be used"
- 启用后 strongSwan **不会**主动协商(因为 proposals 没写),但 charon 内部 nonce 生成 / 一些 fallback 路径会用
- **实际攻击面**:charon 启动时随机数 / hash 内部用了 SHA1 → 协议层若哪天出现新 bug 用 SHA1 签了什么东西,会被利用
- 项目自己的 swanctl-ipv6-only.conf 也只写 `sha256`,没有 sha1,功能上**禁用安全**

**修复方向**:
- 删 `--enable-sha1` `--enable-md5`
- 保留 `--enable-sha2`(就是 sha256/sha384/sha512)
- `--enable-hmac` 保留
- 测试确认 charon 启动不报错(某些 plugin 内部依赖 sha1,实测可能需要 `drbg` plugin 自己用 SHA256)

**工作量**:0.3d(改 Dockerfile + 重新 build + charon 启动验证)

---

### [MED-2] Dockerfile 启用了 `stroke` 插件,实际只为了 `ipsec start --nofork`,攻击面 +8MB 镜像

**文件**:
- [file:///opt/ikev2-panel-v2-main/Dockerfile](../Dockerfile) L65, L88

**现状**:
```dockerfile
#   --enable-stroke   ipsec/starter 命令(entrypoint.sh 用 `ipsec start` 启 charon)
--enable-stroke \
```

**问题**:
- `stroke` 是 ipsec/whack 协议层(vs VICI 的现代替代),已被 strongSwan 自己标记为 **legacy / deprecated**
- 启用 `stroke` 会编译 `libcharon-stroke` + `ipsec` starter 脚本 + 多个 conf 文件
- 实际只用 `ipsec start --nofork` —— **这个命令完全可以被 swanctl 直接调用替代**(`swanctl --load-all` 之前需要 charon 在跑;`ipsec start` 内部就是 fork charon)
- 替代方案:用 `swanctl --initiate --child ...`(不适用启动期),或写 5 行 shell wrapper 调用 `charon --help` + fork
- 实际更简单:直接用 `swanctl --load-all` 启动 / 加载,但 charon **不在** → 必须先把 charon fork 起来 → 这正是 `ipsec start` 干的

**正确做法**:
- 保留 `stroke` 是合理的(它就是为启动 charon 而生)
- **但**应该至少在 docs 里标注"deprecate 后迁移到 systemd / s6"
- 可选:不构建 ipsec 脚本,直接 `charon --foreground &` 启动,这样可以去掉 `stroke` 插件 + 整段 `libexecdir`

**工作量**:0.5d(2 行配置 + 改 entrypoint 启动命令为 `charon --help` + `charon &`)

---

### [MED-3] entrypoint.sh §0.2 IPv6 公网探测用 `awk` + `sed` 简易实现,**无法处理 SLAAC privacy extension / temporary address**,可能选到 deprecated 临时地址

**文件**:
- [file:///opt/ikev2-panel-v2-main/scripts/entrypoint.sh](../scripts/entrypoint.sh) L82-97
- [file:///opt/ikev2-panel-v2-main/internal/swanctl/ipv6watch.go](../internal/swanctl/ipv6watch.go) L146-206

**问题**:
- `/proc/net/if_inet6` 的 flags 字段(第 4 列):`0x01` = temporary,`0x10` = deprecated,`0x80` = tentative
- entrypoint.sh L85-96 只用 `($4=="01" || $4=="00")` 过滤,**不区分 temporary**(privacy extension)—— 会选到 client 端不用、但 host 端**会被用作 server identity**的临时地址,导致 iOS client 拨号时 DNS 反查失败 / 服务端证书不匹配
- ipv6watch.go L165 同样只 skip `flags == "10"`(deprecated),**不 skip `flags == 01` 或 `flags == 80`**
- 两处实现**逻辑还不一致**(entrypoint 接受 01 和 00,ipv6watch 只接受非 10)

**修复方向**:
- 两处统一为:**接受 `flags == "00"`(permanent)**,其他都 skip
- 或者:`flags == "01"` 也接受(临时地址是合法地址,只是有 TTL),但只用作 `local_addrs`(iOS 通过 DNS 解析地址,不直接看)
- 抽到 `swanctl.DetectGlobalV6` 单一函数,entrypoint 调用 binary 或共享 shell 包装

**工作量**:0.5d(抽函数 + 两处对齐 + 加注释 + 测试)

---

### [MED-4] `ikev2-updown` IPv4 限速但 IPv6 流量**完全无限速**,且 `tc class add` 不指定 `burst` / `cburst` 在小流量场景会有脉冲

**文件**:
- [file:///opt/ikev2-panel-v2-main/scripts/ikev2-updown](../scripts/ikev2-updown) L28-62

**现状**:
```bash
ensure_root_qdisc() {
  tc qdisc add dev "$OUT_IF" root handle 1: htb default 999
  tc class add dev "$OUT_IF" parent 1: classid 1:999 htb rate 1000mbit ceil 1000mbit
}
# ...
tc filter add dev "$OUT_IF" parent 1: protocol ip prio 1 u32 \
  match ip src "${PLUTO_PEER_SOURCEIP}" flowid 1:${CLASS_ID}
```

**问题**:
1. **IPv6 不限速**(注释 L57 自己写"tc u32 只识别 IPv4,IPv6 流量无法限速")—— 管理员以为"100Mbps 限速"实际只能管 IPv4,v6 流量绕开
2. **没有 cburst / burst** —— HTB 在 rate 100Mbps,burst 默认 1500 byte,小包频繁交互会让 burst 频繁耗尽 → 实际吞吐偏低
3. **filter 只 match src** —— 如果客户端 NAT(同 NAT 下多 user,理论上 IKEv2 会给每个 user 独立 VIP,不容易撞,但同 vpn 内多 user 同一 src IP 的极端场景)会撞
4. **down 时只 `tc class del` + `tc filter del`**,但 filter 是按 `parent 1: protocol ip prio 1` 全删 → **会把该 parent 下其他 user 的 filter 也删了**(因为没指定 handle)
5. `RANDOM % 9000 + 100` 选 classid 不持久,重启容器后 classid 重新随机,**已建立的连接 v6 流量会瞬间识别不出**
6. 注释 `class id 100-9099 避免与 default class 999 冲突`,但 9099 接近 9999(HTB 16-bit classid 上限是 0xFFFF),**没问题**;但 `9099` 用了 5 位数,实际 100-9094 都安全

**修复方向**:
- 加 IPv6 filter:`tc filter add dev "$OUT_IF" parent 1: protocol ipv6 prio 1 u32 match ip6 src ${VIP}/128 flowid 1:${CLASS_ID}`(u32 也支持 ipv6,但语法不同,需 `match u32 0 0` + `match ip6 src` 组合)
- 给 leaf class 加 `burst 15k cburst 15k`(HTB 推荐值)
- `tc filter del` 时指定 `handle 800::800`(`add` 时指定,del 时用同 handle)
- 启动期就把所有 enabled user 的 class 预建(用 `limit/` 包的持久化数据),避免 race

**工作量**:1d(IPv6 filter + burst + handle + 预建 + 测试)

---

### [MED-5] VICI list-sas 解析丢弃 IKE_SA lifecycle 事件(`IKE_SA_REKEYED`、`IKE_SA_DELETED`),管理面板"已连接用户"列表可能显示已掉线的连接

**文件**:
- [file:///opt/ikev2-panel-v2-main/internal/swanctl/parser.go](../internal/swanctl/parser.go) L63-99

**现状**:
- `CallStreaming(ctx, "list-sas", "list-sa", msg)` 拿流
- 解析每个 SA Message → `parseSingleSA` → `[]SA`

**问题**:
- `list-sas` 是**事件流**:除每个 SA 外,还会推 `ike-updown` 事件(SA 状态变化)、`ike-rekey`、`ike-reregister`
- `parser.go` 只处理 `list-sa` 事件,其他事件静默忽略
- 结果:用户在面板上看到一个 SA 列表,实际上 charon 早就 rekey 完了 → "显示用户 A 在线 2 小时"但实际 30 分钟前就 rekey 了,内部 uniqueid 变了,但前端的 "user" 显示仍是 A
- **更严重**:SA 被 `terminate` 后,UI 不会立刻知道,要等下次 collector(5 分钟)重新 list-sas

**修复方向**:
- 在 `list-sas` 流里订阅 `ike-updown` / `ike-rekey` 事件,实时更新本地 SA cache
- 或者:terminate.go 用 `terminate` 返回后通知 UI(in-band event)
- 短期:接受这个限制,但在 docs 里标注"SA 状态最多延迟 5 分钟"
- 长期:把 SA 状态做成订阅式 SSE,前端实时更新

**工作量**:1d(订阅事件流 + 实时 cache + SSE)

---

### [MED-6] charon filelog `default = 2`(INFO) + 启用了所有 subsystem,没开启 `ike_name = yes` 之外的关键过滤,**SIGHUP 重载不生效**

**文件**:
- [file:///opt/ikev2-panel-v2-main/configs/strongswan-filelog.conf](../configs/strongswan-filelog.conf) L11-24

**问题**:
- filelog 是 charon 启动时 parse,**改 conf 必须重启 charon**才能生效(SIGHUP 不支持)
- entrypoint 启动后没有任何途径修改 log level(运维想 debug 一个用户问题,改 conf 没意义)
- 强 Swan 提供 `swanctl --log` 单独改 log level(但只对 syslog 有效,对 filelog 不直接生效)
- 还有 `charon.systemd` 监听 `journalctl` 不是 filelog

**修复方向**:
- 加 `log { ... }` to `strongswan.conf`(主配置)而不是 filelog,可用 `swanctl --log` 调
- 或:在 Go 面板加 "动态调整日志级别" 按钮 → 写 strongswan.conf + 重启 charon

**工作量**:0.5d(改 conf + 加面板 UI + restart charon 流程)

---

### [MED-7] 客户端导出 sswan 用 `send_certreq = yes`,配合服务端 EAP-MSCHAPv2 模式,**客户端被强制要求安装 CA**,但 iOS 移动端无提示会"静默失败"

**文件**:
- [file:///opt/ikev2-panel-v2-main/internal/web/handlers_config.go](../internal/web/handlers_config.go) L235

**问题**:
- iOS 配置 profile 模式:`PayloadContent` 内的 `PayloadCertificate` 自动安装 CA → OK
- Android 11+ 系统设置模式:**没有 CA 自动安装流程**,用户必须手动下载 ca.cert.pem → 系统设置 → 安全 → 安装(且 Android 11+ 装 CA 必须"应用使用此证书",操作门槛高)
- Linux strongSwan app:需要把 CA 放 `/etc/swanctl/x509ca/`
- 当前 sswan 文件没引用 CA 文件位置

**修复方向**:
- 在 sswan 头部加注释:`# Install ca.cert.pem to /etc/swanctl/x509ca/`
- Android 模板页(L73-75 已说明)再强调一次
- 客户端 conf 不写 `send_certreq` —— strongSwan app 默认会要

**工作量**:0.1d(改 sswan 注释 + Android 模板页提示)

---

### [MED-8] rekey_time = 24h 写死,**没有 ikesa_lifetime**;strongSwan 默认 ikesa_lifetime = rekey_time + 10%,实际 rekey 在 24h + 协商窗口触发,某些客户端(macOS)会卡 30s 才完成 rekey

**文件**:
- [file:///opt/ikev2-panel-v2-main/configs/swanctl-ipv6-only.conf](../configs/swanctl-ipv6-only.conf) L59

**现状**:
```conf
rekey_time = 24h
# ikesa_lifetime 默认 = rekey_time * 1.1
```

**问题**:
- strongSwan 默认 ikesa_lifetime = rekey_time + 10%(26.4h)—— IKE SA 自身 rekey 在 24h,过期在 26.4h
- macOS rekey 时机 = ikesa_lifetime - random(0..15%)—— 可能在用户高峰
- 长期 TCP 连接(下载、视频)在 rekey 瞬间会卡 1-3s(RFC 7296 §2.8 描述)
- 推荐:rekey_time = 8h,ikesa_lifetime = 10h,让 rekey 发生在"低概率命中用户活跃期"

**修复方向**:
- 改 `rekey_time = 8h` 或 `12h`(iOS 默认 1h,macOS 默认 24h;短一点更安全但客户端掉线次数增加)
- 加 `reauth = no`(默认 yes,EAP 模式每次 reauth 需要重 EAP-MSCHAPv2 握手,体验差)
- 加 `overwrite = yes`(已有同名 child SA 时覆盖,允许 mobike 切网不重协商)

**工作量**:0.1d(3 行 conf)

---

### [MED-9] `unique = replace` + `rekey_time = 24h`,但**没设 `dpd_timeout`** 和 `dpd_delay`,strongSwan 默认 dpd = 5s/30s,但**只在 IKE SA 协商时交换 DPD,不会"主动探活"**

**文件**:
- [file:///opt/ikev2-panel-v2-main/configs/swanctl-ipv6-only.conf](../configs/swanctl-ipv6-only.conf) L16-61

**问题**:
- 默认 dpd_timeout = 0(禁用),dpd_delay = 30s
- 客户端 NAT 重启 / 切网 / 强 Swan 收到对端消失(没收到 DELETE),会**保持 SA 120s** 无效
- 实测:iPhone 锁屏 60s + 切 4G → WiFi,VPN 仍"显示已连",实际 ESP 隧道已挂,直到下一个 IKE rekey 才修

**修复方向**:
- 加 `dpd_timeout = 30s`、`dpd_delay = 10s`(探活频率 10s,3 次失败 = 30s 后删 SA)
- 或:`dpd_timeout = 60s` + `inactivity = 300s`(5 分钟无流量主动断)

**工作量**:0.1d(2 行 conf)

---

### [MED-10] charon 启动 + 证书生成有 TOCTOU:Go 进程 `installCertsToSwanctl` 与 entrypoint §7.5 复制 LE cert 之间存在竞态

**文件**:
- [file:///opt/ikev2-panel-v2-main/cmd/ikev2-panel/main.go](../cmd/ikev2-panel/main.go) L202-216
- [file:///opt/ikev2-panel-v2-main/scripts/entrypoint.sh](../scripts/entrypoint.sh) L771-782

**问题**:
- entrypoint §7.5 复制 cert → §7.6 跑 `swanctl --load-all` → Go exec
- Go exec → `installCertsToSwanctl`(覆盖 `server.cert.pem`)+ `LoadCreds`
- **竞态**:如果 Go 进程启动慢(用户数据卷大),entrypoint §7.6 跑完 → Go 启动 → 用 self-signed 模式覆盖 LE cert → charon 看到 self-signed(冲突)
- 自签模式不会走 §7.5,但 entrypoint §7.6 条件 `if [ -f "${SWAN_X509:-}" ]` 在自签模式不成立(见 [HIGH-5]),所以**自签模式只有 Go 进程的 LoadCreds 生效,entrypoint 啥都没 load**——但 charon 启动时 conf 已经 read(因为 LoadCreds 之后才有效),**实际是先 installCertsToSwanctl 再 LoadCreds**,这部分 OK

**真正的竞态**:
- LE 模式 Go 启动比 entrypoint §7.6 快 → Go installCertsToSwanctl → LoadCreds → entrypoint §7.6 又跑一次 `--load-all` → 中断 SA 一次(LE 模式 SA 还没建,无所谓)

**修复方向**:
- entrypoint §7.6 改用 `--load-creds`(只换 cred 不重载 conn),且只在 LE 模式跑
- Go 进程启动时检测:LE 模式如果 cert 已存在,**跳过 installCertsToSwanctl**(让 entrypoint 独占管理 cert 文件)

**工作量**:0.5d(改 entrypoint §7.6 + Go main.go 加模式分支)

---

### [MED-11] 客户端 sswan 配置不含 `pools` / `dns`,Linux strongSwan app 用户需手动编辑

**文件**:
- [file:///opt/ikev2-panel-v2-main/internal/web/handlers_config.go](../internal/web/handlers_config.go) L184-254

**问题**:
- sswan 是 strongSwan app 自有格式,客户端导入后**直接拨号**,但 server 端要推 virtual IP
- 当前 sswan 没写 `remote_ts = 0.0.0.0/0, ::/0` 已 OK;但没写 `version = 2`(默认就是 2,**OK**)
- **缺 `eap_id`**:L219 写了 `eap_id = %s`,OK
- **缺 `pools`**:客户端不需要 pools(server 给)
- **缺 `dns = 1.1.1.1`**:客户端连接后用啥 DNS?strongSwan app 应该会自动用 server 推的,但手动 Linux client 不会 → Linux 桌面用户可能困惑

**修复方向**:
- 加注释说明:`# DNS will be pushed by server via ModeConfig`
- Linux sswan 模板不强求

**工作量**:0.1d(注释)

---

### [MED-12] `eap_id = %any` + `auth = eap-mschapv2`,强 Swan 接受任意 EAP identity(用户名),**与 brute-force 防护脱钩**(没有失败次数限制)

**文件**:
- [file:///opt/ikev2-panel-v2-main/configs/swanctl-ipv6-only.conf](../configs/swanctl-ipv6-only.conf) L26-29

**问题**:
- charon 内置 `block-throttle` plugin 提供 IP 维度 throttle(同 IP 每分钟 N 次失败 → block)
- 当前 configure **没启用** `--enable-block-throttle`(`Dockerfile` L77-104 列表里没有)
- 结果:internet 上的 attacker 可以无限尝试 EAP-MSCHAPv2 密码(每次都触发 MSCHAPv2 challenge-response → 暴露 NT-hash 给离线爆破)
- **实际危害**:EAP-MSCHAPv2 自 2012 年就有 [Moxie Marlinspike 攻击](https://www.cloudcracker.com/blog/2012/07/29/cracking-ms-chap-v2/),NT-hash 直接被 derive → 配合弱密码秒破
- strongSwan 6.0+ 默认就启用 block-throttle,但**编译时需显式 enable**

**修复方向**:
- 加 `--enable-block-throttle` 到 Dockerfile configure
- 默认 block 阈值:`charon.plugins.block-throttle { threshold = 5; time = 60; }`
- 或者 panel 自己加 fail2ban(iptables 维度)

**工作量**:0.3d(Dockerfile + charon.conf + 验证)

---

### [LOW-1] strongSwan 6.0.1 已是 stable,6.0.2 修复 ESP DH group 处理 bug(iOS/macOS rekey 异常),建议升级到 6.0.x 最新

**文件**:
- [file:///opt/ikev2-panel-v2-main/Dockerfile](../Dockerfile) L7-9, L45-46

**问题**:
- 6.0.2 release notes 提到:ESP proposals 中带可选 DH group 时,不再要求 DH 在 ESP rekey 时有
- 6.0.1 → 6.0.2 升级几乎无风险(bug fix only)
- Dockerfile 注释 L8 说"6.1.0 虽然官方 2026-09 发布,但 Docker Hub tag 不稳、官方下载源常 404" —— 应重新评估

**修复方向**:
- 升 6.0.x 最新(目前 2026-09 最新 patch 应是 6.0.3 或 6.0.4)
- 保留 pin + MD5 验证

**工作量**:0.2d(改 ARG + 重新验证)

---

### [LOW-2] `--enable-stroke` + `--enable-swanctl` 同时启用,强 Swan 6.0 之后 swanctl 已覆盖所有常用操作(stroke 是 legacy)

**文件**:
- [file:///opt/ikev2-panel-v2-main/Dockerfile](../Dockerfile) L84-88

**问题**:
- 跟 [MED-2] 重复点:`stroke` 已 deprecate
- 可去掉,改用 `charon --help` + `swanctl` 启动流程
- **但** `ipsec start --nofork` 仍是最简单的启动方式(它内部 fork charon + 读 swanctl.conf)
- 可保留 stroke 但加注释 "deprecated,待 6.x 完全去除"

**修复方向**:与 MED-2 合并处理

**工作量**:0(并入 MED-2)

---

### [LOW-3] `noopen = no`(默认)是,但 filelog 没设 `append` —— entrypoint 启动后 charon 启动,**先 truncate 再 append**,跟 SIGHUP-reopen 行为不一致

**文件**:
- [file:///opt/ikev2-panel-v2-main/configs/strongswan-filelog.conf](../configs/strongswan-filelog.conf)

**问题**:
- filelog 的 default 是 `append = yes`(已开)
- 但 charon 启动**第一次 open** 是 truncate(因为还没数据)
- 实际上默认行为 OK,无需改

**修复方向**:无(确认行为即可)

**工作量**:0

---

### [LOW-4] `time_format = %Y-%m-%dT%H:%M:%S` 不带时区,**日志无法定位 UTC vs 本地时间**

**文件**:
- [file:///opt/ikev2-panel-v2-main/configs/strongswan-filelog.conf](../configs/strongswan-filelog.conf) L14

**修复方向**:改 `%Y-%m-%dT%H:%M:%S%z`(带时区偏移)

**工作量**:0.05d

---

### [LOW-5] `pool addrs` 写到 `addrs = 10.10.0.0/24, fd00:1::/64`,但 IPv6 pool 用 /64,**每个客户端只拿 /128** 单地址,**RFC 5735 没说 IKEv2 ModeConfig 给客户端必须给子网**,但 strongSwan 允许 /128 给单 host

**文件**:
- [file:///opt/ikev2-panel-v2-main/configs/swanctl-ipv6-only.conf](../configs/swanctl-ipv6-only.conf) L83

**问题**:
- `addrs = fd00:1::/64` 实际只给客户端发 /128(`fd00:1::2` 之类),/64 是 pool 上限
- iOS / macOS 把 VPN IPv6 当 virtual address,用 SLACC /64 可能冲突
- 通常 server 给 /128 + 路由 ::/0 即可

**修复方向**:确认 strongSwan 行为,如果有问题改 `addrs = fd00:1::2/128` 之类

**工作量**:0.1d(测试)

---

### [LOW-6] VICI list-sas 流式解析假设 child-sas 是嵌套 `*vici.Message`,但 govici 实际可能返回 `map[string]any`,需要断言更宽松

**文件**:
- [file:///opt/ikev2-panel-v2-main/internal/swanctl/parser.go](../internal/swanctl/parser.go) L115-122

**问题**:
- govici v0.8.x 中,Message.Get 返回 `any`,实际可能是:
  - `*vici.Message`(子 SA 嵌套)
  - `[]any`(list)
  - `string`(简单值)
- 当前代码 `if c, ok := c.(*vici.Message); !ok` 直接报错,实际 govici 在 list-sas 里 child-sas 就是 `*vici.Message` —— 但某些 charon 版本 / 某些 IKE SA 状态可能不一致
- 测试覆盖不够(parser_test.go 没看到相关用例)

**修复方向**:
- 加 fallback:如果 `c` 不是 `*vici.Message`,尝试 `[]any` 然后递归
- 加单元测试覆盖

**工作量**:0.3d(健壮性)

---

### [LOW-7] `charon.plugins.kernel-libipsec.use_netlink` 在 `configs/` 里没有任何显式配置,可能用到强 Swan 默认(OFF?),需要确认

**文件**:无直接配置文件

**问题**:
- 强 Swan 6.0.1 `kernel-libipsec` 默认 `use_netlink = no`(纯 TUN 用户态)
- README / docs 说"启用 libipsec + netlink(双 fallback)"—— 但 Dockerfile configure 只 `--enable-kernel-libipsec` + `--enable-kernel-netlink`(两者都编译)
- 实际启用哪一个看 `/etc/strongswan.d/charon/kernel-libipsec.conf` 是否被覆盖
- 当前 Dockerfile L177-190 补丁强制 load,但 **load + use_netlink = ?** 不明确

**修复方向**:
- 在 `/etc/strongswan.d/charon/kernel-libipsec.conf` 显式:
  ```conf
  kernel-libipsec { load = yes; use_netlink = yes }
  ```
- 文档里说明默认行为

**工作量**:0.2d(加 conf + 文档)

---

### [LOW-8] strongswan 6.0.1 MD5 不在校验脚本里用,改用 SHA256 后,镜像构建时下载 tarball 校验升级

**文件**:
- [file:///opt/ikev2-panel-v2-main/Dockerfile](../Dockerfile) L46, L54

**问题**:
- 当前 `STRONGSWAN_MD5=c3ddc81d1d11ce3d5431e15da7718748`
- 强 Swan 6.0.1 release notes 提供 SHA256 校验
- MD5 已 NIST deprecated(NIST SP 800-131A Rev.2 表 2)

**修复方向**:
- 改 SHA256 校验
- 同时保留 URL fallback

**工作量**:0.1d(改 Dockerfile 1 行 + 找 SHA256)

---

### [LOW-9] `mobike = yes` + IPv4 客户端 5-tuple 变化,**rekey 期间可能触发 mobike 重协商**,实测某些家用 4G 路由会断

**文件**:
- [file:///opt/ikev2-panel-v2-main/configs/swanctl-ipv6-only.conf](../configs/swanctl-ipv6-only.conf) L49

**问题**:
- mobike 让 client 切网不掉 VPN(很好),但 **rekey_time = 24h** 时,rekey 瞬间 client 切网 → mobike 重协商 + rekey 叠加 → iOS 卡 5-10s
- 建议:rekey 时关闭 mobike,或 rekey 失败时回落到完整重协商

**修复方向**:
- 加 `mobike = yes`(已有) + 注释说明 trade-off
- 长期:加 `reauth = no` 避免 EAP-MSCHAPv2 重认证(rekey 时只换 IKE SA key,不重认证)

**工作量**:0.05d(注释)

---

### [LOW-10] `certs = server.cert.pem` 用文件名(相对路径),swanctl 查 `/etc/swanctl/x509/server.cert.pem`,**没断言文件存在性** —— charon 启动 fail 时只 log 一行 WARNING,管理员不一定注意

**文件**:
- [file:///opt/ikev2-panel-v2-main/configs/swanctl-ipv6-only.conf](../configs/swanctl-ipv6-only.conf) L23

**修复方向**:
- entrypoint 启动前 `test -f /etc/swanctl/x509/server.cert.pem`,否则 exit 12
- 或:`swanctl --load-all` 后跑 `swanctl --list-certs | grep server.cert.pem`

**工作量**:0.1d

---

### [LOW-11] 配置 Doxygen / HTML comments 缺失,运维 grep 不友好

**修复方向**:给 `charon { }` 段加 inline 注释(官方模板有)

**工作量**:0(可选)

---

### [LOW-12] `send_cert = always` + iOS 配置 profile 不内嵌 CA(LE 模式)时,iOS 会"trust" 系统信任库,没 server cert 控制权

**文件**:
- [file:///opt/ikev2-panel-v2-main/internal/cert/mobileconfig.go](../internal/cert/mobileconfig.go)(前一轮审计已部分覆盖)

**问题**:
- LE 模式 mobileconfig 不带 CA(cert.IncludeCA = false)
- iOS 信任系统 store 里的 Let's Encrypt 根 → 一切 OK
- 但若 iOS 系统升级后 LE 中间 cert 变了(Let's Encrypt 2024 换了 ISRG Root X2 链),老 mobileconfig 可能 fail(概率低,LE 根 cert 是内置的)

**修复方向**:无(LE 链稳定性高)

**工作量**:0

---

## 不在范围内

- 证书生成(`internal/cert/generate.go`)本身由 audit-2026-09-cert.md 覆盖,本次仅在认证章节引用其输出(server cert EKU + SAN 配置正确)
- 移动面板 UI(CSR / 用户管理 / 限速显示)非 strongSwan 协议层
- DDNS / 证书自动续签流程由 audit-2026-09-cert.md 覆盖
- Docker 镜像体积 / 多阶段 / 安全(privilege/SYS_ADMIN)由 audit-2026-09-docker.md 覆盖
- 整体 HTTPS 面板 / cookie / session 安全由 audit-2026-09-security.md 覆盖

## 建议优先级

| 优先级 | Issue | 工作量 |
|--------|-------|--------|
| 🔴 P0(本周) | [HIGH-2] filelog enc=1 + 无轮转 + 无权限 | 0.5d |
| 🔴 P0(本周) | [HIGH-5] entrypoint §7/§7.6 死循环 load | 0.2d |
| 🔴 P0(本周) | [HIGH-6] LE renew 失败回退 reload 策略 | 0.5d |
| 🟠 P1(本月) | [HIGH-1] MODP2048 降权 + 注释 | 0.3d |
| 🟠 P1(本月) | [HIGH-3] send_certreq 关闭(EAP 模式) | 0.1d |
| 🟠 P1(本月) | [HIGH-4] 客户端 sswan 模板对齐服务端 | 0.3d |
| 🟠 P1(本月) | [MED-1] Dockerfile 删 sha1/md5 | 0.3d |
| 🟠 P1(本月) | [MED-4] 限速加 IPv6 filter + burst + handle | 1d |
| 🟠 P1(本月) | [MED-8] rekey 8h + reauth=no | 0.1d |
| 🟠 P1(本月) | [MED-9] DPD timeout/delay | 0.1d |
| 🟠 P1(本月) | [MED-12] block-throttle 启用 | 0.3d |
| 🟡 P2(下季) | [MED-2] stroke deprecate 处理 | 0.5d |
| 🟡 P2(下季) | [MED-3] IPv6 探测一致性 | 0.5d |
| 🟡 P2(下季) | [MED-5] VICI 事件流订阅 | 1d |
| 🟡 P2(下季) | [MED-6] 动态日志级别 | 0.5d |
| 🟡 P2(下季) | [MED-7] sswan Android/Linux CA 提示 | 0.1d |
| 🟡 P2(下季) | [MED-10] cert 安装竞态 | 0.5d |
| 🟢 P3(可选) | [LOW-1] 升级 6.0.x latest | 0.2d |
| 🟢 P3(可选) | [LOW-4] filelog 时区 | 0.05d |
| 🟢 P3(可选) | [LOW-6] VICI 解析健壮性 | 0.3d |
| 🟢 P3(可选) | [LOW-7] kernel-libipsec.conf 显式 | 0.2d |
| 🟢 P3(可选) | [LOW-8] SHA256 校验 | 0.1d |
| 🟢 P3(可选) | [LOW-10] cert 文件存在性检查 | 0.1d |

**累计**:23 个 issue(6 HIGH / 12 MED / 5 LOW;LOW-2 / LOW-3 / LOW-11 / LOW-12 工作量为 0 不计入)
