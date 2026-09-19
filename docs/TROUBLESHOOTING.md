# 故障排查 / Troubleshooting

> **找症状 → 跟着排查命令跑 → 找到解法。**
> 命令在容器内执行：`docker compose exec ikev2-panel bash`。
> 涉及宿主机操作时会额外标注 `[host]`。

## 目录

1. [容器启动失败](#1-容器启动失败)
2. [mobileconfig 装不上 / 连不上](#2-mobileconfig-装不上--连不上)
   - [2.4 能拨号但上不了网(v2.86-PR12.10 ~ PR12.17)](#24-能拨号但上不了网ios--android-通用v286-pr1210--pr1217-修复)
3. [Let's Encrypt 续签失败](#3-lets-encrypt-续签失败)
4. [DDNS 不更新](#4-ddns-不更新)
5. [限速不生效](#5-限速不生效)
6. [admin 密码忘](#6-admin-密码忘)
7. [DB 损坏](#7-db-损坏)

---

## 1. 容器启动失败

### 1.1 IPv6 forwarding 没开

**症状**：`docker compose up` 报 `Failed to enable IPv6 forwarding` 或容器内 `ip -6 addr` 空。

**排查**：

```bash
cat /proc/sys/net/ipv6/conf/all/forwarding  # 应为 1
```

值为 `0` = 未启用 → docker daemon IPv6 转发没开。

**解法** `[host]`：

```bash
echo "ipv6: true" >> /etc/docker/daemon.json
echo "ip6tables: true" >> /etc/docker/daemon.json
systemctl restart docker
```

容器内 `entrypoint.sh` 会自动 fallback 写 `/proc/sys/net/ipv6/conf/all/forwarding`，但前提是 host netns 可写（host 网络模式 + privileged）。

### 1.2 UDP 500/4500 被占用

**症状**：charon 日志 `bind: address already in use`。

**排查**：

```bash
netstat -ulnp | grep -E ':(500|4500)'
```

**解法** `[host]`：停掉冲突服务（streisand / 旧 ikev2 / Strongswan 主机包等）或改 `docker-compose.yml` 的 `ports:` 映射。

### 1.3 /etc/swanctl/conf.d 不可写

**症状**：entrypoint 报 `chmod: cannot operate on '/etc/swanctl/conf.d'`。

**排查**：

```bash
ls -la /etc/swanctl/conf.d
mount | grep swanctl   # bind mount 状态
```

**解法** `[host]`：检查 `docker-compose.yml` 是否 bind mount 了 `/etc/swanctl`：

```yaml
volumes:
  - /etc/swanctl:/etc/swanctl
```

确保宿主目录有写权限。容器内 `charon` 用户需要 0644 写 conf 文件。

### 1.4 charon 启动 hang

**症状**：日志停在 `charon started (停留在 init)`，swanctl --load-all 卡住。

**排查**：

```bash
tail -50 /var/log/charon.log
swanctl --stats   # 应在 10s 内返回
```

**解法**：通常是证书没准备好（LE 模式首次签发失败）。检查 `/var/log/acme-renew.log` 和 `/data/le/` 目录。

---

## 2. mobileconfig 装不上 / 连不上

### 2.1 iOS 不弹"安装描述文件"

**症状**：Safari 打开 mobileconfig 链接，下载后无提示。

**排查**：

```bash
# 1. 检查 mobileconfig MIME 类型
curl -I https://your-domain:8443/users/3/mobileconfig
# 应返回 Content-Type: application/x-apple-aspen-config
```

**解法**：

- 检查 Safari 是否拦截弹窗（设置 → Safari → 拦截弹窗 → 关）
- 直接打开「设置 → 通用 → VPN 与设备管理」看是否已下载但未提示

### 2.2 装完连不上（iKEv2 协商失败）

**症状**：iOS 显示"无法连接到 VPN"。

**排查**：

```bash
# 容器内实时看 charon 日志
tail -f /var/log/charon.log
```

看到 `NO_PROPOSAL_CHOSEN` → 客户端/server 算法不匹配（少见，因为 mobileconfig 固定了 suite-B）。

看到 `AUTHENTICATION_FAILED` → 用户名密码错。在面板 → 用户详情页 → 重置密码。

### 2.3 Android 原生连不上（11+）

**症状**：Android 设置 → VPN → 连接失败。

**排查**：查看 Android 系统 VPN 日志（开发者选项 → 日志查看器，或 `adb logcat | grep -i ike`）。

常见错误：
- `MSCHAPV2 failed` → 用户名/密码错
- `Server certificate not trusted` → 没装 CA 证书（面板首页 / ca.cert.pem 下载并安装到"系统"信任存储）

### 2.4 能拨号但上不了网（iOS / Android 通用，v2.86-PR12.10 ~ PR12.17 修复）

**症状**：iOS / Android 显示"已连接"，但 Safari / 微信 / 任何 app 都打不开网页，`ping 8.8.8.8` 也超时。`swanctl --list-sas` 显示 ESTABLISHED + INSTALLED。

**根因**：v2-78 ~ v2.85 期间多层叠加：

1. **iOS 拿到 IPv4 虚拟 IP 但 ESP 内层协议族 TS 不匹配**（v2.86-PR12.10 修复）
   - `local_ts = ::/0`（仅 IPv6），客户端拿 IPv4 VIP 后发的 IPv4 inner packet 不匹配 → ESP 内核 libipsec DROP
   - **解法**：`local_ts = 0.0.0.0/0, ::/0`（双栈，跟 IPv6_ONLY 无关）

2. **docker daemon 异步 `iptables-restore` 挤掉 FORWARD ACCEPT / MASQUERADE 规则**（v2.86-PR12.11/PR12.13 引入 retry，PR12.16 彻底切 nft 自定义表根治）
   - 写入 daemon 自己的 `filter` / `nat` 表 → daemon 5-60s 后异步重写 → 规则被清
   - **v2.86-PR12.16 解法**：改用 nft 自定义表 `table ip ikev2` / `table ip6 ikev26` → daemon 完全碰不到

3. **mobileconfig RemoteAddress 用 IP 字面量时 iOS SAN 校验失败**（v2.86-PR12.13 修复）
   - iOS 看到 RemoteAddress 是 IP → 查证书 `IPAddresses` SAN，找不到 → "服务器身份不可信"
   - **解法**：RemoteAddress 优先用 DDNS 域名（`ALIYUN_RR + "." + ALIYUN_DOMAIN`），证书 `dNSName` SAN 包含

4. **IPv6 ESP outer 包被 ip6tables FORWARD policy DROP**（v2.86-PR12.13 修复）
   - 50.63 是 IPv6-only 服务器，ESP 外层是 IPv6 → daemon `table ip6 filter FORWARD policy DROP` 时丢包
   - **解法**：`table ip6 ikev26 forward chain iifname "ipsec0" accept`

**排查命令**：

```bash
# 1. 看 SA 协商是否成功
docker exec ikev2-panel swanctl --list-sas
# 期望看到 ESTABLISHED + INSTALLED + local '0.0.0.0/0 ::/0' + remote '<client_vip>'

# 2. 看 nft 自定义表是否存在 + 计数器是否在涨
docker exec ikev2-panel nft list table ip ikev2
docker exec ikev2-panel nft list table ip6 ikev26
# 注意 grep 'counter packets' 行,有非 0 数据 = 流量在转发

# 3. 看 MSS clamp 是否在工作
docker exec ikev2-panel nft list table ip ikev2 | grep mangle
# 期望 mangle_forward / mangle_output chain 都有 rules

# 4. 看 §7.9 retry 状态
docker logs ikev2-panel 2>&1 | grep -E 'nft|verified|retry'
# 期望 'nft ikev2/ikev26 ruleset verified after N retries' 且 N < 5

# 5. 如果 nft 表丢了,手动重建
docker exec ikev2-panel nft -f /run/ikev2-panel/ikev2.nft
docker exec ikev2-panel nft list table ip ikev2  # 验证回来了

# 6. 看 iptables-warn.flag sentinel(§7.9 失败时写)
docker exec ikev2-panel ls -la /run/ikev2-panel/
docker exec ikev2-panel cat /run/ikev2-panel/iptables-warn.flag 2>/dev/null
```

**确认修复版本**：镜像 tag 必须 >= `v2.86-pr12.16`。检查：

```bash
docker inspect ikev2-panel:v2.86-pr12.17 --format '{{index .Config.Labels "org.opencontainers.image.version"}}'
# 期望: v2.86-pr12.17
```

---

## 3. Let's Encrypt 续签失败

### 3.1 LAST_RENEW_FAILED 标志

**症状**：面板首页红色横幅"LE 续签失败"。

**排查**：

```bash
ls -la /data/le/LAST_RENEW_FAILED   # 标志文件存在 = 续签失败
cat /var/log/ikev2-renew.log | tail -50
cat /var/log/acme-renew.log | tail -50
```

**解法**：

- **acme-renew.log 报 `InvalidAccessKeyId`** → AccessKey 错。检查 `/data/panel-state/aliyun.creds` 或 env `IKEV2_ALIYUN_KEY_*`。
- **acme-renew.log 报 `DomainRecordNotExist`** → 阿里云 DNS 没 A/AAAA 记录。先手动加一条解析。
- **acme-renew.log 报 `rate limit exceeded`** → 等 1 小时，Let's Encrypt 单域有速率限制。

修好后手动重试：

```bash
docker compose exec ikev2-panel bash -c 'rm -f /data/le/LAST_RENEW_FAILED && /etc/ikev2-panel/scripts/renew-cert.sh your.domain.com'
```

### 3.2 DNS-01 challenge 卡住

**症状**：续签 5+ 分钟没结果。

**排查**：

```bash
# 看 acme.sh 是否在等 DNS 传播
cat /var/log/acme-renew.log | tail -30
```

**解法**：通常是阿里云 DNS API 限频。续签 cron 是凌晨 3:30 跑，可改 `entrypoint.sh` 里的 cron 时间避开阿里云每日限频窗口（00:00-01:00）。

---

## 4. DDNS 不更新

### 4.1 凭证缺失

**症状**：面板首页黄色横幅"未配置阿里云凭证"。

**排查**：

```bash
ls -la /data/panel-state/aliyun.creds   # 应存在
```

**解法**：在面板 → 首页 → 阿里云 API 凭证 → 填 AccessKey ID + Secret。

### 4.2 API 错误码精确匹配

**症状**：DDNS 状态卡"同步失败"，详情有 alidns 错误码。

常见错误码（v2.85-PR5 加入精确匹配）：
- `DomainRecordNotBelongToUser` → 域名在别的阿里云账号
- `DomainRecordDuplicate` → 已有同名记录（DDNS 应改而非新建；可能首次同步失败遗留）
- `Throttling` → API 限频，DDNS throttle 60s 内会自动重试（v2.85-PR6）
- `InvalidDomainName` → 域名格式错（`ALIYUN_DOMAIN` 必须是完整域名如 `vpn.example.com`）

**解法**：先在阿里云控制台手动解析一次，确认权限 OK，再让 DDNS 接管。

### 4.3 节流窗口不释放

**症状**：明明 IP 变了，但 60s 内没同步。

**排查**：

```bash
cat /data/ddns.state    # v2.85-PR6 起有 last_sync_a / last_sync_aaaa 字段
```

**解法**：v2.85-PR6 后 throttle 时间戳跨重启保留，所以容器重启不会丢失窗口。等满 60s 或手动删文件重置。

---

## 5. 限速不生效

### 5.1 tc qdisc 没建

**症状**：用户拨入后下载/上传不限速。

**排查**：

```bash
tc qdisc show dev eth0
# 应看到 "qdisc htb 1: root"
```

如果只有 `qdisc fq_codel` 等默认 qdisc → 根 qdisc 没建。

**解法**：首次 SA up 时 `ikev2-updown` 会自动 `tc qdisc add ... root handle 1: htb`。检查 `/etc/ikev2-panel/family.env` 是否存在（v2.85-PR7 要求）。

```bash
cat /etc/ikev2-panel/family.env   # 应有 FAMILY=ipv4|ipv6|dual
```

### 5.2 IPv6 用户被绕过（v2.85-PR7 修复）

**症状**：v2 默认 IPv6-only 模式，用户设了 5 Mbps 但无限速。

**排查**：

```bash
tc filter show dev eth0 parent 1: | grep -A2 ipv6
# 应有 "protocol ipv6 ... flower ... ip6_src ..." 或 "src_ip ..."
```

**解法**：

- v2.85-PR7 起 `tc flower` (Linux 4.1+) 自动启用 IPv6 限速
- 内核 < 4.1 → 升级内核；或临时方案：管理员手工加 flower filter

### 5.3 iptables 计数器异常

**症状**：tc class 显示有流量但没限速效果。

**排查**：

```bash
tc -s class show dev eth0
# 看 bytes 计数 + dropped
```

**解法**：HTB rate 单位是 mbit（小写），不是 Mbps。`1000mbit` = 1 Gbps，写错会变成 1 kbps。

---

## 6. admin 密码忘

### 6.1 用 --reset-password flag（v2.85-PR2 引入）

**排查 + 解法** `[host]`：

```bash
# 1. 停容器
docker compose stop ikev2-panel

# 2. 启动时传环境变量 reset
docker compose run -e IKEV2_RESET_ADMIN_PASSWORD=yes ikev2-panel &
sleep 3

# 3. 看 docker logs 拿到新密码
docker compose logs ikev2-panel | grep "Generated new admin password"

# 4. 停掉这个临时容器
docker compose down
docker compose up -d
```

新密码会写入 `/data/panel-state/INITIAL_ADMIN_PASSWORD.txt`。

### 6.2 DB 直改（无 reset flag 也能用）

**排查 + 解法** `[host]`：

```bash
docker compose exec ikev2-panel bash -c '
  # 1. 安装 python3 + bcrypt（如果镜像里没）
  apt-get update && apt-get install -y python3-pip
  pip3 install bcrypt --break-system-packages

  # 2. 生成新 hash
  python3 -c "import bcrypt; print(bcrypt.hashpw(b\"new-password\", bcrypt.gensalt(12)).decode())"
  # 复制输出的 hash

  # 3. 写回 DB
  sqlite3 /data/panel.db "UPDATE admins SET password_hash = '\''<上面的hash>'\'', updated_at = strftime(\"%s\", \"now\") WHERE id = 1;"
'

# 4. 重启容器让 session 失效（旧 session 仍可用直到过期）
docker compose restart ikev2-panel
```

### 6.3 紧急:重置为默认密码 + INITIAL_ADMIN_PASSWORD.txt

**解法** `[host]`：

```bash
# 删文件 + 删 admin 记录,容器重启时会重新生成
docker compose exec ikev2-panel bash -c '
  rm -f /data/panel-state/INITIAL_ADMIN_PASSWORD.txt
  sqlite3 /data/panel.db "DELETE FROM admins WHERE id = 1;"
'
docker compose restart ikev2-panel
docker compose logs ikev2-panel | grep "INITIAL_ADMIN_PASSWORD"
```

---

## 7. DB 损坏

### 7.1 sqlite3 .recover

**症状**：启动报 `database disk image is malformed`。

**解法**：

```bash
docker compose exec ikev2-panel bash -c '
  # 备份原文件
  cp /data/panel.db /data/panel.db.broken

  # 尝试恢复
  sqlite3 /data/panel.db ".recover" | sqlite3 /data/panel.db.recovered

  # 替换
  mv /data/panel.db.recovered /data/panel.db
'
docker compose restart ikev2-panel
```

### 7.2 VACUUM 整理

**症状**：DB 文件大但实际数据少（碎片）。

**解法**：

```bash
docker compose exec ikev2-panel sqlite3 /data/panel.db "VACUUM;"
```

### 7.3 完全重建（最后手段）

**解法** `[host]`：

```bash
# 警告：所有用户、admin、审计日志丢失
docker compose exec ikev2-panel bash -c 'rm -f /data/panel.db /data/panel.db-*'
docker compose restart ikev2-panel
# 容器会自动重建空 DB + 生成默认 admin 密码
docker compose logs ikev2-panel | grep "INITIAL_ADMIN_PASSWORD"
```

---

## 附:常用诊断命令

```bash
# 1. 容器整体健康
docker compose ps
docker compose logs --tail=100 ikev2-panel

# 2. charon 实时日志
docker compose exec ikev2-panel tail -f /var/log/charon.log

# 3. swanctl SA 状态
docker compose exec ikev2-panel swanctl --list-sas

# 4. 面板 HTTP 健康检查 (v2.85-PR8)
docker compose exec ikev2-panel curl -s http://localhost:8443/healthz | python3 -m json.tool

# 5. Prometheus metrics (v2.85-PR8)
docker compose exec ikev2-panel curl -s http://localhost:8443/metrics

# 6. 操作日志 (v2.85-PR8)
# 登录面板 → 顶部"日志"链接,或:
sqlite3 /data/panel.db "SELECT timestamp, actor, event, details FROM audit_log ORDER BY id DESC LIMIT 20;"

# 7. 用户列表
sqlite3 /data/panel.db "SELECT id, username, enabled, created_at FROM users;"
```

---

## 报告 bug

如果以上步骤都没解决，到 GitHub Issues 提交时附上：

```bash
# 一键收集诊断信息
docker compose exec ikev2-panel bash -c '
  echo "=== ikev2-panel version ===" && cat /etc/ikev2-panel-version 2>/dev/null || echo dev
  echo "=== healthz ===" && curl -s http://localhost:8443/healthz | python3 -m json.tool
  echo "=== charon.log (last 30) ===" && tail -30 /var/log/charon.log
  echo "=== ikev2-renew.log (last 30) ===" && tail -30 /var/log/ikev2-renew.log 2>/dev/null
  echo "=== users ===" && sqlite3 /data/panel.db "SELECT id,username,enabled FROM users;"
  echo "=== audit_log (last 20) ===" && sqlite3 /data/panel.db "SELECT timestamp,actor,event,details FROM audit_log ORDER BY id DESC LIMIT 20;"
' > /tmp/diag-$(date +%Y%m%d-%H%M).txt
```

把 `/tmp/diag-*.txt` 内容贴到 issue。
