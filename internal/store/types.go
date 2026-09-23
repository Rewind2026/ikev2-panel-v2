// 数据模型：User / Session / Admin
package store

import "time"

// User 表的一行。
// enabled=true 表示启用；expires_at > 0 表示有到期时间。
// MobileConfigOpts:每用户的 mobileconfig 覆盖项 JSON 字符串,空 = 用全局默认。
//
// 字段含义见 cert.MobileConfigOpts:
//   - UserDefinedName     连接显示名(空 = "IKEv2 VPN")
//   - DisconnectOnSleep   睡眠时是否断 VPN(nil = false,默认保活)
//   - NATKeepaliveEnabled 是否启用 NAT 保活(nil = true)
//   - NATKeepaliveInterval NAT 保活秒数(0 = 60)
//   - OnDemandEnabled     是否启用按需(nil = true)
//   - OnDemandProfile     按需场景 preset 名称
//   - OnDemandSSID        home_wifi_disconnect 模式用的 SSID
//   - IncludeAllNetworks  全局路由(nil = true)
//   - ExcludeLocalNetworks 排除局域网(nil = true)
//   - DNSServers          DNS 列表(空 = 默认 1.1.1.1/8.8.8.8/...)
//
// v2.86-PR12.21:管理员后台可调,影响 /users/{id}/mobileconfig 渲染。
type User struct {
	ID               int64
	Username         string
	Password         string // 明文（v2 妥协项，见 design §4.3）
	Enabled          bool
	Note             string
	SpeedLimitMbps   int   // 0 = 不限速
	ExpiresAt        int64 // unix timestamp；0 = 永不过期
	BytesInTotal     int64
	BytesOutTotal    int64
	CreatedAt        int64
	UpdatedAt        int64
	LastUsedAt       int64
	MobileConfigOpts string // 每用户 mobileconfig 覆盖项(JSON 字符串)
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