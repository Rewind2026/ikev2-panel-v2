// SA terminate：通过 VICI list-sas 找 unique id，再 VICI terminate 终止。
// 设计见 docs/design.md §3.7
//
// VICI 协议重要发现（M8 实机测试）：
//   - `terminate-IKE` / `terminate-all` 是 swanctl CLI 风格命名，**不在 VICI 协议层**
//   - VICI 协议只提供 `terminate(ike-id=<uniqueid>)` 命令
//   - `ike-id` 是 IKE_SA 的唯一数字 ID，不是 EAP username
//
// 因此本实现走两步：
//   1. list-sas streaming，找到 remote-id 匹配 username 的 IKE_SA，记下 uniqueid
//   2. terminate(ike-id=<uniqueid>, force=yes) 强制终止
//
// 失败不致命：用户可能本来就没连接。
package swanctl

import (
	"context"
	"fmt"
	"strconv"

	"github.com/strongswan/govici/vici"
)

// Terminate 强制下线某用户（按 EAP username 匹配 IKE_SA）。
//
// 流程：
//  1. CallStreaming list-sas 拿所有 SA
//  2. 找到 remote-id 匹配 username（带或不带 "CN=" 前缀）的 SA，记 uniqueid
//  3. Call terminate(ike-id=<uniqueid>, force=yes) 终止
func (m *Manager) Terminate(ctx context.Context, username string) error {
	if username == "" {
		return fmt.Errorf("username is empty")
	}
	sess, err := m.openSession(ctx)
	if err != nil {
		return err
	}
	if sess == nil {
		return nil // SkipVici 模式
	}
	defer sess.Close()

	// 第一步：list-sas streaming，找到匹配 uniqueid
	uniqueIDs, err := m.findSAUniqueIDs(ctx, sess, username)
	if err != nil {
		return fmt.Errorf("vici list-sas for terminate failed: %w", err)
	}
	if len(uniqueIDs) == 0 {
		// 用户没连接，幂等成功
		return nil
	}

	// 第二步：对每个匹配 uniqueid 调 terminate
	var lastErr error
	for _, uid := range uniqueIDs {
		termMsg := vici.NewMessage()
		termMsg.Set("ike-id", strconv.FormatUint(uid, 10))
		termMsg.Set("force", "yes")
		termMsg.Set("timeout", "5000")
		if _, err := sess.Call(ctx, "terminate", termMsg); err != nil {
			lastErr = fmt.Errorf("vici terminate ike-id=%d failed: %w", uid, err)
			// 不立即返回：尽量终止所有匹配 SA
			continue
		}
	}
	if lastErr != nil {
		return lastErr
	}
	return nil
}

// findSAUniqueIDs 调 list-sas streaming，找到所有 remote-id 匹配 username 的 IKE_SA uniqueid。
//
// VICI 协议：list-sas 是 streaming 命令，注册 list-sa event 后多次 yield SA Message，
// 收到 pktCmdResponse 表示流结束。
func (m *Manager) findSAUniqueIDs(ctx context.Context, sess *vici.Session, username string) ([]uint64, error) {
	// openSession 已加锁，list-sas 期间需要持锁
	// 这里通过独立 session 调用避免锁死（govici session 是互斥的）
	sess2, err := vici.NewSession(vici.WithSocketPath(m.viciSock))
	if err != nil {
		return nil, err
	}
	defer sess2.Close()

	var uids []uint64
	listMsg := vici.NewMessage()
	for m, err := range sess2.CallStreaming(ctx, "list-sas", "list-sa", listMsg) {
		if err != nil {
			// 流式错误通常意味着流结束或协议错误
			break
		}
		uidStr := getString(m, "uniqueid")
		if uidStr == "" {
			continue
		}
		uid, err := strconv.ParseUint(uidStr, 10, 64)
		if err != nil {
			continue
		}
		// remote-id 形如 "2 alice" 或 "2 CN=alice"（charon 6.x 是 "type id"）
		remoteID := getString(m, "remote-id")
		if matchUsername(remoteID, username) {
			uids = append(uids, uid)
		}
	}
	return uids, nil
}

// matchUsername 检查 VICI 远程标识是否对应指定 username。
//
// VICI 协议里 remote-id 形如：
//   - "2 alice"          (类型=2 表示 FQDN, ID=alice)
//   - "2 CN=alice"       (charon 6.x EAP 模式)
//
// 我们需要剥掉 "类型 " 前缀和可选 "CN=" 前缀后比较。
func matchUsername(remoteID, username string) bool {
	// 剥掉类型前缀（如 "2 alice" → "alice"）
	idx := -1
	for i, c := range remoteID {
		if c == ' ' {
			idx = i
			break
		}
	}
	if idx >= 0 {
		remoteID = remoteID[idx+1:]
	}
	// 剥掉 "CN=" 前缀
	if len(remoteID) > 3 && remoteID[:3] == "CN=" {
		remoteID = remoteID[3:]
	}
	return remoteID == username
}

// TerminateAll 强制下线所有 SA（管理后台"踢掉所有人"按钮，v2 暂不暴露）。
func (m *Manager) TerminateAll(ctx context.Context) error {
	sess, err := m.openSession(ctx)
	if err != nil {
		return err
	}
	if sess == nil {
		return nil
	}
	defer sess.Close()

	msg := vici.NewMessage()
	msg.Set("timeout", "5000")
	if _, err := sess.Call(ctx, "terminate", msg); err != nil {
		return fmt.Errorf("vici terminate-all failed: %w", err)
	}
	return nil
}