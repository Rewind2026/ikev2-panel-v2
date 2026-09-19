package installtoken

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// TestIssue_Consume_Basic 验证：发 token → 消费 → 拿到 userID
func TestIssue_Consume_Basic(t *testing.T) {
	s := New(time.Minute)
	tok := s.Issue(42)

	got, err := s.Consume(tok)
	if err != nil {
		t.Fatalf("Consume: %v", err)
	}
	if got != 42 {
		t.Errorf("userID: got %d want 42", got)
	}
}

// TestConsume_OneTime 验证：同一个 token 第二次消费返回 ErrNotFound
func TestConsume_OneTime(t *testing.T) {
	s := New(time.Minute)
	tok := s.Issue(1)

	if _, err := s.Consume(tok); err != nil {
		t.Fatalf("first consume: %v", err)
	}
	if _, err := s.Consume(tok); !errors.Is(err, ErrNotFound) {
		t.Errorf("second consume: got %v want ErrNotFound", err)
	}
}

// TestConsume_Expired 验证：过期 token 返回 ErrNotFound
func TestConsume_Expired(t *testing.T) {
	now := time.Now()
	s := New(time.Minute)
	s.now = func() time.Time { return now }

	tok := s.Issue(7)

	// 时间快进 2 分钟，token 已过期
	s.now = func() time.Time { return now.Add(2 * time.Minute) }

	if _, err := s.Consume(tok); !errors.Is(err, ErrNotFound) {
		t.Errorf("expired consume: got %v want ErrNotFound", err)
	}
}

// TestConsume_Unknown 验证：随机 token 返回 ErrNotFound（不抛 panic）
func TestConsume_Unknown(t *testing.T) {
	s := New(time.Minute)
	if _, err := s.Consume("not-a-real-token"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown token: got %v want ErrNotFound", err)
	}
}

// TestToken_Uniqueness 验证：连续 Issue 100 次不重复
func TestToken_Uniqueness(t *testing.T) {
	s := New(time.Minute)
	seen := make(map[string]bool, 100)
	for i := 0; i < 100; i++ {
		tok := s.Issue(int64(i))
		if seen[tok] {
			t.Fatalf("duplicate token at i=%d: %s", i, tok)
		}
		seen[tok] = true
	}
}

// TestToken_Format 验证：token 是 base64url（URL safe），可直接放进 URL path
func TestToken_Format(t *testing.T) {
	s := New(time.Minute)
	tok := s.Issue(1)

	// 32 字节 → base64url 不带 padding = 43 字符
	if len(tok) != 43 {
		t.Errorf("token length: got %d want 43", len(tok))
	}

	// 不能含 + / =（base64 url 编码字符集）
	for _, c := range tok {
		if c == '+' || c == '/' || c == '=' {
			t.Errorf("token 含 URL 不安全字符 %q: %s", c, tok)
		}
	}
}

// TestSweep_RemovesExpired 验证：Sweep 清掉过期的，保留没过期的
func TestSweep_RemovesExpired(t *testing.T) {
	now := time.Now()
	s := New(time.Minute)
	s.now = func() time.Time { return now }

	old1 := s.Issue(1)
	old2 := s.Issue(2)
	fresh := s.Issue(3)

	// 时间快进 2 分钟，old1/old2 过期，fresh 也过期（同一时间发放）
	// 但 fresh 也会被 Sweep 清掉 —— 这里验证"过期的全清"
	s.now = func() time.Time { return now.Add(2 * time.Minute) }
	_ = fresh

	removed := s.Sweep()
	if removed != 3 {
		t.Errorf("Sweep removed: got %d want 3", removed)
	}
	if s.Len() != 0 {
		t.Errorf("after sweep Len: got %d want 0", s.Len())
	}
	_ = old1
	_ = old2
}

// TestSweep_KeepsFresh 验证：Sweep 不清未过期的
func TestSweep_KeepsFresh(t *testing.T) {
	s := New(time.Minute)
	tok := s.Issue(1)

	// 时间只过 30 秒，还在 TTL 内
	now := time.Now()
	s.now = func() time.Time { return now.Add(30 * time.Second) }

	removed := s.Sweep()
	if removed != 0 {
		t.Errorf("Sweep removed: got %d want 0", removed)
	}
	if s.Len() != 1 {
		t.Errorf("Len: got %d want 1", s.Len())
	}

	// token 还能正常消费
	if _, err := s.Consume(tok); err != nil {
		t.Errorf("fresh token consume after sweep: %v", err)
	}
}

// TestConcurrent_IssueConsume 验证：并发 Issue + Consume 不死锁、不重复
func TestConcurrent_IssueConsume(t *testing.T) {
	s := New(time.Minute)
	var wg sync.WaitGroup

	// 100 个 goroutine 并发 Issue
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tok := s.Issue(int64(i))
			if _, err := s.Consume(tok); err != nil {
				t.Errorf("goroutine %d consume: %v", i, err)
			}
		}(i)
	}
	wg.Wait()

	if s.Len() != 0 {
		t.Errorf("after concurrent issue+consume: Len=%d want 0", s.Len())
	}
}