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
	Type     SAEventType
	UniqueID uint32 // charon IKE_SA unique-id(同一 SA 重协商前后不变)
	Remote   string // 客户端 IP(v4 或 v6)
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
// vici event 格式(参考 strongSwan 6.0+ docs):
//   - Event.Name == "ike-updown"
//   - Event.Message 字段:
//       "up" (建立,value="yes") 或 不存在(默认删除) → 决定 SAEstablished/SADeleted
//       "unique" 字段 → IKE SA unique-id(string 格式,govici marshalField 强转)
//       "remote" 字段 → 客户端 IP(v4 或 v6 string)
//
// govici v0.8.1:Get 返回值类型只能是 string / []string / *Message(见 doc):
//   - bool 在 marshalField 里被转成 "yes"/"no" 字符串
//   - 整数被 strconv.FormatInt 转字符串
//   - 所以这里所有字段都要按 string 解析,再 strconv
//
// 字段不一定全有(charon 老版本字段名可能不同),部分缺失就记 None。
func (l *LifecycleListener) handleEvent(ev vici.Event) {
	if ev.Message == nil {
		return
	}
	msg := ev.Message

	// 解析 up:charon 用 "yes"/"no" 字符串(部分老版本用 "up"/"down")
	var up bool
	if v, ok := msg.Get("up").(string); ok {
		up = v == "yes" || v == "up" || v == "true"
	}

	// 解析 unique:string → uint32
	var uniqueID uint32
	if v, ok := msg.Get("unique").(string); ok {
		if n, err := strconv.ParseUint(v, 10, 32); err == nil {
			uniqueID = uint32(n)
		}
	}

	// remote 直接是 string
	var remote string
	if v, ok := msg.Get("remote").(string); ok {
		remote = v
	}

	evtType := SADeleted
	if up {
		evtType = SAEstablished
	}

	evt := SALifecycleEvent{
		Type:     evtType,
		UniqueID: uniqueID,
		Remote:   remote,
	}
	if l.handler != nil {
		l.handler(evt)
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
		details := fmt.Sprintf("unique=%d remote=%s", evt.UniqueID, evt.Remote)
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