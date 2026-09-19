// Web 上下文：从请求 context 取 session + admin。
// 设计见 docs/design.md §3.2
package web

import (
	"context"
	"errors"

	"github.com/yourname/ikev2-panel-v2/internal/store"
)

// ctxKey 上下文键类型，避免与其它包冲突。
type ctxKey int

const (
	ctxKeySession ctxKey = iota
	ctxKeyAdmin
)

// SessionFrom 从请求 context 取 session（由 middleware 写入）。
func SessionFrom(ctx context.Context) (*store.Session, bool) {
	s, ok := ctx.Value(ctxKeySession).(*store.Session)
	return s, ok
}

// AdminFrom 从请求 context 取 admin。
func AdminFrom(ctx context.Context) (*store.Admin, bool) {
	a, ok := ctx.Value(ctxKeyAdmin).(*store.Admin)
	return a, ok
}

// ErrUnauthorized 表示请求缺 session 或 session 无效。
var ErrUnauthorized = errors.New("web: unauthorized")

// csrfTokenOf 安全取出 session 的 CSRF token（无 session 返回空串）。
//
// 所有受保护 handler 在渲染模板前调用，把 token 传给 data，模板生成 hidden input。
func csrfTokenOf(sess *store.Session) string {
	if sess == nil {
		return ""
	}
	return sess.CSRFToken
}