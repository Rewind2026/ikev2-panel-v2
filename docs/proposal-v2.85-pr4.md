# Proposal v2.85-PR4 — 后台 goroutine 统一 WaitGroup + charon ready 探测

> **类型**:PR 级提案(对应 v2.85 release 第 4 个 PR)
> **目标**:修复 SIGTERM 数据丢失(SIGTERM → 后台 goroutine 强切)+ 启动期 charon not-ready boot loop
> **关联审计**:
> - [audit-2026-09-correctness.md §Q2-01](file:///opt/ikev2-panel-v2-main/docs/audit-2026-09-correctness.md)(HIGH — 6 个后台 goroutine 无统一 WaitGroup)
> - [audit-2026-09-correctness.md §Q5-02](file:///opt/ikev2-panel-v2-main/docs/audit-2026-09-correctness.md)(MED — 启动 ReloadAll 不等 charon ready)
> **工作量**:**0.6d**(Q2-01 = 0.3d + Q5-02 = 0.3d)
> **作者**:自查

---

## 背景

Phase 2-4 审计发现 2 个跟"进程生命周期"相关的 Correctness 问题,都是启动/退出时序类:

| # | 现状 | 后果 |
|---|------|------|
| Q2-01 | [main.go:413-540](file:///opt/ikev2-panel-v2-main/cmd/ikev2-panel/main.go#L413-L540) 6 个后台 goroutine(collector / expiry / le-certcheck / ipv6watch / ddns / token-sweep)各自 `go func(){}()` 启动,`httpSrv.Shutdown(ctx, 5s)` 超时后直接 `logger.Info("bye")` 退出 | SIGTERM 触发时,如果 collector 正在写 sqlite + ipv6watch 正在调 `swanctl --load-all`,会被进程强切掉 → 半截 reload → conf.d 不一致;ddns sync 写到一半的更新丢失 |
| Q5-02 | [main.go:64-81](file:///opt/ikev2-panel-v2-main/cmd/ikev2-panel/main.go#L64-L81) 启动序列一上来就 `scm.ReloadAll(ctx)`,但 entrypoint.sh 启动的 `ipsec start` 可能还在 fork() 中 | `swanctl --load-all` 失败 → 日志 error → 用户可能误以为配置没生效,反复重启容器形成 boot loop |

**为什么单独做这个 PR**:
- 这两个都是"进程边界"问题,**只有一处修改点**(main.go 的 goroutine 启动/退出序列 + 一处启动 reload)
- 都是 correctness 修复,不做 release v2.85 不收尾
- Q2-01 影响 SIGTERM 优雅退出(运维基本要求),Q5-02 影响首次启动成功率(用户首次跑就遇)
- 两个 PR 互不依赖,可独立 revert

---

## 目标

1. 6 个后台 goroutine 用 `sync.WaitGroup` 跟踪,`Shutdown` 后 `wg.Wait(5s)` 再退出
2. 每个后台 goroutine 加 `name` 字段(便于日志快速定位)
3. 启动期探测 `/var/run/charon.vici` socket + vici ping,最多 10s
4. 超时 log error 但**不退出**,等 cron / 下次 reload 自然重试

---

## 不在范围(明确不做)

- ❌ 不改任何 handler / DB / swanctl CLI 调用逻辑(只动启动/退出时序)
- ❌ 不引入 context cancellation tree 优化(简单 WaitGroup 够用)
- ❌ 不改 entrypoint.sh / ipsec 启动顺序
- ❌ 不做 charon ready 之外的健康检查(ddns / acme 等另说)
- ❌ 不动 strongSwan 编译参数 / 容器镜像层

---

## 设计

### Issue 1 (Q2-01) — 统一 WaitGroup + name 字段

#### 当前问题

`main.go:413-540` 启动 6 个后台 goroutine,每个独立 `go func(){}()`:

```go
// 当前代码(main.go:425-429)
go func() {
    // 流量采集（每 5 分钟）
    c := limit.NewCollector(scm, st, logger)
    c.Run(rootCtx)
}()
go func() {
    // 过期扫描（每 60 秒）
    c := expiry.NewChecker(scm, st, logger)
    c.Run(rootCtx)
}()
if cfg.CertMode == "letsencrypt" {
    go func() { /* le-certcheck */ }()
}
if cfg.IPv6Only {
    go func() { /* ipv6watch */ }()
}
if ddnsSync != nil {
    go func() { /* ddns sync */ }()
}
go func() { /* install token sweep */ }()
```

`httpSrv.Shutdown(shutdownCtx)` 超时(`shutdownCtx` 5s)后直接:

```go
// main.go:542-550
<-rootCtx.Done()
logger.Info("wait-shutdown signal received, draining")
shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
defer cancel()
if err := httpSrv.Shutdown(shutdownCtx); err != nil {
    logger.Error("graceful shutdown failed", "err", err)
}
logger.Info("bye")  // ← 直接退出,后台 goroutine 还在跑!
```

#### 改造方案

```go
// main.go 顶部 imports 加 "sync"
import (
    "sync"
    // ...
)

// ---------- 后台 goroutines 统一 WaitGroup ----------
var (
    bgWg   sync.WaitGroup
    bgList []string  // 启动顺序列表,启动日志打印
)

// 辅助函数:启动一个命名后台 goroutine
startBG := func(name string, fn func()) {
    bgWg.Add(1)
    bgList = append(bgList, name)
    go func() {
        defer bgWg.Done()
        defer func() {
            if r := recover(); r != nil {
                logger.Error("bg goroutine panic",
                    "name", name,
                    "recover", r,
                    "stack", string(debug.Stack()))
            }
        }()
        fn()
        logger.Debug("bg goroutine exited", "name", name)
    }()
}

// 启动 6 个后台 goroutine(命名)
startBG("collector", func() {
    c := limit.NewCollector(scm, st, logger)
    c.Run(rootCtx)
})
startBG("expiry", func() {
    c := expiry.NewChecker(scm, st, logger)
    c.Run(rootCtx)
})
if cfg.CertMode == "letsencrypt" {
    startBG("le-certcheck", func() {
        // LE 续签健康检查逻辑(从原 goroutine body 搬过来)
    })
}
if cfg.IPv6Only {
    startBG("ipv6watch", func() {
        iface := os.Getenv("IKEV2_OUT_IF")
        if err := swanctl.RunIPv6Watch(rootCtx, swanctl.IPv6WatchConfig{
            ConfPath: "/etc/swanctl/swanctl.conf",
            Iface:    iface,
            Period:   60 * time.Second,
            Logger:   logger,
        }); err != nil {
            logger.Warn("ipv6watch exited", "err", err)
        }
    })
}
if ddnsSync != nil {
    startBG("ddns", func() {
        if err := ddnsSync.Run(rootCtx); err != nil {
            logger.Warn("ddns sync exited", "err", err)
        }
    })
}
startBG("token-sweep", func() {
    ticker := time.NewTicker(5 * time.Minute)
    defer ticker.Stop()
    for {
        select {
        case <-rootCtx.Done():
            return
        case <-ticker.C:
            if n := installTokens.Sweep(); n > 0 {
                logger.Debug("install token sweep", "removed", n)
            }
            if n := flashStore.Sweep(); n > 0 {
                logger.Debug("flash store sweep", "removed", n)
            }
        }
    }
})

logger.Info("background goroutines started", "count", len(bgList), "names", strings.Join(bgList, ","))
```

#### 优雅退出序列

```go
<-rootCtx.Done()
logger.Info("shutdown signal received, draining",
    "timeout", "5s", "bg_goroutines", len(bgList))

// 1) 先 Shutdown HTTP — 停止接新请求,等在飞请求完成
shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
if err := httpSrv.Shutdown(shutdownCtx); err != nil {
    logger.Error("http graceful shutdown failed", "err", err)
}
cancel()

// 2) 等后台 goroutine 自然退出
//    每个 goroutine 都 watch rootCtx,Shutting 时 rootCtx 已 cancel,
//    选 rootCtx 监听 case 的会立刻 return
done := make(chan struct{})
go func() {
    bgWg.Wait()
    close(done)
}()
select {
case <-done:
    logger.Info("all background goroutines exited cleanly")
case <-time.After(5 * time.Second):
    logger.Error("background goroutines drain timeout (5s)",
        "hint", "some goroutines may have been killed mid-operation",
        "risk", "data inconsistency (sqlite write half / swanctl reload half)")
}

// 3) 最终退出
logger.Info("bye")
```

**关键点**:
- rootCtx 已经 cancel(来自 SIGINT/SIGTERM),所有 watch `rootCtx.Done()` 的 goroutine **理论上**会立即 return
- 但 `bgWg.Wait()` 给 5s 兜底:如果某个 goroutine 卡在系统调用(磁盘 IO / network),等它完成
- 超时只 log error,**不再强制 kill**(docker stop SIGKILL 在外层 10s 后兜底,本进程不重复)
- HTTP server 的 5s timeout 跟 background 的 5s timeout **独立**,先后顺序,合计最多 ~10s(在 docker stop 默认 10s 之内)

#### panic recover(可选,顺带做)

后台 goroutine panic 不应该让整个进程崩。当前没有任何 recover → 一旦 collector 抛 panic,整个 panel 进程退出,但 httpSrv 还在 running 的请求也会被拖死。

新加的 `startBG` helper 已经在 `defer recover` 兜住,日志打印 stack,**不退出**。

---

### Issue 2 (Q5-02) — 启动期 charon ready 探测

#### 当前问题

```go
// main.go:64-81 现状
scm := swanctl.New()
if _, err := os.Stat("/etc/swanctl"); err == nil {
    // EAP 模式：swanctl.conf 自身没 secret,但仍调一次 ReloadAll 确保 conn 注册到 charon。
    if err := scm.ReloadAll(ctx); err != nil {
        logger.Warn("swanctl --load-all at startup", "err", err)
    } else {
        logger.Info("swanctl --load-all ok (EAP mode, per-user secrets in conf.d)")
    }
}
```

容器启动时:
1. entrypoint.sh 先启动 charon(`ipsec start` 是 fork()-heavy,需 ~2-5s)
2. 然后 exec Go 进程
3. Go 进程一上来就 `scm.ReloadAll(ctx)` → `swanctl --load-all --noprompt` → 失败(charon 没 ready)

用户看到日志:
```
WARN swanctl --load-all at startup err="swanctl --load-all failed: ... (out=charon is not running)"
```

用户不知道这是**预期的**(charon 还在起),还是**真坏了**,反复 `docker restart` 形成 boot loop。

#### 改造方案:加 `WaitForCharonReady` 函数

```go
// internal/swanctl/loader.go(新增函数,放在 openSession 下面)

// WaitForCharonReady 等待 charon.vici socket 可用 + VICI session 可建立。
//
// 设计动机(Q5-02):容器启动时 entrypoint.sh 启动 charon 是异步 fork(),
// 如果 Go 进程比 charon 先 ready,直接调 ReloadAll 会失败 → 用户误以为配置没生效,
// 反复 docker restart 形成 boot loop。
//
// 探测策略:
//   1. /var/run/charon.vici 文件存在(stat)
//   2. vici.NewSession 能成功建立(开 socket fd + handshake)
//
// 两个条件都满足 → charon ready。
//
// 重试:最多 10 次,每次 sleep 1s,合计 10s 上限。
//
// 失败语义:返回 error,但**不致命**(caller log warn,不 os.Exit)。
// entrypoint.sh 后续会重试启动 charon;cron / 下次 reload 也都能正常工作。
//
// 参数:
//   - sockPath: 自定义 VICI socket 路径(默认 /var/run/charon.vici)
//   - timeout:  总等待上限
//
// 用法:
//   if err := WaitForCharonReady(10*time.Second); err != nil {
//       logger.Warn("charon not ready, ReloadAll may fail", "err", err)
//       // 不退出,继续启动,后续 cron / handler reload 会自然重试
//   }
func WaitForCharonReady(timeout time.Duration) error {
    sockPath := defaultViciSocketPath  // 复用 loader.go 现有常量
    deadline := time.Now().Add(timeout)
    const retryInterval = 1 * time.Second
    const maxAttempts = 10  // 10s 总超时兜底

    for attempt := 1; attempt <= maxAttempts; attempt++ {
        // 1) stat socket 文件
        if _, err := os.Stat(sockPath); err == nil {
            // 2) 试开 VICI session
            sess, err := vici.NewSession(vici.WithSocketPath(sockPath))
            if err == nil {
                _ = sess.Close()
                return nil  // 双重条件满足 → ready
            }
            // stat 成功但 NewSession 失败 → socket 还在 accept 队列里,等等重试
        }

        if time.Now().After(deadline) {
            return fmt.Errorf("charon.vici not ready after %s (attempt=%d, sock=%s)",
                timeout, attempt, sockPath)
        }
        time.Sleep(retryInterval)
    }
    return fmt.Errorf("charon.vici not ready after %d attempts (sock=%s)", maxAttempts, sockPath)
}
```

#### main.go 启动序列改造

```go
// main.go:64-81 改为:
scm := swanctl.New()
if _, err := os.Stat("/etc/swanctl"); err == nil {
    // Q5-02:等 charon ready(最多 10s),避免首次启动时 ReloadAll 失败 → boot loop
    if err := swanctl.WaitForCharonReady(10 * time.Second); err != nil {
        logger.Warn("charon not ready, ReloadAll may fail (will retry via cron/reload later)",
            "err", err,
            "hint", "this is normal if ipsec is still starting; entrypoint.sh will retry")
    } else {
        logger.Info("charon ready")
    }

    // EAP 模式:swanctl.conf 自身没 secret,但仍调一次 ReloadAll 确保 conn 注册到 charon。
    if err := scm.ReloadAll(ctx); err != nil {
        logger.Warn("swanctl --load-all at startup", "err", err)
    } else {
        logger.Info("swanctl --load-all ok (EAP mode, per-user secrets in conf.d)")
    }
} else {
    devConfDir := filepath.Join(cfg.DataDir, "swanctl-conf.d")
    _ = os.MkdirAll(devConfDir, 0o755)
    scm = scm.WithConfDir(devConfDir).WithSkipVici()
    logger.Info("dev mode: swanctl conf dir", "dir", devConfDir)
}
```

**关键点**:
- charon 没 ready → **不退出**,继续启动
- 后续 cron(acme.sh cron / ddns tick) + handler 用户增删时 ReloadAll 自然会重试
- entrypoint.sh 也有自己的 charon 启动 retry 逻辑(参见 `entrypoint.sh`),两层兜底
- dev 模式不调 WaitForCharonReady(SkipVici 已开)

#### vici.NewSession 失败原因 & 兜底

`vici.NewSession` 失败的几种情况:
1. socket 文件存在但 charon 还在 init() → accept 队列未启用 → 重试可恢复
2. socket 文件不存在 → 重试 1-2 次一般会出(因为 charon 启动后第一件事就是 bind)
3. permission denied → **不要 retry**(容器内通常不会出现,真出现是配置错)

兜底:即便 10 次都失败,返回 error 但**不退出**。container 启动完成后,用户首次 reload / cron tick 仍能正常工作。

---

## 改动文件清单

| 文件 | 改动 | 行数估算 |
|------|------|---------|
| [cmd/ikev2-panel/main.go](file:///opt/ikev2-panel-v2-main/cmd/ikev2-panel/main.go) | 加 `sync` import + `bgWg`/`bgList`/`startBG` helper + 6 个 goroutine 改用 `startBG` + 退出序列加 `bgWg.Wait(5s)` + 启动序列加 `WaitForCharonReady(10s)` | +50 / -20 行 |
| [internal/swanctl/loader.go](file:///opt/ikev2-panel-v2-main/internal/swanctl/loader.go) | 新增 `WaitForCharonReady(timeout)` 函数(独立函数,不挂在 Manager 上,纯工具) | +40 行 |

**总计**:**+70 行,改动局部,review 简单**。

---

## 测试方案

### 自动化测试

```bash
# 1. 单元测试(确保 main.go 改动不破坏)
go test ./...

# 2. 编译检查
CGO_ENABLED=0 go build -o /tmp/test-panel ./cmd/ikev2-panel
# (沙箱无 gcc 但 CGO_ENABLED=0 仍可编)

# 3. WaitForCharonReady 单元测试(新加)
// internal/swanctl/loader_test.go 新增:
func TestWaitForCharonReady_SocketMissing(t *testing.T) {
    // tmp dir,fake socket path → 永远 stat 失败
    err := WaitForCharonReady(2*time.Second)
    if err == nil {
        t.Fatal("expected timeout error, got nil")
    }
    // 2s timeout,1s interval → 期望 2 次 attempt 后返回
}

func TestWaitForCharonReady_SocketAppears(t *testing.T) {
    // tmp socket,先用 os.Stat 失败,然后 goroutine 在 1.5s 后创建 fake socket
    // 期望:成功 + 总耗时 ~1.5s
    // 注意:fake socket 需可 accept,否则 NewSession 仍失败 → 改测 "stat 成功但 session 失败" 分支
}

# 4. 跑 race detector(PR-1 加的 runner-test target)
docker build --target runner-test -t ikev2-panel-race:test .
docker run --rm ikev2-panel-race:test
# 期望:ok  github.com/.../internal/swanctl
#       ok  github.com/.../cmd/ikev2-panel
#       全 PASS,race detector 0 warning

# 5. dev 模式启动(本地无 charon.vici)→ 验证 fallback 不退出
mkdir -p /tmp/panel-test
IKEV2_DATA_DIR=/tmp/panel-test IKEV2_LISTEN_ADDR=127.0.0.1:8443 \
    IKEV2_LOG_LEVEL=debug IKEV2_SERVER_CN=test.local \
    IKEV2_COOKIE_SECURE=false /tmp/test-panel &
sleep 5
grep "version=" /tmp/panel-test/*.log
# 期望:启动成功,看到 "charon not ready, ... (will retry via cron/reload later)"
# 期望:进程仍在前台(没退)
kill %1
```

### 集成测试(需要 docker)

```bash
# 1. 完整启动测试(模拟用户场景)
docker compose down -v
docker compose build
docker compose up -d
sleep 15  # 等 charon 启动 + Go 启动 + charon ready 探测
docker compose logs ikev2-panel 2>&1 | grep -E "(charon|swanctl|background|bye)"
# 期望:
#   INFO  starting ikev2-panel version=...
#   INFO  charon ready
#   INFO  swanctl --load-all ok
#   INFO  background goroutines started count=6 names=collector,expiry,le-certcheck,ipv6watch,ddns,token-sweep
# 进程持续 running

# 2. SIGTERM 优雅退出测试
docker compose stop  # 发送 SIGTERM,等 10s 后 SIGKILL
docker compose logs ikev2-panel 2>&1 | tail -30
# 期望:
#   INFO  shutdown signal received, draining timeout=5s bg_goroutines=6
#   INFO  http graceful shutdown ... (可能成功或 timeout)
#   INFO  all background goroutines exited cleanly  ← 新日志
#   INFO  bye
# 没有 "background goroutines drain timeout (5s)" → 5s 内全退

# 3. boot loop 修复测试(人为延迟 charon 启动)
# entrypoint.sh 临时加 sleep 5 在 ipsec start 后
# 期望:Go 进程等 10s,期间日志显示 "charon not ready, ... (will retry)"
# 然后 "charon ready" + 正常 ReloadAll
# **不出现** boot loop
```

### 手动 SIGTERM 验证(沙箱)

```bash
# 本地启动 → SIGTERM → 看是否等 background goroutine 退出
IKEV2_DATA_DIR=/tmp/panel-test /tmp/test-panel &
PANEL_PID=$!
sleep 3
kill -TERM $PANEL_PID
wait $PANEL_PID
# 期望:exit code 0(graceful),日志最后是 "bye"(没有 drain timeout warning)
```

---

## 风险评估

| 风险 | 概率 | 影响 | 缓解 |
|------|------|------|------|
| `bgWg.Wait()` 在 5s 超时未到时 hang(某 goroutine 卡死) | 低 | docker stop 默认 10s 后 SIGKILL,会强杀 | 5s `time.After` 兜底 + 超时只 log 不再阻塞 main |
| `WaitForCharonReady` 实现里有死循环 bug | 低 | 启动挂 10s,但不退出 | 加 `maxAttempts=10` 硬上限 |
| `vici.NewSession` 在 sandbox / dev 环境永远失败 | 低 | 启动失败(但不退出) | 已设计为"不退出",log warn 后继续 |
| `strings.Join(bgList, ",")` 启动日志太长 | 极低 | 日志可读性 | 6 个名字 ~ 50 字符,可接受 |
| `debug.Stack()` panic 恢复吃太多 CPU | 极低 | panic 时打印 stack 是标准做法 | 标准库,无性能问题 |
| container 镜像里 `/var/run/charon.vici` 路径不一致 | 极低 | dev 模式启动错 | `defaultViciSocketPath` 是常量,与 loader.go 现有一致 |
| `bgWg.Add(1)` 在 `startBG` 调用前发生 → 极端情况 race | 极低 | 数据竞争 | `startBG` 是同步函数,在 main goroutine 内顺序调用,无 race |

---

## 不破坏兼容性的承诺

- **现有 v2.84 用户升级**:行为变化只在进程边界(退出/启动),**用户面 HTTP API / DB / 配置 100% 不变**。
- **dev 模式(无 charon)**:跟之前一样 log warn,不退出。
- **container 启动时间**:WaitForCharonReady 最多 +10s,但 charon 通常 2-5s 就 ready,**净增量 ~0-5s**。
- **SIGTERM 行为变化**:之前是"Shutting 后直接 bye",现在是"Shutdown + bgWg.Wait(5s) + bye"。docker stop 默认 10s 留有 5s 余量。**如果用户设了 stop_grace_period < 5s**,会收到 SIGKILL 兜底(行为跟 v2.84 类似)。
- **不引入新依赖**:只用 `sync`(标准库)+ `runtime/debug`(标准库)+ 现有 `vici` package。

---

## 后续 PR 关联

本 PR 是 v2.85 release 第 4 个,**只做**进程边界修复。后续 PR:
- **PR-5**:alidns 错误码精确匹配 + HMAC-SHA256
- **PR-6**:DDNS throttle 持久化 + stopCh sync.Once + collector REKEYING
- **PR-7**:tc IPv6 限速

每个 PR 独立,可单独 revert。

---

## 评审检查项(给自己)

- [x] 改动不超过 80 行
- [x] 不引入新依赖(只用 stdlib + 现有 vici package)
- [x] 不改任何 handler 业务逻辑 / DB schema / HTTP API
- [x] dev 模式(本地无 charon)不受影响
- [x] SIGTERM 优雅退出不超过 docker stop 默认 10s
- [x] 启动失败不退出,后续 retry 仍能工作
- [x] 测试方案具体可执行(单元 + 集成 + 手动)
- [x] panic recover 顺带做(后台 goroutine panic 不应该拖死进程)

---

## 工作量分解

| 步骤 | 时间 |
|------|------|
| Issue 1 设计 + main.go 改造(startBG helper + 6 个 goroutine + 退出序列) | 0.15d |
| Issue 1 panic recover 顺带做 | 0.02d |
| Issue 2 设计 + WaitForCharonReady 函数实现 | 0.10d |
| Issue 2 main.go 启动序列接入 | 0.03d |
| 单元测试(WaitForCharonReady TestWait*) | 0.08d |
| 集成测试(docker compose up + SIGTERM) | 0.10d |
| 跑 race detector + 全包 go test | 0.02d |
| 文档 + review | 0.10d |
| **合计** | **0.60d** |

---

## 实施 Checklist(执行时用)

```markdown
- [ ] main.go 加 "sync" 和 "runtime/debug" import
- [ ] main.go 定义 bgWg / bgList / startBG helper
- [ ] main.go 6 个后台 goroutine 改用 startBG(name, fn)
- [ ] main.go 启动日志改用 "background goroutines started count=N names=..."
- [ ] main.go 退出序列加 bgWg.Wait(5s) + 超时 warn
- [ ] main.go 启动序列加 WaitForCharonReady(10*time.Second) 调用
- [ ] internal/swanctl/loader.go 新增 WaitForCharonReady 函数
- [ ] internal/swanctl/loader.go 新增对应单元测试
- [ ] go test ./... PASS
- [ ] go test -race ./internal/swanctl/... PASS(用 PR-1 加的 runner-test target)
- [ ] CGO_ENABLED=0 go build ./cmd/ikev2-panel PASS
- [ ] docker compose up 验证启动日志含 "charon ready" 或 "charon not ready, ... (will retry)"
- [ ] docker compose stop 验证退出日志含 "all background goroutines exited cleanly"
- [ ] git commit "v2.85-PR4: unified WaitGroup for background goroutines + WaitForCharonReady"
```

---

> 关联:
> - [audit-2026-09-correctness.md §Q2-01](file:///opt/ikev2-panel-v2-main/docs/audit-2026-09-correctness.md)
> - [audit-2026-09-correctness.md §Q5-02](file:///opt/ikev2-panel-v2-main/docs/audit-2026-09-correctness.md)
> - [release-notes-v2.85.md](file:///opt/ikev2-panel-v2-main/docs/release-notes-v2.85.md)(待 PR 合入后写)