// v2.86-PR15:listener 单元测试。
//
// 测试 handleEvent 的解析逻辑(不依赖真 charon / VICI socket)。
// Run() 和重连测试需要真 VICI,跳过(集成测试在 netns 跑)。
package swanctl

import (
	"io"
	"log/slog"
	"testing"

	"github.com/strongswan/govici/vici"
)

// helper:构造一个 vici.Event 带指定字段
//
// v2.86-PR12.18: 真实 payload 结构是 nested —
//   top-level: { "up": "yes", "ikev2-rw": *vici.Message{uniqueid:..., remote-host:...} }
// 之前 PR15 假设 flat (顶层 unique/remote) 是错的,本 helper 现在构造 nested.
func newEvent(name string, kv map[string]any) vici.Event {
	msg := vici.NewMessage()
	// 顶层 up 字段 (是 string)
	if up, ok := kv["up"].(string); ok {
		_ = msg.Set("up", up)
	}
	// 剩余字段塞进 nested ikev2-rw Message
	nested := vici.NewMessage()
	for k, v := range kv {
		if k == "up" {
			continue
		}
		_ = nested.Set(k, v)
	}
	_ = msg.Set("ikev2-rw", nested)
	return vici.Event{
		Name:    name,
		Message: msg,
	}
}

// TestHandleEvent_UpEvent 测试 SA 建立事件(up="yes")。
func TestHandleEvent_UpEvent(t *testing.T) {
	var got SALifecycleEvent
	listener := NewLifecycleListener("", func(evt SALifecycleEvent) {
		got = evt
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	ev := newEvent("ike-updown", map[string]any{
		"up":          "yes",
		"uniqueid":    "12345",
		"remote-host": "203.0.113.5",
	})
	listener.handleEvent(ev)

	if got.Type != SAEstablished {
		t.Errorf("Type = %q, want %q", got.Type, SAEstablished)
	}
	if got.UniqueID != 12345 {
		t.Errorf("UniqueID = %d, want 12345", got.UniqueID)
	}
	if got.Remote != "203.0.113.5" {
		t.Errorf("Remote = %q, want 203.0.113.5", got.Remote)
	}
}

// TestHandleEvent_DownEvent 测试 SA 删除事件(up 不存在)。
func TestHandleEvent_DownEvent(t *testing.T) {
	var got SALifecycleEvent
	listener := NewLifecycleListener("", func(evt SALifecycleEvent) {
		got = evt
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	ev := newEvent("ike-updown", map[string]any{
		"uniqueid":    "67890",
		"remote-host": "2001:db8::1",
	})
	listener.handleEvent(ev)

	if got.Type != SADeleted {
		t.Errorf("Type = %q, want %q", got.Type, SADeleted)
	}
	if got.UniqueID != 67890 {
		t.Errorf("UniqueID = %d, want 67890", got.UniqueID)
	}
	if got.Remote != "2001:db8::1" {
		t.Errorf("Remote = %q, want 2001:db8::1", got.Remote)
	}
}

// TestHandleEvent_MissingUp 测试缺失 up 字段(默认 SADeleted,保守)。
func TestHandleEvent_MissingUp(t *testing.T) {
	var got SALifecycleEvent
	listener := NewLifecycleListener("", func(evt SALifecycleEvent) {
		got = evt
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	ev := newEvent("ike-updown", map[string]any{
		"uniqueid":    "111",
		"remote-host": "10.0.0.1",
	})
	listener.handleEvent(ev)
	if got.Type != SADeleted {
		t.Errorf("missing up should default to SADeleted, got %q", got.Type)
	}
}

// TestHandleEvent_NilMessage 测试 nil Message(不应 panic)。
func TestHandleEvent_NilMessage(t *testing.T) {
	listener := NewLifecycleListener("", nil, slog.New(slog.NewTextHandler(io.Discard, nil)))

	ev := vici.Event{Name: "ike-updown", Message: nil}
	// 不应 panic
	listener.handleEvent(ev)
}

// TestHandleEvent_UpValuesVariants 测试 up 字段不同写法("yes"/"up"/"true")。
func TestHandleEvent_UpValuesVariants(t *testing.T) {
	cases := []struct {
		name string
		val  string
		want SAEventType
	}{
		{"yes", "yes", SAEstablished},
		{"up", "up", SAEstablished},
		{"true", "true", SAEstablished},
		{"no", "no", SADeleted},
		{"down", "down", SADeleted},
		{"false", "false", SADeleted},
		{"empty", "", SADeleted},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got SALifecycleEvent
			listener := NewLifecycleListener("", func(evt SALifecycleEvent) {
				got = evt
			}, slog.New(slog.NewTextHandler(io.Discard, nil)))
			ev := newEvent("ike-updown", map[string]any{
				"up":     tc.val,
				"unique": "1",
				"remote": "1.2.3.4",
			})
			listener.handleEvent(ev)
			if got.Type != tc.want {
				t.Errorf("up=%q Type = %q, want %q", tc.val, got.Type, tc.want)
			}
		})
	}
}

// TestHandleEvent_InvalidUnique 测试非数字 unique(应该是 0,不 panic)。
func TestHandleEvent_InvalidUnique(t *testing.T) {
	var got SALifecycleEvent
	listener := NewLifecycleListener("", func(evt SALifecycleEvent) {
		got = evt
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ev := newEvent("ike-updown", map[string]any{
		"up":     "yes",
		"unique": "not-a-number",
		"remote": "1.2.3.4",
	})
	listener.handleEvent(ev)
	if got.UniqueID != 0 {
		t.Errorf("invalid unique should default to 0, got %d", got.UniqueID)
	}
}

// TestNewLifecycleListener_EmptySocketPath 验证默认 socket path。
func TestNewLifecycleListener_EmptySocketPath(t *testing.T) {
	l := NewLifecycleListener("", nil, nil)
	if l.viciSock != defaultViciSocketPath {
		t.Errorf("viciSock = %q, want default %q", l.viciSock, defaultViciSocketPath)
	}
}