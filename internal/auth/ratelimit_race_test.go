package auth

import (
	"sync"
	"testing"
	"time"
)

// TestVerify_RecordLoginFailRaceFixed 是 v2.86-pr23x data race 修复的回归测试。
//
// 修复前:RecordLoginFail 在 RecordFail 已释放 b.mu 之后,
// 直接读取 b.failCount / b.firstFailAt / b.lockedUntil 写入 rl.persist,
// -race 实测报 ratelimit.go:88 写 vs :181 读。
// 修复后:RecordFail 在锁内返回快照,调用方只用快照。
//
// 本测试构造真实并发:多 goroutine 同时对同一 IP 触发登录失败(模拟暴力破解)。
// 若修复被回退,-race 下必然失败。
func TestVerify_RecordLoginFailRaceFixed(t *testing.T) {
	rl := NewRateLimiter(RateLimitConfig{
		// 阈值调高,避免提前触发锁定清零干扰观测
		LoginFailThreshold: 1000000,
		LoginLockDuration:  time.Minute,
		LoginWindow:        time.Hour,
		GlobalPostLimit:    1000000,
		GlobalPostWindow:   time.Minute,
	})
	now := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 300; j++ {
				rl.RecordLoginFail("1.2.3.4", now)
			}
		}()
	}
	wg.Wait()
}

// TestVerify_RecordFailSnapshotConsistency 验证快照内容与桶内状态一致。
// 防止未来有人改动 RecordFail 时忘记同步 snapshot(),导致持久化数据失真。
func TestVerify_RecordFailSnapshotConsistency(t *testing.T) {
	b := &ipBucket{}
	cfg := RateLimitConfig{
		LoginFailThreshold: 3,
		LoginLockDuration:  time.Minute,
		LoginWindow:        time.Hour,
	}
	now := time.Now()

	// 连续失败,每次都应拿到与桶内一致的快照
	for i := 1; i <= 2; i++ {
		locked, snap := b.RecordFail(now, cfg)
		if locked {
			t.Fatalf("第 %d 次失败不应触发锁定", i)
		}
		if snap.FailCount != i {
			t.Errorf("第 %d 次快照 FailCount = %d, 期望 %d", i, snap.FailCount, i)
		}
		if !snap.FirstFailAt.Equal(now) {
			t.Errorf("第 %d 次快照 FirstFailAt = %v, 期望 %v", i, snap.FirstFailAt, now)
		}
	}

	// 第 3 次达到阈值,应触发锁定并清零计数
	locked, snap := b.RecordFail(now, cfg)
	if !locked {
		t.Error("第 3 次失败应触发锁定")
	}
	if snap.FailCount != 0 {
		t.Errorf("锁定后快照 FailCount = %d, 期望 0(已清零)", snap.FailCount)
	}
	if !snap.LockedUntil.Equal(now.Add(cfg.LoginLockDuration)) {
		t.Errorf("快照 LockedUntil = %v, 期望 %v", snap.LockedUntil, now.Add(cfg.LoginLockDuration))
	}
}
