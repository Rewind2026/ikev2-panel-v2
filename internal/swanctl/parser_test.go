// VICI list-sas 响应 Message 解析测试。
//
// 测试策略：
//   - 手工构造 vici.Message（模拟 charon list-sa event 推送的单个 SA Message）
//   - 测试 parseSingleSA / parseChildSAs / 辅助函数的字段提取
//   - 覆盖：ESTABLISHED SA、CONNECTING SA、多个 child SA、nil 字段
//
// 注意：govici v0.8 的 CallStreaming 返回 iter.Seq2[*Message, error]，是流式迭代器。
//   这里直接测 Message → SA 的转换（不带 list-sa wrapper）。
package swanctl

import (
	"testing"

	"github.com/strongswan/govici/vici"
)

// makeSA 构造一个 IKE_SA Message（含 children）。
//
// children map 的 val 必须是 *vici.Message（govici 协议层要求）。
func makeSA(uniqueid, state, remoteID, remoteAddr string, children map[string]*vici.Message) *vici.Message {
	m := vici.NewMessage()
	m.Set("uniqueid", uniqueid)
	m.Set("state", state)
	m.Set("local-id", "1 ikev2.example.com")
	m.Set("remote-id", "2 "+remoteID)
	m.Set("local-addr", "2001:db8::75[500]")
	m.Set("remote-addr", remoteAddr)

	// child-sas: 嵌套 Message
	if len(children) > 0 {
		csm := vici.NewMessage()
		for name, cm := range children {
			csm.Set(name, cm)
		}
		m.Set("child-sas", csm)
	}
	return m
}

// makeChild 构造一个 child SA Message。
//
// VICI 数字字段用 int64（govici 协议层默认类型）。
func makeChild(bytesIn, bytesOut int64, localTS, remoteTS string) *vici.Message {
	m := vici.NewMessage()
	m.Set("bytes-in", bytesIn)
	m.Set("bytes-out", bytesOut)
	m.Set("local-ts", localTS)
	m.Set("remote-ts", remoteTS)
	return m
}

func TestParseSingleSA_Established(t *testing.T) {
	src := makeSA("1", "ESTABLISHED", "alice", "2001:db8::63[4500]",
		map[string]*vici.Message{
			"ikev2-rw": makeChild(1024, 2048, "0.0.0.0/0, ::/0", "0.0.0.0/0, ::/0"),
		})

	sa, err := parseSingleSA(src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	if sa.UniqueID != "1" {
		t.Errorf("UniqueID: got %q", sa.UniqueID)
	}
	if sa.IkeState != "ESTABLISHED" {
		t.Errorf("IkeState: got %q", sa.IkeState)
	}
	if sa.RemoteID != "alice" {
		t.Errorf("RemoteID: got %q want %q", sa.RemoteID, "alice")
	}
	if sa.RemoteAddr != "2001:db8::63[4500]" {
		t.Errorf("RemoteAddr: got %q", sa.RemoteAddr)
	}
	if sa.LocalID != "ikev2.example.com" {
		t.Errorf("LocalID: got %q", sa.LocalID)
	}

	cs, ok := sa.Children["ikev2-rw"]
	if !ok {
		t.Fatal("missing child ikev2-rw")
	}
	if cs.BytesIn != 1024 {
		t.Errorf("BytesIn: got %d", cs.BytesIn)
	}
	if cs.BytesOut != 2048 {
		t.Errorf("BytesOut: got %d", cs.BytesOut)
	}
}

func TestParseSingleSA_Connecting(t *testing.T) {
	src := makeSA("2", "CONNECTING", "bob", "2001:db8::64[4500]", nil)

	sa, err := parseSingleSA(src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if sa.IkeState != "CONNECTING" {
		t.Errorf("IkeState: got %q", sa.IkeState)
	}
	if sa.RemoteID != "bob" {
		t.Errorf("RemoteID: got %q", sa.RemoteID)
	}
	if len(sa.Children) != 0 {
		t.Errorf("Children: expected empty, got %d", len(sa.Children))
	}
}

func TestParseSingleSA_NoChildren(t *testing.T) {
	src := makeSA("3", "ESTABLISHED", "carol", "2001:db8::65[4500]", nil)

	sa, err := parseSingleSA(src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if sa.Children != nil {
		// 没 child-sas 字段时，应为 nil（parseSingleSA 不主动初始化）
		t.Errorf("Children: expected nil, got %v", sa.Children)
	}
}

func TestParseSingleSA_MultipleChildren(t *testing.T) {
	src := makeSA("1", "ESTABLISHED", "alice", "2001:db8::63[4500]",
		map[string]*vici.Message{
			"ikev2-rw-a": makeChild(100, 200, "0.0.0.0/0", "0.0.0.0/0"),
			"ikev2-rw-b": makeChild(300, 400, "::/0", "::/0"),
		})

	sa, err := parseSingleSA(src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(sa.Children) != 2 {
		t.Fatalf("expected 2 children, got %d", len(sa.Children))
	}
	if sa.Children["ikev2-rw-a"].BytesIn != 100 {
		t.Errorf("child a bytes-in: got %d", sa.Children["ikev2-rw-a"].BytesIn)
	}
	if sa.Children["ikev2-rw-b"].BytesOut != 400 {
		t.Errorf("child b bytes-out: got %d", sa.Children["ikev2-rw-b"].BytesOut)
	}
}

func TestCountActiveIkeSAs(t *testing.T) {
	sas := []SA{
		{UniqueID: "1", IkeState: "ESTABLISHED"},
		{UniqueID: "2", IkeState: "CONNECTING"},
		{UniqueID: "3", IkeState: "ESTABLISHED"},
		{UniqueID: "4", IkeState: "REKEYING"},
	}
	if got := CountActiveIkeSAs(sas); got != 2 {
		t.Errorf("CountActiveIkeSAs: got %d want 2", got)
	}
}

func TestExtractID(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"2 alice", "alice"},
		{"1 ikev2.example.com", "ikev2.example.com"},
		{"alice", "alice"},
		{"", ""},
		{"3 alice@example.com", "alice@example.com"},
	}
	for _, tc := range tests {
		got := extractID(tc.in)
		if got != tc.want {
			t.Errorf("extractID(%q): got %q want %q", tc.in, got, tc.want)
		}
	}
}

func TestGetNumber(t *testing.T) {
	// int64 类型（VICI 协议默认）
	m := vici.NewMessage()
	m.Set("a", int64(42))
	if got := getNumber(m, "a"); got != 42 {
		t.Errorf("getNumber int64: got %d want 42", got)
	}

	// 字符串类型（兼容其他编码路径）
	m2 := vici.NewMessage()
	m2.Set("a", "100")
	if got := getNumber(m2, "a"); got != 100 {
		t.Errorf("getNumber string: got %d want 100", got)
	}

	// 不存在的 key
	if got := getNumber(m, "missing"); got != 0 {
		t.Errorf("getNumber missing: got %d want 0", got)
	}
}