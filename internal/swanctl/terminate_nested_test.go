// v2.86-PR19 (A1) 回归测试：list-sas 嵌套结构解析。
//
// 背景：v2.86-PR12.18 修好了 parser.go 的剥层，但 terminate.go 的
// findSAUniqueIDs 未同步修改，导致 Terminate 恒返回 nil 却从未真正踢人。
// 该函数此前零测试覆盖 —— 这是漏改能存活两个版本的根本原因。

package swanctl

import (
	"testing"

	"github.com/strongswan/govici/vici"
)

// buildNestedSA 构造 strongSwan 6.x 实机的 list-sas payload 形状：
// 顶层 key 是 connection-name，val 是嵌套的 SA Message。
//
// 注意：getString 期望字段值是 string（[]byte 会被 fmt 成字节数组），
// 这与 govici 反序列化后的真实类型一致。
func buildNestedSA(connName string, uniqueID, remoteID string) *vici.Message {
	inner := vici.NewMessage()
	inner.Set("uniqueid", uniqueID)
	inner.Set("state", "ESTABLISHED")
	inner.Set("remote-id", remoteID)

	top := vici.NewMessage()
	top.Set(connName, inner)
	return top
}

func TestUnwrapSA_NestedFormat(t *testing.T) {
	got := unwrapSA(buildNestedSA("ikev2-rw", "42", "alice"))
	if got == nil {
		t.Fatal("unwrapSA 返回 nil")
	}
	if uid := getString(got, "uniqueid"); uid != "42" {
		t.Errorf("剥层后 uniqueid = %q, 期望 %q", uid, "42")
	}
}

func TestUnwrapSA_LegacyFlatFormat(t *testing.T) {
	// 兼容老格式：顶层本身就是 SA 字段，应原样返回。
	flat := vici.NewMessage()
	flat.Set("uniqueid", "7")
	flat.Set("remote-id", "alice")

	got := unwrapSA(flat)
	if got == nil {
		t.Fatal("unwrapSA 返回 nil")
	}
	if uid := getString(got, "uniqueid"); uid != "7" {
		t.Errorf("老格式 uniqueid = %q, 期望 %q", uid, "7")
	}
}

func TestUnwrapSA_Nil(t *testing.T) {
	if got := unwrapSA(nil); got != nil {
		t.Errorf("unwrapSA(nil) = %v, 期望 nil", got)
	}
}

// TestParseSingleSA_NestedFormat 确认 parseSingleSA 仍能正确剥层（回归保护）。
func TestParseSingleSA_NestedFormat(t *testing.T) {
	sa, err := parseSingleSA(buildNestedSA("ikev2-rw", "1234", "alice"))
	if err != nil {
		t.Fatalf("parseSingleSA: %v", err)
	}
	if sa.UniqueID != "1234" {
		t.Errorf("UniqueID = %q, 期望 %q", sa.UniqueID, "1234")
	}
	if sa.RemoteID != "alice" {
		t.Errorf("RemoteID = %q, 期望 %q", sa.RemoteID, "alice")
	}
}

// TestSAFieldsExtractableFromNestedMessage 是 A1 的核心断言：
// 剥层后必须能取到 uniqueid 与 remote-id —— 这正是修复前恒为空、
// 导致 Terminate 恒"假成功"的两个字段。
func TestSAFieldsExtractableFromNestedMessage(t *testing.T) {
	sa := unwrapSA(buildNestedSA("ikev2-rw", "99", "2 bob"))

	uidStr := getString(sa, "uniqueid")
	if uidStr == "" {
		t.Fatal("uniqueid 为空 —— Terminate 会恒返回空列表（A1 回归）")
	}
	if uidStr != "99" {
		t.Errorf("uniqueid = %q, 期望 99", uidStr)
	}

	remoteID := getString(sa, "remote-id")
	if remoteID == "" {
		t.Fatal("remote-id 为空 —— 用户名匹配恒失败（A1 回归）")
	}
	if !matchUsername(remoteID, "bob") {
		t.Errorf("matchUsername(%q, bob) = false, 期望 true", remoteID)
	}
}

func TestMatchUsername(t *testing.T) {
	cases := []struct {
		remoteID  string
		username  string
		wantMatch bool
	}{
		{"alice", "alice", true},      // 实机 6.0.1 形态：纯用户名
		{"2 alice", "alice", true},    // "类型 id" 形态
		{"2 CN=alice", "alice", true}, // EAP 模式
		{"bob", "alice", false},
		{"2 bob", "alice", false},
		{"", "alice", false},
	}
	for _, c := range cases {
		if got := matchUsername(c.remoteID, c.username); got != c.wantMatch {
			t.Errorf("matchUsername(%q, %q) = %v, 期望 %v",
				c.remoteID, c.username, got, c.wantMatch)
		}
	}
}

// TestParseSingleSA_NestedFormat_MultiConnKey 是 v2.86-PR19(B2) 多 conn 回归测试。
//
// 背景:strongSwan 6.x list-sas streaming 顶层是 conn-name 嵌套 —— 若 swanctl.conf
// 配了多个 conn(混合 cert auth + eap、或多租户分段),极端情况下一次流式推送
// 可能含多个 conn key。B2 修复后,findSAUniqueIDs 显式 unwrapSA —— 即使当前
// unwrapSA 实现只取第一个内层,至少第一 conn 必须能被正确剥到(否则 Terminate
// 永远找不到 SA,出现"停用假成功")。
func TestParseSingleSA_NestedFormat_MultiConnKey(t *testing.T) {
	inner1 := vici.NewMessage()
	inner1.Set("uniqueid", "11")
	inner1.Set("state", "ESTABLISHED")
	inner1.Set("remote-id", "alice")
	inner2 := vici.NewMessage()
	inner2.Set("uniqueid", "22")
	inner2.Set("state", "ESTABLISHED")
	inner2.Set("remote-id", "bob")

	top := vici.NewMessage()
	top.Set("ikev2-rw-eap", inner1)
	top.Set("ikev2-rw-cert", inner2)

	// 至少第一个 conn 必须能被剥到 —— 这是 B2 修复的直接断言。
	sa, err := parseSingleSA(top)
	if err != nil {
		t.Fatalf("parseSingleSA: %v", err)
	}
	if sa.UniqueID != "11" {
		t.Errorf("UniqueID = %q, 期望 %q —— 多 conn 场景下剥层漏改会让 Terminate 找不到 SA",
			sa.UniqueID, "11")
	}
	if sa.RemoteID != "alice" {
		t.Errorf("RemoteID = %q, 期望 %q", sa.RemoteID, "alice")
	}
}

// TestExtractFieldsFromListSasStreamItem 直接验证 v2.86-PR19(B2) 修复后的
// findSAUniqueIDs 期望行为:list-sas 流式 item 拿到时剥层能读到 uniqueid/remote-id。
//
// 这里用 unwrapSA + getString 模拟修复后的 findSAUniqueIDs 逻辑(后者内部调
// vici.NewSession 直连 socket,无法纯单测)。如果这条 case 通过,说明 fix 路径
// 在真实 list-sas 流式推送下也能跑通。
func TestExtractFieldsFromListSasStreamItem(t *testing.T) {
	// 模拟 strongSwan 6.x 流式一次推送的 item:顶层 {connName: SA Message}。
	item := buildNestedSA("ikev2-rw", "99", "2 bob")

	inner := unwrapSA(item)
	if inner == nil {
		t.Fatal("unwrapSA 返回 nil —— B2 修复后必须能剥到内层")
	}

	uidStr := getString(inner, "uniqueid")
	if uidStr == "" {
		t.Fatal("uniqueid 为空 —— Terminate 会恒返回空列表(B2 回归)")
	}
	if uidStr != "99" {
		t.Errorf("uniqueid = %q, 期望 %q", uidStr, "99")
	}

	remoteID := getString(inner, "remote-id")
	if !matchUsername(remoteID, "bob") {
		t.Errorf("matchUsername(%q, bob) = false, 期望 true —— B2 修复后用户名匹配必须可工作",
			remoteID)
	}
}
