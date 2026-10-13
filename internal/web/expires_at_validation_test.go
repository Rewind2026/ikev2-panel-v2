// v2.86-PR19(S4):expires_at 输入校验的回归测试。
//
// 原始风险描述：「expires_at 不校验是否早于当前时间（可创建"一出生就过期"的用户）」。
// 注意**格式校验本来就有**（time.Parse("2006-01-02")），缺的是与当前时间的比较。
//
// 危害链：填一个过去的日期 → expires_at 是正数但已小于当前时间 →
// expiry 扫描器（GetExpiredUsers: expires_at > 0 AND expires_at < now）立刻命中 →
// 刚建号就被停用。用户看到的现象是"建号成功但马上变成停用"，
// 没有任何提示指向真正的输入错误。
//
// 本文件覆盖：
//  1. 过去的日期被拒（含昨天、1年前、非法格式）
//  2. 今天/明天/未来被接受（边界：今天仍有效）
//  3. 空值 = 永不过期（哨兵 0），沿用旧行为
//  4. 存进去的值确实是"当天 00:00"而不是别的时刻
//  5. **时区无关性**：本地时区为 UTC+14 / UTC-12 时都不能误判今天
package web

import (
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// TestExpiresAtPastDateRejected 过去的日期必须被拒。
func TestExpiresAtPastDateRejected(t *testing.T) {
	past := []string{
		"2000-01-01",
		"2020-12-31",
		time.Now().AddDate(0, 0, -1).Format("2006-01-02"),  // 昨天
		time.Now().AddDate(0, 0, -30).Format("2006-01-02"), // 30 天前
		time.Now().AddDate(-1, 0, 0).Format("2006-01-02"),  // 一年前
	}
	for _, in := range past {
		t.Run(in, func(t *testing.T) {
			_, errMsg, ok := parseExpiry(t, in)
			if ok {
				t.Fatalf("past date %q was accepted — 会创建一出生就过期的用户", in)
			}
			if !strings.Contains(errMsg, "不能早于今天") {
				t.Errorf("error message should say 不能早于今天, got %q", errMsg)
			}
		})
	}
}

// TestExpiresAtTodayOrFutureAccepted 今天与未来必须被接受。
//
// 边界"今天"：过期判定是 expires_at < now（严格小于），到期日按当天 00:00 解释，
// 所以今天在 23:59 之前都不过期 → 必须接受。
func TestExpiresAtTodayOrFutureAccepted(t *testing.T) {
	future := []string{
		time.Now().Format("2006-01-02"),                   // 今天
		time.Now().AddDate(0, 0, 1).Format("2006-01-02"),  // 明天
		time.Now().AddDate(0, 1, 0).Format("2006-01-02"),  // 一个月后
		time.Now().AddDate(1, 0, 0).Format("2006-01-02"),  // 一年后
		time.Now().AddDate(10, 0, 0).Format("2006-01-02"), // 十年后
	}
	for _, in := range future {
		t.Run(in, func(t *testing.T) {
			unix, errMsg, ok := parseExpiry(t, in)
			if !ok {
				t.Fatalf("valid date %q rejected: %s", in, errMsg)
			}
			if unix == 0 {
				t.Errorf("valid date %q stored as 0 (0 = 永不过期), would silently disable expiry", in)
			}
		})
	}
}

// TestExpiresAtEmptyMeansNeverExpire 空值 = 永不过期，沿用旧行为。
//
// 这一条是刻意保留的：不填不是输入错误，而是"不设过期"。
func TestExpiresAtEmptyMeansNeverExpire(t *testing.T) {
	unix, errMsg, ok := parseExpiry(t, "")
	if !ok {
		t.Fatalf("empty expires_at should be accepted, got error %q", errMsg)
	}
	if unix != 0 {
		t.Errorf("expiresAtUnix = %d, want 0 (0 = 永不过期的哨兵值)", unix)
	}
}

// TestExpiresAtBadFormatStillRejected 格式校验不能被这次改动弄丢。
func TestExpiresAtBadFormatStillRejected(t *testing.T) {
	bad := []string{"abc", "2026/10/10", "10-10-2026", "20261010", "2026-13-01", "2026-02-30", "今天"}
	for _, in := range bad {
		t.Run(in, func(t *testing.T) {
			_, errMsg, ok := parseExpiry(t, in)
			if ok {
				t.Fatalf("malformed date %q was accepted", in)
			}
			if errMsg == "" {
				t.Errorf("input %q rejected without any message", in)
			}
		})
	}
}

// TestExpiresAtStoredAsDayStart 存进去的值必须是"当天 00:00 UTC"。
func TestExpiresAtStoredAsDayStart(t *testing.T) {
	in := time.Now().AddDate(0, 0, 7).Format("2006-01-02")
	unix, errMsg, ok := parseExpiry(t, in)
	if !ok {
		t.Fatalf("rejected: %s", errMsg)
	}
	want, err := time.Parse("2006-01-02", in)
	if err != nil {
		t.Fatalf("test setup: %v", err)
	}
	if unix != want.Unix() {
		t.Errorf("stored unix = %d, want %d (%s 00:00 UTC)", unix, want.Unix(), in)
	}
}

// TestExpiresAtIsTimezoneIndependent 时区回归（这条是本项修复里真实踩到的坑）。
//
// 比较"过去的日期"时若直接比**绝对时刻**，时区会让判定错乱：
// 上面的 t 是 UTC 午夜，若拿它跟"本地午夜"比 ——
//   - 东八区(UTC+8)：本地午夜 = UTC 前一天 16:00 → 今天会被误判为过去
//   - 西五区(UTC-5)：本地午夜 = UTC 当天 05:00 → 昨天会被误判为今天(即被接受)
//
// 正确做法是两边都构造成 UTC 午夜，按日历日期比。
func TestExpiresAtIsTimezoneIndependent(t *testing.T) {
	zones := []*time.Location{
		time.UTC,
		fixedZone("UTC+14", 14*3600),  // Kiritimati
		fixedZone("UTC+8", 8*3600),    // Asia/Shanghai
		fixedZone("UTC-5", -5*3600),   // America/New_York
		fixedZone("UTC-12", -12*3600), // Baker Island
	}

	for _, tz := range zones {
		t.Run(tz.String(), func(t *testing.T) {
			// 必须用真实的 now 推导今天 ——不能用固定日期,否则测试自己
			// 会因为"那个日期早已过去"而全红,测不到时区逻辑。
			local := time.Now().In(tz)
			todayStr := local.Format("2006-01-02")
			yesterday := local.AddDate(0, 0, -1).Format("2006-01-02")

			// 今天：必须接受
			if _, errMsg, ok := runExpiryWithTZ(t, todayStr, tz); !ok {
				t.Errorf("today %q must be accepted in %s, got error %q",
					todayStr, tz, errMsg)
			}
			// 昨天：必须拒绝
			if _, errMsg, ok := runExpiryWithTZ(t, yesterday, tz); ok {
				t.Errorf("yesterday %q must be rejected in %s (errMsg=%q)", yesterday, tz, errMsg)
			}
		})
	}
}

func fixedZone(name string, offset int) *time.Location {
	return time.FixedZone(name, offset)
}

// runExpiryWithTZ 在指定时区下跑 parseAndValidateUserNewForm。
//
// parseAndValidateUserNewForm 内部直接调 time.Now()（本地时区），无法注入时钟，
// 所以这里用 t.Setenv("TZ", ...) + time.Local 覆盖来模拟不同宿主时区 ——
// 这是能在单测里覆盖时区回归的唯一途径（容器内 TZ 恒为 UTC，跑真实时区需改系统时区）。
func runExpiryWithTZ(t *testing.T, expires string, tz *time.Location) (int64, string, bool) {
	t.Helper()
	prev := time.Local
	time.Local = tz
	defer func() { time.Local = prev }()

	form := url.Values{
		"username":   {"alice"},
		"expires_at": {expires},
	}
	r := httptest.NewRequest("POST", "/users", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	data, ok := parseAndValidateUserNewForm(r)
	return data.expiresAtUnix, data.Error, ok
}
