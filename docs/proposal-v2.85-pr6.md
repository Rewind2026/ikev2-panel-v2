# Proposal v2.85-PR6 — DDNS throttle 持久化 + stopCh sync.Once + collector REKEYING

> **类型**:PR 级提案(对应 v2.85 release 第 6 个 PR)
> **目标**:三个 Correctness MED 一起修 — DDNS throttle 跨重启 + stopCh 关闭协议 + collector REKEYING SA 漏算流量
> **关联审计**:
> - [audit-2026-09-correctness.md §Q5-01](file:///opt/ikev2-panel-v2-main/docs/audit-2026-09-correctness.md)(MED — DDNS throttle 不持久化)
> - [audit-2026-09-correctness.md §Q1-02](file:///opt/ikev2-panel-v2-main/docs/audit-2026-09-correctness.md)(MED — stopCh close 协议不完整)
> - [audit-2026-09-correctness.md §Q9-02](file:///opt/ikev2-panel-v2-main/docs/audit-2026-09-correctness.md)(MED — collector 跳过 REKEYING)
> **工作量**:**0.4d**(0.2 + 0.1 + 0.1)
> **作者**:自查

---

## 背景

Phase 1-4 审计留下 3 个 Correctness MED,主题不同但都在"已有正确逻辑,缺最后一公里"区:

| Issue | 症状 | 现状 | 后果 |
|-------|------|------|------|
| Q5-01 | `lastSyncTime` 纯内存,重启后从 0 起 | [sync.go:166](file:///opt/ikev2-panel-v2-main/internal/ddns/sync.go#L166) `lastSyncTime: make(map[string]time.Time)` | 用户重启容器时若 IP 恰好变了 → 1s 内又 upsert → 撞 alidns 30 QPS 限流 → DDNS 失败标志触发 |
| Q1-02 | `Stop()` 用 `select default` 防 double close | [sync.go:305-312](file:///opt/ikev2-panel-v2-main/internal/ddns/sync.go#L305-L312) | 测试场景复用 `Sync` 实例时 stopCh 残留 closed 状态;虽 main.go 不会触发,但代码语义不干净 |
| Q9-02 | collector 只过 ESTABLISHED | [collector.go:116](file:///opt/ikev2-panel-v2-main/internal/limit/collector.go#L116) `if sa.IkeState != "ESTABLISHED" { continue }` | charon 22h 触发 rekeying 时(1-2 分钟)流量不计入 bytes_in_total — 数据精度缺口 |

**为什么打包成一个 PR**:
- 三个 issue 都是"小改动、零行为破坏"型,review 成本低
- 都集中在 v2.85 release 的 Correctness 主题下,独立 PR 反而增加 merge overhead
- 0.4d 单 PR 可一次到位,后续 PR(PR-7 IPv6 tc)继续往前推

---

## 目标

1. **DDNS throttle 跨重启**:`lastSyncTime[rt]` 持久化到 `/etc/ikev2/ddns.conf`,启动时回填;throttle 窗口仍按 60s
2. **stopCh 关闭协议**:`Sync.Stop()` 改 `sync.Once` 保证 close 单次;语义干净、可测试
3. **collector 包含 REKEYING**:`sa.IkeState in {ESTABLISHED, REKEYING}` 视为活跃;REKEYED 仍跳过(已结算)

---

## 不在范围(明确不做)

- ❌ 不改 throttle 默认值(60s 不变),只是把时间戳持久化
- ❌ 不改 DDNS 节流判断逻辑(只在 tick 入口检查的逻辑保留)
- ❌ 不动 collector 的 first-sample / counter-wrap 分支(只扩展状态白名单)
- ❌ 不动 `Sync.Run` 主循环结构(只是 Stop 改 Once,Run 内 `case <-s.stopCh` 不变)
- ❌ 不改 v2-83 老 statefile 兼容(只扩展 INI 解析支持新 key)
- ❌ 不动 strongSwan / charon / swanctl 二进制
- ❌ 不引入新依赖
- ❌ **改 sync.Once vs ctx.Done 的争论 → 写在"设计"里,不阻塞 PR**

---

## 设计

### 设计 1:DDNS throttle 时间戳持久化(Q5-01)

#### 1.1 stateRecord 扩展

```go
// internal/ddns/statefile.go(改动)

// stateRecord v2-85:throttle 时间戳也持久化(per-type)。
//
// 注意:这是 unix 时间戳(秒),不是 RFC3339 — 因为 INI 文件里数字更稳定
// 解析(不会因 timezone / locale 引入歧义);持久化/读取时用 time.Unix(_, 0) 转回 time.Time。
type stateRecord struct {
    Enabled     bool
    Family      string
    LastSyncA     int64 // unix 秒;0 = 从未同步
    LastSyncAAAA  int64 // 同上
}
```

#### 1.2 parseStateFile 扩展

```go
// internal/ddns/statefile.go parseStateFile 内 switch 增加两个 key:

switch key {
case "enabled":
    rec.Enabled = (val == "true" || val == "1" || val == "on")
    hasEnabled = true
case "family":
    if validFamily(val) {
        rec.Family = val
        hasFamily = true
    }
case "last_sync_a":
    // 解析失败 → 忽略,默认 0(等价于"没节流历史")
    if n, perr := strconv.ParseInt(val, 10, 64); perr == nil && n >= 0 {
        rec.LastSyncA = n
    }
case "last_sync_aaaa":
    if n, perr := strconv.ParseInt(val, 10, 64); perr == nil && n >= 0 {
        rec.LastSyncAAAA = n
    }
}
```

#### 1.3 writeStateFile 扩展

```go
// internal/ddns/statefile.go writeStateFile 内序列化:

if rec.LastSyncA > 0 {
    fmt.Fprintf(&b, "last_sync_a=%d\n", rec.LastSyncA)
}
if rec.LastSyncAAAA > 0 {
    fmt.Fprintf(&b, "last_sync_aaaa=%d\n", rec.LastSyncAAAA)
}
```

#### 1.4 NewSync 启动回填

```go
// internal/ddns/sync.go NewSync 末尾(在 s 构造完之后):

if rec, readErr := parseStateFile(s.cfg.StateFile, defaultFamily); readErr == nil {
    s.cfg.Enabled = rec.Enabled
    s.cfg.Family = rec.Family
    // 回填 throttle 时间戳(unix 秒 → time.Time)
    if rec.LastSyncA > 0 {
        s.lastSyncTime[dns.RecordTypeA] = time.Unix(rec.LastSyncA, 0)
    }
    if rec.LastSyncAAAA > 0 {
        s.lastSyncTime[dns.RecordTypeAAAA] = time.Unix(rec.LastSyncAAAA, 0)
    }
}
```

**关键点**:
- `lastSyncTime` 是 map,初始 `make(map[string]time.Time)` 保留;**只在 key 存在时回填**
- map 未初始化的 key 读出来是 zero value,throttle 检查 `!s.lastSyncTime[rt].IsZero()` 已能区分"从未同步"和"已同步过"

#### 1.5 节流更新后落盘

```go
// internal/ddns/sync.go runFamily 内(改 2 处):

// 旧:s.lastSyncTime[rt] = time.Now()
// 新:s.setLastSyncAndPersist(rt, time.Now())

// 新方法:
func (s *Sync) setLastSyncAndPersist(rt string, t time.Time) {
    s.mu.Lock()
    s.lastSyncTime[rt] = t
    enabled := s.cfg.Enabled
    family := s.cfg.Family
    var lastA, lastAAAA int64
    if v := s.lastSyncTime[dns.RecordTypeA]; !v.IsZero() {
        lastA = v.Unix()
    }
    if v := s.lastSyncTime[dns.RecordTypeAAAA]; !v.IsZero() {
        lastAAAA = v.Unix()
    }
    s.mu.Unlock()

    // 落盘(失败仅 warn,不影响本次 upsert 结果)
    rec := stateRecord{Enabled: enabled, Family: family,
        LastSyncA: lastA, LastSyncAAAA: lastAAAA}
    if err := writeStateFile(s.cfg.StateFile, rec); err != nil {
        s.cfg.Logger.Warn("ddns: persist throttle time failed",
            "rt", rt, "err", err)
    }
}
```

**为什么用新方法**:
- 单写入口,避免后续 tick 内多个 goroutine 重复 persist
- 锁内只收集数据,锁外做 IO(减少持锁时间)
- `writeStateFile` 走 `swanctl.AtomicWriteFile`(已存在),不需要新代码

#### 1.6 节流判断逻辑不变

`tick()` 入口的 throttle 检查 [sync.go:351-357](file:///opt/ikev2-panel-v2-main/internal/ddns/sync.go#L351-L357) **完全不动**:
- `now.Sub(s.lastSyncTime[rt]) < s.cfg.Throttle` 判断仍然工作
- 区别只在启动时 `s.lastSyncTime[rt]` 从 0 变成持久化的 unix 时间戳

#### 1.7 持久化频率权衡

| 方案 | 频率 | 风险 |
|------|------|------|
| 每次 upsert 成功都写盘(本方案) | 高 | 写多(每秒级 upsert 才会触发);但 statefile 很小 + atomic write 安全 |
| 后台 ticker 周期落盘(60s) | 低 | 重启若发生在两次落盘之间 → 仍丢 0-60s 节流历史 |

**选每次 upsert 写**:DDNS upsert 频率本身很低(60s 节流,IP 变了才 upsert),实际写盘次数极少;换来的是**精确的 throttle 持久化**(重启后窗口无缝)。

---

### 设计 2:`Sync.Stop()` 改 sync.Once(Q1-02)

#### 2.1 方案对比(写到 proposal,确定选择)

| 方案 | 优点 | 缺点 |
|------|------|------|
| A: `sync.Once` + stopCh 保留 | 改动小(只动 Stop 和字段);保留外部"显式停"的语义 | 多一个字段 |
| B: 删除 stopCh,只用 ctx.Done() | 代码更简单(少一个字段);context 标准做法 | main.go 需要传 cancel func;**外部"显式停"的语义消失**(caller 必须持有 ctx 才能停) |

**选择:方案 A(`sync.Once`)**。
- 改动最小(只动 Sync 结构体 + Stop 函数),不影响 Run 循环
- 保留 `Stop()` 作为公共方法(main.go 可能在 signal handler 中调,不一定等 ctx)
- test 场景可以反复 NewSync 而不踩到 stopCh 残留状态

#### 2.2 代码改动

```go
// internal/ddns/sync.go Sync 结构体(改 1 行):

type Sync struct {
    cfg Config

    mu           sync.Mutex
    lastSync     LastSync
    lastSyncTime map[string]time.Time
    running      bool
    stopCh       chan struct{}
    stopOnce     sync.Once  // v2-85:Stop 只关一次 stopCh
    client       *dns.AliyunClient

    detectV6 func(iface string) (string, error)
    detectV4 func(iface, target string) (string, error)
}

// NewSync 初始化(改 1 行):
s := &Sync{
    cfg: cfg,
    client:       nil,
    detectV6:     nil,
    detectV4:     nil,
    stopCh:       make(chan struct{}),
    stopOnce:     sync.Once{},   // v2-85:zero value 即可,无需显式构造
    lastSyncTime: make(map[string]time.Time),
}

// Stop v2-85:改 sync.Once,语义干净 + 可复用实例。
func (s *Sync) Stop() {
    s.stopOnce.Do(func() {
        close(s.stopCh)
    })
}
```

#### 2.3 Run 循环不变

```go
// Run 循环不动:case <-s.stopCh: 仍然能 fire(Once 关后,channel 永久 closed)
for {
    select {
    case <-ctx.Done():
        ...
    case <-s.stopCh:
        ...
    case <-ticker.C:
        s.tick()
    }
}
```

#### 2.4 副作用分析

- **首次 Stop**:Once 执行 close(stopCh) → Run 在下轮 select 收到 → return nil ✓
- **第二次 Stop**:Once.Do 是 no-op,不重复 close ✓
- **Stop 后又 NewSync**:旧实例 stopCh 已关,但新实例是新的 stopCh + 新的 stopOnce → 完全独立 ✓
- **Stop 与 ctx.Cancel 并发**:两者谁先关都 OK;Run 只 select 一次,先到的赢

---

### 设计 3:collector 包含 REKEYING(Q9-02)

#### 3.1 活跃 SA 状态白名单

参考 strongSwan VICI 协议:

| 状态 | 含义 | 是否计入流量 |
|------|------|--------------|
| `ESTABLISHED` | 正常活跃 SA | ✅ 计入 |
| `REKEYING` | 正在重新协商密钥,旧 SA 仍传输流量 | ✅ **本次新增** |
| `REKEYED` | 新 SA 已建立,旧 SA 准备删除(流量已切到新 SA) | ❌ 跳过(避免重复计) |
| `CONNECTING` | 正在协商,未通过 auth | ❌ 跳过 |
| `DELETING` / `INSTALLED` | 中间态 | ❌ 跳过 |

#### 3.2 代码改动

```go
// internal/limit/collector.go collect 内(改 1 处):

// activeStates v2-85:活跃 SA 状态白名单。
// REKEYING 表示旧 SA 仍在传输流量,必须计入;
// REKEYED 表示已被新 SA 取代,旧 SA 流量已结算到 prevBytes,跳过避免重复。
var activeStates = map[string]struct{}{
    "ESTABLISHED": {},
    "REKEYING":    {},
}

func (c *Collector) collect(ctx context.Context) {
    ...
    for _, sa := range sas {
        if _, ok := activeStates[sa.IkeState]; !ok {
            continue
        }
        // (后面逻辑不变)
    }
}
```

**关键设计点**:
- 用 map 白名单而非 `if state != "ESTABLISHED" && state != "REKEYING"` — 后续加新状态(如有)只改一处
- REKEYED **不**进白名单:此时旧 SA 已被新 SA 取代,流量切到新 SA(uniqueid 不变,counter 继续),但 collector 上次 list-sas 时已记过旧 SA 的 prev,REKEYED 这次再加一次会双重计入
- **唯一的边界**:若 collector 在 REKEYING → REKEYED 切换之间挂了重启,重启后 REKEYED SA 不再被 track(prevBytes 丢);新 SA(uniqueid 可能变了)首次采样 — first-sample 把 current 全计入 — 正确

#### 3.3 delta 计算逻辑不变

```go
// for childName, child := range sa.Children {...} 内逻辑不动:
//   - !exists → first-sample
//   - currentIn < prev[0] → counter reset/wrap
//   - default → current - prev
```

#### 3.4 测试覆盖

加 3 个 test case 到 `internal/limit/collector_test.go`:

```go
// TestCollectorIncludesRekeying 验证：REKEYING 状态 SA 的 child 流量被计入。
func TestCollectorIncludesRekeying(t *testing.T) {
    ms := &mockSwanctl{
        saList: []swanctl.SA{
            {UniqueID: "1", IkeState: "ESTABLISHED", RemoteID: "alice", Children: map[string]swanctl.ChildSA{
                "c1": {BytesIn: 100, BytesOut: 50},
            }},
            {UniqueID: "2", IkeState: "REKEYING", RemoteID: "bob", Children: map[string]swanctl.ChildSA{
                "c1": {BytesIn: 200, BytesOut: 80},
            }},
        },
    }
    mst := &mockStore{bytes: map[string][2]int64{}}
    logger := slog.New(slog.NewTextHandler(io.Discard, nil))
    c := NewCollector(ms, mst, logger)

    c.CollectForTest(context.Background())

    if got := mst.bytes["alice"]; got != [2]int64{100, 50} {
        t.Errorf("alice established: got %v want [100 50]", got)
    }
    if got := mst.bytes["bob"]; got != [2]int64{200, 80} {
        t.Errorf("bob rekeying (should be counted): got %v want [200 80]", got)
    }
}

// TestCollectorSkipsRekeyed 验证：REKEYED 状态 SA 被跳过(避免双重计入)。
func TestCollectorSkipsRekeyed(t *testing.T) {
    ms := &mockSwanctl{
        saList: []swanctl.SA{
            {UniqueID: "1", IkeState: "REKEYED", RemoteID: "alice", Children: map[string]swanctl.ChildSA{
                "c1": {BytesIn: 999, BytesOut: 999},
            }},
        },
    }
    mst := &mockStore{bytes: map[string][2]int64{}}
    logger := slog.New(slog.NewTextHandler(io.Discard, nil))
    c := NewCollector(ms, mst, logger)

    c.CollectForTest(context.Background())

    if _, ok := mst.bytes["alice"]; ok {
        t.Error("REKEYED alice should NOT be counted (流量已切到新 SA)")
    }
    if len(c.prevBytes) != 0 {
        t.Errorf("REKEYED SA should not be tracked, got %d entries", len(c.prevBytes))
    }
}

// TestCollectorMixedStates 验证：一次 list-sas 中混合各种状态,只有活跃的计入。
func TestCollectorMixedStates(t *testing.T) {
    ms := &mockSwanctl{
        saList: []swanctl.SA{
            {UniqueID: "1", IkeState: "ESTABLISHED", RemoteID: "alice", Children: map[string]swanctl.ChildSA{
                "c1": {BytesIn: 100, BytesOut: 50},
            }},
            {UniqueID: "2", IkeState: "REKEYING", RemoteID: "bob", Children: map[string]swanctl.ChildSA{
                "c1": {BytesIn: 200, BytesOut: 80},
            }},
            {UniqueID: "3", IkeState: "REKEYED", RemoteID: "carol", Children: map[string]swanctl.ChildSA{
                "c1": {BytesIn: 999, BytesOut: 999},
            }},
            {UniqueID: "4", IkeState: "CONNECTING", RemoteID: "dave"},
            {UniqueID: "5", IkeState: "DELETING", RemoteID: "eve", Children: map[string]swanctl.ChildSA{
                "c1": {BytesIn: 9999, BytesOut: 9999},
            }},
        },
    }
    mst := &mockStore{bytes: map[string][2]int64{}}
    logger := slog.New(slog.NewTextHandler(io.Discard, nil))
    c := NewCollector(ms, mst, logger)

    c.CollectForTest(context.Background())

    // 期望：alice + bob 计入,carol/dave/eve 跳过
    if _, ok := mst.bytes["alice"]; !ok {
        t.Error("alice ESTABLISHED should be counted")
    }
    if _, ok := mst.bytes["bob"]; !ok {
        t.Error("bob REKEYING should be counted")
    }
    for _, skip := range []string{"carol", "dave", "eve"} {
        if _, ok := mst.bytes[skip]; ok {
            t.Errorf("%s should NOT be counted", skip)
        }
    }
}
```

---

## 改动文件清单

| 文件 | 改动 | 行数估算 |
|------|------|---------|
| [internal/ddns/statefile.go](file:///opt/ikev2-panel-v2-main/internal/ddns/statefile.go) | stateRecord 加 LastSyncA/AAAA + parse 解析 2 个 key + write 序列化 2 行 | +30 行 |
| [internal/ddns/sync.go](file:///opt/ikev2-panel-v2-main/internal/ddns/sync.go) | NewSync 回填 lastSyncTime + Sync 结构体加 stopOnce + Stop 改 Once + 新增 setLastSyncAndPersist + runFamily 调用替换 | +25 / -5 行 |
| [internal/limit/collector.go](file:///opt/ikev2-panel-v2-main/internal/limit/collector.go) | 新增 activeStates 白名单 + collect 内判断改 map lookup | +8 / -3 行 |
| [internal/limit/collector_test.go](file:///opt/ikev2-panel-v2-main/internal/limit/collector_test.go) | 加 3 个 test case | +70 行 |

**总计**:**+128 / -8 行,review 简单,无新依赖**。

---

## 测试方案

### 自动化测试

```bash
# 1. 全包单元测试(确保现有 13 包不挂)
go test ./...

# 2. race detector(Dockerfile runner-test target,PR-1 已铺路)
docker build --target runner-test -t ikev2-panel-race:test .
docker run --rm ikev2-panel-race:test
# 期望:全 PASS,无 race(改 Once 后 stopCh close 协议干净)

# 3. DDNS throttle 持久化专项(integration-like,file-based)
go test -run TestDDNS -v ./internal/ddns/
# 重点：
#   - TestSync_PersistThrottle:tick 一次 → 读 statefile → last_sync_a 有值
#   - TestSync_ReloadThrottle:写 statefile 含 last_sync_a → NewSync → lastSyncTime map 有值
#   - TestSync_BackwardCompatOldFormat:v2-83 裸 "true" 文件 → NewSync 不 panic,默认 throttle=0
```

### collector REKEYING 专项

```bash
go test -v -run "TestCollector(.*Rekey|.*Rekey|.*MixedStates)" ./internal/limit/
# 期望：3 个新 test 全 PASS,覆盖 REKEYING 计入 / REKEYED 跳过 / 混合状态
```

### sync.Once 专项

```bash
go test -v -run TestSync_Stop ./internal/ddns/
# 重点：
#   - TestSync_StopOnce:连续调 Stop() N 次不 panic
#   - TestSync_ReuseInstance:NewSync → Stop → NewSync(复用 struct?) → 各自独立
```

### 手动验证(可选,本地有 docker 时)

```bash
# 1. 启动容器,等 1 分钟让 DDNS upsert 一次
docker compose up -d
sleep 90

# 2. 检查 statefile 含 last_sync_a
docker compose exec ikev2-panel cat /etc/ikev2/ddns.conf
# 期望看到:
#   # ikev2-panel DDNS runtime state (...)
#   enabled=true
#   family=dual
#   last_sync_a=1726700000
#   last_sync_aaaa=1726700000

# 3. 立即重启,确认 throttle 窗口生效(下一个 tick 不应立即 upsert)
docker compose restart ikev2-panel
sleep 5
docker compose exec ikev2-panel cat /etc/ikev2/ddns.conf
# 时间戳应跟重启前一致(throttle 60s 内不会重新 upsert)
```

---

## 风险评估

| 风险 | 概率 | 影响 | 缓解 |
|------|------|------|------|
| statefile 写盘频率过高(每次 upsert 都写) | 极低 | 写多 — 但 DDNS upsert 受 60s 节流,实际每分钟最多写 2 次(A+AAAA) | 已 atomic write 安全;若真出问题降级到 ticker 周期落盘 |
| 旧 v2-83 用户 statefile 是裸 "true" | 中 | migrateStateFileIfNeeded 自动迁移;但迁移不持久化 throttle | 迁移后第一次 tick 后再持久化(隐式),无功能问题 |
| last_sync_a 解析失败(人手编辑 statefile) | 低 | 静默忽略,fallback 0 = 无节流历史 | parseStateFile 已有 "非法 → 忽略" 模式 |
| sync.Once 改后外部 caller 依赖 Stop 的旧行为 | 极低 | main.go 只在 exit 时调一次,Once 不影响 | main.go 用法不变,review 时确认 |
| REKEYING SA 被计入导致双计(REKEYING + REKEYED 同 uniqueid) | 极低 | collector 只在 list-sas 看到的状态中选;同一 SA 一次 list-sas 只会以一种状态出现 | REKEYED **不**进白名单,即使同时出现也只算 REKEYING 那次 |
| collector 测试加 3 个 case 拖慢 CI | 极低 | 跑得快(< 100ms 三个 case) | 复用 mock 框架 |

---

## 不破坏兼容性的承诺

### DDNS throttle 持久化(Q5-01)

- **现有 v2-84 用户升级**:statefile 是 v2-84 INI 格式,新增 `last_sync_a` / `last_sync_aaaa` 两个 key — 老文件没有这两个 key → parse 时默认 0 → 等价于"无节流历史",**行为 = 升级前**。
- **v2-83 老用户**:statefile 是裸 "true"/"false",migrateStateFileIfNeeded 先迁移成 INI;首次 tick 后才会持久化 throttle 时间戳。**迁移不破坏现有行为**。
- **手工编辑 statefile**:不影响 — 新 key 缺失 → 默认 0;非法值(parse 失败)→ 静默忽略。
- **运行环境**:不依赖强 Sown / charon / 网络;只读写本地文件。

### stopCh sync.Once(Q1-02)

- **main.go 用法不变**:只调一次 Stop(),Once 行为对外不可见。
- **测试场景**:可以放心复用 Sync 实例 — 旧实例 stopCh 已关 + stopOnce 已 fired;新 NewSync 拿到全新的 stopCh + 新的 stopOnce。
- **未来 caller 误用**:无影响 — Once.Do 保证 close 只发生一次,无 panic。

### collector REKEYING(Q9-02)

- **现有 v2-84 用户升级**:collector 行为更"宽"(包含 REKEYING);正常用户流量统计**更准确**,不会丢数据。
- **极端情况**:用户 SA 长时间 REKEYING(>5 分钟)→ 持续计入,正确。
- **database schema 不变**:IncrementUserBytes 签名不变。
- **dashboard / 报表**:无影响 — 数据库字段不变。

---

## 工作量分解

| 步骤 | 时间 |
|------|------|
| statefile.go 改(stateRecord + parse + write) | 0.05d |
| sync.go 改(NewSync 回填 + Sync.stopOnce + setLastSyncAndPersist + runFamily 调用替换) | 0.13d |
| collector.go 改(activeStates 白名单 + 判断改 lookup) | 0.03d |
| collector_test.go 加 3 个 case | 0.07d |
| 跑测试(go test + go test -race + manual statefile verify) | 0.05d |
| commit + review | 0.02d |
| **合计** | **0.4d** |

---

## 实施 Checklist(执行时用)

```markdown
- [ ] statefile.go: stateRecord 增加 LastSyncA / LastSyncAAAA 字段
- [ ] statefile.go: parseStateFile 支持 last_sync_a / last_sync_aaaa key
- [ ] statefile.go: writeStateFile 序列化两个新 key(unix 秒)
- [ ] sync.go: Sync 结构体增加 stopOnce sync.Once
- [ ] sync.go: NewSync 增加 lastSyncTime 回填逻辑(parseStateFile 后 if rec.LastSyncA > 0 → time.Unix)
- [ ] sync.go: 新增 setLastSyncAndPersist 方法(锁内收集 + 锁外 writeStateFile)
- [ ] sync.go: runFamily 内 throttle 时间戳赋值改调 setLastSyncAndPersist
- [ ] sync.go: Stop 改 sync.Once.Do(close(stopCh))
- [ ] collector.go: 新增 activeStates map 白名单(ESTABLISHED + REKEYING)
- [ ] collector.go: collect 内 if sa.IkeState != "ESTABLISHED" 改 map lookup
- [ ] collector_test.go: 加 TestCollectorIncludesRekeying
- [ ] collector_test.go: 加 TestCollectorSkipsRekeyed
- [ ] collector_test.go: 加 TestCollectorMixedStates
- [ ] go test ./... PASS
- [ ] go test -race ./... PASS(走 Dockerfile runner-test target)
- [ ] manual: 启动容器 → 等 upsert → cat /etc/ikev2/ddns.conf 看到 last_sync_a=xxx
- [ ] manual: docker compose restart → cat statefile → 时间戳保留
- [ ] git commit "v2.85-PR6: DDNS throttle persist + stopCh sync.Once + collector REKEYING"
```

---

> 关联:
> - [audit-2026-09-phase2-4.md](file:///opt/ikev2-panel-v2-main/docs/audit-2026-09-phase2-4.md) §"Phase 2-4 合并 Top-10"
> - [audit-2026-09-summary.md](file:///opt/ikev2-panel-v2-main/docs/audit-2026-09-summary.md) Phase 1 Top-10
> - [release-notes-v2.85.md](file:///opt/ikev2-panel-v2-main/docs/release-notes-v2.85.md)(待 PR 合入后写)
