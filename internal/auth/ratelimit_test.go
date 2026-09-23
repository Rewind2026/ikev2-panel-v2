// v2.86-PR9:登录 rate limit 单元测试。
//
// 覆盖场景:
//   1. 5 次失败触发锁定
//   2. 锁定期间 CheckLogin 返回 false
//   3. 锁定过期后 CheckLogin 返回 true
//   4. 登录成功清零计数
//   5. 全局 POST 限速
//   6. 持久化往返
//   7. X-Forwarded-For / X-Real-IP / RemoteAddr 三种 IP 来源
package auth

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// helper:构造带 IP 的 *http.Request
func newReqWithIP(ip string) *http.Request {
	r := httptest.NewRequest("POST", "/login", nil)
	r.RemoteAddr = ip + ":12345"
	return r
}

func newReqWithXFF(xff string) *http.Request {
	r := httptest.NewRequest("POST", "/login", nil)
	r.RemoteAddr = "127.0.0.1:12345"
	r.Header.Set("X-Forwarded-For", xff)
	return r
}

func newReqWithXReal(ip string) *http.Request {
	r := httptest.NewRequest("POST", "/login", nil)
	r.RemoteAddr = "127.0.0.1:12345"
	r.Header.Set("X-Real-IP", ip)
	return r
}

// TestRateLimit_FiveFailTriggersLock 测试 5 次失败触发锁定。
func TestRateLimit_FiveFailTriggersLock(t *testing.T) {
	rl := NewRateLimiter(RateLimitConfig{
		LoginFailThreshold: 5,
		LoginLockDuration:  5 * time.Minute,
		LoginWindow:        5 * time.Minute,
	})
	ip := "1.2.3.4"
	now := time.Now()

	// 前 4 次不锁
	for i := 0; i < 4; i++ {
		locked := rl.RecordLoginFail(ip, now)
		if locked {
			t.Fatalf("attempt %d should not lock yet", i+1)
		}
	}
	// 第 5 次触发锁定
	locked := rl.RecordLoginFail(ip, now)
	if !locked {
		t.Fatal("5th attempt should lock the IP")
	}
	// 第 6 次已被锁,CheckLogin 返回 false
	if rl.CheckLogin(ip, now) {
		t.Fatal("CheckLogin should return false while locked")
	}
}

// TestRateLimit_LockExpiresAfterDuration 测试锁定过期。
func TestRateLimit_LockExpiresAfterDuration(t *testing.T) {
	rl := NewRateLimiter(RateLimitConfig{
		LoginFailThreshold: 5,
		LoginLockDuration:  100 * time.Millisecond,
		LoginWindow:        5 * time.Minute,
	})
	ip := "1.2.3.4"
	now := time.Now()

	// 5 次失败触发锁定
	for i := 0; i < 5; i++ {
		rl.RecordLoginFail(ip, now)
	}
	if rl.CheckLogin(ip, now) {
		t.Fatal("should be locked immediately")
	}

	// 锁定过期后放行
	later := now.Add(200 * time.Millisecond)
	if !rl.CheckLogin(ip, later) {
		t.Fatal("lock should have expired")
	}
}

// TestRateLimit_SuccessClearsFailCount 测试登录成功清零。
func TestRateLimit_SuccessClearsFailCount(t *testing.T) {
	rl := NewRateLimiter(RateLimitConfig{
		LoginFailThreshold: 5,
		LoginLockDuration:  5 * time.Minute,
		LoginWindow:        5 * time.Minute,
	})
	ip := "1.2.3.4"
	now := time.Now()

	// 4 次失败(没锁定)
	for i := 0; i < 4; i++ {
		rl.RecordLoginFail(ip, now)
	}
	// 登录成功
	rl.RecordLoginSuccess(ip)
	// 再 4 次失败不会触发锁定
	for i := 0; i < 4; i++ {
		locked := rl.RecordLoginFail(ip, now)
		if locked {
			t.Fatalf("after success, 4 fails should not lock, but attempt %d did", i+1)
		}
	}
}

// TestRateLimit_GlobalPostLimit 测试全局 POST 限速。
func TestRateLimit_GlobalPostLimit(t *testing.T) {
	rl := NewRateLimiter(RateLimitConfig{
		LoginFailThreshold: 5,
		LoginLockDuration:  5 * time.Minute,
		LoginWindow:        5 * time.Minute,
		GlobalPostLimit:    3,
		GlobalPostWindow:   1 * time.Minute,
	})
	ip := "1.2.3.4"
	now := time.Now()

	// 前 3 次放行
	for i := 0; i < 3; i++ {
		if !rl.AllowPost(ip, now) {
			t.Fatalf("POST %d should be allowed", i+1)
		}
	}
	// 第 4 次拒绝
	if rl.AllowPost(ip, now) {
		t.Fatal("POST 4 should be rejected (over limit)")
	}

	// 1 分钟后放行
	later := now.Add(61 * time.Second)
	if !rl.AllowPost(ip, later) {
		t.Fatal("POST after window should be allowed")
	}
}

// TestRateLimit_PersistenceRoundtrip 测试持久化往返。
func TestRateLimit_PersistenceRoundtrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ratelimit.json")

	cfg := RateLimitConfig{
		LoginFailThreshold: 5,
		LoginLockDuration:  5 * time.Minute,
		LoginWindow:        5 * time.Minute,
		GlobalPostLimit:    30,
		GlobalPostWindow:   1 * time.Minute,
		PersistPath:        path,
	}
	rl1 := NewRateLimiter(cfg)
	ip := "5.6.7.8"
	now := time.Now()
	// 5 次失败触发锁定
	for i := 0; i < 5; i++ {
		rl1.RecordLoginFail(ip, now)
	}

	// 检查文件存在且非空
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("persist file not created: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("persist file is empty")
	}

	// 新构造 rate limiter,加载持久化
	rl2 := NewRateLimiter(cfg)
	if rl2.CheckLogin(ip, now) {
		t.Fatal("lock should persist across restarts")
	}
}

// TestRateLimit_ClientIP_XForwardedFor 测试 X-Forwarded-For 取首个 IP。
func TestRateLimit_ClientIP_XForwardedFor(t *testing.T) {
	r := newReqWithXFF("203.0.113.5, 10.0.0.1, 10.0.0.2")
	got := ClientIP(r)
	want := "203.0.113.5"
	if got != want {
		t.Fatalf("ClientIP(XFF) = %q, want %q", got, want)
	}
}

// TestRateLimit_ClientIP_XRealIP 测试 X-Real-IP。
func TestRateLimit_ClientIP_XRealIP(t *testing.T) {
	r := newReqWithXReal("203.0.113.10")
	got := ClientIP(r)
	want := "203.0.113.10"
	if got != want {
		t.Fatalf("ClientIP(X-Real-IP) = %q, want %q", got, want)
	}
}

// TestRateLimit_ClientIP_RemoteAddr 测试无 proxy 头时取 RemoteAddr。
func TestRateLimit_ClientIP_RemoteAddr(t *testing.T) {
	r := newReqWithIP("203.0.113.20")
	got := ClientIP(r)
	want := "203.0.113.20"
	if got != want {
		t.Fatalf("ClientIP(RemoteAddr) = %q, want %q", got, want)
	}
}

// TestRateLimit_MultipleIPsIndependent 测试多 IP 独立计数。
func TestRateLimit_MultipleIPsIndependent(t *testing.T) {
	rl := NewRateLimiter(RateLimitConfig{
		LoginFailThreshold: 5,
		LoginLockDuration:  5 * time.Minute,
		LoginWindow:        5 * time.Minute,
	})
	now := time.Now()

	// IP A 锁定
	for i := 0; i < 5; i++ {
		rl.RecordLoginFail("1.1.1.1", now)
	}
	// IP B 不应受影响
	if !rl.CheckLogin("2.2.2.2", now) {
		t.Fatal("IP B should not be affected by IP A's lock")
	}
}

// TestRateLimit_WindowResets 测试窗口外清零。
func TestRateLimit_WindowResets(t *testing.T) {
	rl := NewRateLimiter(RateLimitConfig{
		LoginFailThreshold: 5,
		LoginLockDuration:  5 * time.Minute,
		LoginWindow:        100 * time.Millisecond, // 短窗口方便测试
	})
	ip := "1.2.3.4"
	now := time.Now()

	// 4 次失败
	for i := 0; i < 4; i++ {
		rl.RecordLoginFail(ip, now)
	}
	// 200ms 后(超出窗口)
	later := now.Add(200 * time.Millisecond)
	// 再 4 次失败不应触发锁定(窗口已重置)
	for i := 0; i < 4; i++ {
		locked := rl.RecordLoginFail(ip, later)
		if locked {
			t.Fatalf("after window reset, attempt %d should not lock", i+1)
		}
	}
}

// TestRateLimit_LoginMiddlewareBlocks 测试中间件层。
func TestRateLimit_LoginMiddlewareBlocks(t *testing.T) {
	rl := NewRateLimiter(RateLimitConfig{
		LoginFailThreshold: 5,
		LoginLockDuration:  5 * time.Minute,
		LoginWindow:        5 * time.Minute,
		GlobalPostLimit:    30,
		GlobalPostWindow:   1 * time.Minute,
	})
	ip := "9.9.9.9"
	now := time.Now()

	// 5 次失败触发锁定
	for i := 0; i < 5; i++ {
		rl.RecordLoginFail(ip, now)
	}

	// 中间件应该返回 429
	called := false
	handler := LoginRateLimitMiddleware(rl, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	rec := httptest.NewRecorder()
	r := newReqWithIP(ip)
	handler.ServeHTTP(rec, r)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rec.Code)
	}
	if called {
		t.Fatal("next handler should not be called when locked")
	}
}