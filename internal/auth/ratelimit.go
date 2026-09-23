// 登录 rate limit + 全局 IP 限速中间件。
//
// v2.86-PR9 设计动机：
//   - 审计 Top-10 #1:登录端点无 rate limit,公网部署直接是暴力破解入口
//   - 审计 Top-10 #5:全 POST 路由无限速,任何 RCE 都可灌量
//   - OWASP Authentication Cheatsheet:5 次失败锁 5-15 分钟
//
// 实现要点：
//   - 内存 map[ip]*bucket + sync.Mutex(单进程够用,Go web server 默认单进程)
//   - 阈值:5 次失败 / 5 分钟锁 IP
//   - 全局 IP 限速:30 req/min 任意 POST(防暴力灌量)
//   - 持久化到 /data/panel-state/ratelimit.json(防重启清零)
//   - 不影响 /healthz /readyz /metrics(public endpoint)
//
// 参考:fail2ban / gorilla/csrf / OWASP API Security Top-10 (API4:2023)
package auth

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/yourname/ikev2-panel-v2/internal/fsutil"
)

// RateLimitConfig 限速参数(可调)。
type RateLimitConfig struct {
	// LoginFailThreshold 登录失败 N 次触发 IP 锁。
	LoginFailThreshold int
	// LoginLockDuration 锁定持续时间。
	LoginLockDuration time.Duration
	// LoginWindow 失败计数窗口(超过此时间未失败则清零)。
	LoginWindow time.Duration
	// GlobalPostLimit 全局 POST 路由限速(每 IP 每分钟请求数)。
	GlobalPostLimit int
	// GlobalPostWindow 统计窗口。
	GlobalPostWindow time.Duration
	// PersistPath 持久化文件路径(/data/panel-state/ratelimit.json)。
	// 空 = 不持久化(测试用)。
	PersistPath string
}

// DefaultRateLimitConfig 默认参数(家用 1-50 人)。
func DefaultRateLimitConfig(dataDir string) RateLimitConfig {
	return RateLimitConfig{
		LoginFailThreshold: 5,
		LoginLockDuration:  5 * time.Minute,
		LoginWindow:        5 * time.Minute,
		GlobalPostLimit:    30,
		GlobalPostWindow:   1 * time.Minute,
		PersistPath:        filepath.Join(dataDir, "panel-state", "ratelimit.json"),
	}
}

// ipBucket 单 IP 的登录失败计数。
type ipBucket struct {
	mu          sync.Mutex
	failCount   int
	firstFailAt time.Time
	lockedUntil time.Time
	postReqs    []time.Time // 全局 POST 时间戳环形缓冲
}

// IsLocked 检查 IP 是否被锁。
func (b *ipBucket) IsLocked(now time.Time) bool {
	return now.Before(b.lockedUntil)
}

// RecordFail 记录一次失败,返回是否触发锁定。
func (b *ipBucket) RecordFail(now time.Time, cfg RateLimitConfig) bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	// 窗口外,清零
	if b.firstFailAt.IsZero() || now.Sub(b.firstFailAt) > cfg.LoginWindow {
		b.firstFailAt = now
		b.failCount = 1
		return false
	}

	b.failCount++
	if b.failCount >= cfg.LoginFailThreshold {
		b.lockedUntil = now.Add(cfg.LoginLockDuration)
		// 触发锁定后清零,下次从 1 开始
		b.firstFailAt = now
		b.failCount = 0
		return true
	}
	return false
}

// RecordSuccess 清零失败计数。
func (b *ipBucket) RecordSuccess() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failCount = 0
	b.firstFailAt = time.Time{}
	b.lockedUntil = time.Time{}
}

// AllowPost 检查全局 POST 是否限速。返回 true = 放行。
func (b *ipBucket) AllowPost(now time.Time, cfg RateLimitConfig) bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	// 清理窗口外的旧时间戳
	cutoff := now.Add(-cfg.GlobalPostWindow)
	filtered := b.postReqs[:0]
	for _, t := range b.postReqs {
		if t.After(cutoff) {
			filtered = append(filtered, t)
		}
	}
	b.postReqs = filtered

	if len(b.postReqs) >= cfg.GlobalPostLimit {
		return false
	}
	b.postReqs = append(b.postReqs, now)
	return true
}

// RateLimiter 全局 rate limit 状态。
type RateLimiter struct {
	mu      sync.Mutex
	cfg     RateLimitConfig
	buckets map[string]*ipBucket
	persist map[string]persistedBucket // ip → 持久化字段(锁定信息)

	// v2.86-PR15 P0 修复:saveToDisk 期间可能并发触发(同一时间多 IP 失败,
	// 每个 IP 各自 RecordLoginFail → saveToDisk)。多个 goroutine 同时写
	// 同一个 .tmp 文件 → 后者 close 时前者已 rename,前者 close 时拿到
	// stale fd → 写错误 / tmp 残留。加 saveMu 串行化 flush。
	//
	// 注意:saveMu 不保护 rl.persist(rl.mu 负责);只保护磁盘 I/O。
	saveMu sync.Mutex
}

// persistedBucket 仅持久化关键字段(login fail count + lockedUntil),
// 不持久化 postReqs(每次重启清零,避免长时间累积)。
type persistedBucket struct {
	FailCount   int       `json:"fail_count"`
	FirstFailAt time.Time `json:"first_fail_at"`
	LockedUntil time.Time `json:"locked_until"`
}

// NewRateLimiter 构造。
func NewRateLimiter(cfg RateLimitConfig) *RateLimiter {
	rl := &RateLimiter{
		cfg:     cfg,
		buckets: make(map[string]*ipBucket),
		persist: make(map[string]persistedBucket),
	}
	if cfg.PersistPath != "" {
		_ = rl.loadFromDisk()
	}
	return rl
}

// CheckLogin 检查 IP 是否被锁。返回 true = 放行。
func (rl *RateLimiter) CheckLogin(ip string, now time.Time) bool {
	b := rl.getBucket(ip)
	b.mu.Lock()
	defer b.mu.Unlock()
	return !b.IsLocked(now)
}

// RecordLoginFail 记录登录失败。返回是否触发新锁定。
func (rl *RateLimiter) RecordLoginFail(ip string, now time.Time) bool {
	b := rl.getBucket(ip)
	locked := b.RecordFail(now, rl.cfg)
	rl.mu.Lock()
	rl.persist[ip] = persistedBucket{
		FailCount:   b.failCount,
		FirstFailAt: b.firstFailAt,
		LockedUntil: b.lockedUntil,
	}
	rl.mu.Unlock()
	if rl.cfg.PersistPath != "" {
		_ = rl.saveToDisk()
	}
	return locked
}

// RecordLoginSuccess 登录成功,清零失败计数。
func (rl *RateLimiter) RecordLoginSuccess(ip string) {
	b := rl.getBucket(ip)
	b.RecordSuccess()
	rl.mu.Lock()
	delete(rl.persist, ip)
	rl.mu.Unlock()
	if rl.cfg.PersistPath != "" {
		_ = rl.saveToDisk()
	}
}

// AllowPost 全局 POST 限速。返回 true = 放行。
func (rl *RateLimiter) AllowPost(ip string, now time.Time) bool {
	b := rl.getBucket(ip)
	return b.AllowPost(now, rl.cfg)
}

func (rl *RateLimiter) getBucket(ip string) *ipBucket {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	b, ok := rl.buckets[ip]
	if !ok {
		b = &ipBucket{}
		rl.buckets[ip] = b
		// 从持久化恢复
		if p, ok := rl.persist[ip]; ok {
			b.failCount = p.FailCount
			b.firstFailAt = p.FirstFailAt
			b.lockedUntil = p.LockedUntil
		}
	}
	return b
}

func (rl *RateLimiter) saveToDisk() error {
	if rl.cfg.PersistPath == "" {
		return nil
	}
	rl.mu.Lock()
	snap := make(map[string]persistedBucket, len(rl.persist))
	for k, v := range rl.persist {
		snap[k] = v
	}
	rl.mu.Unlock()

	data, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return err
	}
	// 串行化磁盘写入,防止并发触发 saveToDisk 时两个 goroutine 撞同一个 .tmp。
	rl.saveMu.Lock()
	defer rl.saveMu.Unlock()
	if err := os.MkdirAll(filepath.Dir(rl.cfg.PersistPath), 0o755); err != nil {
		return err
	}
	// v2.86-PR15 P0 修复:之前用 os.WriteFile 直接覆写 ratelimit.json。
	// RecordLoginFail/Success 每次触发都写一次 → 高频路径 → 容器 OOM kill
	// 发生在 write 中途 → JSON 半截 → loadFromDisk 解析失败 → 整个 rate-limit
	// 状态丢失 → 5 次锁定计数清零(等于让暴力破解不受限)。
	// 走 fsutil.AtomicWriteFile(write tmp + rename),保证持久化文件永远
	// 是完整的旧版或完整的新版,不会半截。
	//
	// 性能注:5 次失败后 + 1 次成功 = 6 次 fsync 写,30 req/min 下多次/秒。
	// fsutil.AtomicWriteFile 内部走 tmp + rename,比直接 WriteFile 慢约 30%
	// 但相对业务耗时(网络/磁盘 I/O)可忽略。如果未来 perf 成问题,可加 timer
	// 批量 flush(每 5 秒 sync 一次),但优先保留每次失败立即落盘的"安全语义"。
	return fsutil.AtomicWriteFile(rl.cfg.PersistPath, data, 0o600)
}

func (rl *RateLimiter) loadFromDisk() error {
	data, err := os.ReadFile(rl.cfg.PersistPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // 文件不存在不算错
		}
		return err
	}
	var snap map[string]persistedBucket
	if err := json.Unmarshal(data, &snap); err != nil {
		return fmt.Errorf("ratelimit: parse %s: %w", rl.cfg.PersistPath, err)
	}
	rl.mu.Lock()
	rl.persist = snap
	rl.mu.Unlock()
	return nil
}

// ClientIP 从 http.Request 取客户端 IP。
//
// v2.86-PR15 P0 修复:之前默认无条件信任 X-Forwarded-For / X-Real-IP。
// 这意味着公网部署时,任何客户端发 "X-Forwarded-For: 127.0.0.1" 即被识别为
// 本机,绕过 5 次锁定阈值 → 登录端点无限速。
//
// 修复:必须显式设置 IKEV2_TRUSTED_PROXY=true 才读 XFF/X-Real-IP(且 XFF
// 取**最左** IP,这是 RFC 7239 标准的"原始客户端"位置)。
// 未设置时,ClientIP 直接走 RemoteAddr — 跟 v2.86-PR9 之前的行为一致,
// 公网直接暴露无 NLB / 无反代时这是正确语义。
//
// 为什么不用 env 自动开启:
//   - "set if behind NLB" 是用户主动决策,不应该自动推断。
//   - 自动推断会让用户配了 NLB 但忘记 env 时出现"有些 IP 信任有些不信任"
//     的诡异行为。
//
// 为什么是 env 不在 web Server 构造时注入:
//   - 保持 ClientIP 是 pure function(http.Request → string),不依赖
//     全局变量,便于测试 + 复用(可换 Redis 限速 / 多 server 复用)。
//
// 测试覆盖:isTrustedProxy 默认读 env,但测试可通过 ratelimit_test.go 内的
// setTrustedProxyForTest() 临时覆盖,测试结束用 t.Cleanup 还原。
func isTrustedProxy() bool {
	return os.Getenv("IKEV2_TRUSTED_PROXY") == "true"
}

// trustedProxyOverride 测试用钩子;非 nil 时覆盖 env 行为。
// 仅测试代码可写(ratelimit_test.go 的 setTrustedProxyForTest),生产路径
// 永远走 isTrustedProxy() 读 env。
var trustedProxyOverride atomic.Pointer[bool]

func ClientIP(r *http.Request) string {
	trusted := isTrustedProxy()
	if v := trustedProxyOverride.Load(); v != nil {
		trusted = *v
	}
	if trusted {
		// X-Forwarded-For 格式:"client, proxy1, proxy2",RFC 7239 标准取**最左** IP
		//(最左是原始客户端,右侧追加每一层代理)。
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			// 取第一个 IP,处理前后空白
			first := strings.TrimSpace(xff)
			if idx := strings.IndexByte(first, ','); idx >= 0 {
				first = strings.TrimSpace(first[:idx])
			}
			if first != "" {
				return first
			}
		}
		// X-Real-IP:Nginx 反代常用(单层反代场景)
		if xri := r.Header.Get("X-Real-IP"); xri != "" {
			return xri
		}
	}
	// 默认走 RemoteAddr。严格 split("host:port"),失败时 fallback r.RemoteAddr 原值
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// LoginRateLimitMiddleware 登录端点专用 rate limit。
// 失败计数 + 锁定 + 全局 POST 限速。
func LoginRateLimitMiddleware(rl *RateLimiter, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := ClientIP(r)
		now := time.Now()
		if !rl.CheckLogin(ip, now) {
			// 锁定中,直接 429
			w.Header().Set("Retry-After", "300")
			http.Error(w, "too many failed attempts; try again in 5 minutes", http.StatusTooManyRequests)
			return
		}
		if !rl.AllowPost(ip, now) {
			w.Header().Set("Retry-After", "60")
			http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// PostRateLimitMiddleware 全局 POST 路由限速(非登录端点)。
// 防止任何已登录用户对受保护 POST 灌量。
func PostRateLimitMiddleware(rl *RateLimiter, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := ClientIP(r)
		if !rl.AllowPost(ip, time.Now()) {
			w.Header().Set("Retry-After", "60")
			http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// RecordLoginFailMiddleware 在 requireSession 之外(登录端点专用),失败后调 rl.RecordLoginFail。
// 设计:登录失败由 handler 自己决定什么时候 record,中间件不做猜测。
// 这个函数是公开 helper,handler 调它:
func RecordLoginFail(rl *RateLimiter, r *http.Request) bool {
	ip := ClientIP(r)
	return rl.RecordLoginFail(ip, time.Now())
}

// RecordLoginSuccessHelper 登录成功 helper。
func RecordLoginSuccess(rl *RateLimiter, r *http.Request) {
	ip := ClientIP(r)
	rl.RecordLoginSuccess(ip)
}
