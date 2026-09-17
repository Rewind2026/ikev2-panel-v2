// VICI list-sas 协议解析。
// 设计见 docs/design.md §3.5 + architecture §11.3 + §18
//
// 协议：list-sas 是 streaming 命令，每次 SA 变化都会推一个 "list-sa" event。
//   用 CallStreaming(ctx, "list-sas", "list-sa", msg) 拿到 iter.Seq2[*Message, error]，
//   每个元素就是单个 IKE_SA 的结构化 Message。
//
// 协议 Message 结构：
//   ike-sa: {
//       uniqueid:    "1"
//       state:       "ESTABLISHED"
//       local-id:    "1 ikev2.example.com"   // "type id"
//       remote-id:   "2 alice"
//       local-addr:  "2001:db8::75[500]"
//       remote-addr: "2001:db8::63[4500]"
//       child-sas:   "..."  (嵌套 Message: key=child name, val=Message)
//   }
//
// 优势（vs 原 os/exec + 字符串解析）：
//   - 原实现需要解析 key=value 缩进、flush child 等复杂状态机
//   - 现在直接 Get() / 类型断言，零字符串分割
//   - 数字（bytes-in/out）走 vici.Number 类型
package swanctl

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/strongswan/govici/vici"
)

// SA 描述一个 IKE_SA。
type SA struct {
	UniqueID   string
	IkeState   string // "ESTABLISHED", "CONNECTING", ...
	RemoteAddr string // "2001:db8::63[4500]"
	LocalAddr  string
	RemoteID   string // remote identity (eap_id, e.g. "alice")
	LocalID    string
	Children   map[string]ChildSA
}

// ChildSA 子 SA（IPsec SA）。
type ChildSA struct {
	BytesIn  int64
	BytesOut int64
	LocalTS  string
	RemoteTS string
}

// ListSAs 调用 VICI list-sas（streaming）并解析为结构化 SA slice。
//
// 错误情况：
//   - charon.vici 不存在 / dial 失败 → 返回 error
//   - charon 推 0 个 SA → 返回 nil（无活跃 SA，不是错误）
func (m *Manager) ListSAs(ctx context.Context) ([]SA, error) {
	sess, err := m.openSession(ctx)
	if err != nil {
		return nil, err
	}
	if sess == nil {
		return nil, nil // SkipVici 模式：无 vici socket 当作无活跃 SA
	}
	defer sess.Close()

	msg := vici.NewMessage()
	out := make([]SA, 0, 8)
	for saMsg, err := range sess.CallStreaming(ctx, "list-sas", "list-sa", msg) {
		if err != nil {
			return nil, fmt.Errorf("vici list-sas stream: %w", err)
		}
		if saMsg == nil {
			continue
		}
		sa, err := parseSingleSA(saMsg)
		if err != nil {
			return nil, err
		}
		out = append(out, sa)
	}
	return out, nil
}

// parseSingleSA 解析单个 IKE_SA Message。
//
// 子段 child-sas 是嵌套 Message，key=child name，val=Message。
func parseSingleSA(m *vici.Message) (SA, error) {
	sa := SA{
		UniqueID:   getString(m, "uniqueid"),
		IkeState:   getString(m, "state"),
		LocalAddr:  getString(m, "local-addr"),
		RemoteAddr: getString(m, "remote-addr"),
		RemoteID:   extractID(getString(m, "remote-id")),
		LocalID:    extractID(getString(m, "local-id")),
	}

	// 解析 child-sas
	if c := m.Get("child-sas"); c != nil {
		childMsg, ok := c.(*vici.Message)
		if !ok {
			return sa, fmt.Errorf("vici child-sas: unexpected type %T", c)
		}
		sa.Children = parseChildSAs(childMsg)
	}
	return sa, nil
}

// parseChildSAs 解析 child-sas Message。
//
// child-sas 是 *Message，key=child name（如 "ikev2-rw"），val=*Message。
func parseChildSAs(m *vici.Message) map[string]ChildSA {
	out := make(map[string]ChildSA)
	for _, name := range m.Keys() {
		raw := m.Get(name)
		cm, ok := raw.(*vici.Message)
		if !ok {
			continue
		}
		out[name] = ChildSA{
			BytesIn:  getNumber(cm, "bytes-in"),
			BytesOut: getNumber(cm, "bytes-out"),
			LocalTS:  getString(cm, "local-ts"),
			RemoteTS: getString(cm, "remote-ts"),
		}
	}
	return out
}

// getString 安全取字符串字段。
func getString(m *vici.Message, key string) string {
	v := m.Get(key)
	if v == nil {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		return fmt.Sprintf("%v", v)
	}
	return s
}

// getNumber 安全取数字字段（int64）。
//
// VICI 协议层数字序列化：govici v0.8 实现里 Get() 返回 int64 / string。
//   - 大多数情况下是 int64（vici 协议数字字段默认类型）
//   - 极少数路径（如 stream 重放）可能返回 string
func getNumber(m *vici.Message, key string) int64 {
	v := m.Get(key)
	if v == nil {
		return 0
	}
	switch x := v.(type) {
	case int64:
		return x
	case int:
		return int64(x)
	case uint64:
		return int64(x)
	case string:
		n, _ := strconv.ParseInt(strings.TrimSpace(x), 10, 64)
		return n
	default:
		s := fmt.Sprintf("%v", x)
		n, _ := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
		return n
	}
}

// extractID 从 "N id" 形式提取最后的 id（VICI local-id / remote-id 格式）。
//
// 例："2 alice" → "alice"
//      "1 ikev2.example.com" → "ikev2.example.com"
func extractID(v string) string {
	parts := strings.Fields(v)
	if len(parts) >= 2 {
		return parts[1]
	}
	return v
}

// CountActiveIkeSAs 统计 ESTABLISHED 状态的 SA 数。
func CountActiveIkeSAs(sas []SA) int {
	n := 0
	for _, sa := range sas {
		if sa.IkeState == "ESTABLISHED" {
			n++
		}
	}
	return n
}