// v2.86-PR15:VICI event stream 订阅 — IKE SA lifecycle 审计。
//
// 背景:
//   - 审计报告 strongswan MED-5:VICI list-sas 丢 REKEYED/DELETED 事件
//   - v2.85-PR6 collector 已修 REKEYING,但 DELETED 仍丢
//   - 24h rekey 后用户连接 "突然没了",运维看不到日志(只有 charon.log)
//   - 用户希望:能在 /audit 看到 "alice 在 10:10 断开连接" 这种记录
//
// v2.86-PR15 设计(避免过度开发):
//   - 只监听 ike-updown 事件(SIMPLE 事件,不需要解 payload)
//   - SA ESTABLISHED → audit event "sa.established" + 远端 IP
//   - SA DELETED → audit event "sa.deleted" + 远端 IP
//   - 监听到后直接 store.WriteAudit(不绕 web 层)
//   - 启动期间失败重试 + 后台 goroutine,生命周期跟 ctx 绑
//
// v2.86-PR15 砍掉的过度开发:
//   - ❌ 提前 1h expire 提醒(charon 不暴露 IKE_SA_EXPIRED 事件,只有 5 个 streaming:
//       list-sa / list-policy / list-conn / list-cert / ike-updown)
//   - ❌ SSE / WebSocket 推到 UI(Web 推送重写 UI 工作量大,用户感知小)
//   - ❌ child-updown(只审计 IKE SA 生命周期,简化)
//
// 参考:
//   - govici v0.8.1:Subscribe(events) + NotifyEvents(chan Event)
//   - strongSwan VICI docs:5 个 streaming 事件
package swanctl

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/strongswan/govici/vici"

	"github.com/yourname/ikev2-panel-v2/internal/store"
)

// SAEventType SA lifecycle 事件类型。
type SAEventType string

const (
	// SAEstablished IKE SA 建立(用户成功拨号)。
	SAEstablished SAEventType = "sa.established"
	// SADeleted IKE SA 删除(用户断线 / rekey / idle timeout)。
	SADeleted SAEventType = "sa.deleted"
)

// SALifecycleEvent 简化的事件载荷(只取审计需要的字段)。
type SALifecycleEvent struct {
	Type      SAEventType
	UniqueID  uint32 // charon IKE_SA unique-id(同一 SA 重协商前后不变)
	Remote    string // 客户端 IP(v4 或 v6,= remote-host)
	RemoteID  string // EAP identity(= remote-id,如 "rewind")— v2.86-PR12.18 新增,用于反查 user
	RemoteVIP string // 虚拟 IP(= remote-vips,如 "10.10.0.1")— v2.86-PR12.18 新增,用于 collector 反查
}

// SALifecycleHandler 事件回调(handler)。nil = 丢弃。
//
// 设计:把"事件处理"和"事件订阅"解耦,listener 只负责订阅 + 解析,
// handler 由 caller(main.go 注入)负责写 audit log。
//
// 原因:写 audit 需要 store.Store,listener 不依赖 web 包;
//     handler 注入让测试可以注入 mock,不需要真 DB。
type SALifecycleHandler func(SALifecycleEvent)

// LifecycleListener 订阅 charon VICI 事件流。
type LifecycleListener struct {
	viciSock string
	handler  SALifecycleHandler
	logger   *slog.Logger
}

// NewLifecycleListener 构造。
func NewLifecycleListener(viciSock string, handler SALifecycleHandler, logger *slog.Logger) *LifecycleListener {
	if viciSock == "" {
		viciSock = defaultViciSocketPath
	}
	return &LifecycleListener{
		viciSock: viciSock,
		handler:  handler,
		logger:   logger,
	}
}

// Run 在 ctx 生命周期内订阅事件流;ctx cancel 自动退出。
//
// 实现细节:
//   - 用独立 session(跟 parser/terminate 共存)
//   - Subscribe("ike-updown") 后,charon 每次 IKE SA 建立/删除会推一个 event
//   - NotifyEvents 把 Event 推到 channel
//   - 我们从 channel 读 + 解析 + 调 handler
//
// 失败重试:session 断连(比如 charon 重启)→ reconnect,最多 retryInterval 间隔。
func (l *LifecycleListener) Run(ctx context.Context) {
	const retryInterval = 5 * time.Second
	for {
		select {
		case <-ctx.Done():
			l.logger.Info("listener: ctx cancelled, exiting")
			return
		default:
		}

		if err := l.runOnce(ctx); err != nil {
			l.logger.Warn("listener: runOnce failed, will retry",
				"err", err, "retry_in", retryInterval)
			select {
			case <-ctx.Done():
				return
			case <-time.After(retryInterval):
			}
		}
	}
}

// runOnce 单次 session 生命周期(session 断连时返回 error,触发 Run() 重连)。
func (l *LifecycleListener) runOnce(ctx context.Context) error {
	sess, err := vici.NewSession(vici.WithSocketPath(l.viciSock))
	if err != nil {
		return fmt.Errorf("vici NewSession: %w", err)
	}
	defer sess.Close()

	// 订阅 ike-updown(SA 建立 / 删除事件)
	if err := sess.Subscribe("ike-updown"); err != nil {
		return fmt.Errorf("vici Subscribe(ike-updown): %w", err)
	}

	// 接收 channel(buffered 64,短时 burst 不丢)
	events := make(chan vici.Event, 64)
	sess.NotifyEvents(events)
	defer sess.StopEvents(events)

	l.logger.Info("listener: subscribed to ike-updown events")

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ev, ok := <-events:
			if !ok {
				return fmt.Errorf("event channel closed (charon restart?)")
			}
			l.handleEvent(ev)
		}
	}
}

// handleEvent 解析单条 ike-updown 事件。
//
// v2.86-PR12.18 重大修正:strongSwan 6.0+ 的 ike-updown 事件 payload 结构是
//   - 顶层 "up" = "yes"/"no"
//   - 顶层 一个或多个 connection-name(如 "ikev2-rw")键,值是嵌套 *vici.Message
//   - 嵌套 Message 里才是 uniqueid / remote-host / remote-id / remote-vips
//
// 之前 v2.86-PR15 误以为顶层字段就是 unique/remote,实测 payload 全部 nil
// → unique=0 remote="" 写到 audit_log,完全没用。
//
// 实测 payload(2026-09-19 50.63,strongSwan 6.0.1):
//   up = "yes"
//   ikev2-rw = {
//     uniqueid = 1
//     state = ESTABLISHED
//     local-host = 2408:822e:...:b567
//     remote-host = 2408:832e:881:6460:...
//     remote-id = rewind           ← EAP 用户名
//     remote-vips = 10.10.0.1      ← 虚拟 IP
//     ...
//   }
func (l *LifecycleListener) handleEvent(ev vici.Event) {
	if ev.Message == nil {
		return
	}
	msg := ev.Message

	// 1. 解析 up
	var up bool
	if v, ok := msg.Get("up").(string); ok {
		up = v == "yes" || v == "up" || v == "true"
	}

	// 2. 遍历顶层所有 connection-name 字段,找到第一个 *vici.Message 嵌套 SA
	var uniqueID uint32
	var remoteHost, remoteID, remoteVips string
	for _, k := range msg.Keys() {
		if k == "up" {
			continue // "up" 是顶层状态字段,跳过
		}
		raw := msg.Get(k)
		saMsg, ok := raw.(*vici.Message)
		if !ok {
			continue
		}

		// 找到了嵌套 SA Message → 解析字段
		uniqueID = parseUint32Field(saMsg, "uniqueid")
		remoteHost = parseStringField(saMsg, "remote-host")
		remoteID = parseStringField(saMsg, "remote-id")
		remoteVips = parseStringField(saMsg, "remote-vips")
		break // 只取第一个 connection(多 conn 场景罕见)
	}

	evtType := SADeleted
	if up {
		evtType = SAEstablished
	}

	evt := SALifecycleEvent{
		Type:      evtType,
		UniqueID:  uniqueID,
		Remote:    remoteHost, // 兼容老逻辑:用 remote-host 作 Remote
		RemoteID:  remoteID,
		RemoteVIP: remoteVips,
	}
	if l.handler != nil {
		l.handler(evt)
	}
}

// parseStringField 辅助:从 vici.Message 拿 string 字段,缺失返回空。
func parseStringField(m *vici.Message, key string) string {
	if m == nil {
		return ""
	}
	v := m.Get(key)
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprintf("%v", v)
}

// parseUint32Field 辅助:从 vici.Message 拿 uint32 字段(支持 string/int64 两种 govici 返回)。
func parseUint32Field(m *vici.Message, key string) uint32 {
	if m == nil {
		return 0
	}
	v := m.Get(key)
	if v == nil {
		return 0
	}
	switch x := v.(type) {
	case int64:
		return uint32(x)
	case int:
		return uint32(x)
	case uint64:
		return uint32(x)
	case string:
		n, _ := strconv.ParseUint(x, 10, 32)
		return uint32(n)
	default:
		s := fmt.Sprintf("%v", v)
		n, _ := strconv.ParseUint(s, 10, 32)
		return uint32(n)
	}
}

// NewSALifecycleAuditHandler 构造一个把 SA lifecycle 事件写到 audit_log 的 handler。
//
// 设计:由 main.go 注入 store.Store,listener 只负责订阅。
//
// actor = "system"(SA 事件由 charon 推,不是 admin 操作)
// details = "unique=<id> remote=<ip>" 形式
func NewSALifecycleAuditHandler(s *store.Store, logger *slog.Logger) SALifecycleHandler {
	return func(evt SALifecycleEvent) {
		if s == nil {
			return
		}
		details := fmt.Sprintf("unique=%d remote=%s eap_id=%s vip=%s",
			evt.UniqueID, evt.Remote, evt.RemoteID, evt.RemoteVIP)
		if err := s.WriteAudit(context.Background(), store.AuditEvent{
			Timestamp: time.Now().UnixNano(),
			Actor:     "system",
			Event:     string(evt.Type),
			Details:   details,
		}); err != nil {
			if logger != nil {
				logger.Warn("audit: sa lifecycle write failed",
					"event", evt.Type, "err", err)
			}
		}
	}
}