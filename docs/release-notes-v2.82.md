# v2.82 (2026-09-18) — 阿里云 DDNS 自动同步

## 概要

针对 **公网 IPv6 动态分配** 的 VPS 场景，把域名 AAAA 解析自动跟着 VPS 的真实 IPv6
变。证书本身跟 IP 无关（SAN 只放域名 + DNS-01 签发），所以**不需要重新签证书**——
DDNS 把 DNS 改了，客户端重新解析就拿到新 IP，连上后证书仍有效。

这是 design.md §19.3 留的扩展点，v2-82 落地实现。

## 用户场景

```
之前（痛点）：
  VPS 重启 → 拿到新 IPv6 (2001:db8::5 → 2001:db8::7)
  → 客户端解析 vpn.example.com → 还是老 IP
  → 连不上 VPN
  → 用户 ssh 进 VPS → 改 .env → docker compose down/up
  → 等 LE 证书续签 → 客户端重拨

现在（v2-82）：
  VPS 重启 → 拿到新 IPv6
  → 60s 后 ddns 探测到变化 → 调阿里云 API 改 AAAA
  → 客户端下次拨号解析新域名 → 拿到新 IP → 直接连上
  → 证书不变（SAN 只有域名）
```

## 用户配置

`.env` 加 4 个变量：

```bash
IKEV2_DDNS_ENABLED=true   # 总开关
ALIYUN_ACCESS_KEY_ID=...  # 阿里云 RAM AccessKey ID
ALIYUN_ACCESS_KEY_SECRET=...  # 阿里云 RAM AccessKey Secret
ALIYUN_DOMAIN=example.com # 主域名
ALIYUN_RR=vpn             # 主机记录(完整域名 = vpn.example.com)
```

### 阿里云 RAM 最小权限策略

```json
{
  "Version": "1",
  "Statement": [{
    "Effect": "Allow",
    "Action": [
      "alidns:DescribeDomainRecords",
      "alidns:UpdateDomainRecord"
    ],
    "Resource": "acs:alidns:*:*:domain/example.com"
  }]
}
```

## 面板 UI

首页多了 **DDNS 同步卡片**（IPv6-only 模式才显示）：

```
┌──────────────────────────────────────────┐
│ DDNS 同步 (阿里云 alidns AAAA 记录)       │
├──────────────────────────────────────────┤
│ 状态：已启用 (节流 60 秒,IP 变时自动同步)   │
│ 当前 IPv6：2001:db8::7                    │
│ 最近同步：2026-09-18T16:30:45Z (成功)       │
│ [关闭 DDNS]                              │
└──────────────────────────────────────────┘
```

连续 3 次同步失败时，**顶部红色告警横幅**：

```
⚠️ DDNS 同步失败：UpdateDomainRecord: alidns API error: code=InvalidAccessKeyId.NotFound
  最后一次同步：2026-09-18T16:30:45Z
```

## 升级步骤

```bash
# 1. 拉取新代码
git pull

# 2. .env 加 DDNS 变量
cat >> .env <<EOF
IKEV2_DDNS_ENABLED=true
ALIYUN_ACCESS_KEY_ID=your_ak
ALIYUN_ACCESS_KEY_SECRET=your_sk
ALIYUN_DOMAIN=example.com
ALIYUN_RR=vpn
EOF

# 3. 重建 + 启动
./scripts/up.sh --build
```

## 关键设计决策

| 决策 | 原因 |
|---|---|
| **不动证书** | SAN 只放域名,DNS-01 跟 IP 无关 |
| **节流 60 秒** | 防止阿里云 API 限流(60s 内最多同步一次) |
| **复用 swanctl.DetectGlobalV6** | 不开新探测 goroutine,共享 60s 周期 |
| **失败重试 3 次**(1s/4s/9s 退避) | 阿里云偶尔 transient 错误,自动恢复 |
| **失败时写 LAST_DDNS_FAILED** | 面板横幅提示,用户能感知 |
| **面板 UI 可切开关** | 调试/关停不用重启容器 |
| **凭证缺失时静默 skip** | 不报错刷屏,等用户补凭证 |
| **状态文件 /etc/ikev2/ddns.conf** | 运行时开关持久化,重启不丢 |
| **IPv4-only 模式不显示 DDNS** | 配置不存在(Sync 为 nil),模板跳过 |

## 文件清单

新增：
- `internal/dns/aliyun.go` - 阿里云 alidns SDK 封装(HMAC-SHA1 签名 + base64)
- `internal/dns/aliyun_test.go` - 签名测试(确定性 + 不同 params 不同 sig + nonce 不重复)
- `internal/ddns/sync.go` - DDNS 后台 goroutine(节流 + 重试 + 状态)
- `internal/ddns/sync_test.go` - 关闭/凭证缺失/节流/state file 测试
- `internal/web/handlers_ddns.go` - 面板 API (GET status + POST toggle)
- `docs/release-notes-v2.82.md` - 本文档

修改：
- `internal/config/config.go` - 加 5 个 DDNS env 字段
- `internal/swanctl/ipv6watch.go` - 导出 DetectGlobalV6 给 ddns 包复用
- `cmd/ikev2-panel/main.go` - 装配 ddnsSync,挂到 srv.DDNSSync
- `internal/web/server.go` - 加 2 个 DDNS 路由 + DDNSSync 字段
- `internal/web/handlers_home.go` - loadDDNSStatus 给 home 模板
- `web/templates/home_content.html` - DDNS 横幅 + 卡片
- `docker-compose.yml` - 加 5 个 env + 升 image tag 到 v2-82
- `.env.example` - 加 DDNS 配置段 + 注释说明
- `docs/design.md` - §19.3 改"v2-82 已实现", §21 路线图更新

## 测试覆盖

- `internal/dns`: 5 个测试,全绿
- `internal/ddns`: 8 个测试,全绿(开关/凭证/节流/state file/默认值)
- 全包 `go test ./...`: 9 个包,全绿
- `go build ./...`: 通过

## 留作未来扩展

- **Cloudflare / Godaddy / 其他 provider**:架构预留 Provider interface,但只实现 aliyun
- **IPv4 DDNS**:当前只动 AAAA(IPv6 场景);IPv4 场景需新加 A 记录同步逻辑
- **历史同步记录**:当前只保留最近一次;想看历史可以加 /data/le/ddns-history.jsonl
