package limit

import (
	"os"
	"path/filepath"
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