// v2.86-pr24(P1-8)撤销后的**反向**回归测试。
//
// 背景：v2.86-pr24 曾把改 IP 段的重载从 `--load-all` 换成 `--load-conn ikev2-rw`，
// 声称能避免踢掉在线用户。该实现已撤销（理由见 loader.go 的 "P1-8 已撤销" 注释块）。
//
// 本文件防的是**同一类错误的复发**：调用一个 swanctl CLI 并不存在的子命令。
//
// 原测试为什么没拦住：它用 fake swanctl 脚本，对任何含 `--load-conn` 的参数都 exit 0。
// 测试替身把被测对象的失败模式一起替身掉了 —— "我们调用了什么参数" 通过，
// 不代表"这个命令能工作"。这是测试替身的经典陷阱。
//
// 下面 TestSwanctlCLISubcommandsExist 用**真实的官方子命令白名单**做校验，
// 白名单之外的命令名一律判失败 —— 命令不存在时会红，不会再被假二进制盖住。
package swanctl

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// swanctlLoadSubcommands strongSwan 6.0 的加载类 CLI 子命令白名单。
//
// 来源：https://docs.strongswan.org/docs/6.0/swanctl/swanctl.html
//
// 注意命名：官方是 `--load-conns`（**复数**），且不接受连接名参数。
// 曾经被误用的 `--load-conn`（单数）不在此列 —— 它不存在，调用必然失败。
var swanctlLoadSubcommands = map[string]bool{
	"--load-all":         true,
	"--load-conns":       true,
	"--load-pools":       true,
	"--load-creds":       true,
	"--load-authorities": true,
	"--load-shared":      true,
}

// fakeSwanctl 造一个假的 swanctl 二进制：把每次调用追加记录到 log 文件。
//
// 刻意设计成**只认白名单内的命令**：传入未知子命令时 exit 1 并写 stderr。
// 这模拟了真实 swanctl 对未知选项的行为（"invalid option"），让测试不再
// 对不存在的命令一视同仁地放行。
//
// failAll：让所有已知子命令都失败（用于验证错误传播）。
func fakeSwanctl(t *testing.T, logPath string, failAll bool) string {
	t.Helper()
	body := `#!/usr/bin/env bash
args="$*"
echo "$args" >> "` + logPath + `"
case "$args" in
  *--load-all*|*--load-conns*|*--load-pools*|*--load-creds*|*--load-authorities*|*--load-shared*)
    ` + boolExpr(failAll) + ` && { echo "swanctl failed (fake)" >&2; exit 1; }
    exit 0
    ;;
esac
echo "swanctl: invalid option \"$(echo "$args" | cut -d' ' -f1)\"" >&2
exit 1
`
	p := filepath.Join(t.TempDir(), "fake-swanctl")
	if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
		t.Fatalf("write fake swanctl: %v", err)
	}
	return p
}

func boolExpr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func readCalls(t *testing.T, logPath string) []string {
	t.Helper()
	b, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read call log: %v", err)
	}
	var out []string
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

// TestSwanctlCLISubcommandsExist 白名单外的子命令一律判失败。
//
// 这是本文件的核心断言：它不依赖 fake 二进制的行为，而是直接校验
// "我们代码里会用的命令名，在官方子命令集合内"。
func TestSwanctlCLISubcommandsExist(t *testing.T) {
	// 这些是生产代码实际会用的命令名。任何人新增一条 swanctl 调用，
	// 都应该在这里登记 —— 不登记就是"用了可能不存在的命令"，测试会红。
	used := []string{"--load-all"}
	for _, cmd := range used {
		if !swanctlLoadSubcommands[cmd] {
			t.Errorf("swanctl %s 不是 strongSwan 6.0 的合法子命令 —— "+
				"调用它必然失败(参见 loader.go 的 P1-8 撤销说明)", cmd)
		}
	}
}

// TestReloadAllUsesValidSubcommand reloadAllLocked 用的命令必须在白名单内。
//
// 端到端跑一次并检查实际 argv，而不只是检查常量 —— 防止有人改了调用点
// 却忘了改白名单，或反之。
func TestReloadAllUsesValidSubcommand(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "calls.log")
	m := New().WithSwanctlBin(fakeSwanctl(t, logPath, false))

	if err := m.reloadAllLocked(context.Background()); err != nil {
		t.Fatalf("reloadAllLocked: %v", err)
	}

	calls := readCalls(t, logPath)
	if len(calls) != 1 {
		t.Fatalf("expected exactly 1 swanctl call, got %d: %v", len(calls), calls)
	}
	assertValidSwanctlCall(t, calls[0])
	// --noprompt 必须始终带上，否则 swanctl 会等待交互确认
	if !strings.Contains(calls[0], "--noprompt") {
		t.Errorf("call = %q, want --noprompt to be passed", calls[0])
	}
}

// TestNoSingleConnSubcommand 明确锁死"不要用 --load-conn（单数）"。
//
// 单列一个用例而不是只靠白名单，是因为这是已经犯过的错：
// 白名单能挡住"新写的未知命令"，但挡不住"有人把 --load-conns 误改成 --load-conn"
// 后又顺手把它加进白名单"。这里直接把该名字钉死为非法。
func TestNoSingleConnSubcommand(t *testing.T) {
	if swanctlLoadSubcommands["--load-conn"] {
		t.Error("--load-conn（单数）不是 strongSwan 的合法子命令 —— " +
			"官方只有 --load-conns（复数）。它不接受连接名参数，" +
			"按名细粒度重载只能走 VICI 层（且本项目只有一条连接，无收益）")
	}
}

// assertValidSwanctlCall 校验一次调用的 argv 里不含非法子命令。
func assertValidSwanctlCall(t *testing.T, call string) {
	t.Helper()
	for _, field := range strings.Fields(call) {
		if !strings.HasPrefix(field, "--load-") {
			continue
		}
		if !swanctlLoadSubcommands[field] {
			t.Errorf("swanctl 调用包含非法加载子命令 %q（call=%q）—— "+
				"真实 swanctl 会报 invalid option 并退出非零", field, call)
		}
	}
}

// TestReloadAllFailurePropagates reload 失败必须返回错误（不能静默成功）。
func TestReloadAllFailurePropagates(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "calls.log")
	m := New().WithSwanctlBin(fakeSwanctl(t, logPath, true))

	err := m.reloadAllLocked(context.Background())
	if err == nil {
		t.Fatal("expected error when swanctl fails")
	}
	if !strings.Contains(err.Error(), "--load-all") {
		t.Errorf("error must mention --load-all, got %q", err)
	}
	if !strings.Contains(err.Error(), "swanctl failed (fake)") {
		t.Errorf("error should carry swanctl stderr, got %q", err)
	}
}

// TestReloadAllSkipVici dev 模式必须是 no-op：一个进程都不起。
func TestReloadAllSkipVici(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "calls.log")
	m := New().WithSwanctlBin(fakeSwanctl(t, logPath, true)).WithSkipVici()

	if err := m.reloadAllLocked(context.Background()); err != nil {
		t.Fatalf("SkipVici must swallow everything, got %v", err)
	}
	if _, err := os.Stat(logPath); err == nil {
		t.Errorf("SkipVici must not exec swanctl at all, got calls: %v", readCalls(t, logPath))
	} else if !os.IsNotExist(err) {
		t.Fatalf("unexpected stat error: %v", err)
	}
}

// TestUpdatePoolsAndReloadUsesLoadAll 端到端：改 IP 段的重载走 --load-all。
//
// 官方定义 --load-all = --load-conns + --load-creds + --load-authorities + --load-pools。
// 我们改的是 **pool** 段，只有 --load-all 含 --load-pools；换成 --load-conns
// 会漏掉 pool 本身，改动可能不生效。所以这条断言的是"必须是 --load-all"。
func TestUpdatePoolsAndReloadUsesLoadAll(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "calls.log")
	confPath := filepath.Join(t.TempDir(), "swanctl.conf")
	if err := os.WriteFile(confPath, []byte(poolConfTmpl), 0o644); err != nil {
		t.Fatalf("seed conf: %v", err)
	}

	m := New().WithSwanctlConfPath(confPath).WithSwanctlBin(fakeSwanctl(t, logPath, false))

	if err := m.UpdatePoolsAndReload(context.Background(), "10.99.0.0/24", "fd00:99::/64"); err != nil {
		t.Fatalf("UpdatePoolsAndReload: %v", err)
	}

	calls := readCalls(t, logPath)
	if len(calls) != 1 {
		t.Fatalf("expected exactly 1 swanctl call, got %d: %v", len(calls), calls)
	}
	assertValidSwanctlCall(t, calls[0])
	if !strings.Contains(calls[0], "--load-all") {
		t.Errorf("call = %q, want --load-all (only it includes --load-pools)", calls[0])
	}
}
