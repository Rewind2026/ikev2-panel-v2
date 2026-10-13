// v2.86-PR19 (A14) 回归测试：metric label 基数上限。
//
// 背景：path/method/status 组合进 map，若请求方可无限造新 path，
// map 无界增长 = 内存耗尽 DoS。加maxLabelSeries 容量兜底。

package metrics

import (
	"fmt"
	"testing"
)

func TestIncHTTPRequest_BoundsCardinality(t *testing.T) {
	r := New()

	// 远超上限的不同 path
	const attempts = maxLabelSeries + 500
	for i := 0; i < attempts; i++ {
		r.IncHTTPRequest("GET", fmt.Sprintf("/random/%d", i), 200)
	}

	r.mu.RLock()
	n := len(r.httpRequests)
	r.mu.RUnlock()

	if n > maxLabelSeries {
		t.Errorf("label 组合数 = %d，超过上限 %d —— 内存耗尽 DoS", n, maxLabelSeries)
	}
	if n == 0 {
		t.Error("label 组合数不应为 0")
	}
}

func TestIncHTTPRequest_OverflowBucketCounts(t *testing.T) {
	r := New()
	for i := 0; i < maxLabelSeries+10; i++ {
		r.IncHTTPRequest("GET", fmt.Sprintf("/x/%d", i), 200)
	}

	r.mu.RLock()
	var overflow int64
	for k, v := range r.httpRequests {
		if k.Path == overflowPath {
			overflow = v
		}
	}
	r.mu.RUnlock()

	if overflow == 0 {
		t.Error("超限请求应被归并到 {overflow} 桶")
	}
}

// TestIncHTTPRequest_ExistingKeyStillCounts 确认超限不影响已有 key 的计数精度。
func TestIncHTTPRequest_ExistingKeyStillCounts(t *testing.T) {
	r := New()
	r.IncHTTPRequest("GET", "/users/{id}", 200)

	// 灌满上限
	for i := 0; i < maxLabelSeries+10; i++ {
		r.IncHTTPRequest("GET", fmt.Sprintf("/y/%d", i), 200)
	}

	// 原有 key 应继续精确累加
	r.IncHTTPRequest("GET", "/users/{id}", 200)
	r.mu.RLock()
	got := r.httpRequests[httpReqKey{"GET", "/users/{id}", 200}]
	r.mu.RUnlock()

	if got != 2 {
		t.Errorf("/users/{id} 计数 = %d, 期望 2（超限不应影响已有 key）", got)
	}
}

func TestObserveHTTPDuration_BoundsCardinality(t *testing.T) {
	r := New()
	for i := 0; i < maxLabelSeries+200; i++ {
		r.ObserveHTTPDuration("GET", fmt.Sprintf("/d/%d", i), 1000000)
	}

	r.mu.RLock()
	n := len(r.httpDuration)
	r.mu.RUnlock()

	if n > maxLabelSeries {
		t.Errorf("duration label 组合数 = %d，超过上限 %d", n, maxLabelSeries)
	}
}
