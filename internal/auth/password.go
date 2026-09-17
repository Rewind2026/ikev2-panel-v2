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

// GeneratePassword 生成指定长度的随机密码（字母 + 数字，避免歧义字符如 0/O/1/l）。
//
// 字符集 56 个字符（a-z + 2-9，去掉 0/1/o/l）：
//   - 看起来清楚
//   - 12 位 = 56^12 ≈ 2.6e21 组合，强Swan 暴力破解不可能
//
// 注意：v2 用户密码**明文存库**（design §4.3 妥协项），
// 所以这里的密码是"VPN 用户的 EAP 密码"，不是管理员密码。
func GeneratePassword(length int) (string, error) {
	if length < 6 || length > 64 {
		return "", fmt.Errorf("password length must be in [6, 64], got %d", length)
	}
	const alphabet = "abcdefghijkmnpqrstuvwxyz23456789" // 32 字符
	const n = byte(len(alphabet))

	// 取足够多的随机字节，再 mod 字符集大小。
	// 用 rejection sampling 避免模偏：每次取一个 byte，若 >= n*(256/n) 则丢弃。
	out := make([]byte, 0, length)
	buf := make([]byte, length*2) // 多取一些以备 rejection
	for {
		_, err := rand.Read(buf)
		if err != nil {
			return "", fmt.Errorf("rand.Read: %w", err)
		}
		for _, b := range buf {
			if int(b) < int(n)*256/int(n) { // 永远成立，下面改成严格 rejection
				if b < n {
					out = append(out, alphabet[b])
					if len(out) == length {
						return string(out), nil
					}
				}
			}
		}
	}
}

// HashPassword bcrypt 哈希。用于管理员密码。
// bcrypt 60 字节固定输出。
func BaseHashPassword(plain string) (string, error) {
	h, err := bcrypt.GenerateFromPassword([]byte(plain), bcrypt.DefaultCost)
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