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
	"sync"
	"time"
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
	mu             sync.Mutex
	failCount      int
	firstFailAt    time.Time
	lockedUntil    time.Time
	postReqs       []time.Time // 全局 POST 时间戳环形缓冲
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
	if err := os.MkdirAll(filepath.Dir(rl.cfg.PersistPath), 0o755); err != nil {
		return err
	}
	return os.WriteFile(rl.cfg.PersistPath, data, 0o600)
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

// ClientIP 从 http.Request 取客户端 IP,处理 X-Forwarded-For(单层反向代理)。
func ClientIP(r *http.Request) string {
	// X-Forwarded-For:格式 "client, proxy1, proxy2",取第一个
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		for i := 0; i < len(xff); i++ {
			if xff[i] == ',' || xff[i] == ' ' {
				return xff[:i]
			}
		}
		return xff
	}
	// X-Real-IP:Nginx 反代常用
	if xri := r.Header.Get("X-Real-IP"); xri != "" {
		return xri
	}
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