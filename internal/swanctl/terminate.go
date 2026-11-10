// SA terminate：通过 VICI list-sas 找 unique id，再 VICI terminate 终止。
// 见 docs/architecture.md §12.3
//
// VICI 协议重要发现（M8 实机测试）：
//   - `terminate-IKE` / `terminate-all` 是 swanctl CLI 风格命名，**不在 VICI 协议层**
//   - VICI 协议只提供 `terminate(ike-id=<uniqueid>)` 命令
//   - `ike-id` 是 IKE_SA 的唯一数字 ID，不是 EAP username
//
// 因此本实现走两步：
//  1. list-sas streaming，找到 remote-id 匹配 username 的 IKE_SA，记下 uniqueid
//  2. terminate(ike-id=<uniqueid>, force=yes) 强制终止
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
//
// 超时：整个流程受 TerminateTimeout 约束（默认 10s），如果 charon 半挂导致
// list-sas 流卡住，会返回 context.DeadlineExceeded 而不永久阻塞。
func (m *Manager) Terminate(ctx context.Context, username string) error {
	if username == "" {
		return fmt.Errorf("username is empty")
	}
	ctx, cancel := withTimeout(ctx, TerminateTimeout)
	defer cancel()

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
		if ctx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("vici list-sas timeout after %s (charon hung?)", TerminateTimeout)
		}
		return fmt.Errorf("vici list-sas for terminate failed: %w", err)
	}
	if len(uniqueIDs) == 0 {
		// 用户没连接，幂等成功
		return nil
	}

	// 第二步：对每个匹配 uniqueid 调 terminate
	var lastErr error
	for _, uid := range uniqueIDs {
		// 超时检查：每个 iterate 前确认还有时间
		if ctx.Err() != nil {
			lastErr = fmt.Errorf("vici terminate aborted: %w", ctx.Err())
			break
		}
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
//
// 超时由 Terminate 入口注入（context 已被 withTimeout 包装），本函数不再二次包超时。
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
		// 超时检查
		if ctx.Err() != nil {
			return uids, ctx.Err()
		}
		// v2.86-PR19(B2):strongSwan 6.x list-sas streaming 顶层 key 是
		// connection-name,val 才是嵌套的 SA Message(含 uniqueid/remote-id)。
		// parser.go 路径已修,本路径漏改 —— 之前 uniqueid 永远为空,
		// Terminate 恒"假成功",跟 parser 注释里说的"漏改存活两个版本"是同一个根因。
		//
		// unwrapSA 也兼容老格式(顶层本身就是 SA 字段时原样返回),无需分支判断。
		inner := unwrapSA(m)
		if inner == nil {
			continue
		}
		uidStr := getString(inner, "uniqueid")
		if uidStr == "" {
			continue
		}
		uid, err := strconv.ParseUint(uidStr, 10, 64)
		if err != nil {
			continue
		}
		// v2.104:匹配优先用 remote-eap-id(= EAP 用户名,全平台一致)。
		// Windows 原生客户端的 remote-id 是机器证书 DN / 计算机名,
		// Linux strongSwan 的 remote-id 是主机名/IP —— 只按 remote-id 匹配时,
		// 对这两个平台的"强制下线"恒找不到 SA,返回幂等成功的假象。
		if saMatchesUsername(getString(inner, "remote-eap-id"), getString(inner, "remote-id"), username) {
			uids = append(uids, uid)
		}
	}
	return uids, nil
}

// saMatchesUsername 判断一个 IKE_SA 是否属于指定 username(v2.104)。
//
// 语义:remote-eap-id 存在时**只**按它匹配 —— 它就是 EAP 登录用户名,
// 全平台一致;remote-id 是 IKE 身份(IDi),Windows/Linux 上是机器身份,
// 拿它兜底反而可能把同机器的其他用户误伤。remote-eap-id 缺失
// (老 charon / 非 EAP)时才回退 remote-id(剥 "N " 前缀与 "CN=",原行为)。
func saMatchesUsername(eapID, remoteID, username string) bool {
	if eapID != "" {
		return matchUsername(eapID, username)
	}
	return matchUsername(remoteID, username)
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
	ctx, cancel := withTimeout(ctx, TerminateTimeout)
	defer cancel()

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
		if ctx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("vici terminate-all timeout after %s (charon hung?)", TerminateTimeout)
		}
		return fmt.Errorf("vici terminate-all failed: %w", err)
	}
	return nil
}
