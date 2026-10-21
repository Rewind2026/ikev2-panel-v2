// v2.93:`swanctl --list-conns` 解析测试。
//
// 背景:home 的 IKE_SA 卡要能区分"没人连"(off)和"conn 压根没注册进
// charon"(danger)。后者在 63 实机真实发生过(自签模式下 entrypoint 的
// load-all 早于证书安装 → "loaded 0 of 1 connections")。
//
// 这里的 fake 二进制输出的是 63 上真实抓到的 `swanctl --list-conns` 格式,
// 解析必须认得出顶层 conn 头、又不能被子 SA 段的同名行骗到。
package swanctl

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// fakeListConns 造一个只会响应 --list-conns 的假 swanctl。
func fakeListConns(t *testing.T, stdout string, exitCode int) string {
	t.Helper()
	body := `#!/usr/bin/env bash
if [ "$1" = "--list-conns" ]; then
  cat <<'EOF'
` + stdout + `
EOF
  exit ` + itoa(exitCode) + `
fi
echo "swanctl: invalid option" >&2
exit 1
`
	p := filepath.Join(t.TempDir(), "fake-swanctl")
	if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
		t.Fatalf("write fake swanctl: %v", err)
	}
	return p
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	return string(rune('0' + n))
}

// realListConnsOutput 63 上真实 `swanctl --list-conns` 的输出(缩进结构原样保留)。
const realListConnsOutput = `ikev2-rw: IKEv2, no reauthentication, rekeying every 86400s, dpd delay 30s
  local:  2408:832e:8a4:5be0:bc24:1100:18c:3236
  remote: %any
  local public key authentication:
    id: vpn.example.com
    certs: O=IKEv2 Panel, CN=vpn.example.com
  remote EAP_MSCHAPV2 authentication:
    eap_id: %any
  ikev2-rw: TUNNEL, rekeying every 3600s, dpd action is none
    local:  0.0.0.0/0 ::/0
    remote: dynamic
`

// TestListLoadedConns_ParsesTopLevelConn 只认第 0 列的 conn 头。
//
// 子 SA 段里也有 "ikev2-rw: TUNNEL, ..." 这一行 —— 它有缩进,不能被当成
// 第二个 conn(否则会误报"加载了 2 个 conn")。
func TestListLoadedConns_ParsesTopLevelConn(t *testing.T) {
	m := New().WithSwanctlBin(fakeListConns(t, realListConnsOutput, 0))

	conns, err := m.ListLoadedConns(context.Background())
	if err != nil {
		t.Fatalf("ListLoadedConns: %v", err)
	}
	if len(conns) != 1 {
		t.Fatalf("expected 1 conn, got %d: %v", len(conns), conns)
	}
	if conns[0] != "ikev2-rw" {
		t.Errorf("conn name = %q, want ikev2-rw", conns[0])
	}
}

// TestListLoadedConns_EmptyOutputMeansNoConn 空输出 = 一个 conn 都没加载。
//
// 这正是 63 故障态的表现(配置里有 conn,charon 里没有)。
func TestListLoadedConns_EmptyOutputMeansNoConn(t *testing.T) {
	m := New().WithSwanctlBin(fakeListConns(t, "", 0))

	conns, err := m.ListLoadedConns(context.Background())
	if err != nil {
		t.Fatalf("ListLoadedConns: %v", err)
	}
	if len(conns) != 0 {
		t.Errorf("expected 0 conns, got %v", conns)
	}
}

// TestListLoadedConns_ErrorPropagates 查询失败必须返回 error。
//
// 调用方靠 error 区分"没加载"和"查不了"—— 查不了时不能误报故障。
func TestListLoadedConns_ErrorPropagates(t *testing.T) {
	m := New().WithSwanctlBin(fakeListConns(t, "boom", 1))

	if _, err := m.ListLoadedConns(context.Background()); err == nil {
		t.Fatal("expected error when swanctl exits non-zero")
	}
}

// TestListLoadedConns_SkipVici dev 模式不 exec swanctl。
func TestListLoadedConns_SkipVici(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "calls.log")
	m := New().WithSwanctlBin(fakeSwanctl(t, logPath, false)).WithSkipVici()

	conns, err := m.ListLoadedConns(context.Background())
	if err != nil {
		t.Fatalf("SkipVici must not error, got %v", err)
	}
	if len(conns) != 0 {
		t.Errorf("expected 0 conns in SkipVici mode, got %v", conns)
	}
	if _, err := os.Stat(logPath); err == nil {
		t.Errorf("SkipVici must not exec swanctl at all, got calls: %v", readCalls(t, logPath))
	}
}
