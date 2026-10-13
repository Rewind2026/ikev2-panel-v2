package ddns

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// v2.86-PR19(B7)回归测试:LAST_DDNS_FAILED 改原子写。
//
// 这个文件是 DDNS 的"失败信号",entrypoint / 运维都会读。裸 os.WriteFile
// 在进程被杀时会留下半截内容,导致误判。

func newRecordResultSync() *Sync {
	return NewSync(Config{Logger: slog.New(slog.NewTextHandler(os.Stderr, nil))})
}

// 失败结果落盘完整、无 tmp 残留。
func TestRecordResult_WritesFailedFileAtomically(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{
		Family:         "dual",
		LastFailedFile: filepath.Join(dir, "sub", "LAST_DDNS_FAILED"),
		Logger:         slog.New(slog.NewTextHandler(os.Stderr, nil)),
	}
	s := newRecordResultSync()

	s.recordResult(cfg, LastSync{Success: false, V4Error: "acpi timeout", V6Error: "no route"})

	body, err := os.ReadFile(cfg.LastFailedFile)
	if err != nil {
		t.Fatalf("LAST_DDNS_FAILED not written: %v", err)
	}
	txt := string(body)
	for _, kw := range []string{"family=dual", "v4=acpi timeout", "v6=no route", "time="} {
		if !strings.Contains(txt, kw) {
			t.Errorf("missing %q in %q", kw, txt)
		}
	}
	entries, err := os.ReadDir(filepath.Dir(cfg.LastFailedFile))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("tmp residue: %v", entries)
	}
}

// 反复失败覆盖写不累积 tmp,内容始终是完整一行。
func TestRecordResult_RepeatedWritesNoAccumulation(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{
		Family:         "v4",
		LastFailedFile: filepath.Join(dir, "LAST_DDNS_FAILED"),
		Logger:         slog.New(slog.NewTextHandler(os.Stderr, nil)),
	}
	s := newRecordResultSync()

	for i := 0; i < 10; i++ {
		s.recordResult(cfg, LastSync{Success: false, V4Error: "boom"})
	}
	body, err := os.ReadFile(cfg.LastFailedFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(body), "family=v4 err=v4=boom time=") {
		t.Fatalf("unexpected content: %q", body)
	}
	if strings.Count(string(body), "family=") != 1 {
		t.Fatalf("content appears appended rather than replaced: %q", body)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("tmp residue: %v", entries)
	}
}

// 成功时必须删除失败标志文件（回归:改原子写不能影响这条路径）。
func TestRecordResult_SuccessRemovesFailedFile(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{
		Family:         "v4",
		LastFailedFile: filepath.Join(dir, "LAST_DDNS_FAILED"),
		Logger:         slog.New(slog.NewTextHandler(os.Stderr, nil)),
	}
	s := newRecordResultSync()

	s.recordResult(cfg, LastSync{Success: false, V4Error: "boom"})
	if _, err := os.Stat(cfg.LastFailedFile); err != nil {
		t.Fatalf("precondition: %v", err)
	}
	s.recordResult(cfg, LastSync{Success: true, V4IP: "203.0.113.9"})
	if _, err := os.Stat(cfg.LastFailedFile); !os.IsNotExist(err) {
		t.Fatalf("LAST_DDNS_FAILED should be removed on success, stat err=%v", err)
	}
}
