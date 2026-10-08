# ikev2-panel-v2 分析结论复核报告

> 复核日期:2026-10-08 | 复核对象:[综合分析报告](file:///opt/ikev2-panel-v2/docs/ANALYSIS-REPORT-20261008.md) | 基线 commit:`d04ce38`
> 复核原则:不采信任何未经验证的转述,每条结论独立回到源码 / 运行时取证

---

## 复核结论速览

| 类别 | 数量 | 说明 |
|---|---|---|
| ✅ 证实 | 2 项高危 XSS、2 项 data race | 四环链路逐行验证;data race 已用 `-race` 实机复现 |
| ❌ **证伪 1 项** | `audit_log` 无 GC | **我上一轮报告的误判**,实际 cron GC 机制完整 |
| ⚠️ **更正 1 项** | ddns data race 范围 | 实际范围比原估更宽(含 `Run()` 启动日志) |
| 🔧 **修复 1 项** | pre-commit XSS 检测 | **我自己建的安全机制存在盲区**,已修复并验证 |
| ➕ 新增 1 项 | 4 处 banner sink 不可利用 | 追查后确认为安全设计,非遗漏 |

---

## 一、复核方法与检查点

### 1.1 采用的验证手段

| 手段 | 用途 | 适用项 |
|---|---|---|
| 逐行读源码 | 验证数据流每一环 | XSS 链路、锁边界 |
| 全局检索反查 | 确认无遗漏 / 无误报 | XSS sink 清点、query 参数清点 |
| **`go test -race` 实机探针** | 运行时证据,非静态推理 | 2 项 data race |
| **故障注入** | 验证检查脚本真的有效 | pre-commit XSS 检测 |
| 反向溯源 | 追查 `.Error`/`.Flash` 真实来源 | 排除"看起来危险实则不可达" |

### 1.2 关键检查点

对每条安全结论追问四问:
1. **数据从哪来?** — 是否真的存在用户可控输入
2. **流经哪几环?** — 每一环都要有代码行号支撑
3. **终点 sink 是否真危险?** — 有无转义层
4. **前置条件是什么?** — 需登录?需特定配置?→ 决定真实严重级别

---

## 二、✅ 证实项

### 2.1 XSS #1 —— `?error=` 反射型 XSS

**四环证据链(逐行核实)**:

| 环 | 位置 | 代码 |
|---|---|---|
| 1. 源 | [handlers_mobileconfig_admin.go:103](file:///opt/ikev2-panel-v2/internal/web/handlers_mobileconfig_admin.go#L103) | `Error: r.URL.Query().Get("error")` |
| 2. 中转 | [admin_mobileconfig_defaults_content.html:181](file:///opt/ikev2-panel-v2/web/templates/admin_mobileconfig_defaults_content.html#L181) | `{{if .Error}}{{template "banner" (dict "Kind" "error" "Msg" .Error)}}{{end}}` |
| 3. sink | [layout.html:802](file:///opt/ikev2-panel-v2/web/templates/layout.html#L802) | `<div class="banner-body">{{.Msg \| safeHTML}}</div>` |
| 4. 转义层 | [templates.go:254-256](file:///opt/ikev2-panel-v2/internal/web/templates.go#L254-L256) | `func safeHTMLTpl(s string) template.HTML { return template.HTML(s) }` |

**关键补充证据**:
- `safeHTML` 在 FuncMap 注册于 [templates.go:147](file:///opt/ikev2-panel-v2/internal/web/templates.go#L147),确认管道语法 `{{.Msg | safeHTML}}` 真实生效
- `safeHTMLTpl` 的注释写着「**仅用于 banner helper 内嵌的 SVG 图标(代码常量、不可被用户注入)。严禁用于用户输入(否则 XSS)**」 —— 而 L802 恰恰把整个 `.Msg` 走了 `safeHTML`,**与注释意图直接矛盾**。这不是设计意图,是实现偏离了约束。

**前置条件核实**([server.go:228](file:///opt/ikev2-panel-v2/internal/web/server.go#L228)):
```go
mux.Handle("GET /admin/mobileconfig-defaults", protect(http.HandlerFunc(srv.handleAdminMobileconfigDefaults)))
```
路由有 `protect`(需有效 session),因此**不是未认证可利用**,而是"诱导已登录管理员点击恶意链接"。**这不降低危害等级** —— 受害者是拥有全部 VPN 凭据管理权的 admin,且同源意味着可直接读取页面上的 CSRF token。

> PoC:`GET /admin/mobileconfig-defaults?error=<script>alert(document.cookie)</script>`

**结论:成立,维持高危。**

### 2.2 XSS #2 —— `?flash=` 反射型 XSS

**可达性条件精确核实**([handlers_config.go:378-381](file:///opt/ikev2-panel-v2/internal/web/handlers_config.go#L378-L381)):
```go
flash := r.URL.Query().Get("flash")
if s.ServerAddr == "" {
    flash = "ServerAddr 未配置(容器未设置 ...)。"   // 仅在 ServerAddr 为空时覆盖
}
```
- `ServerAddr != ""`(生产常态)→ 保留 query 原值 → **可利用**
- `ServerAddr == ""`(配置缺失的异常态)→ 被固定文案覆盖 → 不可利用

链路终点同为 [android_content.html:17](file:///opt/ikev2-panel-v2/web/templates/android_content.html#L17) → `layout.html:802`。

**结论:成立。** 上一轮表述「取决于 `ServerAddr`」准确 —— 复核确认这是精确的可达性条件,而非模糊猜测。

### 2.3 Data race #3 —— `ratelimit.RecordLoginFail`

**锁边界核实**:
- [ratelimit.go:77-79](file:///opt/ikev2-panel-v2/internal/auth/ratelimit.go#L77-L79) `RecordFail` 内 `b.mu.Lock(); defer b.mu.Unlock()` → **返回时锁已释放**
- [ratelimit.go:181-183](file:///opt/ikev2-panel-v2/internal/auth/ratelimit.go#L181-L183) 在**只持 `rl.mu`** 的情况下读 `b.failCount` / `b.firstFailAt` / `b.lockedUntil`

**运行时证据**(自定义探针,`CGO_ENABLED=1 go test -race`):
```
WARNING: DATA RACE
Write at 0x00c000180008 by goroutine 15:
  auth.(*ipBucket).RecordFail()        ratelimit.go:88
  auth.(*RateLimiter).RecordLoginFail() ratelimit.go:178
Previous read at 0x00c000180008 by goroutine 12:
  auth.(*RateLimiter).RecordLoginFail() ratelimit.go:181
FAIL: race detected during execution of test
```

**结论:成立。** 已从「静态推断」升级为「race detector 实机证实」,写读双方在同一内存地址 `0x00c000180008`。

### 2.4 Data race #5 —— `ddns.Sync` 配置热重载

**并发双方核实**:
- 写侧:[SetConfig:543](file:///opt/ikev2-panel-v2/internal/ddns/sync.go#L543) / [SetEnabled:319](file:///opt/ikev2-panel-v2/internal/ddns/sync.go#L319) / [SetFamily:341](file:///opt/ikev2-panel-v2/internal/ddns/sync.go#L341) 全部 `s.mu.Lock()` 内写 `s.cfg.*` → 由 HTTP handler goroutine 调用
- 读侧:`tick()` 与 `Run()` **无锁**读 `s.cfg` → 由 `startBG` 启动的后台 goroutine 调用

**运行时证据**:
```
WARNING: DATA RACE
Write at 0x00c000002248 by goroutine 11:
  ddns.(*Sync).SetConfig()   sync.go:545
Previous read at 0x00c000002248 by goroutine 10:
  ddns.(*Sync).Run()         sync.go:661     ← 注意:不是 tick(),是 Run() 启动日志
```

**⚠️ 更正**:上一轮报告称范围为 `tick()` L731/736/748/770-771。实机证据显示 race 首次触发在 [sync.go:661](file:///opt/ikev2-panel-v2/internal/ddns/sync.go#L661)(`Run()` 的启动日志 `s.cfg.Logger.Info(... "domain", s.cfg.Domain ...)`),**发生在 `tick()` 被调用之前**。同时 L762 的节流检查**在锁内**读 `s.cfg.Throttle`,说明作者已意识到部分字段需保护 —— 保护是不完整的。

**结论:成立,但实际范围比原估更宽。修复时应覆盖 `Run()` 启动段与 `tick()` 全部无锁读点。**

---

## 三、❌ 证伪项:我的一处误判

### 3.1 上一轮的错误结论

> **原报告第 10 项**「`audit_log` 表**生产代码无 DELETE**,会无限增长」—— 中危,列入修复建议。

### 3.2 反查过程

1. 全局检索 `DELETE FROM audit_log` → 命中 3 处:`audit_retention_test.go:48`、`audit-retention.sh:60`、`audit-retention.sh:68`
2. 关键疑问:`audit-retention.sh` 是**注释里的脚本,还是真的被调度?**
3. 追查 [entrypoint.sh:1041-1063](file:///opt/ikev2-panel-v2/scripts/entrypoint.sh#L1041-L1063) → **确认已注册 cron**:

```bash
# ---------- 7.7. audit_log retention cron(Phase 5 A)----------
AUDIT_CRON_FILE="/etc/cron.d/ikev2-audit-retention"
cat > "${AUDIT_CRON_FILE}" <<EOF
0 4 * * * root /etc/ikev2-panel/scripts/audit-retention.sh > /var/log/ikev2-audit-retention.log 2>&1
EOF
```

4. 确认覆盖面:[entrypoint.sh:1045](file:///opt/ikev2-panel-v2/scripts/entrypoint.sh#L1045) 注释明确「**这里不依赖 LE 模式:即使自签模式也有审计**」,且 L1058-1063 在 LE 未启动 cron 时补启一次 → **自签/LE 两种模式都覆盖**
5. 保留期:默认 90 天,`IKEV2_AUDIT_RETENTION_DAYS` 可调(v2.85-PR8 起的既定行为,见 release-notes-v2.85.md:339)

### 3.3 误判成因分析

**根因是我对 agent 结论的转述失真。** agent 原文谨慎表述为「**生产代码中**无 DELETE / 清理逻辑」(字面正确 —— Go 代码里确实没有),但我在汇总时省略了这个限定语,变成了「无 GC、会无限增长」的错误推论。这是一个**从"事实陈述"滑向"因果推论"的经典错误**,不应当发生在我自己的输出中。

### 3.4 结论

**该项证伪,不是问题。** 建议从修复清单中移除(已在主报告中划掉并标注)。

**同时也是一条正面发现**:项目有 47 个 docs,其中 `audit-2026-09-*.md` 系列(15 份审计报告)说明团队此前已做过系统审计 —— 我本应先查既有审计结论再下判断。

---

## 四、➕ 新增结论:4 处 banner sink 经查不可利用

复核时我清点了**全部 22 处 banner 调用点**(原 agent 报告只涉及 3 处)。新增发现的 4 处 `.Error` sink 经溯源后确认**不可利用**,属于安全设计:

| 位置 | `.Error` 来源 | 判定 |
|---|---|---|
| [login_content.html:21](file:///opt/ikev2-panel-v2/web/templates/login_content.html#L21) | `renderLoginError` 传入固定文案 | ✅ 不可利用。且有用户名枚举防护(两分支返回相同文案) |
| [user_new_content.html:14](file:///opt/ikev2-panel-v2/web/templates/user_new_content.html#L14) | 校验固定文案 + `friendlyUserCreateError` | ✅ 不可利用 |
| [user_detail_content.html:64](file:///opt/ikev2-panel-v2/web/templates/user_detail_content.html#L64) | 固定文案 | ✅ 不可利用 |
| [account_content.html:28](file:///opt/ikev2-panel-v2/web/templates/account_content.html#L28) | 改密校验固定文案 | ✅ 不可利用 |

**关键证据** —— `friendlyUserCreateError` 只返回 3 个硬编码文案,**不泄漏底层 err**([handlers_users.go:241-249](file:///opt/ikev2-panel-v2/internal/web/handlers_users.go#L241-L249)):
```go
func friendlyUserCreateError(err error) string {
	if errors.Is(err, errSwanctlWriteFailed) { return "写入 swanctl 配置失败(请检查容器内 /etc/swanctl/conf.d 权限)" }
	if errors.Is(err, errLimiterWriteFailed)  { return "写入限速文件失败(请检查容器内 /var/lib/ikev2-panel/limits 权限)" }
	return "创建用户失败,请查看服务日志"
}
```
这是**正确做法** —— 若直接把 `err.Error()` 透出,底层路径/SQL 错误会进入 `safeHTML` 形成 XSS。

**无遗漏确认**:全局检索 `Query().Get(` 在 `internal/web/` 下**仅 2 处命中**(即 XSS #1/#2 的源),全部流入 sink 的用户输入路径已穷尽。

---

## 五、🔧 修复项:我自己建的安全机制存在盲区

这是本次复核**最有价值的发现** —— 我上一轮声称"已建立安全机制",但该机制本身有漏洞。

### 5.1 缺陷描述

原 pre-commit 第 8 项只检测 `layout.html` 的改动行:
```bash
if git diff --cached -- web/templates/layout.html | grep -E '^\+.*safeHTML'; then
```

**问题**:XSS 的真实触发条件是「用户可控数据进入 `.Msg`」,而 22 处 banner 调用点中有 21 处**不在 layout.html 里**。攻击者(或未来的我)在其他模板新增 `{{template "banner" (dict "Msg" .UserInput)}}` 并在 handler 侧喂入 query 参数,检查会**完全放行**。

### 5.2 故障注入验证

新建探针(新增一处 banner 调用 + 一处 query 读取):
```
=== 测试A: 仅新增 banner 调用 ===
[ OK ] banner sink 提醒已输出
[ OK ] 全部检查通过        ← 旧版本行为:放行(无告警)

=== 测试B: banner + query 注入(旧版本) ===
[ OK ] 全部检查通过        ← 真实 XSS 注入被放行 ❌
```

### 5.3 修复内容

检测面扩大到两条路径:
- **新增 banner 调用** → 告警提示 `.Msg` 经 `layout.html:802` 的 `safeHTML` 输出
- **新增 query → Error/Flash** → 直接判失败(这是真实攻击模式的指纹)
- **layout.html 新增 safeHTML** → 从 fail 降为 warn(它本身不是漏洞,漏洞在喂给它的数据)

### 5.4 修复后验证

```
=== 测试A: 仅新增 banner 调用 ===
[WARN] 新增 banner 调用 — .Msg 最终经 layout.html:802 的 safeHTML 输出,禁止传入用户可控数据
[ OK ] 全部检查通过

=== 测试B: banner + query 注入 ===
[WARN] 新增 banner 调用 — ...
[FAIL] 检测到 query 参数流入模板 — 若其最终到达 banner 的 .Msg 即为反射型 XSS
[FAIL] 新增 query 参数 → Error/Flash:该值会经 safeHTML 输出,构成反射型 XSS(layout.html:802)
[FAIL] 提交被阻止
---EXIT=1---
```

✅ **现在能正确拦截真实注入模式。** 探针已全部清理,基线 7/7 全绿。

---

## 六、修正后的最终风险清单

| # | 级别 | 状态 | 项目 |
|---|---|---|---|
| 1 | 高 | ✅ 证实 | `?error=` 反射型 XSS(路由需登录,诱导已登录 admin) |
| 2 | 高 | ✅ 证实 | `?flash=` 反射型 XSS(`ServerAddr != ""` 时可利用) |
| 3 | 高 | ✅ -race 证实 | `ratelimit.RecordLoginFail` data race |
| 5 | 中 | ✅ -race 证实 | `ddns.Sync` 配置热重载 data race(**范围比原估更宽**) |
| 4 | 中 | ✅ 确认 | 阿里云响应体 → `err.Error()` → `safeHTML` |
| 6-9 | 中 | 未重验 | 明文存库 / 改密不踢 session / admin 密码落盘 / SQLite 连接池 |
| 11-14 | 中 | 未重验 | 全表扫描 / HTTP 路径 panic / 静默吞错 |
| 10 | ~~中~~ | ❌ **已证伪** | ~~audit_log 无 GC~~ — cron 机制完整 |

**未重验项说明**:第 6-9、11-14 项为低耦合、非高危项,本轮聚焦高危项证据链。它们的判定依据是直接可读的代码事实(如 `store/users.go:100` 确为 `password = ?` 明文写入),不存在链路推理,不构成误判风险。

---

## 七、复核方法论沉淀

### 7.1 本次暴露的三个认知盲区

| 盲区 | 表现 | 纠正方式 |
|---|---|---|
| **转述失真** | agent 原文有「生产代码中」限定语,我汇总时省略,推论走向错误 | 引用结论时保留原始限定条件 |
| **推理越界** | 从「Go 代码里没看到 GC」推到「表会无限增长」 | 搜索范围不能止于代码,须覆盖 shell / cron / Dockerfile 等运维层 |
| **自我信任** | 上一轮刚验证过检查脚本"7/7 全绿"就认为机制可靠 | 安全机制必须用**故障注入**验证,而不是只看它跑通 |

### 7.2 对后续修改的约束

1. ~~**改 XSS 相关代码前**先看本报告第四节的 sink 清单 —— `layout.html:802` 是唯一 sink,改它一处即可解决全部~~
   → **已执行(2026-10-08)**:该 sink 已由 `{{.Msg | safeHTML}}` 改为 `{{.Msg}}`,XSS #1/#2 已修复。当前 sink 为自动转义,新增字段默认安全;唯一豁免是代码常量 `$iconSvg`。
2. **改 `ddns` 配置字段**时,注意 `tick()` 及其 5 个下游方法现已接收 `cfg Config` 参数。新增读配置的代码**必须走参数传递**,不要直接读 `s.cfg`(否则重新引入 data race)
3. **新加 banner 调用**时,`.Msg` 绝不可接用户输入;pre-commit 会同时扫描新增 banner 调用与 `URL.Query().Get` 流入(且已剔除注释行避免误报)
4. **再次怀疑某项"缺失功能"**前,先查 `docs/audit-2026-09-*.md`(15 份既有审计)是否已有结论
5. **不要用 `git ls-tree -- '*.go'` 统计文件** —— pathspec 的 `*` 不跨 `/`,会静默返回空集。此类"静默返回空"是本项目两次检查失效的共同根因

---

## 八、修复实施记录(2026-10-08)

第二节证实的 3 项高危(XSS #1、XSS #2、data race #3、#5)已全部修复并验证。

### 8.1 改动清单

| 文件 | 类别 | 内容 |
|---|---|---|
| `web/templates/layout.html` | XSS sink | `{{.Msg \| safeHTML}}` → `{{.Msg}}`,恢复 Go 模板自动转义;`$iconSvg` 保留 safeHTML(代码常量,仅 4 个固定 SVG) |
| `internal/web/handlers_mobileconfig_admin.go` | XSS 源头 | `Error: r.URL.Query().Get("error")` → `Error: ""`,断掉注入路径 |
| `internal/web/handlers_config.go` | XSS 源头 | 移除 `r.URL.Query().Get("flash")`(死代码,无 producer) |
| `internal/auth/ratelimit.go` | data race #3 | 新增 `ipBucket.snapshot()`;`RecordFail` 返回 `(bool, persistedBucket)`,快照在 `b.mu` 内捕获 |
| `internal/ddns/sync.go` | data race #5 | 新增 `Sync.snapshot()`;`tick` 及 5 个下游方法改为接收 `cfg Config` 参数,不再读共享 `s.cfg` |

新增 3 个回归测试文件(防止修复被后续改动悄悄撤销):

- `internal/auth/ratelimit_race_test.go` —— 8 goroutine × 300 次登录失败 + 快照一致性断言
- `internal/ddns/sync_race_test.go` —— 后台 `Run()` 与 handler 并发改配置的 `-race` 探针
- `internal/web/xss_banner_test.go` —— 5 种 payload 渲染断言已转义 + `$iconSvg` 未被误伤

### 8.2 修复过程中的两个教训

| 教训 | 说明 |
|---|---|
| **编译通过 ≠ race 消除** | 第一版 ddns 修复后 `go build` / `go vet` 均通过,但 `-race` 仍报下游方法读 `s.cfg`。配置必须一路作为参数传到底,而不是只改最外层 |
| **人工审查不可靠** | 改完 5 个方法后,`awk` 扫描 `tick()` 区间仍找出 2 处遗漏。编译器不报错,`-race` 因时序问题 flaky(3 pass / 2 fail),连跑 8 次才确认稳定 |

### 8.3 顺带修复:安全机制自身的 3 处缺陷

修复过程中暴露出上一轮建立的检查机制本身有缺陷,均已修正并用故障注入反向验证:

| # | 缺陷 | 根因 | 后果 |
|---|---|---|---|
| 1 | gofmt 基线集合恒为空 | `git ls-tree -- '*.go'` 的 pathspec 中 `*` 不跨 `/`,匹配不到 `internal/web/x.go` | 69 个基线既有文件全被误判为"新增格式问题",**检查永久 FAIL** |
| 2 | 注释行被当成新增代码 | 扫描新增行时未剔除 `//` 注释行 | 本次修复的说明性注释(引用旧写法)触发 XSS 规则**误报** |
| 3 | pre-commit 用绝对 gofmt | `gofmt -l <staged files>` 不区分基线状态 | 本次要提交的 5 个基线 unformatted 文件会**被永久阻塞** |

修正后故障注入验证:

- 注入真实格式错误(空格缩进)到 `ratelimit.go` → `[FAIL] gofmt 增量`,EXIT=1 ✅
- 注入 `{{.Msg | safeHTML}}` + query 参数 → `[FAIL] 单元测试失败` + `[FAIL] 新增 query 参数 → Error/Flash`,EXIT=1 ✅
- 还原后 → 全绿,EXIT=0 ✅

> 附带说明:第一次故障注入时我用了 tab 缩进,而 tab 本身就是合法 gofmt,导致"注入失败"的假象。**注入物本身必须先确认真的违规**,否则会误判检查机制失效。

### 8.4 最终验证结果

```
-race 连跑 8 次(auth + ddns):DATA RACE 计数 = 0 0 0 0 0 0 0 0
go test -count=1 ./...            : 14 包全绿
git diff --cached                 : 15 文件(3 源 + 8 测试 + 2 报告 + 1 脚本 + 1 模板)
pre-commit(全量,含 40s 单元测试)  : 全部通过,EXIT=0
```

---

## 九、修复后的最终风险清单

| # | 级别 | 状态 | 项目 |
|---|---|---|---|
| 1 | 高 | ✅ **已修复** | `?error=` 反射型 XSS |
| 2 | 高 | ✅ **已修复** | `?flash=` 反射型 XSS |
| 3 | 高 | ✅ **已修复** | `ratelimit.RecordLoginFail` data race |
| 5 | 中 | ✅ **已修复** | `ddns.Sync` 配置热重载 data race |
| 4 | 中 | ⚠️ 缓解 | 阿里云响应体 → `err.Error()`:随 XSS sink 修复已自动转义,建议后续改为白名单错误码 |
| 6-9 | 中 | 未处理 | 明文存库 / 改密不踢 session / admin 密码落盘 / SQLite 连接池 |
| 11-14 | 中 | 未处理 | 全表扫描 / HTTP 路径 panic / 静默吞错 |

---

*本次在复核结论基础上完成了 XSS 两项 + data race 两项的修复,新增 3 个回归测试,并修正了安全机制自身的 3 处缺陷。所有验证均通过故障注入反向确认,非仅"跑通即信"。*
*改动尚未提交,需走 `git commit` 触发 pre-commit 完成最终把关(勿用 `--no-verify`)。*
