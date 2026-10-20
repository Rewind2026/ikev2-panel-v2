package panelstate

import (
	"os"
	"strings"
	"testing"
)

// entrypointPath 定位仓库根的 scripts/entrypoint.sh。
func entrypointPath(t *testing.T) string {
	t.Helper()
	p := "../../scripts/entrypoint.sh"
	if _, err := os.Stat(p); err != nil {
		t.Skipf("entrypoint.sh not found at %s: %v", p, err)
	}
	return p
}

// TestEntrypointStartupCleanupRunsBeforeDdosRules 钉住 §4.5c 与 §4.6 的顺序。
//
// 实机 bug:启动清理(按 comment 锚点删自己的旧规则)原本排在 §4.6 加规则之后,
// 于是刚加进去的 connlimit/hashlimit 被紧接着的清理按同样的锚点删掉 ——
// 表现为 entrypoint 日志打印 "connlimit ... added",重启后 DOCKER-USER 却是空的,
// 面板上的 connlimit_per_ip / hashlimit_per_min 保存后完全不生效。
//
// 两段都用同一个 comment 锚点,所以顺序错了自己不会报错,只能靠断言守住。
func TestEntrypointStartupCleanupRunsBeforeDdosRules(t *testing.T) {
	raw, err := os.ReadFile(entrypointPath(t))
	if err != nil {
		t.Fatalf("read entrypoint.sh: %v", err)
	}
	src := string(raw)

	// 启动清理的调用点:裸调用(不是函数定义,也不是 trap 内部的那两处)
	var startupCalls []int
	for i, line := range strings.Split(src, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "cleanup_ikev2_iptables_rules ") {
			continue
		}
		// 排除函数体内的缩进调用(trap _on_exit_cleanup 里的两处,前面有 2 空格缩进)
		if strings.HasPrefix(line, "  ") {
			continue
		}
		startupCalls = append(startupCalls, i)
	}
	if len(startupCalls) != 2 {
		t.Fatalf("expected 2 top-level cleanup calls (iptables + ip6tables), got %d", len(startupCalls))
	}

	// §4.6 加规则的实际调用点:apply_ddos_protection "iptables" / "ip6tables"。
	// 注意不能去比 -I "$chain" 那两行 —— 那是 apply_ddos_protection 的函数体,
	// 函数体按定义位置排在前面是正常的,真正决定顺序的是调用点。
	var addIdx []int
	for i, line := range strings.Split(src, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, `apply_ddos_protection "`) {
			addIdx = append(addIdx, i)
		}
	}
	if len(addIdx) != 2 {
		t.Fatalf("expected 2 apply_ddos_protection calls in §4.6, got %d", len(addIdx))
	}

	firstCleanup := startupCalls[0]
	firstAdd := addIdx[0]
	if firstCleanup > firstAdd {
		t.Errorf("启动清理(第 %d 行)必须早于 §4.6 加规则(第 %d 行):\n"+
			"否则刚加的 connlimit/hashlimit 会被同一批锚点规则删掉,面板的 "+
			"connlimit_per_ip / hashlimit_per_min 保存后不生效",
			firstCleanup+1, firstAdd+1)
	}
}

// TestEntrypointCleanupFunctionDefinedBeforeCall 防止函数定义与调用顺序颠倒。
// shell 逐行执行,定义在调用之后会直接 "command not found" 且被 set -e 打死。
func TestEntrypointCleanupFunctionDefinedBeforeCall(t *testing.T) {
	raw, err := os.ReadFile(entrypointPath(t))
	if err != nil {
		t.Fatalf("read entrypoint.sh: %v", err)
	}
	lines := strings.Split(string(raw), "\n")

	defIdx, callIdx := -1, -1
	for i, line := range lines {
		if defIdx < 0 && strings.HasPrefix(line, "cleanup_ikev2_iptables_rules() {") {
			defIdx = i
		}
		if callIdx < 0 && strings.HasPrefix(strings.TrimSpace(line), "cleanup_ikev2_iptables_rules \"iptables\"") {
			callIdx = i
		}
	}
	if defIdx < 0 {
		t.Fatal("cleanup_ikev2_iptables_rules 定义没找到")
	}
	if callIdx < 0 {
		t.Fatal("cleanup_ikev2_iptables_rules 顶层调用没找到")
	}
	if defIdx > callIdx {
		t.Errorf("函数定义(第 %d 行)必须早于调用(第 %d 行),否则 shell 报 command not found", defIdx+1, callIdx+1)
	}
}
