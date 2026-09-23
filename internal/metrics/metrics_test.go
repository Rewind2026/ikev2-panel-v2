package metrics

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

// TestRegistry_BasicGauges 验证 gauge 写入输出。
func TestRegistry_BasicGauges(t *testing.T) {
	r := New()
	r.SetVPNActiveSAs(5)
	r.SetDDNSLastSync("v4", 1700000000)
	r.SetLECertExpiry(0) // 自签

	var buf bytes.Buffer
	if err := r.Emit(&buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	wants := []string{
		"vpn_active_sas 5",
		`ddns_last_sync_unixtime{family="v4"} 1700000000`,
		`ddns_last_sync_unixtime{family="v6"} 0`,
		"le_cert_expiry_unixtime 0",
	}
	for _, w := range wants {
		if !strings.Contains(out, w) {
			t.Errorf("output missing %q\n---\n%s", w, out)
		}
	}
}

// TestRegistry_Counters 验证 counter 累加。
func TestRegistry_Counters(t *testing.T) {
	r := New()
	r.IncLoginAttempts("success")
	r.IncLoginAttempts("success")
	r.IncLoginAttempts("fail")
	r.IncHTTPRequest("GET", "/users", 200)
	r.IncHTTPRequest("GET", "/users", 200)
	r.IncHTTPRequest("POST", "/users", 422)

	var buf bytes.Buffer
	_ = r.Emit(&buf)
	out := buf.String()
	wants := []string{
		`panel_login_attempts_total{result="success"} 2`,
		`panel_login_attempts_total{result="fail"} 1`,
		`http_requests_total{method="GET",path="/users",status="200"} 2`,
		`http_requests_total{method="POST",path="/users",status="422"} 1`,
	}
	for _, w := range wants {
		if !strings.Contains(out, w) {
			t.Errorf("output missing %q\n---\n%s", w, out)
		}
	}
}

// TestRegistry_Histogram 验证 histogram 桶累计正确。
func TestRegistry_Histogram(t *testing.T) {
	r := New()
	r.ObserveHTTPDuration("GET", "/users", 2*time.Millisecond)   // → 0.005 桶
	r.ObserveHTTPDuration("GET", "/users", 30*time.Millisecond)  // → 0.05 桶
	r.ObserveHTTPDuration("GET", "/users", 200*time.Millisecond) // → 0.5 桶
	r.ObserveHTTPDuration("GET", "/users", 2*time.Second)        // → +Inf

	var buf bytes.Buffer
	_ = r.Emit(&buf)
	out := buf.String()
	wants := []string{
		`http_request_duration_seconds_bucket{method="GET",path="/users",le="0.005"} 1`,
		`http_request_duration_seconds_bucket{method="GET",path="/users",le="0.05"} 2`,
		`http_request_duration_seconds_bucket{method="GET",path="/users",le="0.5"} 3`,
		`http_request_duration_seconds_bucket{method="GET",path="/users",le="+Inf"} 4`,
		`http_request_duration_seconds_count{method="GET",path="/users"} 4`,
	}
	for _, w := range wants {
		if !strings.Contains(out, w) {
			t.Errorf("output missing %q\n---\n%s", w, out)
		}
	}
}

// TestRegistry_ConcurrentInc 验证并发安全。
func TestRegistry_ConcurrentInc(t *testing.T) {
	r := New()
	done := make(chan struct{})
	for i := 0; i < 10; i++ {
		go func() {
			for j := 0; j < 100; j++ {
				r.IncHTTPRequest("GET", "/x", 200)
			}
			done <- struct{}{}
		}()
	}
	for i := 0; i < 10; i++ {
		<-done
	}
	var buf bytes.Buffer
	_ = r.Emit(&buf)
	if !strings.Contains(buf.String(), `http_requests_total{method="GET",path="/x",status="200"} 1000`) {
		t.Errorf("concurrent inc: expected 1000, got:\n%s", buf.String())
	}
}
