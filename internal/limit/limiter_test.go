package limit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteAndRemoveLimitFile(t *testing.T) {
	dir := t.TempDir()
	l := New().WithDir(dir)

	if err := l.WriteLimitFile("alice", 10); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "alice")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "10" {
		t.Errorf("content: got %q want %q", string(data), "10")
	}

	if err := l.RemoveLimitFile("alice"); err != nil {
		t.Fatal(err)
	}
	// idempotent
	if err := l.RemoveLimitFile("alice"); err != nil {
		t.Errorf("second remove should be idempotent: %v", err)
	}
}

// ---- v2.85-PR7:BuildTcCommands 测试 ----

func TestFamily_Valid(t *testing.T) {
	cases := []struct {
		f    Family
		want bool
	}{
		{FamilyIPv4, true},
		{FamilyIPv6, true},
		{FamilyDual, true},
		{Family(""), false},
		{Family("garbage"), false},
		{Family("IPv4"), false}, // 大小写敏感
	}
	for _, c := range cases {
		if got := c.f.Valid(); got != c.want {
			t.Errorf("Family(%q).Valid() = %v, want %v", string(c.f), got, c.want)
		}
	}
}

func TestBuildTcCommands_IPv4Up(t *testing.T) {
	cmds := BuildTcCommands(FamilyIPv4, "up", "eth0", 1234, "10.13.0.5", "10.13.0.1")
	if len(cmds) != 2 {
		t.Fatalf("v4 up should emit 2 cmds (class + filter), got %d: %v", len(cmds), cmds)
	}
	joined := joinAll(cmds)
	if !strings.Contains(joined, "protocol ip") {
		t.Errorf("v4 filter must use 'protocol ip', got:\n%s", joined)
	}
	if !strings.Contains(joined, "u32") {
		t.Errorf("v4 filter must use u32 classifier, got:\n%s", joined)
	}
	if !strings.Contains(joined, "match ip src 10.13.0.5") {
		t.Errorf("v4 filter must match src VIP, got:\n%s", joined)
	}
	if strings.Contains(joined, "flower") {
		t.Errorf("v4-only must NOT emit flower, got:\n%s", joined)
	}
	if strings.Contains(joined, "ipv6") {
		t.Errorf("v4-only must NOT contain 'ipv6', got:\n%s", joined)
	}
	if !strings.Contains(joined, "classid 1:1234") {
		t.Errorf("v4 up must pin to classid 1:1234, got:\n%s", joined)
	}
}

func TestBuildTcCommands_IPv6Up(t *testing.T) {
	cmds := BuildTcCommands(FamilyIPv6, "up", "eth0", 1234, "fd00:1::5", "fd00:1::1")
	if len(cmds) != 2 {
		t.Fatalf("v6 up should emit 2 cmds (class + filter), got %d: %v", len(cmds), cmds)
	}
	joined := joinAll(cmds)
	if !strings.Contains(joined, "protocol ipv6") {
		t.Errorf("v6 filter must use 'protocol ipv6', got:\n%s", joined)
	}
	if !strings.Contains(joined, "flower") {
		t.Errorf("v6 filter must use flower classifier, got:\n%s", joined)
	}
	if !strings.Contains(joined, "src_ip fd00:1::5") {
		t.Errorf("v6 filter must match src_ip (flower syntax), got:\n%s", joined)
	}
	if strings.Contains(joined, "u32") {
		t.Errorf("v6 must NOT use u32, got:\n%s", joined)
	}
	if strings.Contains(joined, "match ip src") {
		t.Errorf("v6 must NOT use match ip src, got:\n%s", joined)
	}
}

func TestBuildTcCommands_DualUpSharesClassID(t *testing.T) {
	cmds := BuildTcCommands(FamilyDual, "up", "eth0", 5678, "10.13.0.5", "fd00:1::1")
	// dual = 1 class + 2 filters (v4 + v6) 共 3 命令
	if len(cmds) != 3 {
		t.Fatalf("dual up should emit 3 cmds (1 class + 2 filters), got %d: %v", len(cmds), cmds)
	}
	// class 命令在第一位
	if cmds[0].Op != "class" {
		t.Errorf("dual[0] should be class, got %q", cmds[0].Op)
	}
	joined := joinAll(cmds)
	if !strings.Contains(joined, "protocol ip prio 1 u32") {
		t.Errorf("dual missing v4 u32 filter:\n%s", joined)
	}
	if !strings.Contains(joined, "protocol ipv6 prio 1 flower") {
		t.Errorf("dual missing v6 flower filter:\n%s", joined)
	}
	// 两个 filter 都指向同一个 classid
	v4Classid := strings.Count(joined, "flowid 1:5678")
	if v4Classid != 2 {
		t.Errorf("dual should have 2 flowid refs to 1:5678, got %d:\n%s", v4Classid, joined)
	}
}

func TestBuildTcCommands_Down(t *testing.T) {
	cases := []struct {
		name   string
		family Family
		want   int
	}{
		{"ipv4-down", FamilyIPv4, 2}, // 1 filter del + 1 class del
		{"ipv6-down", FamilyIPv6, 2}, // 1 filter del + 1 class del
		{"dual-down", FamilyDual, 3}, // 1 filter del (v4) + 1 class del + 1 filter del (v6)
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cmds := BuildTcCommands(c.family, "down", "eth0", 5678, "", "")
			if len(cmds) != c.want {
				t.Fatalf("down %s should emit %d cmds, got %d: %v", c.name, c.want, len(cmds), cmds)
			}
			joined := joinAll(cmds)
			if !strings.Contains(joined, "filter del") {
				t.Errorf("down must emit filter del:\n%s", joined)
			}
			if !strings.Contains(joined, "class del") {
				t.Errorf("down must emit class del:\n%s", joined)
			}
			// filter del 必须在 class del 之前(避免 "HTB class in use")
			filterIdx := strings.Index(joined, "filter del")
			classIdx := strings.Index(joined, "class del")
			if filterIdx > classIdx {
				t.Errorf("filter del must precede class del (got filter=%d class=%d):\n%s", filterIdx, classIdx, joined)
			}
		})
	}
}

func TestBuildTcCommands_InvalidInputs(t *testing.T) {
	cases := []struct {
		name   string
		family Family
		verb   string
		outIf  string
	}{
		{"garbage family", Family("garbage"), "up", "eth0"},
		{"empty family", Family(""), "up", "eth0"},
		{"uppercase family", Family("IPv4"), "up", "eth0"},
		{"invalid verb", FamilyIPv4, "purge", "eth0"},
		{"empty outif", FamilyIPv4, "up", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cmds := BuildTcCommands(c.family, c.verb, c.outIf, 100, "x", "y")
			if len(cmds) != 0 {
				t.Errorf("invalid input must return empty, got %d cmds: %v", len(cmds), cmds)
			}
		})
	}
}

func TestBuildTcCommands_LineFormat(t *testing.T) {
	cmds := BuildTcCommands(FamilyIPv4, "up", "eth0", 42, "10.0.0.1", "10.0.0.254")
	if len(cmds) != 2 {
		t.Fatalf("want 2 cmds, got %d", len(cmds))
	}
	line0 := cmds[0].Line()
	if !strings.HasPrefix(line0, "tc class ") {
		t.Errorf("cmds[0].Line() = %q, want prefix 'tc class '", line0)
	}
	line1 := cmds[1].Line()
	if !strings.HasPrefix(line1, "tc filter ") {
		t.Errorf("cmds[1].Line() = %q, want prefix 'tc filter '", line1)
	}
	if !strings.Contains(line1, "10.0.0.1") {
		t.Errorf("cmds[1].Line() must contain src VIP, got %q", line1)
	}
}

func TestSetRate(t *testing.T) {
	cmds := BuildTcCommands(FamilyDual, "up", "eth0", 100, "10.13.0.5", "fd00:1::1")
	out := SetRate(cmds, 50)
	if len(out) != len(cmds) {
		t.Fatalf("SetRate changed length: %d → %d", len(cmds), len(out))
	}
	for i, c := range out {
		if c.Op == "class" {
			joined := strings.Join(c.Args, " ")
			if !strings.Contains(joined, "rate 50mbit") {
				t.Errorf("cmd[%d] class rate not replaced:\n%s", i, joined)
			}
			if !strings.Contains(joined, "ceil 50mbit") {
				t.Errorf("cmd[%d] class ceil not replaced:\n%s", i, joined)
			}
		}
	}
	// filter 命令不受影响
	for i, c := range out {
		if c.Op == "filter" {
			joined := strings.Join(c.Args, " ")
			if strings.Contains(joined, "1000mbit") {
				t.Errorf("cmd[%d] filter should not have 1000mbit:\n%s", i, joined)
			}
			if strings.Contains(joined, "50mbit") {
				t.Errorf("cmd[%d] filter should not have 50mbit either:\n%s", i, joined)
			}
		}
	}
	// 输入不应被改
	for i, c := range cmds {
		if c.Op == "class" {
			joined := strings.Join(c.Args, " ")
			if !strings.Contains(joined, "1000mbit") {
				t.Errorf("input cmd[%d] should still have 1000mbit:\n%s", i, joined)
			}
		}
	}
}

func TestSetRate_ZeroMbps(t *testing.T) {
	cmds := BuildTcCommands(FamilyIPv6, "up", "eth0", 1, "fd00::1", "")
	out := SetRate(cmds, 0)
	joined := joinAll(out)
	if !strings.Contains(joined, "rate 0mbit") || !strings.Contains(joined, "ceil 0mbit") {
		t.Errorf("SetRate(0) should produce 0mbit rates, got:\n%s", joined)
	}
}

// joinAll helper 把 TcCommand 切片拼成单行字符串便于 contains 检查。
func joinAll(cmds []TcCommand) string {
	var b strings.Builder
	for _, c := range cmds {
		b.WriteString(c.Line())
		b.WriteString("\n")
	}
	return b.String()
}
