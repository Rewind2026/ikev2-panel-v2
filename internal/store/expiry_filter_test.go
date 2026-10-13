// v2.86-PR19 (A2) 回归测试：GetExpiredUsers 的 enabled 过滤。
//
// 背景：原SQL 缺 `AND enabled = 1`，已处理过的过期用户每 60s 被重复处理一次，
// 而凭证吊销会触发 swanctl --load-all（中断所有活跃 SA）→ 每天约 1440 次全量 reload。

package store

import (
	"context"
	"testing"
	"time"
)

// 复用 store_test.go 的 newTestStore(t)。

// TestGetExpiredUsers_SkipsDisabled 验证已禁用用户不再被重复处理。
func TestGetExpiredUsers_SkipsDisabled(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	past := time.Now().Add(-24 * time.Hour).Unix()

	// alice 已过期且启用中
	if _, err := st.CreateUser(ctx, &User{
		Username: "alice", Password: "pw1", Enabled: true, ExpiresAt: past,
	}); err != nil {
		t.Fatalf("create alice: %v", err)
	}
	// bob 已过期但已被停用（凭证已吊销，不应再处理）
	uid, err := st.CreateUser(ctx, &User{
		Username: "bob", Password: "pw2", Enabled: false, ExpiresAt: past,
	})
	if err != nil {
		t.Fatalf("create bob: %v", err)
	}
	if uid == 0 {
		t.Fatal("create bob 返回 id=0")
	}
	// carol 未过期
	if _, err := st.CreateUser(ctx, &User{
		Username: "carol", Password: "pw3", Enabled: true,
		ExpiresAt: time.Now().Add(24 * time.Hour).Unix(),
	}); err != nil {
		t.Fatalf("create carol: %v", err)
	}
	// dave 永不过期
	if _, err := st.CreateUser(ctx, &User{
		Username: "dave", Password: "pw4", Enabled: true, ExpiresAt: 0,
	}); err != nil {
		t.Fatalf("create dave: %v", err)
	}

	got, err := st.GetExpiredUsers(ctx, time.Now().Unix())
	if err != nil {
		t.Fatalf("GetExpiredUsers: %v", err)
	}

	if len(got) != 1 {
		var names []string
		for _, u := range got {
			names = append(names, u.Username)
		}
		t.Fatalf("期望仅 1 个待处理过期用户, 实际 %d 个: %v", len(got), names)
	}
	if got[0].Username != "alice" {
		t.Errorf("待处理用户 = %q, 期望 alice", got[0].Username)
	}
}

// TestGetExpiredUsers_DisabledThenReenabled 验证"撤销失败保持 enabled=1"的幂等设计。
func TestGetExpiredUsers_RepeatedScanAfterDisable(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	past := time.Now().Add(-24 * time.Hour).Unix()

	id, err := st.CreateUser(ctx, &User{
		Username: "eve", Password: "pw", Enabled: true, ExpiresAt: past,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// 第一轮扫描：应返回
	got, err := st.GetExpiredUsers(ctx, time.Now().Unix())
	if err != nil || len(got) != 1 {
		t.Fatalf("第一轮应返回 1 个, 实际 %d (err=%v)", len(got), err)
	}

	// 模拟处理成功：置 enabled=false
	if err := st.SetUserEnabled(ctx, id, false); err != nil {
		t.Fatalf("SetUserEnabled: %v", err)
	}

	// 第二轮扫描：应返回 0（不再重复处理 → 不再触发 1440 次 reload）
	got2, err := st.GetExpiredUsers(ctx, time.Now().Unix())
	if err != nil {
		t.Fatalf("第二轮 GetExpiredUsers: %v", err)
	}
	if len(got2) != 0 {
		t.Errorf("停用后不应再被扫描到, 实际返回 %d 个 —— 会导致重复全量 reload", len(got2))
	}
}

// TestGetExpiredUsers_ReenabledExpiredUserIsProcessedAgain
// 验证续期后重新启用、再次过期的用户会重新进入处理流程（撤销失败重试路径）。
func TestGetExpiredUsers_ReenabledExpiredUserIsProcessedAgain(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	past := time.Now().Add(-24 * time.Hour).Unix()

	id, err := st.CreateUser(ctx, &User{
		Username: "frank", Password: "pw", Enabled: true, ExpiresAt: past,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := st.SetUserEnabled(ctx, id, false); err != nil {
		t.Fatalf("disable: %v", err)
	}
	// 重新启用（续期后）
	if err := st.SetUserEnabled(ctx, id, true); err != nil {
		t.Fatalf("re-enable: %v", err)
	}

	got, err := st.GetExpiredUsers(ctx, time.Now().Unix())
	if err != nil {
		t.Fatalf("GetExpiredUsers: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("重新启用后的过期用户应重新进入处理流程, 实际 %d 个", len(got))
	}
}
