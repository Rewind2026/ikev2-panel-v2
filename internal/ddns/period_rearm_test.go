// v2.86-PR19(B8):同步周期调度 + 面板热改生效的回归测试。
//
// 两个被测缺陷:
//  1. B8 本体 —— 老实现用 time.Ticker,周期在 NewTicker 时固定死,面板改
//     Period 只改内存,循环仍按老周期 fire,必须重启容器才生效。
//  2. 修复过程中引入的 Timer 漏 Reset —— time.Timer 是一次性的,fire 之后
//     不 Reset 就永远不再响(表现为"只同步两轮然后永远不动")。
//     TestTriggerDoesNotStarveScheduledTicks 与 TestPeriodShrink... 会抓到它。
//
// 观测手段:tick() 在凭证齐 + Domain 非空 + EnableA 时会调 detectV4,
// 把 detectV4 注入成计数器即可精确数出"跑了几轮 tick";detect 返回空 IP
// → tick 走"探测失败"分支,不调阿里云 API,不打网络。
package ddns

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// probeSync 造一个"每 tick 必调 detectV4、绝不联网"的同步器。
func probeSync(t *testing.T, period time.Duration) (*Sync, *atomic.Int32) {
	t.Helper()
	var ticks atomic.Int32
	s := NewSync(Config{
		Enabled:               true,
		Family:                "dual",
		EnableA:               true,
		EnableAAAA:            false, // 只跑 v4 分支,便于计数
		Domain:                "example.com",
		AliyunAccessKeyID:     "fake-id",
		AliyunAccessKeySecret: "fake-secret",
		Period:                period,
		Throttle:              time.Millisecond,
		StateFile:             filepath.Join(t.TempDir(), "state"),
		Logger:                slog.New(slog.NewTextHandler(os.Stderr, nil)),
	})
	// 返回空 IP + error → tick 走"探测失败"分支,不调 alidns API。
	s.SetDetectV4(func(iface, target string) (string, error) {
		ticks.Add(1)
		return "", errors.New("probe disabled in test")
	})
	s.SetDetectV6(func(iface string) (string, error) {
		return "", errors.New("probe disabled in test")
	})
	return s, &ticks
}

// setPeriod 模拟"面板改了周期"。
//
// 走 SetConfig 的话 periodSeconds 单位是**秒**且下限 clamp 到 10s,做不了
// 亚秒级的调度断言(一个用例要十几秒)。这里直接改内存,等价于面板改完
// SetConfig 已经生效的那一帧。SetConfig 层的秒→duration 换算与 clamp 由
// TestSync_SetConfig_PeriodClamp / TestSync_SetConfig_PeriodZero 覆盖。
func setPeriod(s *Sync, d time.Duration) {
	s.mu.Lock()
	s.cfg.Period = d
	s.mu.Unlock()
}

// waitTicks 等到计数达到 target;超时返回 false。
func waitTicks(t *testing.T, ticks *atomic.Int32, target int32, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ticks.Load() >= target {
			return true
		}
		time.Sleep(2 * time.Millisecond)
	}
	return false
}

// runSync 起 Run 并返回清理函数。
func runSync(t *testing.T, s *Sync) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	return func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Run returned %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Error("Run did not exit after ctx cancel")
		}
	}
}

// TestRunKeepsTickingPeriodically 基线:不碰任何配置,Run 也必须一直按周期跑。
// 这条专门盯"Timer 漏 Reset → 只跑两轮就停"。
func TestRunKeepsTickingPeriodically(t *testing.T) {
	s, ticks := probeSync(t, 20*time.Millisecond)
	stop := runSync(t, s)
	defer stop()

	// 400ms / 20ms = 20 轮,留一半余量
	if !waitTicks(t, ticks, 8, 2*time.Second) {
		t.Fatalf("Run stopped ticking: only %d ticks, want >= 8", ticks.Load())
	}
}

// TestPeriodShrinkTakesEffectWithoutRestart 300ms → 20ms:改完后 3 轮应在
// 500ms 内跑完。老 ticker 实现要 3×300ms = 900ms。
func TestPeriodShrinkTakesEffectWithoutRestart(t *testing.T) {
	s, ticks := probeSync(t, 300*time.Millisecond)
	stop := runSync(t, s)
	defer stop()

	if !waitTicks(t, ticks, 1, 2*time.Second) {
		t.Fatal("first tick never happened")
	}
	before := ticks.Load()

	setPeriod(s, 20*time.Millisecond)

	// 3 轮 × 20ms = 60ms;老实现需要 3 × 300ms = 900ms
	if !waitTicks(t, ticks, before+3, 500*time.Millisecond) {
		t.Fatalf("shrinking period did not take effect: got %d extra ticks in 500ms, want >= 3",
			ticks.Load()-before)
	}
}

// TestPeriodGrowTakesEffectWithoutRestart 20ms → 400ms:改完后 400ms 内最多
// 只允许再跑 1 轮(下一次 arm 生效前的最后一轮老周期),之后必须安静。
// 老 ticker 实现会在这 400ms 里跑 ~20 轮。
func TestPeriodGrowTakesEffectWithoutRestart(t *testing.T) {
	s, ticks := probeSync(t, 20*time.Millisecond)
	stop := runSync(t, s)
	defer stop()

	if !waitTicks(t, ticks, 1, 2*time.Second) {
		t.Fatal("first tick never happened")
	}
	setPeriod(s, 400*time.Millisecond)

	// 等"改周期前已 arm 的那一轮"跑完(≤20ms),然后记录基线
	time.Sleep(80 * time.Millisecond)
	base := ticks.Load()

	// 接下来 200ms(新周期 400ms 的一半)必须不再有 tick
	time.Sleep(200 * time.Millisecond)
	if extra := ticks.Load() - base; extra > 0 {
		t.Errorf("period did not grow: %d extra ticks in 200ms with new period 400ms", extra)
	}
}

// TestTriggerDoesNotStarveScheduledTicks 手动同步不能推迟计划同步。
//
// 修 B8 时的实现选择:triggerCh 分支不 Reset timer。若误改成"每轮都 Reset",
// 用户狂点"手动同步"就会把计划同步一直往后推,DDNS 永远不同步。
func TestTriggerDoesNotStarveScheduledTicks(t *testing.T) {
	const period = 100 * time.Millisecond
	s, ticks := probeSync(t, period)
	stop := runSync(t, s)
	defer stop()

	if !waitTicks(t, ticks, 1, 2*time.Second) {
		t.Fatal("first tick never happened")
	}

	// 手动触发风暴:每 10ms 点一次,持续 200ms(= 两个完整周期)。
	for i := 0; i < 20; i++ {
		s.TriggerNow()
		time.Sleep(10 * time.Millisecond)
	}
	stormEnd := ticks.Load()

	// 风暴期间手动 + 计划的 tick 混在一起,计数不可直接判读;关键断言是
	// 风暴结束后计划 tick 仍在按原节奏到达(没被触发无限推迟)。
	// 给 3 个周期的宽限。
	if !waitTicks(t, ticks, stormEnd+2, 3*period) {
		t.Fatalf("scheduled tick starved by manual triggers: %d ticks after storm, want >= %d within %v",
			ticks.Load(), stormEnd+2, 3*period)
	}
}

// TestZeroPeriodDoesNotBusyLoop 若 Period 被弄成非正值(理论上 SetConfig 已
// clamp,这里是防御性回归),timer 不能变成 0 → Reset(0) 立即响 → 热循环打爆 CPU。
//
// 期望行为:沿用上一个**有效**周期(这里 50ms)继续跑,而不是停摆也不是打满。
func TestZeroPeriodDoesNotBusyLoop(t *testing.T) {
	s, ticks := probeSync(t, 50*time.Millisecond)
	stop := runSync(t, s)
	defer stop()

	if !waitTicks(t, ticks, 1, 2*time.Second) {
		t.Fatal("first tick never happened")
	}

	setPeriod(s, 0)

	time.Sleep(80 * time.Millisecond) // 等最后一轮按 50ms 跑完
	base := ticks.Load()
	time.Sleep(200 * time.Millisecond)
	// 沿用 50ms 周期 → 200ms 里约 4 轮。热循环(Reset(0))会是几百轮。
	if extra := ticks.Load() - base; extra > 8 {
		t.Errorf("zero period caused a busy loop: %d ticks in 200ms (expected ~4 at the retained 50ms period)", extra)
	}
}
