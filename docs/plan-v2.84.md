# v2-84 实施计划

> 配套文档:[docs/proposal-v2.84.md](file:///opt/ikev2-panel-v2-main/docs/proposal-v2.84.md)
> 状态:已过 3-agent 评审(proposal),8 个 P0 已修进设计,准备开工
> 预估代码量:~280 行净增(含测试),4 个文件改、3 个文件加

---

## 任务拆分(按依赖顺序)

### T1. Aliyun 客户端 Type 参数化 【基础,最先做】

**文件**:[internal/dns/aliyun.go](file:///opt/ikev2-panel-v2-main/internal/dns/aliyun.go)

| 步骤 | 内容 |
|------|------|
| 1.1 | `FindAAAARecord` 改名为 `FindRecord`,签名加 `recordType string` 参数,L88/102/112 三处硬编码 "AAAA" 替为参数 |
| 1.2 | `UpdateRecordValue` 签名加 `recordType string`,L122 硬编码替为参数 |
| 1.3 | 新增 `RecordTypeA = "A"` / `RecordTypeAAAA = "AAAA"` 常量 |
| 1.4 | `FindRecord` / `UpdateRecordValue` 入参校验:`recordType` ∈ {"A", "AAAA"},否则返回 error |
| 1.5 | 新增测试 `internal/dns/aliyun_test.go`:白名单拒绝非法 Type |

**验收**:`go vet ./internal/dns/...` 通过;现有调用方(sync.go L281)编译失败需要同时修(在 T3 一起)。

---

### T2. swanctl 新增 IPv4 探测函数 【基础,与 T1 并行】

**新建文件**:[internal/swanctl/ipv4watch.go](file:///opt/ikev2-panel-v2-main/internal/swanctl/ipv4watch.go)

| 步骤 | 内容 |
|------|------|
| 2.1 | 新增 `isGlobalV4(ip net.IP) bool`,按 D2 列表过滤:0.0.0.0、loopback、link-local、RFC1918、CGNAT、multicast |
| 2.2 | 新增 `DetectGlobalV4(iface, target string) (string, error)`:`net.Dialer{Timeout: 3*time.Second}.Dial("udp", target+":80")` 拿 `LocalAddr()` 解析 IP |
| 2.3 | iface 非空时:`net.InterfaceByName(iface)` 拿该接口地址列表,跟 dial 出的 IP 比对;不匹配 → return error |
| 2.4 | `target` 默认 `8.8.8.8`,调用方可覆盖 |
| 2.5 | 测试 `internal/swanctl/ipv4watch_test.go`:注入 mock dialer,验证 5 个场景(无 iface / 指定 iface 匹配 / iface 不匹配 / 169.254 / 10.0.0.5 都返回 error) |

**验收**:`go test ./internal/swanctl/...` 通过;本地容器跑 `DetectGlobalV4("", "8.8.8.8")` 返回宿主机出口 IPv4;`DetectGlobalV4("", "127.0.0.1")` 返回 error(loopback 过滤)。

---

### T3. ddns.sync.go family-aware 改造 【核心,依赖 T1+T2】

**文件**:[internal/ddns/sync.go](file:///opt/ikev2-panel-v2-main/internal/ddns/sync.go)

| 步骤 | 内容 |
|------|------|
| 3.1 | `Config` 加 `Family string` 字段(v2-84),注释说明取值 v4/v6/dual |
| 3.2 | `Sync` 结构:`detectV4 func(iface, target string) (string, error)` + `SetDetectV4()` + `detectTarget string` |
| 3.3 | **新增 `LastSync` 结构化字段**:`V4IP/V4Error/V6IP/V6Error` 独立,顶层 `Success` 语义定义(见 D5) |
| 3.4 | **新增 per-type 节流**:`map[string]time.Time`(key="A"/"AAAA")替代表面 `lastSyncTime` |
| 3.5 | **抽 `upsertRecord(recordType, currentIP)` 函数**(从 L268-322 提取):FindRecord → 0 条/多条 WARN skip → 1 条比对 → UpdateRecordValue retry 3 次 → 识别 RAM 权限错误不重试 |
| 3.6 | **新 sync() 函数体**:errgroup 并发跑 v4 / v6(各自独立 upsertRecord),Wait 后聚合 LastSync |
| 3.7 | `recordFailure` / `recordSuccess` 拆 v4 / v6 版本;`LAST_DDNS_FAILED` 写盘条件改为"v4+v6 都尝试过且都失败" |
| 3.8 | 测试 `internal/ddns/sync_v4_test.go`:用 mock client + mock detector,跑 v4 / v6 / dual / dual-v4fail / dual-v6fail 五组 case |

**验收**:`go test ./internal/ddns/...` 通过;v6-only 模式(不回填 family)行为跟 v2-83 一致(回归测试保证)。

---

### T4. 状态文件 atomic + 迁移 【T3 并行,新文件】

**新建文件**:[internal/ddns/statefile.go](file:///opt/ikev2-panel-v2-main/internal/ddns/statefile.go)

| 步骤 | 内容 |
|------|------|
| 4.1 | `parseStateFile(path string) (enabled bool, family string, err error)` |
| 4.2 | 兼容三种格式:整文件=="true"/"false"(v2-83 旧格式)、INI-style key=value、空文件 |
| 4.3 | `writeStateFile(path string, enabled bool, family string) error`:temp file + `os.Rename`(atomic)|
| 4.4 | 首次启动检测到旧格式 → 自动重写为新格式(用 writeStateFile) |
| 4.5 | 抽出 `atomicWriteFile(path string, data []byte, perm fs.FileMode) error`(跟 swanctl.AtomicWriteFile 同模式,直接 import 复用) |
| 4.6 | 测试 `internal/ddns/statefile_test.go`:三种格式读取、写入回读 round-trip、并发写(应该不会 truncate) |

**验收**:v2-83 用户的裸 `true` 文件 → 启动后变 `enabled=true\nfamily=dual`;模拟 SIGKILL 写中途 → 下次启动仍可读旧值(不损坏)。

---

### T5. config 加 DDNSFamily + DDNSProbeTarget 字段 【T1~T4 后做】

**文件**:[internal/config/config.go](file:///opt/ikev2-panel-v2-main/internal/config/config.go)

| 步骤 | 内容 |
|------|------|
| 5.1 | `Config` 加 `DDNSFamily string` + `DDNSProbeTarget string` 字段 |
| 5.2 | `Load()` 里 `os.Getenv("IKEV2_DDNS_FAMILY")` + `os.Getenv("IKEV2_DDNS_PROBE_TARGET")` |
| 5.3 | `parseFamily(s string) (string, *slog.Logger)`:合法值返回原值;空 → "dual";非法 → "dual" + WARN 日志 |
| 5.4 | `parseProbeTarget(s string) string`:空 → "8.8.8.8";非法 IP → "8.8.8.8" + WARN |
| 5.5 | 测试 `internal/config/config_test.go`:合法/非法/空 family;合法/非法/空 target |

---

### T6. main.go 装配放宽 【T5 后做】

**文件**:[cmd/ikev2-panel/main.go](file:///opt/ikev2-panel-v2-main/cmd/ikev2-panel/main.go)

| 步骤 | 内容 |
|------|------|
| 6.1 | L303 `if cfg.IPv6Only` 改为 `if cfg.DDNSEnabled` |
| 6.2 | `ddns.Config{...}` 加 `Family: cfg.DDNSFamily` 字段 |
| 6.3 | `ddnsSync.SetDetectV6(...)` 后加 `ddnsSync.SetDetectV4(...)` + 注入 `cfg.DDNSProbeTarget` |
| 6.4 | 注释更新:ddnsSync 不再依赖 cfg.IPv6Only |

---

### T7. 面板 UI + handlers 【T6 后做】

**文件**:[internal/web/handlers_ddns.go](file:///opt/ikev2-panel-v2-main/internal/web/handlers_ddns.go) + [internal/web/server.go](file:///opt/ikev2-panel-v2-main/internal/web/server.go) + `web/templates/home_content.html`

| 步骤 | 内容 |
|------|------|
| 7.1 | `DDNSStatusResp` 加 `Family string` + `V4IP/V4Error/V6IP/V6Error` 字段 |
| 7.2 | `handleDDNSStatus` 从 `s.DDNSSync.Family()` 填 `resp.Family`(新增方法) |
| 7.3 | 新增 `handleDDNSFamily(w, r)`:ParseForm → `r.PostFormValue("family")` → **双重白名单校验**(handler + SetFamily)→ `s.DDNSSync.SetFamily(...)`(atomic 写) |
| 7.4 | `Sync` 加 `SetFamily(string) error` / `Family() string` 方法(走 statefile.parseStateFile + writeStateFile)|
| 7.5 | server.go L112 后加 `mux.Handle("POST /api/ddns/family", protectPOST(...))` |
| 7.6 | home_content.html DDNS 卡片:family radio + v4/v6 状态独立显示 |
| 7.7 | 测试 `internal/web/handlers_ddns_family_test.go`:GET 含 family 字段;POST family 合法/非法值;protectPOST 校验 |

---

### T8. 文档 + 配置

| 文件 | 操作 |
|------|------|
| `docker-compose.yml` | 加 `IKEV2_DDNS_FAMILY: ${IKEV2_DDNS_FAMILY:-dual}` + 可选 `IKEV2_DDNS_PROBE_TARGET` |
| `.env.example` | family + probe_target 说明 + IPv4-only 场景示例 |
| `docs/design.md` | §19.8 v2-84 设计节(family 枚举、探测、面板 UI) + §21 路线图加 v2-84 行 |
| `docs/release-notes-v2.84.md` | 新建:概要 / 升级步骤 / dual 默认值行为变更 / host 网络约束 / 兼容性矩阵 |
| `README.md` | DDNS 一节加 family 说明 |

---

## 验收测试

### 单元测试(必跑)

```bash
go test ./internal/dns/...        # T1
go test ./internal/swanctl/...     # T2
go test ./internal/ddns/...        # T3 + T4
go test ./internal/config/...      # T5
go test ./internal/web/...         # T7
go vet ./...
```

### 集成测试(手测 checklist)

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

### 回归测试

- 不设 `IKEV2_DDNS_FAMILY` 升级 → 行为 = dual(比 v2-83 多同步 A)
- v2-83 用户想保持 v6-only → 设 `IKEV2_DDNS_FAMILY=v6`,行为 = v2-83 完全一致

---

## 风险与缓解(评审后)

| 风险 | 缓解 |
|------|------|
| dual 默认值让 v2-83 用户意外同步 A 记录 | release notes 强调;IPv4 探测失败时不计入失败(不写 LAST_DDNS_FAILED) |
| bridge 网络下 IPv4 探测返回容器 IP | 强制 host 网络约束,release notes 强提示 |
| dual 下 v4 retry sleep 阻塞 v6 | errgroup 并发 + per-type 节流 |
| 状态文件 race | atomic write + parse 兼容旧格式 |
| 阿里云 RAM 权限错误无限重试 | 识别错误码 → WARN 一次不重试 |
| `net.Dial` 在受限网络长时间阻塞 | 3 秒 timeout + 可配置探测目标 |

---

## 不在 v2-84 范围(明确 deferred)

来自 v2-83 复盘,用户确认"往后放":

1. **登录端点限速**(design §4.1 声称"5 次失败锁 5 分钟"未实现)→ P2
2. **IPv6 限速**(`tc u32` 不支持 IPv6,改 `tc flower`)→ P2
3. **backup.sh** 设计文档 §2.1 列入但实际不需要 → P2(设计文档收口)

---

## 开工顺序

```
T1 ─┐
T2 ─┤─→ T3 ─→ T5 ─→ T6 ─→ T7 ─→ T8
T4 ─┘
```

预计总工作:~280 行代码 + ~200 行测试 + ~150 行文档。

---

## 完成定义(Definition of Done)

- [ ] T1~T8 全部完成
- [ ] `go test ./...` 全绿
- [ ] `go vet ./...` 无 warning
- [ ] 集成测试 10 个场景全部通过
- [ ] docs/release-notes-v2.84.md 发布
- [ ] design.md §19.8 + §21 同步
- [ ] 升级说明明确写出 "dual 默认值" 行为变更 + host 网络约束
- [ ] 8 个 P0 评审问题全部修复(可对照 proposal §评审)
