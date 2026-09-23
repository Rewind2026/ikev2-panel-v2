package panelstate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMaskKeyID 掩码展示。
func TestMaskKeyID(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", "***"},
		{"short", "***"},
		{"12345678", "***"},
		{"LTAI5tabcdefghijklmnopqrstuvwxyz1234", "LTAI...1234"},
		{"LTAI5txxxxxxxxxxxxxxxxxxxxxxxxxxxxxxab12", "LTAI...ab12"},
	}
	for _, c := range cases {
		got := MaskKeyID(c.in)
		if got != c.want {
			t.Errorf("MaskKeyID(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestAliyunCreds_JSONMarshal JSON tag 是 snake_case(shell 工具能 jq 读)。
func TestAliyunCreds_JSONMarshal(t *testing.T) {
	c := AliyunCreds{KeyID: "LTAI5t", KeySecret: "secret123"}
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// 不应包含原始大写字段名
	if !strings.Contains(string(b), `"key_id":"LTAI5t"`) {
		t.Errorf("expected snake_case key_id, got: %s", string(b))
	}
	if !strings.Contains(string(b), `"key_secret":"secret123"`) {
		t.Errorf("expected snake_case key_secret, got: %s", string(b))
	}
}

// TestWriteAliyun_Empty 拒绝空 KeyID/Secret。
func TestWriteAliyun_Empty(t *testing.T) {
	s := &Store{}
	err := s.WriteAliyun(AliyunCreds{})
	if err == nil {
		t.Error("expected error for empty creds, got nil")
	}
	if !strings.Contains(err.Error(), "key_id") {
		t.Errorf("error should mention key_id, got: %v", err)
	}

	err = s.WriteAliyun(AliyunCreds{KeyID: "LTAI5t"})
	if err == nil {
		t.Error("expected error for empty secret, got nil")
	}
	if !strings.Contains(err.Error(), "key_secret") {
		t.Errorf("error should mention key_secret, got: %v", err)
	}
}

// TestAliyunPath_Constant 路径符合预期。
func TestAliyunPath_Constant(t *testing.T) {
	p := aliyunPath()
	if filepath.Base(p) != "aliyun.creds" {
		t.Errorf("expected filename aliyun.creds, got %s", filepath.Base(p))
	}
	if !strings.HasPrefix(p, Dir) {
		t.Errorf("path should start with %s, got %s", Dir, p)
	}
}

// TestClearAliyun_Missing 不存在文件 → 不报错。
func TestClearAliyun_Missing(t *testing.T) {
	// /data/panel-state/aliyun.creds 在测试环境通常不存在
	// 如果存在(测试污染),ClearAliyun 会真删 → 跳过
	if _, err := os.Stat(aliyunPath()); err == nil {
		t.Skip("aliyun.creds exists in test env, skip destructive test")
	}
	s := &Store{}
	if err := s.ClearAliyun(); err != nil {
		t.Errorf("ClearAliyun on missing file should be nil, got: %v", err)
	}
}

// TestAliyunExists_Missing 不存在 → false。
func TestAliyunExists_Missing(t *testing.T) {
	if _, err := os.Stat(aliyunPath()); err == nil {
		t.Skip("aliyun.creds exists in test env")
	}
	s := &Store{}
	if s.AliyunExists() {
		t.Error("AliyunExists should return false when file missing")
	}
}
