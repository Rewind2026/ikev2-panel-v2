// 一次性 flash 消息：通过 session 存储,渲染时消费一次就消失。
//
// 设计动机（P1-A 修复 v2-80+ backlog）：
//
// v2 之前密码、状态消息走 query string：
//
//	/users?flash_new=<password>
//	/users/<id>?flash_pw=<password>&flash=已重置
//
// 问题：
//   - 密码出现在 URL → 浏览器历史、nginx 代理日志、Server log、Referer header 都泄露
//   - 用户从 /users 跳走再按"后退"按钮，能再看到密码一次
//   - 把密码 paste 给第三方时容易把整 URL 发出去
//
// 解决方案：把 flash 消息存到 server-side store（按 sessionID 索引），redirect 后
// 模板渲染时取出并立即销毁。
//
// 类似 Django 的 messages framework / Rails 的 flash。但本项目只需要
// 两个字段：Flash（普通提示）+ NewPassword（密码）。所以不需要泛型
// messages 框架，直接做专用 store 即可。
//
// 安全模型：
//   - FlashStore key = session.ID（cookie session 是 256 bit 熵）
//   - 每个 message 自带 TTL（默认 5 分钟）
//   - 一次性消费（Consume 后删除，避免后退按钮再看到）
//   - 绑定到 session：不绑 IP/UA（同一 admin 可能多设备登录，IP 会变）
//
// 区别于 installtoken：
//   - installtoken 是给"陌生访客扫码"用的，无 session
//   - flash 是给"已登录管理员"用的，强依赖 session
//   - 因此放在 internal/web 包内（web 包已经依赖 session），不开新包
package web

import (
	"errors"
	"sync"
	"time"
)

// ErrFlashNotFound flash 不存在 / 已消费 / 已过期。
var ErrFlashNotFound = errors.New("flash not found")

// Flash 一次性消息。
type Flash struct {
	NewPassword string    // 用户创建/重置密码后一次性显示
	Message     string    // 普通提示（"用户已删除"等）
	// Kind v2.86-pr23h:flash 类别。
	//   "" / "info"  — 普通提示(蓝色 banner-info)
	//   "success"   — 成功(绿色)
	//   "error"     — 错误(红色 banner-danger)
	// 模板渲染时按 Kind 切换 CSS class,前端 JS 也用 data-kind 做自动滚动/dismiss 行为。
	Kind      string
	ExpiresAt time.Time
}

// FlashStore flash 存储（按 sessionID 索引）。
type FlashStore struct {
	mu    sync.Mutex
	flashes map[string]Flash // key = sessionID
	ttl   time.Duration
	now   func() time.Time // 可注入，便于测试
}

// NewFlashStore 构造 flash 存储，ttl 通常 5 分钟。
func NewFlashStore(ttl time.Duration) *FlashStore {
	return &FlashStore{
		flashes: make(map[string]Flash),
		ttl:    ttl,
		now:    time.Now,
	}
}

// Set 给指定 sessionID 写一条 flash（覆盖同 session 的旧 flash）。
//
// 设计选择：不是 push（保留多个），是 set（最新覆盖）。
// 原因：用户不会同时有"密码 + 普通消息"两条需要展示，
//       且 set 更简单，避免"取哪个"的歧义。
func (s *FlashStore) Set(sessionID string, f Flash) {
	s.SetFor(sessionID, "", f)
}

// SetFor v2.86-pr23l:把 flash 写到指定 slot,slot 为空 = 默认 slot(等同 Set)。
//
// 设计:支持「同 session 多个 slot」(如 aliyun 保存 / ddns 保存 分别有不同的反馈),
// 互不覆盖,模板各取各的。slot 内仍然是 set(最新覆盖),保证语义简洁。
//
// 用途:
//   - aliyun save/clear handler → SetFor(sess.ID, "aliyun", Flash{...})
//   - ddns  handler 继续用 Set()(默认 slot,语义不变)
//   - 渲染时:Consume → 默认 slot,ConsumeFor(sess.ID, "aliyun") → aliyun slot
func (s *FlashStore) SetFor(sessionID, slot string, f Flash) {
	if sessionID == "" {
		return // 没有 session（未登录或登录前），不能 set
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	f.ExpiresAt = s.now().Add(s.ttl)
	s.flashes[s.flashKey(sessionID, slot)] = f
}

// Consume 取并删除 flash（一次性）。
//
// 失败（不存在 / 过期）：返回 ErrFlashNotFound。
// 不区分原因，避免泄露 flash 状态。
func (s *FlashStore) Consume(sessionID string) (Flash, error) {
	return s.ConsumeFor(sessionID, "")
}

// ConsumeFor v2.86-pr23l:从指定 slot 取并删除 flash。
// slot 为空 = 默认 slot(等同 Consume)。
func (s *FlashStore) ConsumeFor(sessionID, slot string) (Flash, error) {
	if sessionID == "" {
		return Flash{}, ErrFlashNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	f, ok := s.flashes[s.flashKey(sessionID, slot)]
	if !ok {
		return Flash{}, ErrFlashNotFound
	}
	// 一次性：消费即删
	delete(s.flashes, s.flashKey(sessionID, slot))

	if s.now().After(f.ExpiresAt) {
		return Flash{}, ErrFlashNotFound
	}
	return f, nil
}

// flashKey v2.86-pr23l:组合 sessionID + slot 为 map key。
//
// 内部实现细节,不导出。slot 为空时不带 "|" 后缀,保证默认 slot 与历史
// 写入的 key 完全一致(Set() 写的 key 就是 sessionID)。
func (s *FlashStore) flashKey(sessionID, slot string) string {
	if slot == "" {
		return sessionID
	}
	return sessionID + "|" + slot
}

// Sweep 清扫过期 flash（防 map 无限增长）。
//
// 5 分钟一次足够（ttl=5min，flash 过期就立刻被清掉）。
// 返回清除数。
func (s *FlashStore) Sweep() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	removed := 0
	for k, f := range s.flashes {
		if now.After(f.ExpiresAt) {
			delete(s.flashes, k)
			removed++
		}
	}
	return removed
}

// Len 测试用。
func (s *FlashStore) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.flashes)
}