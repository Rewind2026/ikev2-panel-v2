// 过期账户自动清理 goroutine：每 60 秒扫过期用户 → terminate + 禁用。
// 设计见 docs/design.md §3.7 + §5.9
package expiry

import (
	"context"
	"log/slog"
	"time"

	"github.com/yourname/ikev2-panel-v2/internal/store"
)

// SwanctlManager swanctl.Manager 子集（用于 terminate）。
type SwanctlManager interface {
	Terminate(ctx context.Context, username string) error
}

// UserStore store 子集。
type UserStore interface {
	GetExpiredUsers(ctx context.Context, nowUnix int64) ([]*store.User, error)
	SetUserEnabled(ctx context.Context, id int64, enabled bool) error
}

// Checker 周期扫描过期用户。
type Checker struct {
	swanctl SwanctlManager
	store   UserStore
	logger  *slog.Logger
	period  time.Duration
}

// NewChecker 构造 Checker（默认 60 秒）。
func NewChecker(s SwanctlManager, st UserStore, logger *slog.Logger) *Checker {
	return &Checker{
		swanctl: s,
		store:   st,
		logger:  logger,
		period:  60 * time.Second,
	}
}

// WithPeriod 自定义扫描周期（测试用）。
func (c *Checker) WithPeriod(d time.Duration) *Checker {
	c.period = d
	return c
}

// Run 阻塞运行，直到 ctx 取消。
func (c *Checker) Run(ctx context.Context) {
	t := time.NewTicker(c.period)
	defer t.Stop()
	c.logger.Info("expiry: starting", "period", c.period.String())
	for {
		select {
		case <-ctx.Done():
			c.logger.Info("expiry: stopped")
			return
		case <-t.C:
			c.check(ctx)
		}
	}
}

// check 单次扫描。
func (c *Checker) check(ctx context.Context) {
	now := time.Now().Unix()
	expired, err := c.store.GetExpiredUsers(ctx, now)
	if err != nil {
		c.logger.Error("expiry: get expired failed", "err", err)
		return
	}
	if len(expired) == 0 {
		return
	}

	for _, u := range expired {
		// 1. 强制下线已建立的 SA
		if err := c.swanctl.Terminate(ctx, u.Username); err != nil {
			// 不是致命错误（用户可能本来就没连接）
			c.logger.Warn("expiry: terminate failed", "user", u.Username, "err", err)
		}
		// 2. 标记 enabled = 0（但保留 .conf 和 DB 记录，方便续期）
		if err := c.store.SetUserEnabled(ctx, u.ID, false); err != nil {
			c.logger.Error("expiry: set enabled failed", "user", u.Username, "err", err)
			continue
		}
		c.logger.Info("expiry: user expired and terminated",
			"user", u.Username,
			"expires_at", u.ExpiresAt,
			"id", u.ID,
		)
	}
}