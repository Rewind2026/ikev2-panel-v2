// v2.86-PR19(B13):alidns 客户端 HTTP 加固的回归测试。
//
// 两个被测缺陷:
//  1. 响应体无上限 —— call() 直接 io.ReadAll(resp.Body),异常/恶意响应
//     可以把内存吃光(面板进程 OOM)。
//  2. 请求不绑 ctx —— 以前用 http.Client.PostForm(内部 context.Background()),
//     DDNS 停机 / 面板取消请求都中断不了在途调用,只能干等 Timeout=10s。
//
// 另外顺带验证 Transport 复用:http.Client 每次 NewAliyunClient 新建,
// 但 Transport 为 nil → 走进程级 http.DefaultTransport,连接池共享。
package dns

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// withEndpoint 把 endpoint 指向测试 server,返回清理函数。
func withEndpoint(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	SetAlidnsEndpoint(srv.URL)
	t.Cleanup(func() {
		SetAlidnsEndpoint("https://alidns.aliyuncs.com/")
		srv.Close()
	})
	return srv
}

// TestResponseTooLargeIsRejected 响应体超过上限 → 返回 ResponseTooLarge 错误,
// 而不是把整个 body 读进内存。
func TestResponseTooLargeIsRejected(t *testing.T) {
	withEndpoint(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		// 分块写超过 maxResponseBytes;每块 64KB,总共 ~2MB。
		chunk := strings.Repeat("A", 64*1024)
		for i := 0; i < 33; i++ {
			if _, err := fmt.Fprint(w, chunk); err != nil {
				return
			}
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
	})

	c := NewAliyunClient("id", "secret")
	_, err := c.FindRecord(context.Background(), "example.com", "vpn", RecordTypeA)
	if err == nil {
		t.Fatal("expected error for oversized response, got nil")
	}
	var ae *AliyunError
	if !errors.As(err, &ae) {
		t.Fatalf("expected *AliyunError, got %T: %v", err, err)
	}
	if ae.Code != "ResponseTooLarge" {
		t.Errorf("AliyunError.Code = %q, want ResponseTooLarge", ae.Code)
	}
}

// TestResponseJustUnderLimitIsAccepted 上限之内(且是合法 JSON)必须正常解析,
// 防止限长把正常响应也挡掉。
func TestResponseJustUnderLimitIsAccepted(t *testing.T) {
	withEndpoint(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"RecordId":"rec-1","Value":"203.0.113.7"}`)
	})

	c := NewAliyunClient("id", "secret")
	if err := c.UpdateRecordValue(context.Background(), "rec-1", "vpn", RecordTypeA, "203.0.113.7", 600); err != nil {
		t.Fatalf("normal-size response should be accepted, got %v", err)
	}
}

// TestContextCancelAbortsInFlightRequest ctx 取消后,在途请求必须立刻返回,
// 不能等 HTTPClient.Timeout(10s)。
func TestContextCancelAbortsInFlightRequest(t *testing.T) {
	var hit atomic.Int32
	// release 兜底:Go 的 http.Server 不保证在 handler 阻塞期间立刻感知
	// 客户端断连(没有读 body 时),所以给一个显式释放通道,免得测试
	// 被 srv.Close() 拖到超时。
	release := make(chan struct{})
	withEndpoint(t, func(w http.ResponseWriter, r *http.Request) {
		hit.Add(1)
		// 模拟慢端点:阻塞到客户端断开 / 测试收尾 / 超时。
		select {
		case <-r.Context().Done():
		case <-release:
		case <-time.After(30 * time.Second):
		}
	})
	// t.Cleanup 是 LIFO:这个先跑(释放 handler),再跑 withEndpoint 的 srv.Close()。
	t.Cleanup(func() { close(release) })

	c := NewAliyunClient("id", "secret")
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		_, err := c.FindRecord(ctx, "example.com", "vpn", RecordTypeA)
		done <- err
	}()

	// 等请求真的打到服务端再取消,否则测的是"请求还没发出"。
	deadline := time.Now().Add(2 * time.Second)
	for hit.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if hit.Load() == 0 {
		t.Fatal("request never reached the server")
	}
	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected error after ctx cancel, got nil")
		}
		if !errors.Is(err, context.Canceled) {
			t.Errorf("expected context.Canceled, got %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("FindRecord did not return within 3s after ctx cancel (ctx not honored)")
	}
}

// TestAlreadyCancelledContextFailsFast 已取消的 ctx 不该发出请求。
func TestAlreadyCancelledContextFailsFast(t *testing.T) {
	var hit atomic.Int32
	withEndpoint(t, func(w http.ResponseWriter, _ *http.Request) {
		hit.Add(1)
		fmt.Fprint(w, `{}`)
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	c := NewAliyunClient("id", "secret")
	if _, err := c.FindRecord(ctx, "example.com", "vpn", RecordTypeA); err == nil {
		t.Fatal("expected error with cancelled ctx, got nil")
	}
}

// TestNewClientSharesDefaultTransportTransport 复用:
// http.Client 是每次新建的,但 Transport 必须为 nil(→ http.DefaultTransport 共享),
// 否则每个 tick 都新建连接池,DDNS 每次同步都要重新 TCP + TLS 握手。
func TestNewClientSharesDefaultTransportTransport(t *testing.T) {
	c := NewAliyunClient("id", "secret")
	if c.HTTPClient.Transport != nil {
		t.Errorf("Transport = %v, want nil (share http.DefaultTransport)", c.HTTPClient.Transport)
	}
	if c.HTTPClient.Timeout != 10*time.Second {
		t.Errorf("Timeout = %v, want 10s", c.HTTPClient.Timeout)
	}
}

// TestRequestSetsFormContentType 手写 NewRequestWithContext 后,别忘了
// Content-Type —— 少了阿里云会返回签名错误。
func TestRequestSetsFormContentType(t *testing.T) {
	var gotType, gotMethod string
	var gotSig string
	withEndpoint(t, func(w http.ResponseWriter, r *http.Request) {
		gotType = r.Header.Get("Content-Type")
		gotMethod = r.Method
		_ = r.ParseForm()
		gotSig = r.PostForm.Get("Signature")
		fmt.Fprint(w, `{"RecordId":"rec-1"}`)
	})

	c := NewAliyunClient("id", "secret")
	if _, err := c.AddDomainRecord(context.Background(), "example.com", "vpn", RecordTypeA, "1.2.3.4", 600); err != nil {
		t.Fatalf("AddDomainRecord: %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotType != "application/x-www-form-urlencoded" {
		t.Errorf("Content-Type = %q, want application/x-www-form-urlencoded", gotType)
	}
	if gotSig == "" {
		t.Error("Signature parameter missing")
	}
}
