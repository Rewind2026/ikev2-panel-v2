package ddns

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestVerify_DDNSCfgRaceFixed 是 v2.86-pr23x data race 修复的回归测试。
//
// 修复前:tick()/Run() 无锁读 s.cfg,与 handler 侧 SetConfig 锁内写并发,
// -race 实测报 sync.go:545 写 vs :661 读。
// 修复后:Run 每轮经 snapshot() 在锁内取值类型副本,tick 全程只用该副本。
//
// 本测试构造与生产一致的真实并发:后台 Run() 循环 + 面板 handler 并发改配置。
// 若修复被回退(改回直接读 s.cfg),本测试在 -race 下必然失败。
func TestVerify_DDNSCfgRaceFixed(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{
		Enabled:               true,
		EnableA:               true,
		EnableAAAA:            true,
		Domain:                "example.com",
		RR:                    "vpn",
		Period:                10 * time.Millisecond,
		Throttle:              10 * time.Millisecond,
		AliyunAccessKeyID:     "ak",
		AliyunAccessKeySecret: "sk",
		// 用 temp 目录,避免测试写 /etc/ikev2/ddns.conf
		LastFailedFile: filepath.Join(dir, "LAST_DDNS_FAILED"),
		StateFile:      filepath.Join(dir, "ddns.conf"),
	}
	s := NewSync(cfg)

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup

	// 后台同步循环(生产中由 main.go 的 startBG 启动)
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = s.Run(ctx)
	}()

	// 面板 handler 并发热改配置(生产中由 HTTP handler goroutine 调用)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 300; i++ {
			_ = s.SetConfig("vpn", i%2 == 0, i%3 == 0, 10, "example.com")
			_ = s.SetEnabled(i%2 == 1)
			_ = s.SetFamily([]string{"v4", "v6", "dual"}[i%3])
		}
	}()

	time.Sleep(400 * time.Millisecond)
	cancel()
	wg.Wait()
}
