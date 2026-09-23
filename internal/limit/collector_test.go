package limit

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/yourname/ikev2-panel-v2/internal/swanctl"
)

// TestCollectorFirstSampleDelta 验证：第一次采集把当前累计值当 delta 入库（first-sample）。
func TestCollectorFirstSampleDelta(t *testing.T) {
	ms := &mockSwanctl{
		saList: []swanctl.SA{
			{UniqueID: "1", IkeState: "ESTABLISHED", RemoteID: "alice", Children: map[string]swanctl.ChildSA{
				"c1": {BytesIn: 100, BytesOut: 50},
			}},
			{UniqueID: "2", IkeState: "CONNECTING", RemoteID: "bob"},
		},
	}
	mst := &mockStore{bytes: map[string][2]int64{}}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := NewCollector(ms, mst, logger)

	c.CollectForTest(context.Background())

	if got := mst.bytes["alice"]; got != [2]int64{100, 50} {
		t.Errorf("alice first sample: got %v want [100 50]", got)
	}
	if _, ok := mst.bytes["bob"]; ok {
		t.Error("bob should not be in bytes (CONNECTING state)")
	}
}

// TestCollectorDeltaTracksIncrement 验证：第二次采集正确计算 delta，不累加全量。
//
// 这是 B7 修复的核心场景：旧版本会把 200 当增量入库，实际应该只入 100。
func TestCollectorDeltaTracksIncrement(t *testing.T) {
	ms := &mockSwanctl{
		saList: []swanctl.SA{
			{UniqueID: "1", IkeState: "ESTABLISHED", RemoteID: "alice", Children: map[string]swanctl.ChildSA{
				"c1": {BytesIn: 100, BytesOut: 50},
			}},
		},
	}
	mst := &mockStore{bytes: map[string][2]int64{}}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := NewCollector(ms, mst, logger)

	c.CollectForTest(context.Background())
	// alice 此时应该是 [100, 50]

	// 第二次：模拟流量增加 100 in, 100 out
	ms.SetSAs([]swanctl.SA{
		{UniqueID: "1", IkeState: "ESTABLISHED", RemoteID: "alice", Children: map[string]swanctl.ChildSA{
			"c1": {BytesIn: 200, BytesOut: 150},
		}},
	})
	c.CollectForTest(context.Background())

	if got := mst.bytes["alice"]; got != [2]int64{200, 150} {
		t.Errorf("alice delta after 2 ticks: got %v want [200 150] (first=100+50, delta=100+100)", got)
	}
}

// TestCollectorCounterWrap 验证：SA 重建/计数器回绕 → delta 视为 current。
func TestCollectorCounterWrap(t *testing.T) {
	ms := &mockSwanctl{
		saList: []swanctl.SA{
			{UniqueID: "1", IkeState: "ESTABLISHED", RemoteID: "alice", Children: map[string]swanctl.ChildSA{
				"c1": {BytesIn: 1000, BytesOut: 500},
			}},
		},
	}
	mst := &mockStore{bytes: map[string][2]int64{}}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := NewCollector(ms, mst, logger)

	c.CollectForTest(context.Background())
	// alice: [1000, 500]

	// SA 重建：uniqueid 仍为 1 但 child 计数器清零
	ms.SetSAs([]swanctl.SA{
		{UniqueID: "1", IkeState: "ESTABLISHED", RemoteID: "alice", Children: map[string]swanctl.ChildSA{
			"c1": {BytesIn: 50, BytesOut: 20}, // current < prev → 视为重建
		}},
	})
	c.CollectForTest(context.Background())

	// 重设后 delta = current(50,20)，累加上次 = (1000+50, 500+20) = (1050, 520)
	if got := mst.bytes["alice"]; got != [2]int64{1050, 520} {
		t.Errorf("alice after wrap: got %v want [1050 520]", got)
	}
}

// TestCollectorDisconnectClearsPrev 验证：SA 断开后 prevBytes 清掉对应 key（避免内存泄漏）。
func TestCollectorDisconnectClearsPrev(t *testing.T) {
	ms := &mockSwanctl{
		saList: []swanctl.SA{
			{UniqueID: "1", IkeState: "ESTABLISHED", RemoteID: "alice", Children: map[string]swanctl.ChildSA{
				"c1": {BytesIn: 100, BytesOut: 50},
			}},
		},
	}
	mst := &mockStore{bytes: map[string][2]int64{}}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := NewCollector(ms, mst, logger)

	c.CollectForTest(context.Background())
	if len(c.prevBytes) != 1 {
		t.Fatalf("after first sample: prevBytes size = %d, want 1", len(c.prevBytes))
	}

	// SA 断开：返回空列表
	ms.SetSAs([]swanctl.SA{})
	c.CollectForTest(context.Background())

	if len(c.prevBytes) != 0 {
		t.Errorf("after disconnect: prevBytes size = %d, want 0", len(c.prevBytes))
	}
}

// TestCollectorMultiChildSameUser 验证：同一用户多 child 时按用户聚合 delta。
func TestCollectorMultiChildSameUser(t *testing.T) {
	ms := &mockSwanctl{
		saList: []swanctl.SA{
			{UniqueID: "10", IkeState: "ESTABLISHED", RemoteID: "alice", Children: map[string]swanctl.ChildSA{
				"c1": {BytesIn: 100, BytesOut: 50},
				"c2": {BytesIn: 200, BytesOut: 80},
			}},
		},
	}
	mst := &mockStore{bytes: map[string][2]int64{}}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := NewCollector(ms, mst, logger)

	c.CollectForTest(context.Background())

	// first sample：alice 应该是 (100+200, 50+80) = (300, 130)
	if got := mst.bytes["alice"]; got != [2]int64{300, 130} {
		t.Errorf("alice multi-child first sample: got %v want [300 130]", got)
	}

	// 第二次：每个 child 增加 50 / 30
	ms.SetSAs([]swanctl.SA{
		{UniqueID: "10", IkeState: "ESTABLISHED", RemoteID: "alice", Children: map[string]swanctl.ChildSA{
			"c1": {BytesIn: 150, BytesOut: 80},
			"c2": {BytesIn: 250, BytesOut: 110},
		}},
	})
	c.CollectForTest(context.Background())

	// delta: c1(50,30) + c2(50,30) = (100, 60); 总计 = (300+100, 130+60) = (400, 190)
	if got := mst.bytes["alice"]; got != [2]int64{400, 190} {
		t.Errorf("alice multi-child delta: got %v want [400 190]", got)
	}
}

// mockSwanctl implements SwanctlManager + 可动态改 SA 列表。
type mockSwanctl struct {
	mu    sync.Mutex
	saList []swanctl.SA
	calls int
}

func (m *mockSwanctl) ListSAs(_ context.Context) ([]swanctl.SA, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	// 返回副本避免外部修改影响
	out := make([]swanctl.SA, len(m.saList))
	copy(out, m.saList)
	return out, nil
}

func (m *mockSwanctl) SetSAs(sas []swanctl.SA) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.saList = sas
}

// mockStore implements UserStore
type mockStore struct {
	mu    sync.Mutex
	bytes map[string][2]int64
}

func (m *mockStore) IncrementUserBytes(_ context.Context, user string, in, out int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	prev := m.bytes[user]
	m.bytes[user] = [2]int64{prev[0] + in, prev[1] + out}
	return nil
}

// unused（保留兼容旧测试文件可能直接引用）
var _ = time.Hour

// TestCollectorIncludesRekeying 验证:REKEYING 状态 SA 的 child 流量被计入。
//
// v2.85-PR6 (Q9-02):charon 每 22h 触发 rekeying,旧 SA 仍传输流量。
// 修复前 REKEYING 被 skip,流量统计丢 1-2 分钟数据;修复后计入。
func TestCollectorIncludesRekeying(t *testing.T) {
	ms := &mockSwanctl{
		saList: []swanctl.SA{
			{UniqueID: "1", IkeState: "ESTABLISHED", RemoteID: "alice", Children: map[string]swanctl.ChildSA{
				"c1": {BytesIn: 100, BytesOut: 50},
			}},
			{UniqueID: "2", IkeState: "REKEYING", RemoteID: "bob", Children: map[string]swanctl.ChildSA{
				"c1": {BytesIn: 200, BytesOut: 80},
			}},
		},
	}
	mst := &mockStore{bytes: map[string][2]int64{}}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := NewCollector(ms, mst, logger)

	c.CollectForTest(context.Background())

	if got := mst.bytes["alice"]; got != [2]int64{100, 50} {
		t.Errorf("alice ESTABLISHED: got %v, want [100 50]", got)
	}
	if got := mst.bytes["bob"]; got != [2]int64{200, 80} {
		t.Errorf("bob REKEYING (should be counted): got %v, want [200 80]", got)
	}
}

// TestCollectorSkipsRekeyed 验证:REKEYED 状态 SA 被跳过(避免双重计入)。
//
// v2.85-PR6 (Q9-02):REKEYED 表示旧 SA 已被新 SA 取代,流量切到新 SA。
// 旧 SA 这边的 prevBytes 还留着,如果计入会双计。
func TestCollectorSkipsRekeyed(t *testing.T) {
	ms := &mockSwanctl{
		saList: []swanctl.SA{
			{UniqueID: "1", IkeState: "REKEYED", RemoteID: "alice", Children: map[string]swanctl.ChildSA{
				"c1": {BytesIn: 999, BytesOut: 999},
			}},
		},
	}
	mst := &mockStore{bytes: map[string][2]int64{}}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := NewCollector(ms, mst, logger)

	c.CollectForTest(context.Background())

	if _, ok := mst.bytes["alice"]; ok {
		t.Error("REKEYED alice should NOT be counted (流量已切到新 SA,避免双计)")
	}
	if len(c.prevBytes) != 0 {
		t.Errorf("REKEYED SA should not be tracked, got %d entries", len(c.prevBytes))
	}
}

// TestCollectorMixedStates 验证:一次 list-sas 中混合各种状态,只有活跃的计入。
//
// v2.85-PR6 (Q9-02):覆盖混合场景,确保白名单逻辑正确。
func TestCollectorMixedStates(t *testing.T) {
	ms := &mockSwanctl{
		saList: []swanctl.SA{
			{UniqueID: "1", IkeState: "ESTABLISHED", RemoteID: "alice", Children: map[string]swanctl.ChildSA{
				"c1": {BytesIn: 100, BytesOut: 50},
			}},
			{UniqueID: "2", IkeState: "REKEYING", RemoteID: "bob", Children: map[string]swanctl.ChildSA{
				"c1": {BytesIn: 200, BytesOut: 80},
			}},
			{UniqueID: "3", IkeState: "REKEYED", RemoteID: "carol", Children: map[string]swanctl.ChildSA{
				"c1": {BytesIn: 999, BytesOut: 999},
			}},
			{UniqueID: "4", IkeState: "CONNECTING", RemoteID: "dave"},
			{UniqueID: "5", IkeState: "DELETING", RemoteID: "eve", Children: map[string]swanctl.ChildSA{
				"c1": {BytesIn: 9999, BytesOut: 9999},
			}},
		},
	}
	mst := &mockStore{bytes: map[string][2]int64{}}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := NewCollector(ms, mst, logger)

	c.CollectForTest(context.Background())

	// 期望:alice + bob 计入,carol/dave/eve 跳过
	if _, ok := mst.bytes["alice"]; !ok {
		t.Error("alice ESTABLISHED should be counted")
	}
	if _, ok := mst.bytes["bob"]; !ok {
		t.Error("bob REKEYING should be counted")
	}
	for _, skip := range []string{"carol", "dave", "eve"} {
		if _, ok := mst.bytes[skip]; ok {
			t.Errorf("%s should NOT be counted (got %v)", skip, mst.bytes[skip])
		}
	}
}