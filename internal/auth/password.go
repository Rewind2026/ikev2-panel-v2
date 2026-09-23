// 管理员密码：随机生成 + bcrypt 哈希校验。
// 设计见 docs/design.md §4.1
package auth

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"

	"golang.org/x/crypto/bcrypt"
)

// ErrInvalidPassword 密码校验失败。
var ErrInvalidPassword = errors.New("auth: invalid password")

// bcryptCost 是 admin 密码哈希的 cost 参数。
// v2.85-PR2:从 bcrypt.DefaultCost (=10) 升级到 12。
//   - OWASP Password Storage Cheat Sheet (2023) 推荐 bcrypt cost ≥ 12
//   - cost=10 单次 hash ≈ 60ms(2026 主流 CPU)
//   - cost=12 单次 hash ≈ 240ms,登录可接受;离线暴力破解 ×4 成本
//   - 旧 cost=10 生成的 hash 仍能验证通过(因为 bcrypt hash 自带 cost 字段),
//     仅新生成 hash 走 cost=12;不做 rehash on login(留 v3.0)
const bcryptCost = 12

// GeneratePassword 生成指定长度的随机密码（字母 + 数字，避免歧义字符如 0/O/1/l）。
//
// 字符集 32 个字符(a-z 去掉 o/l + 2-9,共 24+8 = 32):
//   - 看起来清楚(避免手抄出错)
//   - 12 位 = 32^12 ≈ 1.15e18 组合,强Swan 暴力破解实际不可行
//
// 注意：v2 用户密码**明文存库**（design §4.3 妥协项），
// 所以这里的密码是"VPN 用户的 EAP 密码"，不是管理员密码。
func GeneratePassword(length int) (string, error) {
	if length < 6 || length > 64 {
		return "", fmt.Errorf("password length must be in [6, 64], got %d", length)
	}
	const alphabet = "abcdefghijkmnpqrstuvwxyz23456789" // 24 字母 + 8 数字 = 32 字符
	const n = byte(len(alphabet))                      // n = 32

	// rejection sampling: 只接受 < n 的字节,避免 mod 偏。
	// n=32 能整除 256,所以 `b < n` 等价于取字节低 5 bit 后判 < 32,
	// 256 个字节 → 8 个落入 [0,32),其余 7/8 丢弃;取 length*2 字节足够。
	out := make([]byte, 0, length)
	buf := make([]byte, length*2)
	for {
		if _, err := rand.Read(buf); err != nil {
			return "", fmt.Errorf("rand.Read: %w", err)
		}
		for _, b := range buf {
			if b < n {
				out = append(out, alphabet[b])
				if len(out) == length {
					return string(out), nil
				}
			}
		}
	}
}

// HashPassword bcrypt 哈希。用于管理员密码。
// bcrypt 60 字节固定输出。
// v2.85-PR2:cost 从 10 (bcrypt.DefaultCost) 升级到 12 (auth.bcryptCost)。
func BaseHashPassword(plain string) (string, error) {
	h, err := bcrypt.GenerateFromPassword([]byte(plain), bcryptCost)
	if err != nil {
		return "", fmt.Errorf("bcrypt.GenerateFromPassword: %w", err)
	}
	return string(h), nil
}

// VerifyPassword bcrypt 校验。
func VerifyPassword(hash, plain string) error {
	err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(plain))
	if err != nil {
		if errors.Is(err, bcrypt.ErrMismatchedHashAndPassword) {
			return ErrInvalidPassword
		}
		return fmt.Errorf("bcrypt.CompareHashAndPassword: %w", err)
	}
	return nil
}

// GenerateSessionID 生成 32 字节随机 session id（hex 64 字符）。
func GenerateSessionID() (string, error) {
	return randomHex(32)
}

// GenerateCSRFToken 生成 32 字节随机 CSRF token（hex 64 字符）。
func GenerateCSRFToken() (string, error) {
	return randomHex(32)
}

func randomHex(nBytes int) (string, error) {
	b := make([]byte, nBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("rand.Read: %w", err)
	}
	// base64.RawURLEncoding：无 padding，URL-safe
	return base64.RawURLEncoding.EncodeToString(b), nil
}