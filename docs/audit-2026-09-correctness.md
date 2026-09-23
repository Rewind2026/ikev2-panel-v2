# 正确性(Correctness)审计报告 — 2026-09-19

> 维度 3(Q1-Q10) — 关注:并发安全、资源管理、错误处理、时区、重启恢复、边界条件、第三方集成正确性。
> 关联:[audit-framework.md](file:///opt/ikev2-panel-v2-main/docs/audit-framework.md) 维度 3 / [audit-2026-09-summary.md](file:///opt/ikev2-panel-v2-main/docs/audit-2026-09-summary.md) Top-10(已规避重复)。

---

## 范围

- 代码:`/opt/ikev2-panel-v2-main/internal/{swanctl,store,ddns,dns,panelstate,web,cert,expiry,limit,auth,installtoken,config}/`
- 测试:`go test ./...` 已跑通(全 13 包 PASS)
- `-race`:**未执行成功** — 容器内无 `gcc`,`cgo` 不可用,race detector 需要 cgo(`-race requires cgo; enable cgo by setting CGO_ENABLED=1` → `cgo: C compiler "gcc" not found`)。建议 CI 阶段必须解决 gcc 可用性。
- 配置 / 文档:`go.mod`、`internal/config/config.go`、`docs/design.md`

---

## 工具 / 参考

- [Go race detector](https://go.dev/blog/race-detector) / [Go Memory Model](https://go.dev/ref/mem)
- [阿里云 OpenAPI 签名机制 v3](https://help.aliyun.com/zh/sdk/developer-reference/v3-request-structure-and-signature) — HMAC-SHA256 实际为阿里云 v3 推荐
- [alidns 错误码](https://help.aliyun.com/zh/dns/developer-reference/error-codes) — `Throttling.User` / `DomainRecordNotBelongToRAM` 等
- [strongSwan VICI 协议](https://docs.strongswan.org/docs/strongswanInterop.html)
- [acme.sh dns_ali wiki](https://github.com/acmesh-official/acme.sh/wiki/dnsapi)
- [ddns-go](https://github.com/jeessy2/ddns-go) / [ddclient](https://github.com/ddclient/ddclient) 探测策略对比
- [RFC 5952 — IPv6 文本表示](https://datatracker.ietf.org/doc/html/rfc5952)
- [RFC 8247 — IKEv2 安全建议](https://datatracker.ietf.org/doc/html/rfc8247)

---

## Q1 并发安全

### [SEVERITY-MED] ISSUE-Q1-01:`Sync.results` buffered channel 可能泄漏:goroutine 派发后 panic 不会通过 channel 反馈
- **位置**:[sync.go:396, 414, 442](file:///opt/ikev2-panel-v2-main/internal/ddns/sync.go#L396-L414)、`results := make(chan result, 2)`
- **问题**:`tick()` 启动 v4 / v6 两个 goroutine 各自向 `results` 写;`wg.Wait()` 后再 `close(results)` + `for range results`。如果任一 goroutine 在 `runFamily` 中 panic(目前代码路径不会,但未来加 retry/timeout 容易引入),既不会写 results 也不会调用 `wg.Done()`,导致 `wg.Wait()` 永久阻塞 → 整个 DDNS tick 永不结束 → 后续 ticks 永远卡在 ticker。
- **参考**:[Go Concurrency Patterns](https://go.dev/blog/pipelines) — 推荐"goroutine 内 defer wg.Done + recover"
- **风险**:用户后续在 runFamily 内引入新逻辑(如网络抖动重试)时易触发 panic,DDNS 静默停止同步,告警标志靠 LAST_DDNS_FAILED 才用户可见。
- **修复方案**:在 `runFamily` 头部加 `defer func(){ if r := recover(); r != nil { ... write result{err: ...} } }` + `defer wg.Done()`,保证 panic 也能 release slot。
- **工作量**:0.2d

### [SEVERITY-MED] ISSUE-Q1-02:`Sync.stopCh` 关闭协议不完整 — Stop 后 Run 已退出但 stopCh 仍是 closed,第二次 Stop 会 select-default 跳过但不再通知
- **位置**:[sync.go:305-312](file:///opt/ikev2-panel-v2-main/internal/ddns/sync.go#L305-L312)
- **问题**:`Stop()` 用 `select default` 防止 double close。但 Run() 退出后(因 ctx.Done 或 stopCh),`stopCh` 仍保留 closed 状态 — 此时任何外部代码误以为"再调一次 Stop()"会失效,但 ticker 已在 Run 内退出。当前 main.go 只在程序退出时调用,实际不会触发,但 `Sync` 实例若被复用(如测试场景)会留下副作用。
- **参考**:典型 close-channel-as-signal 反模式
- **风险**:测试场景下测试套件复用 Sync 实例会失效。当前 main.go 不会触发。
- **修复方案**:改 `sync.Once` + `stopCh := make(chan struct{})` 仅由 Once 关闭;或干脆只用 `ctx.Done()`,移除 stopCh(框架已提供 cancel)。
- **工作量**:0.1d

### [SEVERITY-MED] ISSUE-Q1-03:`Manager.findSAUniqueIDs` 新建 sess2 但不参与 reloadMu,Terminate + ReloadAll 并发时可能破坏 govici session 状态
- **位置**:[terminate.go:92-99](file:///opt/ikev2-panel-v2-main/internal/swanctl/terminate.go#L92-L99)
- **问题**:`Terminate()` 调用 `m.findSAUniqueIDs(ctx, sess, username)` — 该函数额外开 `sess2`,但 `sess` 是 `openSession()` 出口的 session。注释说"通过独立 session 调用避免锁死",但实际没加任何锁:`ReloadAll` 在 reloadMu 保护下会调用 `swanctl --load-all`(进程外操作),跟 `sess.Call("terminate", ...)` 走独立 unix socket。理论上是安全的,但强 Sown `swanctl --load-all` 期间会卸载所有连接,`terminate` 命令的 `ike-id` 可能无效(`ike-id` 在 reload 后被回收)。
- **参考**:strongSwan VICI 协议 spec — `terminate(ike-id=...)` 必须依赖 unique-id 当前仍存活。
- **风险**:reload 与 terminate 并发时,terminate 返回 `ike-id not found` 但 uniqueID 是从 reload 前的 list-sas 拿到的;`terminate` 失败但不算 fatal(用户断流是预期)。属设计妥协但应在代码注释中明示。
- **修复方案**:Terminate 调用前 acquire reloadMu(release before Call),或 doc-comment 明确 "ReloadAll 与 Terminate 并发:后者可能失败,需 caller 重试"。
- **工作量**:0.1d

### [SEVERITY-LOW] ISSUE-Q1-04:`loadActiveSAs` 调用 swanctl.ListSAs 不带 timeout,home 页请求可能因 charon 半挂而 block 10s
- **位置**:[handlers_home.go:106-109](file:///opt/ikev2-panel-v2-main/internal/web/handlers_home.go#L106-L109)
- **问题**:首页渲染同步调用 `s.Swanctl.ListSAs(ctx)`,虽然 `ListSAs` 内部 `withTimeout(ctx, ListSAsTimeout)` (10s),但 10s 在 HTTP 请求里仍是 UX 问题:用户刷新首页可能 10s 才出来 — 这跟其他 Phase 1 报告中的"用户体验卡顿"类问题重复度不高,属于功能问题。
- **风险**:单用户 / 单管理员场景下,首页刷新可能慢 10s。
- **修复方案**:home 页用 `goroutine + 5s timeout` 或缓存 30s 的 SA 列表;短期可在 `loadActiveSAs` 加 context.WithTimeout(parent, 3s) 截断。
- **工作量**:0.2d

### [SEVERITY-LOW] ISSUE-Q1-05:全局 `go test -race ./...` 无法在本环境执行,缺少 gcc
- **位置**:系统层(`/usr/bin/gcc` not found)
- **问题**:`cgo` 需要 gcc。审计沙箱无 gcc,`go test -race` 全部 `FAIL` 在 build 阶段。`-race` 是并发安全最直接的探测手段,缺它等于这一维度的核心验证手段失效。
- **风险**:CI / Docker 镜像构建阶段必须包含 gcc 才能跑 race;当前 Dockerfile 是否包含 gcc 未确认(Phase 1 docker 报告未单列)。
- **修复方案**:在 Dockerfile `RUN apt-get install -y gcc libc6-dev` 给 race-test 阶段(单独 target `runner-test`),生产镜像不含 gcc。
- **工作量**:0.1d

### Q1 小结

- 已知 Race 风险点:Stop + panic 在 dual-tick goroutine 中的部分细节未完整测试。
- 整体并发安全设计**结构合理**:`swanctl.Manager.reloadMu`(单写锁)、`installtoken.Store.mu`(`Mutex` 而非 `RWMutex` — 单写多读但并发是 admin 操作,合理)、`panelstate.Store.RWMutex`(读多写少,选 RWMutex 合理)、`FlashStore.Mutex`、`Sync.Mutex`(单写读多 — 这里其实可以换 RWMutex,但 Sync 锁内只是读 cfg+lastSyncTime map,Mutex 完全够用且避免 RWMutex 误用)。
- `loadActiveSAs` 走共享 `Swanctl.ListSAs`,该函数独立 openSession 每次新建 unix socket,无共享状态,理论 race-free。
- 唯一**结构性可疑**:`runFamily` 中 `results <- result{...}` 是 unbuffered-cap buffered 通道(buffer=2)。若 panic 在 `wg.Done()` 之前发生,既不 Done 也不 send,WaitGroup 泄漏。本环境 `-race` 不可跑,无法 race-detect 确认。

---

## Q2 资源泄漏

### [SEVERITY-HIGH] ISSUE-Q2-01:cmd/ikev2-panel/main.go 启动 6 个后台 goroutine(collector / expiry / le-certcheck / ipv6watch / ddns / token-sweep)+ HTTPS server goroutine + SIGHUP goroutine,全部没有统一的 WaitGroup / errgroup
- **位置**:[main.go:413-540](file:///opt/ikev2-panel-v2-main/cmd/ikev2-panel/main.go#L413-L540)
- **问题**:背景 goroutine 各自 `go func() { ... }()`(无命名),通过 `rootCtx.Done()` 通知退出。但有几个问题:
  1. `httpSrv.Shutdown` 后,`ddnsSync.Run` / `collector.Run` / `expiry.Run` 是否真的返回?**没有 wait** — `Shutdown` 超时 5s 就直接 `logger.Info("bye")` 退出 main,后台 goroutine 可能仍在写入 DB / swanctl,造成数据丢失(已通过 audit-2026-09-net 的 docfile 分析过类似问题)。
  2. `ddnsSync.Run` 内 goroutine(dual 模式下 fork v4 / v6 worker)— 这些 worker 不在 main.go 的 wait 范围。
  3. SIGTERM 时若有活跃的 swanctl reload(`swanctl --load-all` ~500ms),`exec.CommandContext` 收到 SIGTERM → charon 子进程被 kill,但 `Manager.ReloadAll` 返回的 error 没被 log,因为 Run 已退出。
- **参考**:[signal.NotifyContext](https://pkg.go.dev/os/signal#NotifyContext) + [http.Server.Shutdown](https://pkg.go.dev/net/http#Server.Shutdown) 的正确用法
- **风险**:container stop 时可能有 swanctl 半截 reload 导致 conf.d 不一致(下个启动需 reloadAll 才恢复)。
- **修复方案**:在 main.go 用 `sync.WaitGroup` 跟踪所有 background goroutine,`Shutdown` 后 `wg.Wait()` 5s 再强制 exit。
- **工作量**:0.3d

### [SEVERITY-MED] ISSUE-Q2-02:`Manager.openSession` 创建的 vici.Session 在 terminate 多 SA 路径中未显式 close(成功路径 OK,失败路径 leak)
- **位置**:[terminate.go:33-84](file:///opt/ikev2-panel-v2-main/internal/swanctl/terminate.go#L33-L84)
- **问题**:`Terminate()` 通过 `defer sess.Close()` 关闭自己开的 sess;但内部 `findSAUniqueIDs` 另开 `sess2` 也 defer close。两个 session 都正确关闭。然而 **ListSAs() 内部** 对 `m.openSession()` 也只走 `defer sess.Close()` — 是 OK 的。但 `ReloadAll` 路径的 `cmd` 没显式 cancel ctx,`defer cancel()` 已在调用前,实际 OK。
- **风险**:几乎无 — 但 `openSession` 在 SkipVici=true 时返回 `(nil, nil)` — `sess.Close()` 对 nil 是 no-op,但 defer 仍会触发。属于防御性编程 OK。
- **修复方案**:无。
- **工作量**:0d

### [SEVERITY-MED] ISSUE-Q2-03:`swanctl.AtomicWriteFile` 的 tmpRemoved 标志模式有缺陷 — `f.Close()` 失败后 tmpRemoved 仍保持 false → defer 删 tmp,但 Close 后 rename 用同一 tmp 名(rename 已成功但 Close 错),最终 tmp 被错误删除
- **位置**:[atomic.go:65-88](file:///opt/ikev2-panel-v2-main/internal/swanctl/atomic.go#L65-L88)
- **问题**:rename 成功 → `tmpRemoved = true` → defer 不删。rename 失败 → tmpRemoved = false → defer 删 tmp。但 Close 失败(f.Sync 成功后)时,代码已 return,此时 `tmpRemoved = false`,defer 删 tmp — 但 tmp 实际已被 fsync 过且 rename 还没调,**这是预期行为**:Close 失败不应 rename。但 `os.Remove(tmp)` 在 rename 之前是 OK 的(remove + rename 不冲突)。
  实际 race:`f.Close()` 错后 tmp 还没 rename,defer Remove 删 tmp(OK);但 `tmpRemoved` 没设,defer 不删 — 是预期行为。**但**:`tmpRemoved` 标志模式在 `Sync()` 之后(`f.Sync()` 成功)、`Close()` 失败、`return` 时,`defer` 会执行 Remove — 但 tmp 还在(Close 失败保留文件),正确。
  再细看:`f.Close()` 失败后**应该不 rename**(避免 rename 半关闭的 fd),代码确实 return 了。OK。
  **真正问题**:race detector 不可跑的情况下,无法 100% 验证清理逻辑在 panic / 并发场景下正确。
- **风险**:可观测风险低,但代码可读性差 — `tmpRemoved` 布尔+defer 模式可改为更显式的"先写 tmp,最后才原子 rename,中途失败用 defer cleanup"统一骨架。
- **修复方案**:重构为 `defer cleanup { if renameDone { return } os.Remove(tmp) }`,简化逻辑。
- **工作量**:0.2d

### [SEVERITY-MED] ISSUE-Q2-04:`cmd/ikev2-panel/main.go` 启动顺序中,Reloads 在 `EnsureDefaultAdmin` 之前 — 若 EnsureDefaultAdmin 失败退出,已调过 ReloadAll 但用户没生成
- **位置**:[main.go:68-107](file:///opt/ikev2-panel-v2-main/cmd/ikev2-panel/main.go#L68-L107)
- **问题**:第 71 行 `scm.ReloadAll(ctx)`,然后 95 行 `EnsureDefaultAdmin` 失败 → `os.Exit(1)`。ReloadAll 已跑一次(影响 500ms 活跃 SA),但 admin 还没创建。这种情况极少(只会在 bcrypt 失败 / DB 故障时),属边缘。
- **风险**:极低 — admin 失败退出时 container restart 会重跑 ReloadAll,无害。
- **修复方案**:无。
- **工作量**:0d

### [SEVERITY-LOW] ISSUE-Q2-05:`panelstate.WriteAliyun` 的 tmp cleanup `defer` 用 stat 检查 → 关闭后 rename 成功但 tmp 仍存在(stat 成功),defer Remove 删 tmp — 但 tmp 已被 rename 成 target!
- **位置**:[state.go:150-156](file:///opt/ikev2-panel-v2-main/internal/panelstate/state.go#L150-L156)
- **问题**:`os.CreateTemp` 创建 tmp → write → chmod → close → `os.Rename(tmpName, target)` — rename 后 tmp 文件不再以原 tmpName 存在(同一 inode 移到 target)。然后 defer 检查 `os.Stat(tmpName)`:如果 Stat 成功(理论上不应,因为已 rename),就 Remove(tmpName)— 这会删 target!
  实际:rename 之后 Stat(tmpName) → `no such file or directory` → Remove 不执行。OK。
  但代码意图模糊;且 `os.CreateTemp` 默认权限 0600 — 这里后续又 `tmp.Chmod(0o600)`,多余但无害。
- **风险**:低 — 实际 OK,只是 cleanup 逻辑语义不清。
- **修复方案**:用 `tmpRenamed bool` 标志替代 stat 检查(同 ISSUE-Q2-03 模式)。
- **工作量**:0.1d

### Q2 小结

- DB 连接:`sql.Open` 后 `defer st.Close()` ✓ — 但 `Open` 失败时 `_ = db.Close()` 已关闭,OK。
- 文件句柄:AtomicWriteFile + cleanup defer ✓ — 模式 OK。
- HTTP server Shutdown:有 5s timeout ✓ 但 **未 wait background goroutines**(ISSUE-Q2-01)。
- Goroutine 退出:基本走 ctx.Done(),但 IPv6Watch 跟 DDNS sync 的嵌套 goroutine 在 SIGTERM 时可能 still running。

---

## Q3 错误处理

### [SEVERITY-HIGH] ISSUE-Q3-01:阿里云 RAM 错误码匹配过宽,会把成功响应里偶然出现的子串误判为错误
- **位置**:[sync.go:554-558](file:///opt/ikev2-panel-v2-main/internal/ddns/sync.go#L554-L558)
- **问题**:
  ```go
  if strings.Contains(err.Error(), "RecordNotBelongToRAM") ||
      strings.Contains(err.Error(), "Forbidden") {
  ```
  Phase 1 summary 已列 #8 提到的 `RecordNotBelongToRAM` 不存在 → 但 `Forbidden` 极宽泛:**任何 aliyun 错误信息里出现 "Forbidden" 子串就立即终止 retry** — 但 alidns 也用 `Forbidden.RAM`、`Forbidden.UA`、`Forbidden.User` 等区分,这里一刀切,可能把"限流"也当作权限错(限流 API 错误码是 `Throttling.User` 不是 `Forbidden`,但 alidns 可能 throw `Forbidden.Throttling` 这种变体,需要确认)。
  更严重的是:成功响应里**根本不会**有 "Forbidden" 子串 — 但错误信息拼接时,如果 `rec.RecordID` 或 `value` 含 "Forbidden"(正常不会,但用户把 RR 设为 "forbidden-test" 也可能),`err.Error()` 就会匹配上 — 这就 bug 了。
- **参考**:`errors.Is(err, target)` / 自定义 `var ErrRAMDenied = errors.New(...)` 后用 `if errors.Is(err, ErrRAMDenied)`
- **风险**:**误判权限错** → 永久不重试 → DDNS 真正失败 → 用户域解析失效,VPN 用户连不上。
- **修复方案**:自定义错误类型 `type AliyunAPIError struct{ Code, Message string }`,用 `errors.As` 解出 Code,在 retry 决策时白名单 `["Forbidden.RAM", "Forbidden.User", "InvalidAccessKeyId.NotFound"]`,其余 retry。
- **工作量**:0.3d

### [SEVERITY-MED] ISSUE-Q3-02:`Sync.upsertRecord` retry sleep 是同步 sleep,影响其他 family 在 dual 模式的并发性
- **位置**:[sync.go:545-564](file:///opt/ikev2-panel-v2-main/internal/ddns/sync.go#L545-L564)
- **问题**:retry 逻辑用 `time.Sleep(time.Duration(attempt*attempt) * time.Second)`(1s / 4s / 9s)— 这是**单 family 内部**的退避,不影响另一 family。但当 v4 第三次 retry 进入 9s 睡眠时,v6 已成功完。整个 tick 仍在等 wg.Wait() 阻塞。
  更深层问题:`runFamily` 把 throttle 时间戳(`s.lastSyncTime[rt] = time.Now()`)设在 retry **之前** — 即使 retry 全部失败,throttle 时间也已设置,下次 tick 跳过节流,看似 OK;但 retry 期间的 throttle 不影响 v6 节流(独立 map key)→ OK。
  但 **未处理 ctx.Done**:`time.Sleep` 期间 ctx 取消 → 还在睡。`upsertRecord` 的 ctx 是 `tick()` 入口传入的 `ctx`,但 `Run()` 入口 ctx 是 rootCtx — SIGTERM 时整个 retry 链 sleep 9s 才退出,期间 DB / swanctl 子进程可能已被 main 关闭。
- **参考**:`time.NewTimer + select ctx.Done()` 模式
- **风险**:SIGTERM 后 main 等 5s → DDNS goroutine 仍在 sleep 9s → `Shutdown` 5s 后强制 exit,goroutine 被丢弃(无 wait)。
- **修复方案**:把 `time.Sleep(attempt*attempt * time.Second)` 替换为 `select { case <-time.After(...): case <-ctx.Done(): return ..., ctx.Err() }`。
- **工作量**:0.2d

### [SEVERITY-MED] ISSUE-Q3-03:`loadActiveSAs` 把 ListSAs 错误吞到 `Debug`,首页无任何视觉提示用户"Swanctl 出错"
- **位置**:[handlers_home.go:106-109](file:///opt/ikev2-panel-v2-main/internal/web/handlers_home.go#L106-L109)
- **问题**:`s.Logger.Debug("home: list SAs failed", ...)` — debug 级别,生产默认 `info` 看不到。用户访问首页看到 "活跃 SA: 0" 时以为是真实的,实际是 charon 故障。
- **参考**:首页监控面板的"异常路径"应至少在 homeData 里暴露一个布尔(可前端渲染警示)。
- **风险**:管理员失去对 charon 故障的可见性,依赖命令行日志才发现。
- **修复方案**:homeData 加 `SwanctlUnreachable bool` 字段,loadActiveSAs 返回 error 时置 true + 渲染模板加 banner。
- **工作量**:0.2d

### [SEVERITY-MED] ISSUE-Q3-04:`aliyun.call` 在 `json.Unmarshal` 后对业务错误只取 `Code != ""` 判断,但 `apiResp.Body` 字段声明却没赋值,result 反序列化仍是 `nil` 字段
- **位置**:[aliyun.go:202-219](file:///opt/ikev2-panel-v2-main/internal/dns/aliyun.go#L202-L219)
- **问题**:
  ```go
  var apiResp struct {
      Code    string          `json:"Code"`
      Message string          `json:"Message"`
      Body    json.RawMessage `json:"-"`
  }
  ```
  `Body` 字段用 `json:"-"` 显式排除 → 永远不会被填充。然后 `json.Unmarshal(body, result)` 第二次反序列化,但此时 `result` 接收整个 body 也包含 `Code/Message`,会覆盖结构体已有字段(若 result 里也定义了同名字段)。
  当前 `result` 是 `{DomainRecords: {Record: []Record}}`,跟 `Code/Message` 字段不冲突,OK。但**语义不清**:Body 字段是 dead code,反序列化是浪费,容易让维护者误以为有"先解析 envelope,再解析 body"的逻辑。
- **风险**:低,但代码质量差。
- **修复方案**:删除 `Body` 字段,直接 `json.Unmarshal(body, &apiResp)` 后根据 Code 分支。
- **工作量**:0.1d

### [SEVERITY-MED] ISSUE-Q3-05:`swanctl.AtomicWriteFile` 的 `f.Sync()` 错误处理:write 已落 disk 但 fsync 失败时,f.Close() 没在 Sync 失败后显式调用 — defer 也不存在
- **位置**:[atomic.go:71-79](file:///opt/ikev2-panel-v2-main/internal/swanctl/atomic.go#L71-L79)
- **问题**:`f.Sync()` 失败 → `_ = f.Close()` 显式调用 → return。此时 f 没 close 但 defer 也没,后续如果有重试逻辑复用 f 会出错 — 不过代码是 return,无人复用,**OK**。但 `_ = f.Close()` 吞了 close 错误(可能泄漏 fd)— 极端场景(磁盘满)Close 失败留下 fd。
- **参考**:`defer f.Close()` 模式
- **风险**:极低 — fd 数量达到 ulimit 才出问题。
- **修复方案**:用 `defer f.Close()` 包整个块。
- **工作量**:0.1d

### [SEVERITY-LOW] ISSUE-Q3-06:`home` handler 中 `admin, ok := AdminFrom(...)` 后 `sess, _ := SessionFrom(...)` 忽略 ok
- **位置**:[handlers_home.go:47-52](file:///opt/ikev2-panel-v2-main/internal/web/handlers_home.go#L47-L52)
- **问题**:`admin, ok := AdminFrom(...)` 正常情况下 requireSession 中间件已确保 ctx 里有 admin,`ok=false` 属编程错误。代码返回 500。OK。
  `sess, _ := SessionFrom(...)` 之后传给模板的 CSRFToken — sess 可能 nil(理论上不会) → `csrfTokenOf(nil)` 返回空串(防御性 OK)→ 模板渲染空 token → POST 触发 CSRF mismatch → 用户卡住。
- **风险**:理论性,实际上不可能(requireSession 中间件已保证)。
- **修复方案**:无。
- **工作量**:0d

### Q3 小结

- `_ =` 吞 error 的位置共 **36 处**(grep 统计)— 多数是 HTTP write 后的 `_ = w.Write(...)`(连接已断,无害)和 test fixture(测试用 OK)。
- 真问题:`swanctl/openSession` 后续错误处理、`aliyun.go` 错误码 string match(ISSUE-Q3-01)、`sync.go` retry sleep 不响应 ctx(ISSUE-Q3-02)。
- `errors.Is` 使用:**8 处**(auth、store、web handler)— 覆盖良好。
- `errors.As` 使用:**0 处** — Aliyun API error 应改成自定义类型用 errors.As。
- `log.Print(err)`:**0 处** — 全用 slog,良好。

---

## Q4 时区 / 时间

### [SEVERITY-MED] ISSUE-Q4-01:`time.Now().Unix()` 与 `time.Now().UTC().Unix()` 混用 — DB 存的是 local unix,但 aliyun API 用 UTC
- **位置**:
  - store:[users.go:22](file:///opt/ikev2-panel-v2-main/internal/store/users.go#L22),[users.go:94](file:///opt/ikev2-panel-v2-main/internal/store/users.go#L94),[admins.go:17](file:///opt/ikev2-panel-v2-main/internal/store/admins.go#L17),[admins.go:56](file:///opt/ikev2-panel-v2-main/internal/store/admins.go#L56)
  - web:[handlers_auth.go:86-87](file:///opt/ikev2-panel-v2-main/internal/web/handlers_auth.go#L86-L87)
  - ddns:[sync.go:607](file:///opt/ikev2-panel-v2-main/internal/ddns/sync.go#L607) — 用 UTC,OK
  - expiry:[expiry.go:66](file:///opt/ikev2-panel-v2-main/internal/expiry/expiry.go#L66)
- **问题**:`time.Now().Unix()` 返回的是**绝对时间戳**(epoch seconds)— 与时区无关,UTC vs local 输出**相同数字**。所以严格意义上"DB 存的是 local unix"是错的:**Unix() 跟 UTC 等价**(都返回 epoch seconds)。
  但 `time.Now().UTC().Format(...)` 与 `time.Now().Format(...)` 在 Asia/Hong_Kong 主机下输出**差 8 小时**!涉及 **字符串展示**的代码:[sync.go:607](file:///opt/ikev2-panel-v2-main/internal/ddns/sync.go#L607) ✓ 已用 UTC;但 [handlers_ddns.go:58](file:///opt/ikev2-panel-v2-main/internal/web/handlers_ddns.go#L58) 用 `last.Time.UTC().Format` ✓;[handlers_home.go:190](file:///opt/ikev2-panel-v2-main/internal/web/handlers_home.go#L190) ✓;GeneratePassword 测试用 UTC — OK。
- **结论**:Unix() 调用一致,UTC.Format() 调用一致,**无 bug**。
- 但有 1 个隐患:docker 容器默认时区 UTC(Asia/Hong_Kong 用户操作 container 时如果 mount /etc/localtime 到 host 可能变成 HKT),`time.Now()` 在两种时区下 Unix() 一致,但 Format(layout) 会差 8 小时。当前所有展示路径均显式 UTC ✓。

### [SEVERITY-LOW] ISSUE-Q4-02:`cert.GeneratePassword` rejection sampling 的边界条件写错了,实际不构成 rejection
- **位置**:[password.go:42-49](file:///opt/ikev2-panel-v2-main/internal/auth/password.go#L42-L49)
- **问题**:
  ```go
  for _, b := range buf {
      if int(b) < int(n)*256/int(n) { // 永远成立,下面改成严格 rejection
          if b < n {
              out = append(out, alphabet[b])
              ...
  ```
  `int(n)*256/int(n)` = 256(整数除法) → `b < 256` **永远成立**。**注释自己承认**"永远成立,下面改成严格 rejection" — 但代码并未改!实际行为:**完全没有 rejection sampling**,直接 `b < n` 取字节 → 模偏严重。
  alphabet 长度 = 32 (`"abcdefghijkmnpqrstuvwxyz23456789"`,注释说是 56 字符其实是 32 — 注释和代码不符)。
  n = 32。b 在 [0, 256)。`b < 32` 概率 = 32/256 = 12.5% — 其余 75% 浪费但不影响随机性(只是低效)。**不构成模偏**,因为被丢弃的字节根本没参与 sampling。
  但 **注释说"避免模偏",实际避免了** — 是巧合,因为 `b < n` 这个早退出分支里 n=32 严格 ≤ 256,且 `n` 是 2 的幂附近的 32(实际 32 = 2^5)— 任意 `b` < 32 直接取。
  严格地说:rejection sampling 应丢弃 b ≥ n,这里直接 break inner loop 在 `b < n` 时 append — 实现是正确的,只是注释和变量名误导。
- **风险**:行为正确(密码生成 OK),但代码可读性差 + 注释与实现不符。
- **修复方案**:删除注释 "永远成立,下面改成严格 rejection",改为 "直接 early-return 跳过 b ≥ n 的字节,等概率采样(因 n | 256 不严格成立,但 n=32 ≤ 256 单字节足够)。若 alphabet 长度未来变 > 256,需严格 rejection"。
- **工作量**:0.1d

### [SEVERITY-LOW] ISSUE-Q4-03:`expiry.Checker` 的周期是 60s,但 `GetExpiredUsers` 用 `now := time.Now().Unix()` 后立即传给 SQL — 单调时间戳,在两次查询间一致
- **位置**:[expiry.go:66-67](file:///opt/ikev2-panel-v2-main/internal/expiry/expiry.go#L66-L67)
- **问题**:当前 60s 周期 + tick 内 `now` 立即用 OK,但如果周期改短到 < 100ms 而 SQL 查询耗时 > tick 周期,可能并发跑 — 当前没这问题。
- **风险**:低。
- **修复方案**:无。
- **工作量**:0d

### [SEVERITY-MED] ISSUE-Q4-04:`cert/generate.go` 的 `NotBefore = time.Now().Add(-1 * time.Hour)` — 容忍时钟偏差,但 `NotAfter = time.Now().Add(certValidity)` 没容忍正向时钟漂移
- **位置**:[generate.go:156-157, 205-206](file:///opt/ikev2-panel-v2-main/internal/cert/generate.go#L156-L157)、[generate.go:205-206](file:///opt/ikev2-panel-v2-main/internal/cert/generate.go#L205-L206)
- **问题**:NotBefore 设当前时间前 1 小时(容忍主机时钟慢导致 cert 立即生效),但 NotAfter 没设当前时间前 N 小时(若主机时钟快,cert 过期比预期早)。
  这是 [RFC 5280](https://datatracker.ietf.org/doc/html/rfc5280) 的标准做法 — 但应**同时**为两端加 skew tolerance。
- **风险**:主机时钟漂移几分钟 → cert 提前过期 — 对 10 年期证书无影响(2026-09 → 2036-09),但 LE 90 天证书会在边界场景触发"刚签发就过期"。
- **修复方案**:`NotBefore = now.Add(-1h)`,`NotAfter = now.Add(certValidity).Add(-5*time.Minute)` — 容忍 ±5min 漂移。注:不在 LE 路径(acme.sh 自己处理)。
- **工作量**:0.1d

### Q4 小结

- UTC vs local:**Unix() 一致**(都是 epoch);**Format(layout) 差 8h**;所有展示路径已用 `.UTC()` ✓。
- Cron / 周期:无 cron(`expiry.Run` 用 ticker),无 cron 表达式。
- 证书时间:有 1h skew tolerance 在 NotBefore,NotAfter 没有 → ISSUE-Q4-04。

---

## Q5 重启恢复

### [SEVERITY-MED] ISSUE-Q5-01:容器重启后,DDNS throttle 时间戳丢失 → 重启后第一个 tick 立即跑(没节流),可能触发阿里云限流
- **位置**:[sync.go:166](file:///opt/ikev2-panel-v2-main/internal/ddns/sync.go#L166),`lastSyncTime: make(map[string]time.Time)`
- **问题**:`lastSyncTime` 是纯内存 map,重启丢失。设计妥协"重启后立即跑一次"已在 Phase 1 audit-alidns 中讨论;但若用户重启前刚 upsert 过,重启后 1s 内立即又 upsert(若 IP 变),会触发阿里云"短时间内多次修改"限流。
- **参考**:aliddns Throttling.User(单用户 30 QPS)
- **风险**:重启恰好赶上 IP 变化 → 阿里云 429 → DDNS 失败标志 → 用户告警。
- **修复方案**:把 `lastSyncTime` 持久化到 statefile(同一 file 多加两行 `last_sync_a=epoch` `last_sync_aaaa=epoch`)。
- **工作量**:0.2d

### [SEVERITY-MED] ISSUE-Q5-02:`Manager.ReloadAll` 在 startup 已被调用一次,但容器重启后 charon 可能还没启动完全 → first ReloadAll 失败,但 log 只说 "VARN not ok"
- **位置**:[main.go:71-76](file:///opt/ikev2-panel-v2-main/cmd/ikev2-panel/main.go#L71-L76)
- **问题**:启动序列是 `swanctl.New()` → `ReloadAll(ctx)`(没等 charon ready)→ 后续 cron goroutine 才启动。strongSwan Docker entrypoint 通常先跑 `charon` 再启 Go 进程,但**如果 main.go 在 entrypoint.sh 完成前被启动**(docker-compose 没设 depends_on 且 Go 在后台启动),ReloadAll 会因 charon.vici socket 不存在而失败。
  代码用 `if m.SkipVici { return nil }` 但 SkipVici 只在 dev 模式设,生产不会 skip。
- **风险**:首次 ReloadAll 失败 → 主配置未加载 → 所有用户拨号失败,但日志只有 "swanctl --load-all at startup" warn。
- **修复方案**:在 main.go 启动时增加 `/var/run/charon.vici` socket-wait(最多 30s,带退避),wait OK 再 Run。
- **工作量**:0.3d

### [SEVERITY-MED] ISSUE-Q5-03:restart 后 `installtoken.Store` 和 `FlashStore` 都是空 map → 新 token 正常,但重启前用户的 flash 永久丢失
- **位置**:[main.go:339-341](file:///opt/ikev2-panel-v2-main/cmd/ikev2-panel/main.go#L339-L341)
- **问题**:重启后用户被重定向到 /users 的 redirect 链接会丢失 flash(因为 redirect 后页面还没渲染就 reboot)。
- **风险**:用户被告知"用户已创建"但实际 flash 已丢 — 用户刷新看不到密码 → 困惑(密码在哪?)。
- **修复方案**:无 — 当前是 in-memory design,有合理理由。
- **工作量**:0d(设计妥协,标记为"已知")

### [SEVERITY-MED] ISSUE-Q5-04:重启后 LE 续签 cron 的续签状态(`/data/le/LAST_RENEW_FAILED`)不持久化到 panelstate / database
- **位置**:[acme.go:36-44](file:///opt/ikev2-panel-v2-main/internal/cert/acme.go#L36-L44)
- **问题**:`LAST_RENEW_FAILED` 是 acme.sh 写盘的 marker,确实持久化(在 `/data/le/`)✓ — 但 Go 端 `CertMode == "letsencrypt"` 的判断依赖 `cfg.CertMode`(启动 env)— 如果用户在 Go 运行中改 env(几乎不可能,但 docker restart 重新读 env 是新 CertMode)→ CertMode 可能从 letsencrypt 变 self-signed → LE 告警路径关闭。
  实际 docker compose 重启会读新 env,逻辑 OK。属设计妥协。
- **风险**:无。
- **修复方案**:无。
- **工作量**:0d

### [SEVERITY-MED] ISSUE-Q5-05:`main.go:222-249` 的 v2-76 EAP recovery 逻辑只 backfill **enabled 的用户**,但密码字段 DB 里的值(`u.Password`)是明文,直接写 conf.d — 这意味着 DB 损坏时密码也丢失
- **位置**:[main.go:229-249](file:///opt/ikev2-panel-v2-main/cmd/ikev2-panel/main.go#L229-L249)
- **问题**:v2-76 的 recovery 是"DB 在,重写 conf.d"。但如果 conf.d 被清空(用户场景:容器重 build,挂载卷还在)— DB 在,conf 写。OK。
  但若 DB 在,conf 在,但 charon 没 reload(`/etc/swanctl` 是 container 内的非持久化目录)— recovery 会写 conf 到 `confDir`(`/etc/swanctl/conf.d`),confDir 在容器内层,Rebuild 后丢失(虽然 v2-76 注释已说明 "容器内 conf.d 不持久化")。
  真正问题:conf.d 重写后 `ReloadAll` 失败(见 ISSUE-Q5-02),recovery 失败但用户没感知。
- **风险**:首次启动失败 → reload 失败 → 后台 goroutine 持续尝试 → 但 LE / DDNS 正常运行,只有 VPN 用户连不上。
- **修复方案**:跟 ISSUE-Q5-02 一起解决。
- **工作量**:0d

### [SEVERITY-LOW] ISSUE-Q5-06:重启后 `prevBytes` map(`Collector`)丢失 — 重启后第一次采集的 delta 是绝对值,会被累加进 `bytes_in_total`
- **位置**:[collector.go:44-47](file:///opt/ikev2-panel-v2-main/internal/limit/collector.go#L44-L47)
- **问题**:代码注释明确承认"重启后第一次采集只能记录'重启后至今'的增量——这是设计妥协"。Collector.go:135-138 first-sample 把 `deltaIn = currentIn`(把当前 SA 累计字节全算入增量)— 若某用户重启前已用 100GB,重启后 5 分钟又用 1GB,会被记为 1GB,正确;但**重启前流量无法累加** — 设计妥协,**正确但有数据缺口**。
- **风险**:监控数据精度问题,不影响功能。
- **修复方案**:无。
- **工作量**:0d

### Q5 小结

- charon reload:`main.go` 启动时 ReloadAll,但**未等 charon ready**(ISSUE-Q5-02)。
- DDNS state:`lastSyncTime` 不持久化(ISSUE-Q5-01)。
- 用户密码:DB 持久化 + conf.d 启动 backfill ✓(但靠 DB 明文,Phase 1 #6 已记录)。
- SIGHUP 路径:✓ — `reloadPanelTLSCert` 实现良好。

---

## Q6 边界条件

### [SEVERITY-MED] ISSUE-Q6-01:IPv4-only / IPv6-only / dual 三种 family 在 DDNS 内的 `tick()` 全部分支未对**空 IP + 非空 IP 错误**组合做细致聚合
- **位置**:[sync.go:397-462](file:///opt/ikev2-panel-v2-main/internal/ddns/sync.go#L397-L462)
- **问题**:`tick()` 在 dual 模式下:
  - v6IP == "" 且 v6Err == nil → detector 返回 ("", nil)(如 IPv4-only 主机无 v6)→ `runFamily` 收到空 IP,不发 upsert,只 send result{err: nil, newIP: ""} → 聚合 ls.V6IP = "" → `ls.Success` 不算 v6 成功。OK。
  - v6IP != "" 且 v4IP == "" 且 v4Err == nil(IPv6-only)→ 类似,只算 v6 成功。OK。
  - v6IP == "" 且 v6Err != nil → ls.V6Error = err,ls.V6IP = "" → `ls.Success` 仅看 v4。OK。
  - v6IP != "" 且 v6Err != nil → ls.V6Error = err,**但 ls.V6IP 已被赋值为 newIP**!此时 ls.V6IP 反映"探测到的 IP"但 ls.V6Error 反映"upsert 失败",用户从面板看到"v6 IP = 2001:db8::1 但 v6 错误 = ..." → 困惑。
- **风险**:面板 UI 在 dual 模式下渲染异常组合 → 用户困惑。
- **修复方案**:v6IP != "" 且 v6Err != nil 时,ls.V6IP 保留但加额外字段 `V6UpdateFailed bool`(或 `V6IPUpdated bool`)。当前 `LastSync` 结构体已有 V6IP / V6Error,正确做法是在 `ls` 增字段。
- **工作量**:0.2d

### [SEVERITY-MED] ISSUE-Q6-02:`DetectGlobalV6` 在 `Iface=""` 时遍历**所有接口**,但跳过 deprecated / link-local / ULA / loopback — 主机上若有多个 v6 接口(如 docker0 + eth0 + tunl0),只取第一个找到的 global v6
- **位置**:[ipv6watch.go:146-205](file:///opt/ikev2-panel-v2-main/internal/swanctl/ipv6watch.go#L146-L205)
- **问题**:返回 first global unicast,**但不一定是用户期望的接口**(用户配置 `IKEV2_OUT_IF=eth0` 但 docker 启动时 iface 还是空,拿到的可能是 docker0 的 v6 — 但 docker0 不会有 2000::/3 global unicast,所以实际不会被命中)。
  真正的边界:**多 ISP 场景**(主机有 2 个有公网 v6 的接口,如 eth0 + eth1)— DDNS 同步哪个?当前取 first → 不可预测。
- **风险**:多 v6 接口场景下 IP 选错 → DDNS 同步错地址 → VPN 用户连不上。
- **修复方案**:如果 `iface` 配置了,严格只取该接口;若未配置,WARN + 取 first + log 详细信息让用户感知。
- **工作量**:0.3d

### [SEVERITY-MED] ISSUE-Q6-03:`DDNS sync.go` 在 `FindRecord` 找不到记录时只 WARN 不 upsert — 但 `UpdateRecordValue` 找不到 recordID(已删除)的场景没考虑
- **位置**:[sync.go:514-566](file:///opt/ikev2-panel-v2-main/internal/ddns/sync.go#L514-L566)
- **问题**:`upsertRecord` 流程:FindRecord → 比对 IP → UpdateRecordValue。如果 FindRecord 和 UpdateRecordValue 之间用户在阿里云控制台删除了 record → UpdateRecordValue 返回 "DomainRecordNotBelongToRAM" / "RecordNotFound" — 当前 retry 3 次都会失败(错误信息里没 "Forbidden" 也不立即停止 — 实际 retry 3 次)。
- **风险**:极端边界 — 用户手动删了记录,DDNS 会持续 3 次重试并最终写 LAST_DDNS_FAILED(但用户已经手动解决了)。
- **修复方案**:`UpdateRecordValue` 返回 error 时,检查 `Code == "DomainRecordNotBelongToRAM"` 或 `RecordId.NotFound` → 立即停止 retry,记 info 日志 "record gone, will retry next tick if user re-creates"。
- **工作量**:0.2d

### [SEVERITY-MED] ISSUE-Q6-04:`DetectGlobalV4` 探测目标 8.8.8.8 在大陆网络常被黑洞或限速,3s 超时不够
- **位置**:[ipv4watch.go:47-53](file:///opt/ikev2-panel-v2-main/internal/swanctl/ipv4watch.go#L47-L53)
- **问题**:默认目标 8.8.8.8 在中国大陆运营商常被 UDP 黑洞(不返 RST/ICMP)→ `d.Dial("udp", target)` 走 UDP 不实际发包,内核只做路由选择 → **不会触发黑洞**(只是 lookup),3s timeout 极少真触发。但若主机无 default route(初始启动 / IPv6-only 网络配置),`Dial` 立即 ENETUNREACH → 3s 内返回,但 retry 没做。
- **风险**:DDNS IPv4 启用但主机无 v4 路由 → 每次 tick fail → LAST_DDNS_FAILED 持续刷。
- **修复方案**:`DetectGlobalV4` 失败时退避 retry(同 tick 内 1 次,不跨 tick)。
- **工作量**:0.2d

### [SEVERITY-LOW] ISSUE-Q6-05:`Config.validate()` 对 `IPv6Only=true && ServerAddrV6=""` 不报错(注释说"启动后会由 caller 决定是否 fatal"),导致 mobileconfig 渲染失败但启动 OK
- **位置**:[config.go:155-158](file:///opt/ikev2-panel-v2-main/internal/config/config.go#L155-L158)
- **问题**:`Validate()` 对 IPv6-only + 无 v6 地址组合仅 log warn,容器启动 OK 但 mobileconfig 渲染时报 "ServerAddr missing"(handlers_config.go:29) → 用户点击下载 → 500 错误,体验差。
- **风险**:典型"启动 OK 但运行时失败"的边界。
- **修复方案**:`validate()` 在生产模式(非 dev)硬校验;dev 模式 keep 宽松。
- **工作量**:0.1d

### [SEVERITY-LOW] ISSUE-Q6-06:`aliddns` `FindRecord` 多条记录(用户误建)返回 error,但 `UpdateRecordValue` 不传 `RecordId`(只有一条时);如果用户在阿里云控制台建了 5 条同名记录,FindRecord 返回多条 error → 不更新任何一条,但用户的"应该更新哪条"无解
- **位置**:[aliyun.go:130-138](file:///opt/ikev2-panel-v2-main/internal/dns/aliyun.go#L130-L138)
- **问题**:已经处理多条情况(error)。但用户在面板看到 "multiple records found" 时,只能去阿里云手动删除多余 — UI 没给引导。
- **风险**:用户卡在面板。
- **修复方案**:UI 显示更具体的操作指引("请去阿里云控制台手动删除多余的 A/AAAA 记录,保留一条")。
- **工作量**:0.1d

### [SEVERITY-LOW] ISSUE-Q6-07:`upsertRecord` 找不到记录时仅 WARN,但面板 DDNS 卡显示"上次同步成功"(`Success=true` 因为 v4 或 v6 成功过)→ 用户误以为 A 记录也更新了
- **位置**:[sync.go:520-527](file:///opt/ikev2-panel-v2-main/internal/ddns/sync.go#L520-L527)
- **问题**:dual 模式下若 AAAA 成功但 A 没记录(返回 "", nil),ls.Success=true,ls.V4Error="" — 但实际 A 记录从未被 upsert。面板看不到这个区分。
- **风险**:用户以为 IPv4 DDNS 在跑,实际没记录。
- **修复方案**:`upsertRecord` 返回 `(oldIP, status)` 增加 `Status: "missing"` / `"updated"` / `"unchanged"`;LastSync 增加 `V4Status string` / `V6Status string`。
- **工作量**:0.3d

### Q6 小结

- dual / v4 / v6 边界:有 ISSUE-Q6-01(成功/失败组合展示不清)和 ISSUE-Q6-07(missing record 不区分)。
- 空配置:`Config.validate()` 部分宽松(IPv6-only 边界)。
- 凭证错:`FindRecord` 返回 `InvalidAccessKeyId.NotFound` 但当前代码走通用 retry → 见 ISSUE-Q3-01 修复后会好。
- 域未解析:不发生(API 调用,客户端不解析域)。

---

## Q7 acme.sh 集成

### [SEVERITY-MED] ISSUE-Q7-01:Go 端 `internal/cert/acme.go` 只读 LE 状态文件,**不触发 acme.sh**,也没有 retry 策略或退避
- **位置**:`internal/cert/acme.go` 全文件(66 行)
- **问题**:文件本身是 LE 状态读取(CheckLERenewStatus),没有 acme.sh 调用代码(由 entrypoint.sh + renew-cert.sh 负责)。但 Go 端对 `LAST_RENEW_FAILED` 的检测是**被动**(仅 stat 文件)— 续签失败持续 7 天也只每 60s log warn 一次(handlers_home.go:141)。
- **参考**:容器内 acme.sh cron 是 `0 3 * * * /etc/ikev2/renew-cert.sh`,默认每天 3am 跑;renew-cert.sh 内部有 retry(已查 docs/release-notes)。
- **风险**:LE 续签失败后,管理员没有"自动 retry 续签"机制 — 只能等下次 cron / 手动跑 entrypoint。
- **修复方案**:`acme.sh --renew --force` 由 Go 端在 14 天告警阈值时自动触发(代替"等下次 cron")。
- **工作量**:0.5d

### [SEVERITY-MED] ISSUE-Q7-02:LE 模式下,Go 进程启动时检查 `fullchain.pem` 是否存在,失败 `os.Exit(1)`(main.go:144-148)— 但 entrypoint.sh 应该已经跑了 acme.sh,如果 entrypoint 失败,容器重启循环
- **位置**:[main.go:144-149](file:///opt/ikev2-panel-v2-main/cmd/ikev2-panel/main.go#L144-L149)
- **问题**:Go 进程的 LE cert 依赖 entrypoint.sh 成功。若 entrypoint.sh 因域名 DNS 解析失败 / API 限流 / 网络问题 失败,Go 进程 `os.Exit(1)`,docker-compose restart policy 默认 `unless-stopped` → 容器反复重启,每次都重跑 entrypoint.sh(可能跑 60s+ acme.sh 等待)→ **boot loop + 阿里云 API 配额被快速耗尽**。
- **参考**:Phase 1 cert 报告 #9 已记录 gitee clone 无 commit pin,这里**关联风险**。
- **风险**:boot loop + API 配额雪崩。
- **修复方案**:启动时 cert 缺失时**降级到 self-signed 模式 + WARN**,而不是 Exit(1) — 这是 better-than-boot-loop 的兜底。同时 entrypoint.sh 加 `--no-issue` fallback。
- **工作量**:0.3d

### [SEVERITY-MED] ISSUE-Q7-03:acme.sh env 注入通过 `~/.acme.sh/account.conf` 文件,但 Go 端读 panelstate 不写入 acme.sh account.conf — 运行时改凭证后,**acme.sh 下次续期仍用旧 env**(因为 entrypoint 只在容器启动时读 panelstate → 写 account.conf)
- **位置**:[design.md §19.6](file:///opt/ikev2-panel-v2-main/docs/design.md)(acme.sh env 注入流程)+ main.go:318-324(CredentialGetter)
- **问题**:CredentialGetter 给 DDNS 用,DDNS tick 时立即生效。但 acme.sh 是 entrypoint 启动时复制 env → 后续 cron 跑 renew-cert.sh 用的是 **entrypoint 时刻的 env**,不是运行时 panelstate 改的值。
- **参考**:Phase 1 cert 报告已知设计妥协(注释"acme.sh 重启容器才生效")
- **风险**:用户在面板改凭证 → DDNS 用新凭证工作 → 但 30 天后 acme.sh 续签时仍用旧 env(被 RAM 删了)→ 续签失败 → LAST_RENEW_FAILED。
- **修复方案**:让 Go 进程 SIGHUP 或定时把 panelstate → 写 `~/.acme.sh/account.conf`(acme.sh 重读)— 复杂,留作长期。
- **工作量**:1.0d(复杂)

### [SEVERITY-LOW] ISSUE-Q7-04:`cert/acme.go:36` 的 `certPath` 来自 caller,但 `filepath.Join(filepath.Dir(certPath), "LAST_RENEW_FAILED")` 用的是 `filepath.Dir(certPath)` — LE 模式下 caller 传 `/data/le/fullchain.pem`,Dir = `/data/le`,但 `LAST_RENEW_FAILED` 由 entrypoint.sh 写到 `/data/le/LAST_RENEW_FAILED`(acme.sh 习惯)— 巧合匹配,但隐式依赖脆弱。
- **位置**:[acme.go:41](file:///opt/ikev2-panel-v2-main/internal/cert/acme.go#L41)
- **风险**:如未来 entrypoint.sh 改路径,Go 端不会自动跟随。
- **修复方案**:常量 `/data/le/LAST_RENEW_FAILED` 直接硬编码(跟 entrypoint.sh 同步)。
- **工作量**:0.1d

### Q7 小结

- `--issue --dns dns_ali --reloadcmd` 调用顺序由 entrypoint.sh 处理,Go 不参与 ✓。
- env 注入:`entrypoint.sh → ~/.acme.sh/account.conf`(Issue-Q7-03 改凭证不实时同步)。
- `--renew --force` 副作用:不在 Go 端触发(Issue-Q7-01)。
- 重试 / 退避:由 renew-cert.sh 处理,Go 不参与。

---

## Q8 阿里云 API 调用

### [SEVERITY-HIGH] ISSUE-Q8-01:`SignatureMethod=HMAC-SHA1` 但阿里云 OpenAPI v3 已推荐 HMAC-SHA256,且代码注释自身矛盾("用 HMAC-SHA256" vs "HMAC-SHA1")
- **位置**:[aliyun.go:176, 222-266](file:///opt/ikev2-panel-v2-main/internal/dns/aliyun.go#L176),[aliyun.go:222-266](file:///opt/ikev2-panel-v2-main/internal/dns/aliyun.go#L222-L266)
- **问题**:
  - L176 显式 `params.Set("SignatureMethod", "HMAC-SHA1")`
  - L228 注释写 "用 AccessKeySecret + "&" HMAC-SHA256"
  - L259 注释写 "HMAC-SHA1(阿里云 alidns RPC v1 签名算法)"
  - L260 实际 `hmac.New(sha1.New, ...)` —— **是 SHA1**
  
  实际工作(SHA1)能被阿里云验证接受(老签名方式未 deprecated,但 [2022 后阿里云推荐 SHA256](https://help.aliyun.com/zh/sdk/developer-reference/v3-request-structure-and-signature))。
- **参考**:NIST SP 800-131A Rev.2 — HMAC-SHA1 在新部署中不建议(SHA1 collision 风险)
- **风险**:HMAC-SHA1 强度低于 SHA256 — 在 RAM key 高度敏感场景下,NIST/PCI DSS 合规可能不接受。当前用 HMAC-SHA1 在功能上 OK,但 **不是 best practice**。
- **修复方案**:改用 SHA256:`hmac.New(sha256.New, ...)` + `params.Set("SignatureMethod", "HMAC-SHA256")`,同步更新注释。
- **工作量**:0.2d

### [SEVERITY-MED] ISSUE-Q8-02:`randomNonce` 16 字节随机 + fallback 到 `time.Now().UnixNano()`,但 fallback 概率下唯一性仍 OK,但 entropy 极弱
- **位置**:[aliyun.go:268-275](file:///opt/ikev2-panel-v2-main/internal/dns/aliyun.go#L268-L275)
- **问题**:`crypto/rand.Read` 失败是 OS 级别问题(读 /dev/urandom 失败),fallback 用 `time.Now().UnixNano()` — **纳秒级精度下两次调用**很可能不同,但 entropy 从 128 bit 跌到 ~30 bit(纳秒时间戳在毫秒精度下只占几位)。
  实际 `crypto/rand.Read` 在 Linux 上几乎不可能失败(内核熵池足够),fallback 路径是防御性编程,真实风险 0。
- **风险**:0(几乎不可能触发);但代码注释无"防御性编程"标记,会让 reviewer 误以为真有风险。
- **修复方案**:加注释"OS 级别熵源失败极罕见,fallback 仅作防御"。
- **工作量**:0.1d

### [SEVERITY-MED] ISSUE-Q8-03:API 错误码匹配用 `strings.Contains(err.Error(), "Forbidden")` 匹配太宽(已在 Q3-01 详述)
- **位置**:[sync.go:554-558](file:///opt/ikev2-panel-v2-main/internal/ddns/sync.go#L554-L558)
- **风险**:**误判权限错** → 永久不重试 → DDNS 真正失败。
- **修复方案**:见 ISSUE-Q3-01。
- **工作量**:0.3d(已在 Q3-01 计入)

### [SEVERITY-LOW] ISSUE-Q8-04:`HTTPClient.Timeout = 10 * time.Second` — 单次调用总超时 10s,包含 DNS 解析 + TLS 握手 + 服务器响应
- **位置**:[aliyun.go:79](file:///opt/ikev2-panel-v2-main/internal/dns/aliyun.go#L79)
- **问题**:在 dual 模式下,v4 + v6 各调用 2 次(Find + Update)共 4 次,每次 10s — 单 tick 最坏 40s。但 `tick()` 在 dual 模式下并发跑,**实际 wall clock 是 max(40s,40s) = 40s**。
  阿里云 SLA 通常 API 响应 < 1s,10s 足够。但若 SLA 退化,整个 DDNS tick 阻塞 40s → ticker 累积 → goroutine 永远赶不上节流节奏。
- **风险**:阿里云故障期间 DDNS tick 累积,但因为每个 tick 跑在自己的 goroutine + ticker 重启时只触发一次(默认 ticker 不累积),实际 OK。
- **修复方案**:无。
- **工作量**:0d

### [SEVERITY-LOW] ISSUE-Q8-05:`aliddns` 错误码表里 `Throttling.User` / `Throttling` 限流应该 retry(指数退避),但当前 `sync.go` 重试 sleep 是固定 1s/4s/9s,跟阿里云建议的"retry after X seconds" 不一定对齐
- **位置**:[sync.go:563](file:///opt/ikev2-panel-v2-main/internal/ddns/sync.go#L563)
- **问题**:阿里云限流返回 body 含 `Recommend: <秒数>`,应按 recommend 重试。当前固定 9s 可能撞限流重发。
- **修复方案**:解析 Recommend 字段 → 按其建议 sleep;无则 fallback 9s。
- **工作量**:0.3d

### Q8 小结

- HMAC 算法:SHA1(功能性 OK,**非最佳实践**)— ISSUE-Q8-01。
- Nonce:16 字节 + 防御 fallback,OK。
- 错误码:字符串 contains 匹配,**过宽** — ISSUE-Q8-03(同 Q3-01)。
- 超时:10s 总超时,合理但可以更细(connect / read 各自分)。

---

## Q9 strongSwan VICI

### [SEVERITY-MED] ISSUE-Q9-01:`terminate.go:137-153` 的 `matchUsername` 处理 `CN=` 前缀,但 strongSwan 6.x 在 EAP 模式下用 `2 CN=alice` 形式(已覆盖),老版本可能用 `2 alice@realm` 或 `2 alice@fqdn`
- **位置**:[terminate.go:136-153](file:///opt/ikev2-panel-v2-main/internal/swanctl/terminate.go#L136-L153)
- **问题**:测试覆盖 `["2 alice", "2 CN=alice"]`(parser_test.go),但 `alice@realm` 没测。strongSwan 文档中 EAP-MSCHAPv2 默认 EAP identity = username(无 `@realm` 后缀),但某些配置可能加 `@realm`。
- **风险**:极端配置下,按 username terminate 找不到匹配 SA → SA 不被终止 → 用户的"踢人"按钮失效。
- **修复方案**:`matchUsername` 增加 `strings.TrimSuffix(remoteID, "@"+realm)` 兜底;或加 debug log "remoteID format unknown"。
- **工作量**:0.2d

### [SEVERITY-MED] ISSUE-Q9-02:`parser.go` 解析 `state` 字段只信任 "ESTABLISHED" / "CONNECTING",但 strongSwan 还有 `REKEYING` / `REKEYED` / `DELETING` / `INSTALLED` 等
- **位置**:[parser.go:106-107](file:///opt/ikev2-panel-v2-main/internal/swanctl/parser.go#L106-L107)
- **问题**:代码注释说 "IkeState 'ESTABLISHED', 'CONNECTING'",但 `CountActiveIkeSAs` 只数 ESTABLISHED。`collector.go` 也只过 ESTABLISHED。其他状态如 REKEYING(续命中)— **流量暂时在跑,应该累计**!但被跳过 → 漏算流量。
- **参考**:strongSwan [VICI protocol](https://docs.strongswan.org/docs/strongswanInterop.html)
- **风险**:用户拨号 24h,charon 在 23h 触发 rekeying(默认 rekey_time=24h 但 22h 提前 rekeying)→ 续命中 1-2 分钟内 collector 跳过 → bytes_in/out 不入库 — **小流量数据缺口**。
- **修复方案**:collector.go:115-118 改为 `if sa.IkeState != "ESTABLISHED" && sa.IkeState != "REKEYING" { continue }`,或者把"活跃"定义为 `IkeState in {ESTABLISHED, REKEYING, REKEYED}`。
- **工作量**:0.1d

### [SEVERITY-MED] ISSUE-Q9-03:`terminate.go:74-79` 循环 terminate 各 uniqueID,失败时 `continue`(继续下一个),但 `lastErr` 被最后覆盖 → caller 只看到最后一个错误
- **位置**:[terminate.go:62-83](file:///opt/ikev2-panel-v2-main/internal/swanctl/terminate.go#L62-L83)
- **问题**:`var lastErr error` 单一变量,每轮覆盖。如果第一个 SA 终止失败(关键错误,如 charon 整体不可用),第二个 SA 终止成功,`lastErr` 被覆盖为 nil → caller 完全没意识到第一个错误。
- **风险**:用户按"踢掉 bob",实际上 3 个 SA 中 1 个失败 2 个成功,但 caller 返回 nil,UI 显示"已踢掉",bob 仍有一个连接。
- **修复方案**:用 `errgroup.Group` 或 `[]error`,返回 combined error 或部分成功响应。
- **工作量**:0.2d

### [SEVERITY-LOW] ISSUE-Q9-04:`getNumber` 兜底 `fmt.Sprintf("%v", x)` 解析未知类型,但 JSON null 解析后会得到 nil,导致 `fmt.Sprintf("%v", nil)` = "<nil>" → ParseInt 失败返回 0
- **位置**:[parser.go:164-184](file:///opt/ikev2-panel-v2-main/internal/swanctl/parser.go#L164-L184)
- **问题**:VICI bytes-in 字段为 null(罕见但可能)→ getNumber 返回 0 — 流量漏算(累加进 bytes_in 但被认为是 0)。
- **风险**:极低。
- **修复方案**:debug log "vici unknown type for key X" + 返回 0(当前行为)。
- **工作量**:0d

### [SEVERITY-LOW] ISSUE-Q9-05:govici 协议版本兼容 — 项目固定 `github.com/strongswan/govici v0.8`,strongSwan 5.9.x 的 VICI 协议在 charon 6.0.1 上有差异,需实测覆盖
- **位置**:[loader.go:31](file:///opt/ikev2-panel-v2-main/internal/swanctl/loader.go#L31) `import "github.com/strongswan/govici/vici"`
- **问题**:Phase 1 strongswan 报告 #11 提到 VICI list-sas 流处理只 list-sa 事件(注释明确"charon 6.0.1 不实现 load-creds / load-all"),所以走 shell-out。本审计内 `govici` 仅用于 list-sas + terminate。terminate 是非流式 Call,list-sas 是流式 CallStreaming — govici v0.8 实测能用。
- **风险**:govici 升级可能 break(目前 strongSwan 官方推荐 govici v0.10+,但 v0.8 测试覆盖)。
- **修复方案**:go.mod 固定版本,不自动 minor 升级。
- **工作量**:0d

### Q9 小结

- SA 解析完整性:`parser.go` 解析完整,但 collector 忽略 REKEYING(ISSUE-Q9-02)。
- terminate 按 user 准确性:`matchUsername` 在 `CN=` 之外的 prefix 未覆盖(ISSUE-Q9-01)。
- VICI 版本兼容:govici v0.8 + charon 6.0.1 实测 OK(Phase 1 strongswan 报告)。

---

## Q10 DDNS IPv4 / IPv6 探测

### [SEVERITY-MED] ISSUE-Q10-01:`net.Dial("udp", "8.8.8.8:80")` 在某些 ISP 强制透明 DNS 代理 + NAT 场景下,LocalAddr 可能**不是**用户真实出口 IP
- **位置**:[ipv4watch.go:135-183](file:///opt/ikev2-panel-v2-main/internal/swanctl/ipv4watch.go#L135-L183)
- **问题**:UDP dial 不实际发包,内核走 routing 拿到 source IP — 但若主机有 multiple default routes(双 ISP,IPv6-only + IPv4 fallback),可能拿到错的 IP。
  跟 ddns-go / ddclient 对比:
  - ddns-go:[用 HTTP GET http://ip.qq.com/ 或 http://ifconfig.me/](https://github.com/jeessy2/ddns-go) → 服务器侧告诉客户端其 IP,**更可靠**。
  - ddclient:[用 checkip.dyndns.org / checkip.amazonaws.com](https://github.com/ddclient/ddclient),也是 HTTP 侧探测。
- **参考**:[ddns-go 探测策略](https://github.com/jeessy2/ddns-go/blob/master/internal/dnsloop/detect.go)
- **风险**:多 default route / NAT 场景下,DDNS 同步的不是真实出口 IP。
- **修复方案**:增加可选 HTTP-based 探测(curl ifconfig.me,取 HTTP response body) — 跟 ddns-go 一致。但增加出方向 HTTP 流量。
- **工作量**:0.5d

### [SEVERITY-MED] ISSUE-Q10-02:`DetectGlobalV6` 读 `/proc/net/if_inet6` 在 Linux 4.x+ 工作,但**容器内**读的是宿主 /proc(netns 共享或独立)
- **位置**:[ipv6watch.go:147](file:///opt/ikev2-panel-v2-main/internal/swanctl/ipv6watch.go#L147)
- **问题**:容器默认是 host network(`/etc/swanctl` 存在暗示 host net),`/proc/net/if_inet6` 反映宿主全局接口 — OK。但若未来改成 bridge 网络,容器内 `/proc/net/if_inet6` 只看到容器侧接口(可能没 v6) → `detectGlobalV6` 返回 "", DDNS 跳过。
  当前是 host net ✓。
- **风险**:bridge 网络部署下 v6 探测失效。
- **修复方案**:无 — 当前架构 host net。
- **工作量**:0d

### [SEVERITY-MED] ISSUE-Q10-03:`RunIPv6Watch` 的 `Period` 默认 60s,IPv6 prefix 变化可能在 SLAAC 数秒内完成 — 检测延迟最坏 60s
- **位置**:[ipv6watch.go:36-38](file:///opt/ikev2-panel-v2-main/internal/swanctl/ipv6watch.go#L36-L38)
- **问题**:用户 ISP 重拨 → SLAAC 分配新 prefix → 主机 v6 立即变 → swanctl.conf 还是旧 v6 → VPN 客户端连不上旧地址。直到下次 ticker(60s 内)才更新。
  业界 best practice:用 netlink RTMGRP_IPV6_IFADDR 订阅,变化立即触发。但代码注释说"留作 future work"。
- **参考**:[govici/netlink subscribe](https://pkg.go.dev/github.com/vishvananda/netlink) — `AddrSubscribeWithOptions`
- **风险**:用户每次 ISP 重拨后最坏 60s VPN 不可用。
- **修复方案**:用 netlink 订阅替代 polling(若用户接受)。
- **工作量**:1.0d

### [SEVERITY-LOW] ISSUE-Q10-04:`DetectGlobalV4` 不带 ctx,3s 超时是 dialer-level,但 host 上可能 sysctl `net.ipv4.ip_forward=0` + iptables 阻 UDP,导致 dial 超时 3s
- **位置**:[ipv4watch.go:135](file:///opt/ikev2-panel-v2-main/internal/swanctl/ipv4watch.go#L135)
- **问题**:3s 在大多网络够用,但容器内 iptables 默认 filter(FORWARD DROP)可能让 dial 走 slow path。
- **风险**:DDNS tick 经常 3s 拖尾。
- **修复方案**:用 `net.ListenUDP` 替代 `net.Dial("udp", ...)`,直接 listen 后看 LocalAddr — 更快(不实际 dial)。但需要选端口。
- **工作量**:0.3d

### Q10 小结

- `net.Dial("udp", "8.8.8.8:80")`:**功能性可用,但有边界**(多路由 / NAT 场景下可能错)— ISSUE-Q10-01。
- ddns-go / ddclient 用 HTTP 侧探测,理论上更可靠;当前实现在多数 VPS 场景下 OK。
- 接口地址变化检测延迟:**最坏 60s**(ISSUE-Q10-03)。

---

## 不在范围内

- 以下项已纳入 Phase 1 Top-10 或其他专项,**不再重复**:
  - 登录端点 rate limit(security/web Top-1)
  - IKEv2 套件 weak algorithms(strongswan Top-2)
  - cert 切换非 atomic(cert Top-3 + strongswan Top-6)
  - Docker privileged / SYS_ADMIN(docker Top-4)
  - HTTP security headers(web Top-5)
  - VPN 密码明文 + DB 无加密(security/storage Top-6)
  - IPv6 限速零(net Top-7)
  - RAM 错误码匹配错(alidns Top-8 + security Top-8)
  - acme.sh gitee clone 无 commit pin + apt 清华源无 GPG(cert Top-9 + docker Top-3)
  - DDoS 防护 + TCP MSS clamp(net Top-10)
  - audit log / DB 备份 / migration / strongSwan --enable-sha1 / filelog enc=1 / VICI list-sas 流延迟 / swanctl --load-all 跑两次 / POST input validation / Cookie Secure / flash query string 回归(都已在 Top-11~20 或专项报告)
- 测试 `-race` 不可跑(无 gcc)— 留给 CI / Dockerfile 修复后做正式 race 检测。
- `cgo` 缺失带来的 race detector 不可用 → 当前审计靠**静态分析 + 单测覆盖**,并发 race 只能**人工 review**。

---

## 建议优先级

### 本周(W2,5d)

| # | Issue | 工作量 | 备注 |
|---|-------|--------|------|
| 1 | ISSUE-Q3-01 aliyun 错误码匹配过宽 | 0.3d | 跟 Top-8 部分重叠,但这里覆盖整个 retry 决策路径 |
| 2 | ISSUE-Q8-01 HMAC-SHA1 → SHA256 | 0.2d | 合规 + 注释修正 |
| 3 | ISSUE-Q2-01 main.go 缺统一 WaitGroup | 0.3d | SIGTERM 数据丢失 |
| 4 | ISSUE-Q5-01 DDNS throttle 持久化 | 0.2d | 重启后限流防护 |
| 5 | ISSUE-Q5-02 charon ready wait | 0.3d | 启动 reload 失败 boot loop |

### 下周(W3)

| # | Issue | 工作量 |
|---|-------|--------|
| 6 | ISSUE-Q1-01 runFamily panic recover | 0.2d |
| 7 | ISSUE-Q3-02 retry sleep ctx.Done | 0.2d |
| 8 | ISSUE-Q3-03 home 暴露 swanctl 故障状态 | 0.2d |
| 9 | ISSUE-Q6-01 dual 模式 IP/Error 聚合 | 0.2d |
| 10 | ISSUE-Q6-02 多 v6 接口选择 | 0.3d |
| 11 | ISSUE-Q9-02 collector 包含 REKEYING | 0.1d |
| 12 | ISSUE-Q9-03 terminate 多 SA 错误聚合 | 0.2d |
| 13 | ISSUE-Q10-03 v6 watch netlink 订阅 | 1.0d |
| 14 | ISSUE-Q7-02 LE 启动 cert 缺失降级 | 0.3d |

### 延后 / 低优

| # | Issue | 工作量 | 备注 |
|---|-------|--------|------|
| 15 | ISSUE-Q1-04 home ListSAs timeout | 0.2d | UX 优化 |
| 16 | ISSUE-Q4-02 GeneratePassword 注释错误 | 0.1d | 代码卫生 |
| 17 | ISSUE-Q4-04 cert NotAfter skew tolerance | 0.1d | 边界 |
| 18 | ISSUE-Q7-01 LE 自动 retry 续签 | 0.5d | 复杂 |
| 19 | ISSUE-Q7-03 panelstate → acme.sh 实时同步 | 1.0d | 复杂 |
| 20 | ISSUE-Q10-01 HTTP-based v4 探测 | 0.5d | 引入 HTTP 出方向 |
| 21 | ISSUE-Q10-04 ListenUDP 替代 Dial | 0.3d | 性能优化 |
| 22 | ISSUE-Q1-05 Dockerfile race-test target | 0.1d | CI 友好 |

### 永不修(明确设计妥协)

- ISSUE-Q5-03 installtoken / FlashStore 重启丢失(明确 in-memory 设计)
- ISSUE-Q5-06 collector 重启后流量精度缺口(明确写注释的妥协)
- ISSUE-Q9-04 govici 版本固定(go.mod 控版本即可)

---

## 累计工作量

- HIGH:2 个(0.5d 实际工作量)
- MED:约 22 个(总 ~5d)
- LOW:约 8 个(总 ~1d)
- **合计 unique 工作量:约 6.5d**(≈ 1.5 周),跟 Phase 1 summary 节奏一致。

---

## 验证项

| 验证 | 结果 |
|------|------|
| `go test ./...` 全包 | ✓ PASS(13/13) |
| `go test -race ./...` | ✗ 无法跑(无 gcc) |
| `grep -rn "_ = .*(" internal/` | 36 处,绝大多数合理(测试 + HTTP write) |
| `grep -rn "errors.Is\|errors.As" internal/` | 8 处 Is / 0 处 As |
| `grep -rn "log.Print\|fmt.Fprintln.*err" internal/` | 0 处,全 slog |
| `grep -rn "time.Now().UTC" internal/` | 4 处,展示路径全 UTC ✓ |
| `grep -rn "time.Now().Unix()" internal/` | 11 处,DB 写入(epoch,跟时区无关 ✓) |
| 读 Phase 1 7 份报告,确认无重复 | ✓ |

---

## 下一步

1. 本周修 W2 优先级 #1-5(HIGH + 关键 MED)
2. 同步修 Dockerfile race-test target(CI 验证手段)
3. 排期 W3
4. Q3/Q5/Q7/Q10 部分 issue 需要专项设计(如 panelstate → acme.sh 实时同步)→ 留 v3 路线图