package expiry

import (
	"context"
	"testing"
)

// mockSwanctl implements SwanctlManager
type mockSwanctl struct {
	calls []string
}

func (m *mockSwanctl) Terminate(_ context.Context, username string) error {
	m.calls = append(m.calls, username)
	return nil
}

func TestSwanctlManagerMock(t *testing.T) {
	ms := &mockSwanctl{}
	for _, u := range []string{"alice", "bob"} {
		if err := ms.Terminate(context.Background(), u); err != nil {
			t.Fatal(err)
		}
	}
	if len(ms.calls) != 2 || ms.calls[0] != "alice" || ms.calls[1] != "bob" {
		t.Errorf("calls: %v", ms.calls)
	}
}