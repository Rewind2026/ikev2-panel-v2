package auth

import (
	"errors"
	"strings"
	"testing"
)

func TestGeneratePassword(t *testing.T) {
	tests := []int{6, 12, 24, 64}
	for _, n := range tests {
		pw, err := GeneratePassword(n)
		if err != nil {
			t.Fatalf("GeneratePassword(%d): %v", n, err)
		}
		if len(pw) != n {
			t.Errorf("len mismatch: got %d want %d", len(pw), n)
		}
		// 不应包含 0/1/O/l 等歧义字符
		for _, c := range pw {
			if c == '0' || c == '1' || c == 'O' || c == 'o' || c == 'l' || c == 'I' {
				t.Errorf("ambiguous char %q in password %q", c, pw)
			}
		}
	}
}

func TestGeneratePasswordBadLength(t *testing.T) {
	for _, n := range []int{0, 5, 65, 100} {
		if _, err := GeneratePassword(n); err == nil {
			t.Errorf("expected error for length %d", n)
		}
	}
}

func TestGeneratePasswordRandomness(t *testing.T) {
	a, _ := GeneratePassword(12)
	b, _ := GeneratePassword(12)
	if a == b {
		t.Error("two consecutive generations returned the same password (bug)")
	}
}

func TestHashAndVerifyPassword(t *testing.T) {
	hash, err := BaseHashPassword("hello-world")
	if err != nil {
		t.Fatal(err)
	}
	if hash == "hello-world" {
		t.Error("hash should not equal plaintext")
	}
	if !strings.HasPrefix(hash, "$2a$") && !strings.HasPrefix(hash, "$2b$") {
		t.Errorf("unexpected hash format: %s", hash)
	}

	if err := VerifyPassword(hash, "hello-world"); err != nil {
		t.Errorf("verify correct: %v", err)
	}
	if err := VerifyPassword(hash, "wrong"); !errors.Is(err, ErrInvalidPassword) {
		t.Errorf("verify wrong: expected ErrInvalidPassword, got %v", err)
	}
}

func TestGenerateSessionID(t *testing.T) {
	id, err := GenerateSessionID()
	if err != nil {
		t.Fatal(err)
	}
	// base64.RawURLEncoding(32 bytes) = 43 chars
	if len(id) != 43 {
		t.Errorf("len: got %d want 43", len(id))
	}
	id2, _ := GenerateSessionID()
	if id == id2 {
		t.Error("two consecutive generations returned the same id")
	}
}