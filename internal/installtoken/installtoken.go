// 一次性安装 token：二维码扫码后浏览器访问 /install/{token}，
// 服务端验证 token 有效后 stream mobileconfig，iOS 看到正确的 MIME
// 类型会自动弹"安装描述文件"。
//
// 设计动机（2026-09-17）：
//
//	之前试过 A 方案（base64 data URL 内联）—— 二维码密度到了 Version 40 + High
//	容错极限（177x177 模块、30% 容错），手机相机根本识别不出来。
//	B 方案 QR 里只编码短 URL（< 80 字节），二维码密度正常，手机扫码好使。
//
// 安全模型：
//   - token 是 32 字节随机数（base64url）→ 256 bit 熵，无法猜测
//   - 一次性：消费即删（即使是同一个 token 第二次访问也 404）
//   - TTL：默认 10 分钟（用户扫码 + 走流程必须 < 10 分钟完成）
//   - 不要求登录：扫码场景就是"用户没账号密码也能装配置"，跟登录态互斥
//   - token 只绑定 userID，不绑 IP/UA（移动网络 IP/UA 易变）
//
// 并发：使用 sync.Mutex 保护 map（panel 是低 QPS 场景，没必要引入 sync.Map）。
package installtoken

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"sync"
	"time"
)

// ErrNotFound token 不存在或已过期 / 已消费。
var ErrNotFound = errors.New("install token not found")

// Token 一次性安装 token。
type Token struct {
	UserID    int64
	ExpiresAt time.Time
}

// Store token 存储。
type Store struct {
	mu    sync.Mutex
	tokens map[string]Token
	ttl   time.Duration
	now   func() time.Time // 可注入，便于测试
}

// New 构造 token 存储。
func New(ttl time.Duration) *Store {
	return &Store{
		tokens: make(map[string]Token),
		ttl:   ttl,
		now:   time.Now,
	}
}

// Issue 生成新 token，绑定 userID，记录过期时间。
//
// 返回的 token 字符串可直接放进 URL：/install/{token}
func (s *Store) Issue(userID int64) string {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		// crypto/rand 失败是 OS 级别问题，没法 fallback
		panic("installtoken: crypto/rand failed: " + err.Error())
	}
	token := base64.RawURLEncoding.EncodeToString(raw[:])

	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokens[token] = Token{
		UserID:    userID,
		ExpiresAt: s.now().Add(s.ttl),
	}
	return token
}

// Consume 验证并消费 token（一次性）。
//
// 成功：返回 userID + 删除 token。
// 失败（不存在 / 过期）：返回 ErrNotFound（不区分原因，避免泄露状态）。
func (s *Store) Consume(token string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	t, ok := s.tokens[token]
	if !ok {
		return 0, ErrNotFound
	}
	// 一次性：消费即删
	delete(s.tokens, token)

	if s.now().After(t.ExpiresAt) {
		return 0, ErrNotFound
	}
	return t.UserID, nil
}

// Sweep 清扫过期 token（防止 map 无限增长）。
//
// 调用方应周期性执行（例如每 5 分钟）。goroutine 写在 cmd/server 里。
// 返回被清除的 token 数。
func (s *Store) Sweep() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	removed := 0
	for k, t := range s.tokens {
		if now.After(t.ExpiresAt) {
			delete(s.tokens, k)
			removed++
		}
	}
	return removed
}

// Len 返回当前 token 数（测试用）。
func (s *Store) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.tokens)
}