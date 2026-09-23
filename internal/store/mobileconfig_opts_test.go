// v2.86-PR12.21:每用户 mobileconfig 覆盖项的 store 层测试。
//
// 覆盖:
//   - 新列 mobileconfig_opts 写入 / 读出
//   - UpdateUserMobileConfigOpts 覆盖与清空
//   - 重复 migrate 不会报错(已有 mobileconfig_opts 列时跳过 ALTER)
package store

import (
	"context"
	"path/filepath"
	"testing"
)

func TestUser_MobileConfigOpts_Roundtrip(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := Open(ctx, dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	// 创建用户
	id, err := s.CreateUser(ctx, &User{Username: "alice", Password: "pw"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// 读出,MobileConfigOpts 应为空
	got, err := s.GetUserByID(ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.MobileConfigOpts != "" {
		t.Errorf("new user should have empty MobileConfigOpts, got %q", got.MobileConfigOpts)
	}

	// 写入 opts
	const payload = `{"user_defined_name":"Home","on_demand_profile":"always"}`
	if err := s.UpdateUserMobileConfigOpts(ctx, id, payload); err != nil {
		t.Fatalf("update: %v", err)
	}

	got, err = s.GetUserByID(ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.MobileConfigOpts != payload {
		t.Errorf("opts not persisted: got %q", got.MobileConfigOpts)
	}

	// 清空
	if err := s.UpdateUserMobileConfigOpts(ctx, id, ""); err != nil {
		t.Fatalf("clear: %v", err)
	}
	got, err = s.GetUserByID(ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.MobileConfigOpts != "" {
		t.Errorf("opts not cleared, got %q", got.MobileConfigOpts)
	}
}

func TestUser_MobileConfigOpts_NotFound(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := Open(ctx, dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	err = s.UpdateUserMobileConfigOpts(ctx, 9999, "x")
	if err != ErrNotFound {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

func TestUser_MobileConfigOpts_MigrationIdempotent(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	// 第一次 open 跑 migration
	s1, err := Open(ctx, dir)
	if err != nil {
		t.Fatalf("open1: %v", err)
	}
	s1.Close()

	// 第二次 open 必须不报错(列已存在时跳过 ALTER)
	s2, err := Open(ctx, dir)
	if err != nil {
		t.Fatalf("open2 (re-migrate): %v", err)
	}
	defer s2.Close()

	// 写入读出仍然 OK
	id, err := s2.CreateUser(ctx, &User{Username: "bob", Password: "pw"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := s2.GetUserByID(ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Username != "bob" {
		t.Errorf("got %q want bob", got.Username)
	}
}

// 阻止 unused import 警告
var _ = filepath.Join