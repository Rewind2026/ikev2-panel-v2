// 流量采集后台 goroutine：每 5 分钟调 swanctl --list-sas → 计算 delta → 累加到 DB。
//
// 设计见 docs/architecture.md §3.6
//
// 关键设计（v2 修复 B7：流量统计精度修复）：
//   - VICI list-sas 推回的是每个 Child SA 的累计 bytes-in/out（自 SA 建立以来总字节数）
//   - 不能直接累加：每 5 分钟采集会把全量当增量写库，导致 bytes_in_total 数值
//     远超实际流量
//   - 正确做法：collector 内部维护 map[saKey]prev{bytesIn, bytesOut}，
//     下次 list-sas 拿到同 key 时 delta = current - prev，delta 入库
//   - SA 断开（uniqueid 不再出现）→ 把对应 prev 删掉，避免内存泄漏
//
// saKey 格式："<uniqueid>/<child-name>"，同一 IKE_SA 下多 child 时分别统计
package limit

import (
	"context"
	"log/slog"
	"time"

	"github.com/yourname/ikev2-panel-v2/internal/swanctl"
)

// SwanctlManager 是 swanctl.Manager 的最小子集（便于测试用 mock）。
type SwanctlManager interface {
	ListSAs(ctx context.Context) ([]swanctl.SA, error)
}

// UserStore 是 store.Store 的最小子集。
type UserStore interface {
	IncrementUserBytes(ctx context.Context, username string, in, out int64) error
}

// Collector 周期采集活跃 SA 的流量增量。
type Collector struct {
	swanctl SwanctlManager
	store   UserStore
	logger  *slog.Logger
	period  time.Duration

	// prevBytes 内存里跟踪每个 SA+child 的累计字节。
	//   key = "<ike-sa-uniqueid>/<child-name>"
	//   val = 上一次 list-sas 时的 bytes-in / bytes-out
	// 重启 collector 时丢失：因为 bytes_in_total 是累计绝对值（不可能精确还原）；
	// 重启后第一次采集只能记录"重启后至今"的增量——这是设计妥协，可接受。
	prevBytes map[string][2]int64
}

// NewCollector 构造 Collector（默认 5 分钟）。
func NewCollector(s SwanctlManager, st UserStore, logger *slog.Logger) *Collector {
	return &Collector{
		swanctl:   s,
		store:     st,
		logger:    logger,
		period:    5 * time.Minute,
		prevBytes: make(map[string][2]int64),
	}
}

// WithPeriod 自定义采集周期（测试用）。
func (c *Collector) WithPeriod(d time.Duration) *Collector {
	c.period = d
	return c
}

// Run 阻塞运行，直到 ctx 取消。
func (c *Collector) Run(ctx context.Context) {
	t := time.NewTicker(c.period)
	defer t.Stop()
	c.logger.Info("collector: starting", "period", c.period.String())
	for {
		select {
		case <-ctx.Done():
			c.logger.Info("collector: stopped")
			return
		case <-t.C:
			c.collect(ctx)
		}
	}
}

// CollectForTest 单次采集（测试专用）。
func (c *Collector) CollectForTest(ctx context.Context) {
	c.collect(ctx)
}

// collect 单次采集：拉所有 SA → 计算每个 SA+child 的 delta → 入库。
//
// delta 计算规则：
//   - 当前 bytes < prev：SA 重建（uniqueid 复用但重新协商），delta 视为 current（绝对值）
//     不做"防回绕"处理，因为这场景罕见且难以精确处理；记 debug 日志
//   - prev 不存在（首次见到该 SA）：delta = current，first-sample 入库
//   - 当前 SA+child 不再出现：从 prevBytes 删除（SA 断开，避免内存泄漏）
func (c *Collector) collect(ctx context.Context) {
	sas, err := c.swanctl.ListSAs(ctx)
	if err != nil {
		c.logger.Error("collector: list-sas failed", "err", err)
		return
	}
	if len(sas) == 0 {
		// 所有 SA 都断了——清空 prevBytes（防止以后同 uniqueid 重用导致错误 delta）
		if len(c.prevBytes) > 0 {
			c.logger.Debug("collector: all SAs cleared, reset prevBytes", "prev_count", len(c.prevBytes))
			c.prevBytes = make(map[string][2]int64)
		}
		return
	}

	// seen 本轮出现过的所有 key（用于检测 SA 断开）
	seen := make(map[string]struct{}, len(c.prevBytes))

	// 按 username 聚合 delta（一个用户可能有多个 SA/Child SA，合并上报）
	deltaByUser := make(map[string][2]int64) // [in, out]

	for _, sa := range sas {
		if sa.IkeState != "ESTABLISHED" {
			continue
		}
		user := sa.RemoteID
		if user == "" {
			continue
		}
		for childName, child := range sa.Children {
			key := sa.UniqueID + "/" + childName
			seen[key] = struct{}{}

			prev, exists := c.prevBytes[key]
			currentIn := child.BytesIn
			currentOut := child.BytesOut

			var deltaIn, deltaOut int64
			switch {
			case !exists:
				// 首次见到该 SA：delta = current（first-sample）
				deltaIn = currentIn
				deltaOut = currentOut
				c.logger.Debug("collector: first sample",
					"user", user, "sa", sa.UniqueID, "child", childName,
					"in", deltaIn, "out", deltaOut)
			case currentIn < prev[0] || currentOut < prev[1]:
				// 计数器回绕或 SA 重建：delta = current（视为绝对值）
				c.logger.Debug("collector: counter reset/wrap",
					"user", user, "sa", sa.UniqueID, "child", childName,
					"prev_in", prev[0], "curr_in", currentIn,
					"prev_out", prev[1], "curr_out", currentOut)
				deltaIn = currentIn
				deltaOut = currentOut
			default:
				deltaIn = currentIn - prev[0]
				deltaOut = currentOut - prev[1]
			}

			c.prevBytes[key] = [2]int64{currentIn, currentOut}

			prev2 := deltaByUser[user]
			deltaByUser[user] = [2]int64{prev2[0] + deltaIn, prev2[1] + deltaOut}
		}
	}

	// 清理断开的 SA key（避免内存泄漏，也避免 SA 重建时误算 delta）
	for k := range c.prevBytes {
		if _, ok := seen[k]; !ok {
			delete(c.prevBytes, k)
		}
	}

	for user, d := range deltaByUser {
		if d[0] == 0 && d[1] == 0 {
			continue // 没有流量变化就不写 DB（减负）
		}
		if err := c.store.IncrementUserBytes(ctx, user, d[0], d[1]); err != nil {
			c.logger.Error("collector: increment failed", "user", user, "err", err)
			continue
		}
	}
	c.logger.Debug("collector: stats collected",
		"users", len(deltaByUser), "sas", len(sas), "tracked_keys", len(c.prevBytes))
}