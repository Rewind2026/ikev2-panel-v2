# v2-84 提案(待评审):DDNS 支持 IPv4 / IPv6 / 双栈可选

> 状态:**草案**,等待评审。
> 关联文档:docs/design.md §19,docs/release-notes-v2.82.md,docs/release-notes-v2.83.md。
> 前置:v2-82 引入 DDNS(v6-only),v2-83 统一阿里云凭证 + HTTPS 热重载。
> 评审轮次:已过 3-agent 评审(代码合理性 / 安全 / 边界场景),8 个 P0 已修进本文。

---

## 背景

### 现状(用户实测)

- 当前 DDNS 实现只同步 **AAAA 记录**([internal/ddns/sync.go](file:///opt/ikev2-panel-v2-main/internal/ddns/sync.go) L268-322 + [internal/dns/aliyun.go](file:///opt/ikev2-panel-v2-main/internal/dns/aliyun.go) L84-135)。
- 注释里直接写着 "没 IPv6(IPv4-only 场景),DDNS 暂不处理 v4"(sync.go L275)。
- [cmd/ikev2-panel/main.go](file:///opt/ikev2-panel-v2-main/cmd/ikev2-panel/main.go) L303 把 `ddnsSync` 创建限定在 `cfg.IPv6Only == true` —— 即 **IPv4-only 用户连 DDNS 卡片都看不到**,彻底不可用。
- 现实场景至少三种:
  1. **纯 IPv4** VPS(国内云小鸡、IPv6 没开)→ 需要同步 A 记录
  2. **纯 IPv6** VPS(海外 NAT VPS)→ 当前能用,需要保留
  3. **双栈** VPS(主流 VPS)→ A + AAAA 都要同步

### 用户确认(2026-09-18 复盘)

> "ddns 可以单独配置 v4 或者 v6 或者双栈 是这功能是必要的 其他的可以往后放"

明确:**v2-84 范围仅 DDNS family 选择**,其他 backlog(登录端点限速 / IPv6 限速 / backup.sh 文档收口)延后。

---

## 目标

| ID | 目标 | 验收标准 |
|----|------|----------|
| G1 | **DDNS family 可选**:`v4` / `v6` / `dual`,运行时可切换 | `IKEV2_DDNS_FAMILY=dual` 默认行为=A+AAAA 都同步;`v4` 只同步 A;`v6` 只同步 AAAA(向后兼容 v2-83) |
| G2 | **IPv4-only 部署可见 DDNS 卡片**:`cfg.IPv6Only == true` 不再作为前置条件 | `IKEV2_IPV6_ONLY=false` 且 `IKEV2_DDNS_ENABLED=true` 时,面板能看到并切换 DDNS |
| G3 | **向后兼容**:v2-83 用户升级无感(行为可预测) | 不设置 `IKEV2_DDNS_FAMILY` 时默认 `dual`,但 v2-83 老用户不会被"v4 失败横幅"误导(见 D7) |
| G4 | **阿里云 API 支持**:能查/改 A 记录 | `AliyunClient.FindRecord(type)` / `UpdateRecordValue(...,type)` 支持 `Type=A` |

---

## 设计方案

### D1. 配置项:枚举比双 bool 直观

**新增**:`IKEV2_DDNS_FAMILY`,取值 `v4` / `v6` / `dual`,默认 `dual`。

为什么不拆成 `IKEV2_DDNS_V4_ENABLED` + `IKEV2_DDNS_V6_ENABLED`:
- 三个合法组合(v4 / v6 / dual),两个 bool 会出现第四种 `both=false` = 完全禁用,语义重复(已用 `IKEV2_DDNS_ENABLED` 控制)
- 枚举值自带校验,面板 UI 单选控件更干净
- 写 .env 时少一个变量

### D2. 探测:复用 IPv6 路径,新增 IPv4 路径

| family | 探测函数 | 来源 |
|--------|---------|------|
| `v4` | `DetectGlobalV4(iface, target)` | **新增**(`net.Dial` + 过滤 + timeout) |
| `v6` | `DetectGlobalV6(iface)` | 已有(swanctl 包 L137) |
| `dual` | 两个都调 | 并发(errgroup) |

**IPv4 探测方案**(评审 P0 修正):
- `net.Dialer{Timeout: 3*time.Second}.Dial("udp", target+":80")` 拿 `LocalAddr()`
- `target` 默认 `8.8.8.8`,可由 `IKEV2_DDNS_PROBE_TARGET` 覆盖(中国大陆用户改 `223.5.5.5`)
- **强制过滤**(照搬 `detectGlobalV6` 的 `isGlobalV6` 范式,新写 `isGlobalV4`):
  - `0.0.0.0` / `255.255.255.255`(无效)
  - `127.0.0.0/8`(loopback)
  - `169.254.0.0/16`(link-local)
  - `10.0.0.0/8`、`172.16.0.0/12`、`192.168.0.0/16`(RFC1918)
  - `100.64.0.0/10`(CGNAT)
  - `224.0.0.0/4`(multicast)
- 命中过滤 → return error,**绝不继续 upsert**
- iface 参数非空时:`net.InterfaceByName(iface)` 拿该接口地址列表,跟 dial 出的 IP 比对;不匹配说明默认路由选了别的接口 → return error(防止 v4 探测返回非 iface 地址)

### D3. Aliyun 客户端:Type 参数化

**改动**:
- `FindAAAARecord(domainName, rr)` → `FindRecord(domainName, rr, recordType)`(`recordType` ∈ {"A","AAAA"})
- `UpdateRecordValue(recordID, rr, value, ttl)` 加 `recordType string` 参数(当前 L122 写死 `"AAAA"`)
- **入参校验**(评审 S2):`recordType` 非白名单值 → return error,不调 API

### D4. sync.go 调度逻辑:并发 + per-type 节流 + 结构化 LastSync

评审反馈当前 plan 串行写法 + 单一 LastSync 在 dual 模式下不可用,**核心重做**:

```
sync():
  // 凭证校验(整轮共享)
  if getter nil/empty -> skip 整轮
  
  // 节流 per-type(独立 lastSyncTime,key = "A"|"AAAA")
  // v4 retry 1s/4s/9s 不会阻塞 v6
  
  // errgroup 并发(评审 S7)
  eg.Go(func() error { return s.upsertRecord("A", v4IP, v4) })
  eg.Go(func() error { return s.upsertRecord("AAAA", v6IP, v6) })
  eg.Wait()
  
  // 聚合:
  //   Success = 任一 family 成功过 OR (v4 未启用 && v6 未启用)
  //   V4IP/V4Error / V6IP/V6Error 独立记录
```

`upsertRecord(s, recordType, currentIP, throttleKey)` 函数:
- 节流:per-type `map[string]time.Time` 检查
- 找记录(`FindRecord(... recordType)`)
- **0 条 → WARN 日志,return nil**(`rec == nil` 不计入失败,避免 IPv4-only 用户升级上来后横幅永远红)
- **多条 → WARN 日志,return nil**(评审 K3:不返 error,跟 "0 条 skip" 语义对称;不影响另一 family)
- **1 条 → 比对 IP,变了才 `UpdateRecordValue(... recordType)`,retry 3 次**
- 阿里云返回 RAM 权限错误(`RecordNotBelongToRAM` / 类似)→ WARN 一次,不再重试(避免无谓重试)

### D5. LastSync 结构化(评审 H2 修正)

**当前**:
```go
type LastSync struct {
    Time, Success, Error, NewIP, OldIP
}
```

**v2-84**:
```go
type LastSync struct {
    Time    time.Time  // 整轮最近一次 tick 时间
    Success bool       // 整轮 = 任一 family 成功过(注意:不严格 AND,因为 dual 下 v4 失败是常态)
    V4IP    string     // 空 = 该 family 未启用 / 未探测 / 探测失败
    V4Error string
    V6IP    string
    V6Error string
    // 兼容旧字段(置空,JSON omitempty),v2-83 客户端不破
    NewIP string `json:",omitempty"`
    OldIP string `json:",omitempty"`
    Error string `json:",omitempty"`
}
```

- `LAST_DDNS_FAILED` 写盘条件:`!Success` AND `v4+v6 都尝试过`(v4-only 模式下只看 v4)
- 面板 JS 读 `v4_ip` / `v4_error` / `v6_ip` / `v6_error` 分别渲染

### D6. 状态文件格式迁移(评审 H1 P0)

**当前** `/etc/ikev2/ddns.conf`:
- v2-83 实际内容:`true` 或 `false`(裸字符串),`readStateFile` 走 `strings.TrimSpace == "true"`

**v2-84**:
- 内容:`enabled=true\nfamily=dual`(INI-style key=value,每行一对)
- **兼容读**:`parseStateFile()` 同时识别两种格式:
  - 整文件 == "true" → 视为 `enabled=true`,family 用 env / 默认值
  - 整文件 == "false" → 视为 `enabled=false`
  - 含 `enabled=` 行 → 按 key=value 解析
- **写**:统一写 key=value 格式,temp file + atomic rename(评审 S1)
- **首次启动自动迁移**:检测到裸 `true|false` → 立即重写成 key=value 格式

**新增 helper**:`internal/ddns/statefile.go` 独立文件,封装 `parseStateFile` / `writeStateFile`(都用 atomic write)。

### D7. 面板 UI:DDNS 卡片加 family 单选

`web/templates/home_content.html` DDNS 卡片:
- 现有:开关(enabled toggle)
- 新增:`family` 单选(`仅 IPv4` / `仅 IPv6` / `双栈`)
- 当前值由 `GET /api/ddns/status` 返回新增字段 `family` 渲染
- 切换走 `POST /api/ddns/family`,form 参数 `family=v4|v6|dual`(handler + SetFamily 双重白名单校验,防注入)

`internal/web/handlers_ddns.go`:
- `DDNSStatusResp` 加 `Family string` + `V4IP/V4Error/V6IP/V6Error` 字段
- 新增 `handleDDNSFamily(w, r)` handler(用 `protectPOST`,跟 `/api/ddns/toggle` 一致 → CSRF 保护)
- `Server.DDNSSync` 加方法 `SetFamily(string) error` / `Family() string`(白名单校验 + atomic 写)

状态文件:D6 决定的 INI 格式,`family=dual` 行跟 `enabled=true` 同文件。

### D8. main.go 装配:放宽 IPv6Only 强约束

[cmd/ikev2-panel/main.go](file:///opt/ikev2-panel-v2-main/cmd/ikev2-panel/main.go) L303 现状:

```go
if cfg.IPv6Only {
    ddnsSync = ddns.NewSync(...)
}
```

**改为**:

```go
if cfg.DDNSEnabled {  // family 字段 parseFamily 已保底默认 dual,!= "" 是冗余防御
    ddnsSync = ddns.NewSync(...)
}
```

含义:
- `IKEV2_DDNS_ENABLED=false` → 完全不创建 sync(向后兼容 v2-82/v2-83)
- `IKEV2_DDNS_ENABLED=true` → 创建 sync,family 由 `IKEV2_DDNS_FAMILY` 决定(默认 dual)
- 跟 `cfg.IPv6Only` 完全解耦

新增 `DDNSFamily` 字段到 [internal/config/config.go](file:///opt/ikev2-panel-v2-main/internal/config/config.go),env 名 `IKEV2_DDNS_FAMILY`,合法值校验在 `config.parseFamily()`(非法 → 默认 dual + WARN 日志)。

### D9. entrypoint.sh:透传新 env

[scripts/entrypoint.sh](file:///opt/ikev2-panel-v2-main/scripts/entrypoint.sh) **不动**(只透传 env 到 Go 进程环境变量,Docker compose 里加一行即可)。entrypoint 本身不直接读 family,family 是 Go 进程的运行时决策。

### D10. 网络模式约束(评审 S5)

**强约束**:IPv4 DDNS **必须 host 网络**(`network_mode: host`)。bridge 网络下:
- `/proc/net/fib_trie` 是宿主视角,容器内拨号可能命中容器默认路由 → 返回容器 IP
- `net.Dial` 的 LocalAddr 是 docker bridge NAT 后的容器 IP(172.17.x.x)

当前 docker-compose 默认 host,所以"开箱即用"。但用户可能改 network_mode → release-notes 强提示。

---

## 文件清单

| 路径 | 操作 | 说明 |
|------|------|------|
| `internal/dns/aliyun.go` | 改 | `FindRecord(type)` / `UpdateRecordValue(...,type)` + 白名单校验 |
| `internal/swanctl/ipv4watch.go` | 新 | `DetectGlobalV4(iface, target)` + `isGlobalV4` 过滤 |
| `internal/ddns/sync.go` | 改 | `Config.Family`;并发 upsert(errgroup);per-type 节流;`LastSync` 拆分 |
| `internal/ddns/statefile.go` | 新 | `parseStateFile`(兼容裸 true/false + INI)+ `writeStateFile`(atomic) |
| `internal/ddns/sync.go` 抽 | 改 | `upsertRecord(recordType, ip, throttleKey)` 函数 |
| `internal/config/config.go` | 改 | `DDNSFamily` + `parseFamily()` + `DDNSProbeTarget`(可选) |
| `cmd/ikev2-panel/main.go` | 改 | 装配放宽 + `SetDetectV4` |
| `internal/web/handlers_ddns.go` | 改 | `DDNSStatusResp.Family` + V4/V6 字段;`handleDDNSFamily` |
| `internal/web/server.go` | 改 | 注册 `POST /api/ddns/family`(`protectPOST`) |
| `web/templates/home_content.html` | 改 | DDNS 卡片加 family radio + v4/v6 状态独立显示 |
| `docker-compose.yml` | 改 | 加 `IKEV2_DDNS_FAMILY: dual`、`IKEV2_DDNS_PROBE_TARGET`(可选) |
| `.env.example` | 改 | family + probe_target 说明 |
| `docs/design.md` | 改 | §19.8 v2-84 设计 + §21 路线图 |
| `docs/release-notes-v2.84.md` | 新 | 升级步骤 + dual 默认值行为变更 + host 网络约束 |
| 测试 | 新 | T1~T7 各包 _test.go(详见 plan) |

---

## 兼容性

| 升级路径 | 行为 |
|---------|------|
| v2-83 → v2-84 | 未设 `IKEV2_DDNS_FAMILY` → 默认 `dual`,**自动启用 A 同步**(行为变更,见 release-notes) |
| v2-83 用户想要 v6-only | 显式 `IKEV2_DDNS_FAMILY=v6` 即可 |
| v2-83 用户 `ddns.conf` 是裸 `true` | 启动时自动迁移到 `enabled=true\nfamily=dual` |
| v2-82 → v2-84 | 升级路径上同 v2-83 步骤 + 这条 env |
| IPv4-only 新装 | `IKEV2_DDNS_FAMILY=v4`,**必须 host 网络**,面板能看到 DDNS 卡片 |

**风险点**:`dual` 默认值意味着 v2-83 用户升级后**自动开始同步 A 记录**(如果他们环境能探测到 IPv4)。
- 缓解:`detectGlobalV4` 失败时静默 WARN,不写 `LAST_DDNS_FAILED`(评审 A:不计入失败)
- 如果用户没有 A 记录,会打印 "no existing A record, skipping update" WARN(跟现有 AAAA 行为一致),不会覆盖别处的记录
- release notes 强调这一点,建议 IPv6-only 用户显式设 `v6`

---

## 评审请回答

1. **D1 枚举 vs 双 bool**:枚举已定,有反对意见吗?
2. **D4 dual 并发**:用 errgroup 起 2 个 goroutine,v4 retry sleep 不阻塞 v6 —— 接受?
3. **D5 LastSync 拆分**:`V4IP/V4Error/V6IP/V6Error` 独立字段,顶层 `Success = 任一 family 成功过` —— 是否过宽?要不要改成"严格 OR:至少一个 enabled family 成功"?
4. **D6 状态文件迁移**:首次启动自动把裸 `true` 重写成 `enabled=true\nfamily=dual` —— OK 还是保守起见写 `family=dual` 进默认值?
5. **D10 host 网络约束**:release-notes 强提示还是硬错误(启动检测到 bridge 网络 + family 含 v4 → WARN)?
6. **dual 模式下 `LastSync.Time` 取哪个**:v4 / v6 最近一次 tick 时间中的较大值,还是整轮 tick 时间?—— 倾向后者(语义清晰)
