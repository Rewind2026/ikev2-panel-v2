# v2-84 Release Notes

> 发布日期:2026-09-18
> 主要内容:**DDNS 支持 IPv4 / IPv6 / 双栈可选**(v4 / v6 / dual 枚举)

---

## 概要

v2-82 引入 DDNS 时只同步 AAAA 记录(IPv6),IPv4-only VPS 用户根本看不到 DDNS 卡片。
v2-84 把 family 做成枚举可选,默认 dual 双栈同步,IPv4-only 用户也能用上 DDNS。

**核心改动**(5 项):

| # | 改动 | 用户感知 |
|---|------|----------|
| 1 | **DDNS family 枚举**(`v4` / `v6` / `dual`) | 面板 DDNS 卡片新增单选 radio,可运行时切换 |
| 2 | **A 记录同步**(阿里云 alidns A) | IPv4-only / 双栈 VPS 自动同步 A 记录 |
| 3 | **结构化 LastSync**(`V4IP/V4Error/V6IP/V6Error` 独立) | 面板分别显示 v4 / v6 状态,不再"v4 失败吞掉 v6 成功" |
| 4 | **并发 upsert**(v4 / v6 各自独立 goroutine) | v4 retry 1s+4s+9s 不阻塞 v6 同步 |
| 5 | **状态文件 INI 格式 + atomic write + 自动迁移** | v2-83 用户的裸 `true` 文件自动升级成 `enabled=true\nfamily=dual` |

**新增环境变量**:

| 变量 | 取值 | 默认 | 说明 |
|------|------|------|------|
| `IKEV2_DDNS_FAMILY` | `v4` / `v6` / `dual` | `dual` | 同步哪些记录类型 |
| `IKEV2_DDNS_PROBE_TARGET` | 任意 IPv4 地址 | `8.8.8.8` | IPv4 探测目标(中国大陆可改 `223.5.5.5`) |

---

## ⚠️ 行为变更(升级前请看)

### 1. **dual 默认值意味着 v2-83 用户升级后自动同步 A 记录**

- 如果 VPS 能探测到 IPv4,DDNS 现在会**同时**同步 A + AAAA
- 如果域名已有 A 记录但 IP 不对 → 会被自动更新(就像 v2-83 处理 AAAA 一样)
- 如果域名没建 A 记录 → 打 WARN 日志"no existing A record",不会自动建
- **建议 IPv6-only 用户显式设** `IKEV2_DDNS_FAMILY=v6` 保留 v2-83 行为

### 2. **状态文件格式变了(自动迁移)**

- v2-83:`/etc/ikev2/ddns.conf` 内容是裸 `true` / `false`
- v2-84:INI 格式,例如:
  ```
  # ikev2-panel DDNS runtime state
  enabled=true
  family=dual
  ```
- 首次启动自动迁移(写新格式),迁移失败会重试一次
- Atomic write(SIGKILL 写中途不会损坏文件)

### 3. **IPv4 DDNS 必须 host 网络**

- 当前 docker-compose 默认就是 host 网络,**开箱即用**
- 如果你改成了 bridge / ipvlan,IPv4 DDNS 会拿到容器 IP(不是公网 IP)→ WARN 日志
- IPv6 DDNS 不受网络模式影响

---

## 升级说明

### ⚠️ 行为不变(升级无感)

- `IKEV2_DDNS_ENABLED=false` → 完全不创建 DDNS sync,跟 v2-82/v2-83 一致
- v2-83 用户不设 `IKEV2_DDNS_FAMILY` → 默认 `dual`,但升级第一次启动会迁移状态文件
- v2-83 的 `ddns.conf` 裸 `true` / `false` → 自动迁移成 INI 格式
- 现有 v2-83 阿里云凭证兼容(无需改动)

### 升级步骤

```bash
# 1. 拉新镜像
git pull
docker compose build

# 2.(可选)如果你想保持 v2-83 的 v6-only 行为,改 .env:
echo "IKEV2_DDNS_FAMILY=v6" >> .env

# 3. 重启
docker compose up -d

# 4. 验证
docker logs ikev2-panel | grep -E "ddns|family"
# 应该看到 "ddns sync starting" + family=dual/v6/v4
```

### IPv4-only 新装

```yaml
# .env
IKEV2_DDNS_ENABLED=true
IKEV2_DDNS_FAMILY=v4
ALIYUN_ACCESS_KEY_ID=your_key_id
ALIYUN_ACCESS_KEY_SECRET=your_key_secret
ALIYUN_DOMAIN=example.com
ALIYUN_RR=vpn
```

**前置条件**:
- docker-compose 默认 host 网络(已满足)
- 阿里云解析里**预先建好 A 记录**(DDNS 不会自动创建新记录,避免误操作)
- RAM 策略包含 `alidns:UpdateDomainRecord`

---

## 兼容性矩阵

| 升级路径 | 行为 |
|---------|------|
| v2-83 → v2-84 | 未设 `IKEV2_DDNS_FAMILY` → 默认 `dual`,**自动启用 A 同步**(行为变更) |
| v2-83 → v2-84 + `IKEV2_DDNS_FAMILY=v6` | 完全保持 v2-83 行为 |
| v2-82 → v2-84 | 升级路径上同 v2-83 步骤 + 这条 env |
| v2-80 → v2-84 | 升级路径:先升 v2-82(引入 DDNS)→ v2-83(凭证统一)→ v2-84(family) |
| IPv4-only 新装 | `IKEV2_DDNS_FAMILY=v4`,**必须 host 网络** |
| 双栈新装 | `IKEV2_DDNS_FAMILY=dual`(默认),A + AAAA 都同步 |

---

## 风险与缓解

| 风险 | 缓解 |
|------|------|
| dual 默认值让 v2-83 用户意外同步 A 记录 | release notes 强提示;IPv4 探测失败时不计入失败(不写 LAST_DDNS_FAILED) |
| bridge 网络下 IPv4 探测返回容器 IP | 强制 host 网络约束,release notes 强提示 |
| dual 下 v4 retry 阻塞 v6 | 并发 upsert + per-type 节流 |
| 状态文件 race | atomic write + 自动迁移兼容 |
| 阿里云 RAM 权限错误无限重试 | 识别错误码 → WARN 一次不重试 |
| `net.Dial` 在受限网络长时间阻塞 | 3 秒 timeout + 可配置探测目标 |

---

## 集成测试 checklist

部署到测试机,跑:

| 场景 | 操作 | 期望 |
|------|------|------|
| 1 | `IKEV2_DDNS_FAMILY=v6`(回退兼容) | 仅同步 AAAA,面板 DDNS 卡片只显示 IPv6 |
| 2 | `IKEV2_DDNS_FAMILY=v4` | 仅同步 A,面板只显示 IPv4 |
| 3 | `IKEV2_DDNS_FAMILY=dual`(默认) | A+AAAA 都同步,面板 v4/v6 独立显示 |
| 4 | IPv4-only 部署 + `IKEV2_DDNS_FAMILY=v4` | 探测到 v4 → 同步 A 记录成功;面板能看到 DDNS 卡片 |
| 5 | `IKEV2_DDNS_FAMILY=invalid` | 启动 WARN 日志,fallback dual,不 panic |
| 6 | dual 模式 v4 探测失败(bridge 网络) | 面板 v4 红色告警 + v6 仍正常同步(不互相阻塞) |
| 7 | 面板切 family v4 → dual | 状态文件 atomic 写 `family=dual`;下次 sync tick 起开始同步 v6 |
| 8 | v2-83 状态文件 `ddns.conf` 内容为 `true` | 启动后自动迁移到 `enabled=true\nfamily=dual` |
| 9 | 模拟 SIGKILL 写状态文件中途 | 重启后状态文件仍可读,不损坏 |
| 10 | dual 模式下阿里云返回 RAM 权限错误 | WARN 一次,不重试 3 次 |

---

## 不在 v2-84 范围(明确 deferred)

来自 v2-83 复盘,用户确认"往后放":

1. **登录端点限速**(design §4.1 声称"5 次失败锁 5 分钟"未实现)→ P2
2. **IPv6 限速**(`tc u32` 不支持 IPv6,改 `tc flower`)→ P2
3. **backup.sh** 设计文档 §2.1 列入但实际不需要 → P2(设计文档收口)

---

## 文件清单(本次改动)

| 类型 | 路径 |
|------|------|
| 新增 | `internal/swanctl/ipv4watch.go` + `internal/swanctl/ipv4watch_test.go` |
| 新增 | `internal/ddns/statefile.go`(atomic + 迁移) |
| 新增 | `internal/ddns/sync_v4_test.go`(family / 并发 / 迁移专项测试) |
| 新增 | `internal/dns/aliyun_test.go`(recordType 白名单) |
| 改 | `internal/dns/aliyun.go`(`FindRecord(type)` + `UpdateRecordValue(...,type)`) |
| 改 | `internal/ddns/sync.go`(family-aware tick + 并发 upsert + per-type 节流 + 结构化 LastSync) |
| 改 | `internal/ddns/sync_test.go`(适配新 statefile API) |
| 改 | `internal/config/config.go`(`DDNSFamily` + `DDNSProbeTarget` + `parseFamily`) |
| 改 | `cmd/ikev2-panel/main.go`(装配条件改为 `cfg.DDNSEnabled`,加 `SetDetectV4`) |
| 改 | `internal/web/handlers_ddns.go`(`Family` 字段 + `handleDDNSFamily`) |
| 改 | `internal/web/handlers_home.go`(`homeData` 加 family + per-family 字段) |
| 改 | `internal/web/server.go`(注册 `POST /api/ddns/family`) |
| 改 | `web/templates/home_content.html`(family radio + per-family 状态显示) |
| 改 | `docker-compose.yml`(image 升 v2-84 + 2 个 env 透传) |
| 改 | `docs/design.md` §19.8 + §21 路线图 |
| 新增 | `docs/release-notes-v2.84.md`(本文件) |
| 新增 | `docs/proposal-v2.84.md` + `docs/plan-v2.84.md`(评审用) |

**总代码量**:~280 行净增(含测试),3 个新文件,11 个改文件。
