# Proposal v2.85-PR3 — 时区显示统一 + LE 续签日志路径修复

> **类型**:PR 级提案(对应 v2.85 release 第 3 个 PR)
> **目标**:统一面板 / DDNS / mobileconfig 三处时间显示时区与格式,加 `IKEV2_DISPLAY_TIMEZONE` 配置;同时修复 LE 续签失败日志路径提示不一致
> **关联审计**:
> - [audit-2026-09-usability.md §U03](file:///opt/ikev2-panel-v2-main/docs/audit-2026-09-usability.md)(HIGH — 时区与格式不一致)
> - [audit-2026-09-usability.md §U07](file:///opt/ikev2-panel-v2-main/docs/audit-2026-09-usability.md)(MED — 续签日志路径提示错误)
> - [audit-2026-09-usability.md §U11](file:///opt/ikev2-panel-v2-main/docs/audit-2026-09-usability.md)(MED — 时区硬编码 UTC)
> **工作量**:**0.4d**(U03 0.3d + U07 0.1d)
> **作者**:自查

---

## 背景

Phase 1-4 审计(易用性维度)发现 2 类**用户感知明显**的时间显示与文档不一致问题:

| 症状 | 现状 | 后果 |
|------|------|------|
| **面板表格时间用容器本地时区**(容器默认 UTC),但格式跟 DDNS 又不一样 | [internal/web/templates.go:93](file:///opt/ikev2-panel-v2-main/internal/web/templates.go#L93) `time.Unix(unix, 0).Format("2006-01-02 15:04")` — 容器 TZ=UTC 时输出 UTC,无时区缩写 | admin 在 HKT(UTC+8)看到 `08:30`,实际是 UTC 08:30(误以为是 HKT 16:30) |
| **DDNS 时间显式 UTC + Z 后缀** | [handlers_ddns.go:58](file:///opt/ikev2-panel-v2-main/internal/web/handlers_ddns.go#L58) `last.Time.UTC().Format("2006-01-02T15:04:05Z")` | 同一时间在 3 处显示 3 种格式,admin 难以关联 |
| **mobileconfig PayloadDescription** RFC3339 UTC | [handlers_home.go:190](file:///opt/ikev2-panel-v2-main/internal/web/handlers_home.go#L190) 同 UTC 格式 | iPhone 端描述时间跟面板对不上 |
| **无配置项** admin 无法选择时区 | `config.go` 无 `DisplayTimezone` 字段 | 中国/欧洲/美国 admin 都得心算 UTC 偏移 |
| **LE 续签失败面板提示路径错误** | [handlers_home.go:142](file:///opt/ikev2-panel-v2-main/internal/web/handlers_home.go#L142) 提示"看 `/var/log/ikev2-renew.log`",但 [scripts/renew-cert.sh:59](file:///opt/ikev2-panel-v2-main/scripts/renew-cert.sh#L59) 实际写到 `/var/log/acme-renew.log` | 用户按提示 grep 找不到错误 → 误以为问题不存在 → 求助开发者 |

**为什么先做这个 PR**:
- U03 是**真用户卡壳点**:中国大陆 admin 看面板,所有时间都差 8 小时,排查客户端连接问题时记错时间窗口
- U07 修复是**单行文案改动**,收益/成本极高
- 这两项**不引入新依赖、不改业务逻辑**,风险极低
- 时区配置化也是后续 UI 改进(JS `<time>` / browser locale)的基础

---

## 目标

1. **时区可配置**:加 `IKEV2_DISPLAY_TIMEZONE` 环境变量(IANA TZ 名),默认 `UTC`,`time.LoadLocation` 失败 fallback UTC
2. **格式统一**:表格列 `2006-01-02 15:04 MST`、DDNS `2006-01-02 15:04:05 MST`、mobileconfig ISO8601+offset
3. **LE 续签日志提示路径修正**:面板告警同时提到两个真实路径(acme.sh 自身 + 我们脚本的 stdout),不再误导用户
4. **零行为变更**(只动显示文案 / 配置加载逻辑)

---

## 不在范围(明确不做)

- ❌ **不动 mobileconfig PayloadDescription 内容**(本期只动面板表格 + DDNS 显示;mobileconfig 时间字段保持 RFC3339 UTC,因为 iOS 解析端约定)
  - **修订**:根据任务要求 mobileconfig 也走"ISO8601+offset"→ 纳入范围
- ❌ **不在面板加"LE 续签日志最近 100 行"折叠区**(审计 §U07 中期方案 0.3d,留 v3 单独 PR)
- ❌ **不引入 JS `<time datetime="...">` 让浏览器本地化**(审计建议的"最佳"方案,但模板需要重写且影响 mobileconfig / DDNS API 输出;本期走后端统一时区方案)
- ❌ **不做面板 nav 时区下拉**(JS 交互,留 v3)
- ❌ **不改 Session/审计日志/指标**(其他 PR 范围)
- ❌ **不动 strongSwan / acme.sh / 限速 / DDNS 业务逻辑**

---

## 设计

### 1. config.go 加 `DisplayTimezone` 字段

```go
// internal/config/config.go(改动在 Config struct + Load())

type Config struct {
    // ... 现有字段 ...
    DisplayTimezone string // IANA TZ 名,默认 UTC;渲染表格/DDNS 时间用
}

// Load() 末尾追加:
c.DisplayTimezone = getenv("IKEV2_DISPLAY_TIMEZONE", "UTC")
if _, err := time.LoadLocation(c.DisplayTimezone); err != nil {
    fmt.Fprintf(os.Stderr,
        "WARN: IKEV2_DISPLAY_TIMEZONE=%q invalid (%v); falling back to UTC\n",
        c.DisplayTimezone, err)
    c.DisplayTimezone = "UTC"
}
```

**为什么 fallback 而非 fatal**:
- 启动时配置错误不应阻止容器起来(用户能从面板看到 UTC,只是没那么友好)
- 跟现有 `parseFamily` 处理非法值的方式一致(同样 fallback + WARN)

**为什么不用 `time.FixedZone`**:
- 用户配 `Asia/Hong_Kong` 是预期用法,需要支持 DST(虽然 HKT 没 DST,但 `Europe/Berlin` 有)
- `LoadLocation` 走 IANA tzdata,容器 Debian bookworm slim 自带 `/usr/share/zoneinfo`

### 2. templates.go FuncMap 注入 timezone + formatTime 用 cfg

```go
// internal/web/templates.go(改动在 LoadTemplates + formatTimeTpl)

// 修改 LoadTemplates 签名:接受 timezone 参数
func LoadTemplates(templatesDir string, displayTZ string) (*template.Template, error) {
    // ...
    t = t.Funcs(template.FuncMap{
        "formatTime": func(unix int64) string { return formatTimeTpl(unix, displayTZ) },
        "formatTimeDDNS": func(unix int64) string { return formatTimeDDNS(unix, displayTZ) },
        // formatBytes / pageContent 不变
    })
    // ...
}

// formatTimeTpl 表格列用:"2026-01-02 15:04 MST"
func formatTimeTpl(unix int64, tzName string) string {
    if unix <= 0 {
        return "-"
    }
    loc, err := time.LoadLocation(tzName)
    if err != nil {
        loc = time.UTC
    }
    return time.Unix(unix, 0).In(loc).Format("2006-01-02 15:04 MST")
}

// formatTimeDDNS DDNS/API 用:"2026-01-02 15:04:05 MST"
func formatTimeDDNS(unix int64, tzName string) string {
    if unix <= 0 {
        return "-"
    }
    loc, err := time.LoadLocation(tzName)
    if err != nil {
        loc = time.UTC
    }
    return time.Unix(unix, 0).In(loc).Format("2006-01-02 15:04:05 MST")
}
```

**调用方改动**:
```go
// cmd/ikev2-panel/main.go 创建 Server 时:
// 旧: templates, err := web.LoadTemplates(templatesDir)
// 新: templates, err := web.LoadTemplates(templatesDir, cfg.DisplayTimezone)
```

**为什么 FuncMap 闭包而不是直接传 tzName 到 FuncMap**:
- Go html/template FuncMap 值是 `interface{}`,不能直接传 string
- 用闭包捕获 tzName → template 调用时拿得到
- 模板调用语法不变:`{{formatTime .User.CreatedAt}}` / `{{formatTimeDDNS .LastSync}}`

### 3. handlers_ddns.go + handlers_home.go 改用 cfg 注入的 timezone

**改动前**:
```go
// handlers_ddns.go:58
resp.LastSync.Time = last.Time.UTC().Format("2006-01-02T15:04:05Z")
// handlers_home.go:190
out.lastTime = last.Time.UTC().Format("2006-01-02T15:04:05Z")
```

**改动后**:
```go
// 方案 A:把 cfg.DisplayTimezone 透传到 Server struct(已有 DisplayTimezone 字段)
//         handler 里:
//   loc, _ := time.LoadLocation(s.DisplayTimezone)
//   if loc == nil { loc = time.UTC }
//   resp.LastSync.Time = last.Time.In(loc).Format("2006-01-02 15:04:05 MST")
//
// 方案 B:让 DDNS API 返回 unix timestamp,由前端 JS 用 Intl.DateTimeFormat 渲染
//         → 但本期不动前端,选 A
```

**最终选 A**:
```go
// handlers_ddns.go(新增 import "time")
loc, err := time.LoadLocation(s.DisplayTimezone)
if err != nil || loc == nil {
    loc = time.UTC
}
resp.LastSync.Time = last.Time.In(loc).Format("2006-01-02 15:04:05 MST")
```

```go
// handlers_home.go:190 同模式
loc, err := time.LoadLocation(s.DisplayTimezone)
if err != nil || loc == nil {
    loc = time.UTC
}
out.lastTime = last.Time.In(loc).Format("2006-01-02 15:04:05 MST")
```

### 4. mobileconfig ISO8601 + offset

`mobileconfig` 走 iOS 标准:`2006-01-02T15:04:05Z07:00`(RFC3339 with offset)或带 `Z` 都合法。

```go
// internal/cert/mobileconfig.go(改动)
// 旧: time.Now().UTC().Format(time.RFC3339)
// 新: time.Now().In(loc).Format("2006-01-02T15:04:05Z07:00")
//
// 实际 cert 包需要拿到 DisplayTimezone:
//   - 选项 1: cert.RenderMobileconfig 加参数 tzName
//   - 选项 2: Server 渲染时把 cfg 透传下去
//
// 选选项 1(更解耦):RenderMobileconfig(ctx, user, serverAddr, qrBaseURL, certPEM, keyPEM, tzName)
```

**iOS 兼容性**:RFC3339 with offset 是 iOS NSDateFormatter 100% 支持的标准格式。`+08:00` / `Z` 都行。

### 5. LE 续签日志路径修复

**改动前**(handlers_home.go:142):
```go
return "⚠️ Let's Encrypt 续签失败！最后一次续签失败,面板仍用旧证书运行。请检查 /var/log/ikev2-renew.log"
```

**改动后**:
```go
return "⚠️ Let's Encrypt 续签失败！最后一次续签失败,面板仍用旧证书运行。" +
    "请检查 /var/log/acme-renew.log（acme.sh 输出）和 /var/log/ikev2-renew.log（renew-cert.sh 输出）"
```

**为什么同时提两个路径**:
- `renew-cert.sh:59` 写 `> /var/log/acme-renew.log 2>&1` — acme.sh 自己的输出(详细)
- `entrypoint.sh` 在某些场景下会执行 `renew-cert.sh`(待 entrypoint grep 确认),它的 stdout 走 docker logs → 如果 entrypoint 还写 `ikev2-renew.log` 是另一回事
- 当前事实:`acme-renew.log` 是**确定存在的**;`ikev2-renew.log` 是**未必存在的**(只有当 entrypoint.sh 走我们自己的 wrapper 时才生成)
- 两个都提,**用户少走弯路**

**更稳的方案**(本期不做,留 v3):在面板加"LE 续签日志最近 100 行"折叠区,直接渲染 `/var/log/acme-renew.log` 末尾。本期只修文案。

### 6. docker-compose.yml + .env.example 加 env

**docker-compose.yml**(在 `environment:` 块加一行):
```yaml
# 面板/日志时间显示时区（IANA TZ 名,默认 UTC）
# 推荐:中国用户: Asia/Shanghai 或 Asia/Hong_Kong
#        美东用户: America/New_York
#        欧洲用户: Europe/Berlin
# 注意:此选项只影响面板渲染 / DDNS API / mobileconfig 描述的时间字符串;
#       不影响系统 TZ、证书有效期、cron 时间计算。
IKEV2_DISPLAY_TIMEZONE: "${IKEV2_DISPLAY_TIMEZONE:-UTC}"
```

**.env.example**(在文件末尾加新小节):
```bash
# ============= 面板时间显示时区 (v2.85 新增) =============
# 影响范围:
#   - 面板表格列(用户创建时间 / 最后登录 / 过期时间)
#   - DDNS 最近同步时间
#   - mobileconfig PayloadDescription 时间
# 不影响:
#   - 容器系统 TZ(entrypoint 仍按 UTC 跑 cron / acme.sh)
#   - 证书有效期(由 CA 决定)
#
# IANA TZ 名称: https://en.wikipedia.org/wiki/List_of_tz_database_time_zones
# 常用值:
#   UTC                  (默认)
#   Asia/Shanghai        (中国大陆)
#   Asia/Hong_Kong       (香港)
#   Asia/Tokyo           (日本)
#   America/New_York     (美东)
#   Europe/London        (英国)
#   Europe/Berlin        (中欧)
IKEV2_DISPLAY_TIMEZONE=UTC
```

---

## 改动文件清单

| 文件 | 改动 | 行数估算 |
|------|------|---------|
| [internal/config/config.go](file:///opt/ikev2-panel-v2-main/internal/config/config.go) | `Config.DisplayTimezone` 字段 + `Load()` 解析 + LoadLocation fallback | +10 行 |
| [internal/web/templates.go](file:///opt/ikev2-panel-v2-main/internal/web/templates.go) | `LoadTemplates` 签名 +tz 参数 + formatTimeTpl/formatTimeDDNS 改签名 + 加 formatTimeDDNS | +20 / -5 行 |
| [internal/web/server.go](file:///opt/ikev2-panel-v2-main/internal/web/server.go) | `Server.DisplayTimezone` 字段(用于 handler 渲染)+ `LoadTemplates` 调用点传 cfg.DisplayTimezone | +5 行 |
| [internal/web/handlers_ddns.go](file:///opt/ikev2-panel-v2-main/internal/web/handlers_ddns.go) | `:58` UTC.Format → In(loc).Format + tzName 走 `s.DisplayTimezone` | -1 / +4 行 |
| [internal/web/handlers_home.go](file:///opt/ikev2-panel-v2-main/internal/web/handlers_home.go) | `:190` 同上 + `:142` 文案改双路径 | -1 / +5 行 |
| [internal/cert/mobileconfig.go](file:///opt/ikev2-panel-v2-main/internal/cert/mobileconfig.go) | `RenderMobileconfig` 加 tzName 参数 + RFC3339 改 RFC3339 with offset | +6 行 |
| [internal/web/handlers_config.go](file:///opt/ikev2-panel-v2-main/internal/web/handlers_config.go) | mobileconfig 渲染调用点透传 tzName | +1 行 |
| [cmd/ikev2-panel/main.go](file:///opt/ikev2-panel-v2-main/cmd/ikev2-panel/main.go) | 创建 Server 时把 `cfg.DisplayTimezone` 赋给 `s.DisplayTimezone` + `LoadTemplates` 第二参数 | +2 行 |
| [docker-compose.yml](file:///opt/ikev2-panel-v2-main/docker-compose.yml) | environment 块加 `IKEV2_DISPLAY_TIMEZONE` | +6 行 |
| [.env.example](file:///opt/ikev2-panel-v2-main/.env.example) | 文件末尾加新小节 | +18 行 |

**总计**:**~+65 行 / -7 行,改动分散但每处都很小**。

---

## 测试方案

### 自动化测试

```bash
# 1. 单元测试(确保 config 改动不破坏)
go test ./internal/config/...
go test ./internal/web/...

# 2. 编译检查
CGO_ENABLED=0 go build -o /tmp/test-panel ./cmd/ikev2-panel
```

### config.DisplayTimezone 解析测试(手测)

```bash
# A. 默认 UTC(不设环境变量)
unset IKEV2_DISPLAY_TIMEZONE
go run ./cmd/ikev2-panel 2>&1 | head -5
# 期望:WARN 0 条,启动日志可看到 version=dev

# B. 合法 TZ
IKEV2_DISPLAY_TIMEZONE=Asia/Shanghai go run ./cmd/ikev2-panel 2>&1 | head -5
# 期望:WARN 0 条

# C. 非法 TZ(应 fallback UTC + WARN)
IKEV2_DISPLAY_TIMEZONE=Mars/Olympus_Mons go run ./cmd/ikev2-panel 2>&1 | head -5
# 期望:WARN: IKEV2_DISPLAY_TIMEZONE="Mars/Olympus_Mons" invalid (...); falling back to UTC

# D. 空字符串(应走默认值 UTC)
IKEV2_DISPLAY_TIMEZONE="" go run ./cmd/ikev2-panel 2>&1 | head -5
# 期望:WARN 0 条(getenv 用 LookupEnv,空值走 default)
```

### 时区显示验证(集成测试,沙箱里跑)

```bash
# 启动面板,登录后看用户列表 / DDNS 卡片
# 期望:created_at / last_used_at 显示 "2026-09-19 16:30 CST"(配 Asia/Shanghai)
# 期望:DDNS "最后同步" 显示 "2026-09-19 16:30:15 CST"

# 截图对比:
#   1. IKEV2_DISPLAY_TIMEZONE=UTC → 全部 UTC + "UTC" 后缀
#   2. IKEV2_DISPLAY_TIMEZONE=Asia/Hong_Kong → "HKT" 后缀
```

### LE 续签提示文案验证

```bash
# 模拟续签失败:
touch /data/le/LAST_RENEW_FAILED
# 触发面板首页加载,查看 LE 告警横幅
# 期望文案包含:
#   "请检查 /var/log/acme-renew.log（acme.sh 输出）和 /var/log/ikev2-renew.log（renew-cert.sh 输出）"
# 期望不再单独说 "/var/log/ikev2-renew.log"
```

### mobileconfig 时间字段验证

```bash
# 下载 mobileconfig,用 plutil 或 jq 检查 PayloadDescription
IKEV2_DISPLAY_TIMEZONE=Asia/Shanghai
# 生成的 mobileconfig 内的时间戳应类似:2026-09-19T16:30:15+08:00
# iOS 端显示应跟面板一致
```

### 回归测试(确保不动行为)

- 用户创建 / 删除 / 密码重置 — 时间显示格式变了,但值正确(用 `date` 命令对比)
- DDNS 启用 / 停用 / 失败 → 状态切换正常
- mobileconfig 下载 → iOS 仍能安装并拨通

---

## 风险评估

| 风险 | 概率 | 影响 | 缓解 |
|------|------|------|------|
| `time.LoadLocation("Asia/Shanghai")` 失败(容器 tzdata 缺失) | 极低 | 时区退回到 UTC | Debian bookworm slim 默认有 `/usr/share/zoneinfo`;失败 fallback UTC + WARN,不 fatal |
| 用户配错时区名(如 `Asia/Shang Hai` 拼写错误) | 中 | 看到 UTC + 启动 WARN 日志 | fallback UTC + stderr WARN,容器仍起;面板首页 DDNS 时间能看出来"不对劲" |
| 现有模板调用 `{{formatTime .X}}` 签名变了 → panic | 已消除 | 编译不过 | formatTimeTpl 仍是 `func(unix int64) string` 签名(闭包捕获 tz),调用方零改动 |
| DDNS API 客户端(自己面板 JS)解析新格式 `2026-09-19 16:30:15 CST` 失败 | 低 | JS `new Date("...CST")` 在某些浏览器失败 | JS 端不解析这个字段(只显示);如要解析走 unix timestamp;本期面板 JS 端无需改 |
| iOS 解析 `+08:00` offset 格式失败 | 极低 | iOS 显示原始字符串 | RFC3339 是 iOS NSDateFormatter 标准,100% 支持 |
| 改 config.Load() 加 LoadLocation 调用 → 启动多 ~1ms | 极低 | 无感知 | LoadLocation 内部有缓存 |
| 改 RenderMobileconfig 签名 → 其他 caller 编译失败 | 中 | 编译不过(已知风险,可控) | 用 grep 找全部 caller,统一加 tzName 参数(本期就 handler_config.go 一处) |

---

## 不破坏兼容性的承诺

- **`IKEV2_DISPLAY_TIMEZONE` 缺失**:默认 `UTC`,行为跟 v2.84 完全一致(只是**多写了时区缩写后缀**,如 `UTC`)
- **`IKEV2_DISPLAY_TIMEZONE=""` 空值**:`getenv` 实现里空值走 default,fallback `UTC`,无 WARN
- **`IKEV2_DISPLAY_TIMEZONE=Asia/Shanghai` 非法**:LoadLocation 失败 → stderr WARN + fallback UTC,**容器照常启动**
- **现有 `formatTime` 模板调用语法不变**(闭包捕获 tz),所有 `{{formatTime .X}}` 无需改
- **mobileconfig PayloadDescription 时间字段**:旧格式 RFC3339 `Z` 是 RFC3339 子集,新格式 `+08:00` 也是 RFC3339 子集 → iOS 解析 100% 兼容
- **DDNS API JSON 输出**:`time` 字段类型从 `2026-09-19T08:30:15Z` 变 `2026-09-19 16:30:15 HKT`:
  - 客户端只要**显示**,无影响
  - 客户端若做 `Date.parse`,应改用 unix timestamp(本期不动 API 加 unix 字段,留 v3)
- **未配 `IKEV2_DISPLAY_TIMEZONE` 的老 docker-compose.yml 升级**:环境变量块无此 key → 容器内也没此 env → `getenv` 返回 default `"UTC"` → 行为跟 v2.84 一致(只是多了 `UTC` 后缀)
- **面板模板不需要改**:所有 `{{formatTime .X}}` 调用语法不变
- **session / 审计 / DDNS 业务逻辑 / 限速 / strongSwan**:0 改动

---

## 后续 PR 关联

本 PR 是 v2.85 release 第 3 个,**只做**时区配置化 + 日志路径文案修复。后续:

- **PR-4**:main.go 统一 WaitGroup(Correctness Q1-04)
- **PR-5**:alidns 错误码精确匹配 + HMAC-SHA256(Correctness Q3-*)
- **PR-6**:DDNS throttle 持久化 + stopCh sync.Once + collector REKEYING
- **PR-7**:tc IPv6 限速

未来 v3 候选(从本审计 §U03 中期方案提取):
- v3.x:面板顶部 nav 加 "时区: UTC ▼" 下拉(JS,无须重启)
- v3.x:面板加 "LE 续签日志最近 100 行" 折叠区(读 `/var/log/acme-renew.log` 渲染)
- v3.x:JS `<time datetime="...">` 让浏览器本地化(需重写模板调用)

---

## 评审检查项(给自己)

- [x] 改动不超过 70 行
- [x] 不引入新依赖(用 stdlib `time.LoadLocation`)
- [x] 不改任何业务逻辑(只动显示文案 + 配置加载)
- [x] 默认值兼容老用户(`IKEV2_DISPLAY_TIMEZONE` 缺失 → UTC)
- [x] 非法值 fallback + WARN,不 fatal
- [x] 模板调用语法不变(闭包捕获 tz)
- [x] mobileconfig 时间字段保持 iOS 兼容(RFC3339 with offset)
- [x] LE 续签日志路径文案同时提 2 个真实路径
- [x] 面板日志折叠区明确剔除(留 v3)
- [x] 测试方案具体可执行(4 套命令)

---

## 工作量分解

| 步骤 | 时间 |
|------|------|
| config.go 加 DisplayTimezone 字段 + LoadLocation 解析 + fallback | 0.03d |
| templates.go LoadTemplates 加 tz 参数 + formatTimeTpl/DDNS 改签名 | 0.05d |
| server.go 加 DisplayTimezone 字段 + main.go 注入 | 0.02d |
| handlers_ddns.go / handlers_home.go 改用 s.DisplayTimezone | 0.03d |
| handlers_home.go L142 续签文案改双路径 | 0.01d |
| cert/mobileconfig.go 加 tzName 参数 + RFC3339 with offset | 0.05d |
| handlers_config.go 透传 tzName | 0.01d |
| docker-compose.yml + .env.example 加 env | 0.02d |
| 跑测试(go test + dev 启动验证 4 种 env) | 0.08d |
| **合计** | **0.3d**(U03)+ **0.1d**(U07)= **0.4d** |

---

## 实施 Checklist(执行时用)

```markdown
- [ ] config.go Config 加 DisplayTimezone 字段
- [ ] config.go Load() 加 LoadLocation 解析 + fallback UTC + stderr WARN
- [ ] templates.go LoadTemplates 签名加 tzName 参数
- [ ] templates.go formatTimeTpl 闭包化 + 新增 formatTimeDDNS
- [ ] server.go Server 加 DisplayTimezone 字段
- [ ] main.go 创建 Server 时设 s.DisplayTimezone = cfg.DisplayTimezone
- [ ] main.go LoadTemplates 调用传 cfg.DisplayTimezone
- [ ] handlers_ddns.go L58 改用 s.DisplayTimezone + In(loc).Format 带 MST
- [ ] handlers_home.go L190 同上
- [ ] handlers_home.go L142 文案改双路径(acme-renew.log + ikev2-renew.log)
- [ ] cert/mobileconfig.go RenderMobileconfig 加 tzName 参数 + RFC3339 with offset
- [ ] handlers_config.go 调用点透传 tzName
- [ ] docker-compose.yml environment 加 IKEV2_DISPLAY_TIMEZONE
- [ ] .env.example 文件末尾加 "面板时间显示时区" 小节
- [ ] go test ./... PASS
- [ ] CGO_ENABLED=0 go build ./cmd/ikev2-panel PASS
- [ ] 4 种 env 启动验证(默认 / 合法 TZ / 非法 TZ / 空字符串)
- [ ] dev 启动登录面板截图对比(UTC vs Asia/Shanghai)
- [ ] LE 续签失败模拟 + 文案确认双路径
- [ ] mobileconfig 下载 + iOS 安装拨通(回归)
- [ ] git commit "v2.85-PR3: IKEV2_DISPLAY_TIMEZONE config + LE renew log path fix"
```

---

> 关联:
> - [audit-2026-09-usability.md §U03](file:///opt/ikev2-panel-v2-main/docs/audit-2026-09-usability.md)(HIGH — 时区显示不统一)
> - [audit-2026-09-usability.md §U07](file:///opt/ikev2-panel-v2-main/docs/audit-2026-09-usability.md)(MED — 续签日志路径错误)
> - [audit-2026-09-usability.md §U11](file:///opt/ikev2-panel-v2-main/docs/audit-2026-09-usability.md)(MED — 时区硬编码,无配置项)
> - [audit-2026-09-summary.md](file:///opt/ikev2-panel-v2-main/docs/audit-2026-09-summary.md) Phase 1 Top-10
> - [release-notes-v2.85.md](file:///opt/ikev2-panel-v2-main/docs/release-notes-v2.85.md)(待 PR 合入后写)
