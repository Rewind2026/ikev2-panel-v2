// v2.85-PR8(U04):Prometheus-style metrics endpoint without prom client lib.
//
// 不用 prometheus/client_golang,避免引入新依赖;直接用 Go 标准 net/http
// 写 Prometheus exposition format 文本。
//
// 暴露的 metric:
//   vpn_active_sas              gauge   当前 ESTABLISHED SA 数
//   ddns_last_sync_unixtime{family} gauge 上次同步 unix 时间,per family
//   le_cert_expiry_unixtime     gauge   LE 证书过期 unix 时间(0 = 自签)
//   panel_login_attempts_total{result} counter 登录尝试 success/fail
//   http_requests_total{method,path,status} counter
//   http_request_duration_seconds{method,path} histogram(简化版:5 桶 + +Inf)
//
// Prometheus text 格式见 https://prometheus.io/docs/instrumenting/exposition_formats/
package metrics

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Registry 集中管理 metric state。
//
// 并发安全:用 sync.RWMutex 保护 map 操作;atomic int64 用于高频 counter。
type Registry struct {
	mu sync.RWMutex

	// http_requests_total{method,path,status} → counter
	httpRequests map[httpReqKey]int64
	// http_request_duration_seconds{method,path} → histogram(简化桶)
	httpDuration map[httpDurKey]*histogram

	// panel_login_attempts_total{result} → counter
	loginAttempts map[string]int64

	// gauges 由 handlers 主动调 Set 写最新值,无需 map
	gaugeVPNActiveSAs atomic.Int64
	gaugeDDNSV4       atomic.Int64
	gaugeDDNSV6       atomic.Int64
	gaugeLEExpiry     atomic.Int64
}

// httpReqKey HTTP request 标签复合 key。
type httpReqKey struct {
	Method string
	Path   string
	Status int
}

// httpDurKey HTTP duration 标签复合 key(method + path,不计 status,符合 prom 默认)。
type httpDurKey struct {
	Method string
	Path   string
}

// histogram 5 桶 + +Inf(Prometheus 标准近似)。
//
// 桶边界(s):0.005, 0.01, 0.05, 0.1, 0.5, +Inf
type histogram struct {
	counts [6]int64 // 5 个 finite 桶 + 1 个 +Inf
	sum    float64  // 总秒数(浮点累积)
}

// New 构造 Registry。
func New() *Registry {
	return &Registry{
		httpRequests:  make(map[httpReqKey]int64),
		httpDuration:  make(map[httpDurKey]*histogram),
		loginAttempts: make(map[string]int64),
	}
}

// IncHTTPRequest 计一次 HTTP 请求,带 method/path/status。
func (r *Registry) IncHTTPRequest(method, path string, status int) {
	r.mu.Lock()
	r.httpRequests[httpReqKey{method, path, status}]++
	r.mu.Unlock()
}

// ObserveHTTPDuration 记录一次请求耗时。
//
// Prometheus histogram bucket 是**累积**的(le=0.05 包括所有 ≤ 0.05 的样本),
// 所以我们要从最大桶倒着加,或者用增量方法后导出时累加。
// 这里采用直接累积写入:每次 observe 把所有 le >= dur 的桶 +1。
func (r *Registry) ObserveHTTPDuration(method, path string, dur time.Duration) {
	key := httpDurKey{method, path}
	r.mu.Lock()
	h, ok := r.httpDuration[key]
	if !ok {
		h = &histogram{}
		r.httpDuration[key] = h
	}
	seconds := dur.Seconds()
	for i, b := range histogramBuckets {
		if seconds <= b {
			h.counts[i]++
		}
	}
	h.counts[5]++ // +Inf always
	h.sum += seconds
	r.mu.Unlock()
}

// IncLoginAttempts 计一次登录尝试,result = "success" | "fail"。
func (r *Registry) IncLoginAttempts(result string) {
	r.mu.Lock()
	r.loginAttempts[result]++
	r.mu.Unlock()
}

// SetVPNActiveSAs 由 healthz/readyz refresh 时调。
func (r *Registry) SetVPNActiveSAs(n int64) { r.gaugeVPNActiveSAs.Store(n) }

// SetDDNSLastSync per-family unix 时间(0 = 从未同步)。
func (r *Registry) SetDDNSLastSync(family string, ts int64) {
	if family == "v4" {
		r.gaugeDDNSV4.Store(ts)
	} else if family == "v6" {
		r.gaugeDDNSV6.Store(ts)
	}
}

// SetLECertExpiry unix 时间(0 = 自签 / 未知)。
func (r *Registry) SetLECertExpiry(ts int64) { r.gaugeLEExpiry.Store(ts) }

// histogramBuckets Prometheus 默认近似桶(秒)。
var histogramBuckets = []float64{0.005, 0.01, 0.05, 0.1, 0.5}

// Emit 写 Prometheus exposition format 到 w。
//
// 格式:
//   # HELP <name> <help>
//   # TYPE <name> <type>
//   <name>{labels} <value>
func (r *Registry) Emit(w io.Writer) error {
	// 1) gauges
	if err := writeGauge(w, "vpn_active_sas", "Number of currently ESTABLISHED IKEv2 SAs.", "",
		float64(r.gaugeVPNActiveSAs.Load())); err != nil {
		return err
	}
	if err := writeGauge(w, "ddns_last_sync_unixtime", "Unix timestamp of last DDNS sync, per family.",
		`family="v4"`, float64(r.gaugeDDNSV4.Load())); err != nil {
		return err
	}
	if err := writeGauge(w, "ddns_last_sync_unixtime", "",
		`family="v6"`, float64(r.gaugeDDNSV6.Load())); err != nil {
		return err
	}
	if err := writeGauge(w, "le_cert_expiry_unixtime", "Unix timestamp when current LE cert expires (0 = self-signed).",
		"", float64(r.gaugeLEExpiry.Load())); err != nil {
		return err
	}

	r.mu.RLock()
	defer r.mu.RUnlock()

	// 2) panel_login_attempts_total
	if _, err := fmt.Fprintln(w, "# HELP panel_login_attempts_total Panel login attempts, labeled by result."); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(w, "# TYPE panel_login_attempts_total counter"); err != nil {
		return err
	}
	results := make([]string, 0, len(r.loginAttempts))
	for k := range r.loginAttempts {
		results = append(results, k)
	}
	sort.Strings(results)
	for _, k := range results {
		if _, err := fmt.Fprintf(w, "panel_login_attempts_total{result=%q} %d\n", k, r.loginAttempts[k]); err != nil {
			return err
		}
	}

	// 3) http_requests_total
	if _, err := fmt.Fprintln(w, "# HELP http_requests_total Total HTTP requests, labeled by method/path/status."); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(w, "# TYPE http_requests_total counter"); err != nil {
		return err
	}
	keys := make([]httpReqKey, 0, len(r.httpRequests))
	for k := range r.httpRequests {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].Method != keys[j].Method {
			return keys[i].Method < keys[j].Method
		}
		if keys[i].Path != keys[j].Path {
			return keys[i].Path < keys[j].Path
		}
		return keys[i].Status < keys[j].Status
	})
	for _, k := range keys {
		if _, err := fmt.Fprintf(w, "http_requests_total{method=%q,path=%q,status=%q} %d\n",
			k.Method, k.Path, statusLabel(k.Status), r.httpRequests[k]); err != nil {
			return err
		}
	}

	// 4) http_request_duration_seconds(histogram)
	if _, err := fmt.Fprintln(w, "# HELP http_request_duration_seconds HTTP request duration in seconds."); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(w, "# TYPE http_request_duration_seconds histogram"); err != nil {
		return err
	}
	durKeys := make([]httpDurKey, 0, len(r.httpDuration))
	for k := range r.httpDuration {
		durKeys = append(durKeys, k)
	}
	sort.Slice(durKeys, func(i, j int) bool {
		if durKeys[i].Method != durKeys[j].Method {
			return durKeys[i].Method < durKeys[j].Method
		}
		return durKeys[i].Path < durKeys[j].Path
	})
	for _, k := range durKeys {
		h := r.httpDuration[k]
		for i, b := range histogramBuckets {
			if _, err := fmt.Fprintf(w, "http_request_duration_seconds_bucket{method=%q,path=%q,le=%q} %d\n",
				k.Method, k.Path, formatFloat(b), h.counts[i]); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprintf(w, "http_request_duration_seconds_bucket{method=%q,path=%q,le=\"+Inf\"} %d\n",
			k.Method, k.Path, h.counts[5]); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "http_request_duration_seconds_sum{method=%q,path=%q} %s\n",
			k.Method, k.Path, formatFloat(h.sum)); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "http_request_duration_seconds_count{method=%q,path=%q} %d\n",
			k.Method, k.Path, h.counts[5]); err != nil {
			return err
		}
	}

	return nil
}

func writeGauge(w io.Writer, name, help, labels string, v float64) error {
	if help != "" {
		if _, err := fmt.Fprintf(w, "# HELP %s %s\n", name, help); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(w, "# TYPE %s gauge\n", name); err != nil {
		return err
	}
	labelStr := ""
	if labels != "" {
		labelStr = "{" + labels + "}"
	}
	_, err := fmt.Fprintf(w, "%s%s %s\n", name, labelStr, formatFloat(v))
	return err
}

// statusLabel 把 200/404/500 转成字符串 label。
func statusLabel(s int) string {
	return fmt.Sprintf("%d", s)
}

// formatFloat 格式化浮点数,prometheus 习惯不带 e 记法。
func formatFloat(f float64) string {
	return strings.TrimRight(strings.TrimRight(
		fmt.Sprintf("%.6f", f), "0"), ".")
}
