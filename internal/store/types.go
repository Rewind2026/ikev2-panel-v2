// 数据模型：User / Session / Admin
package store

import "time"

// User 表的一行。
// enabled=true 表示启用；expires_at > 0 表示有到期时间。
type User struct {
	ID             int64
	Username       string
	Password       string // 明文（v2 妥协项，见 design §4.3）
	Enabled        bool
	Note           string
	SpeedLimitMbps int   // 0 = 不限速
	ExpiresAt      int64 // unix timestamp；0 = 永不过期
	BytesInTotal   int64
	BytesOutTotal  int64
	CreatedAt      int64
	UpdatedAt      int64
	LastUsedAt    int64
}

// IsExpired 在当前时刻判断用户是否过期。
func (u *User) IsExpired(now time.Time) bool {
	if u.ExpiresAt == 0 {
		return false
	}
	return u.ExpiresAt < now.Unix()
}

// Session 表的一行。
type Session struct {
	ID        string // 32 字节 hex
	CSRFToken string // 32 字节 hex
	CreatedAt int64
	ExpiresAt int64
	UserAgent string
}

// Admin 表的一行。v2 全表只有 id=1 一条记录。
type Admin struct {
	ID           int64
	Username     string
	PasswordHash string // bcrypt
	CreatedAt    int64
	UpdatedAt    int64
}