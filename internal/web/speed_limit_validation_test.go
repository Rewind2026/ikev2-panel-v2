// v2.86-PR19(S4):speed_limit_mbps 输入校验的回归测试。
//
// 原代码:
//
//	if speedStr := r.PostFormValue("speed_limit_mbps"); speedStr != "" {
//	    if v, err := strconv.Atoi(speedStr); err == nil {
//	        data.SpeedLimit = v
//	    }
//	}
//
// strconv.Atoi 的 err 被丢弃 → 用户填 "abc" / "20mb" / "1e3" / "-" 时,
// 值被**静默**丢弃,SpeedLimit 保持 defaultSpeedMbps,页面照样渲染"保存成功"。
// 用户以为自己设了 20 Mbps,实际跑的是默认值 —— 排查时完全想不到是输入被吞。
//
// 修复后:非整数一律显式报错,不再静默回退。
package web

import (
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// parseSpeed 走真实的 parseAndValidateUserNewForm,只取限速这一项的判定结果。
func parseSpeed(t *testing.T, speed string) (int, string, bool) {
	t.Helper()
	form := url.Values{
		"username":         {"alice"},
		"note":             {""},
		"speed_limit_mbps": {speed},
	}
	r := httptest.NewRequest("POST", "/users", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	data, ok := parseAndValidateUserNewForm(r)
	return data.SpeedLimit, data.Error, ok
}

// parseExpiry 走真实的 parseAndValidateUserNewForm,只取过期日期这一项的结果。
func parseExpiry(t *testing.T, expires string) (int64, string, bool) {
	t.Helper()
	form := url.Values{
		"username":   {"alice"},
		"note":       {""},
		"expires_at": {expires},
	}
	r := httptest.NewRequest("POST", "/users", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	data, ok := parseAndValidateUserNewForm(r)
	return data.expiresAtUnix, data.Error, ok
}

// TestSpeedLimitNonNumericIsRejected 非数字输入必须被拒(而不是静默取默认值)。
func TestSpeedLimitNonNumericIsRejected(t *testing.T) {
	bad := []string{"abc", "20mb", "1e3", "-", "10.5", "0x10", "20 30", "Mbps", "１0"}
	for _, in := range bad {
		t.Run(in, func(t *testing.T) {
			speed, errMsg, ok := parseSpeed(t, in)
			if ok {
				t.Fatalf("input %q was accepted (speed=%d) — 静默回退已回来", in, speed)
			}
			if errMsg == "" {
				t.Errorf("input %q rejected without any error message", in)
			}
			if !strings.Contains(errMsg, "限速") {
				t.Errorf("error message should mention 限速, got %q", errMsg)
			}
		})
	}
}

// TestSpeedLimitEmptyKeepsDefault 空值 = 不限速语义之外的"未填",沿用默认值。
//
// 这一条是**刻意保留**的旧行为:空字段不是"输入错误",而是"没填" → 用默认值。
func TestSpeedLimitEmptyKeepsDefault(t *testing.T) {
	speed, errMsg, ok := parseSpeed(t, "")
	if !ok {
		t.Fatalf("empty speed_limit_mbps should be accepted, got error %q", errMsg)
	}
	if speed != defaultSpeedMbps {
		t.Errorf("speed = %d, want defaultSpeedMbps %d", speed, defaultSpeedMbps)
	}
}

// TestSpeedLimitValidValues 正常值(0 / 中间值 / 上限)必须照常通过。
func TestSpeedLimitValidValues(t *testing.T) {
	for _, v := range []struct {
		in   string
		want int
	}{
		{"0", 0}, // 0 = 不限速
		{"1", 1},
		{"20", 20},
		{" 30 ", 30}, // 前后空白容忍
	} {
		t.Run(v.in, func(t *testing.T) {
			speed, errMsg, ok := parseSpeed(t, v.in)
			if !ok {
				t.Fatalf("input %q rejected: %s", v.in, errMsg)
			}
			if speed != v.want {
				t.Errorf("speed = %d, want %d", speed, v.want)
			}
		})
	}
}

// TestSpeedLimitOutOfRangeStillRejected 越界值仍然被范围校验拦住。
func TestSpeedLimitOutOfRangeStillRejected(t *testing.T) {
	for _, in := range []string{"-1", "100000"} {
		t.Run(in, func(t *testing.T) {
			_, errMsg, ok := parseSpeed(t, in)
			if ok {
				t.Fatalf("out-of-range input %q was accepted", in)
			}
			if !strings.Contains(errMsg, "限速") {
				t.Errorf("error message should mention 限速, got %q", errMsg)
			}
		})
	}
}
