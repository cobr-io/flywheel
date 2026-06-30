package up

import (
	"errors"
	"fmt"
	"testing"
)

func TestIsPortAllocatedErr(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"docker publish conflict", errors.New("k3d registry create acme: exit status 1\nBind for 0.0.0.0:50002 failed: port is already allocated"), true},
		{"bind conflict", fmt.Errorf("wrap: %w", errors.New("listen tcp 0.0.0.0:8080: bind: address already in use")), true},
		{"unrelated error", errors.New("k3d not found"), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isPortAllocatedErr(tc.err); got != tc.want {
				t.Errorf("isPortAllocatedErr(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestRetryOnPortCollision(t *testing.T) {
	portErr := errors.New("Bind for 0.0.0.0:50002 failed: port is already allocated")
	otherErr := errors.New("docker daemon not running")

	t.Run("port collision then reheal+retry succeeds", func(t *testing.T) {
		var creates, reheals int
		create := func() error {
			creates++
			if creates == 1 {
				return portErr
			}
			return nil
		}
		reheal := func() error { reheals++; return nil }
		if err := retryOnPortCollision(create, reheal); err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		if creates != 2 || reheals != 1 {
			t.Errorf("creates=%d reheals=%d, want 2 and 1", creates, reheals)
		}
	})

	t.Run("non-port error returns immediately, no reheal", func(t *testing.T) {
		var creates, reheals int
		create := func() error { creates++; return otherErr }
		reheal := func() error { reheals++; return nil }
		if err := retryOnPortCollision(create, reheal); !errors.Is(err, otherErr) {
			t.Fatalf("err = %v, want %v", err, otherErr)
		}
		if creates != 1 || reheals != 0 {
			t.Errorf("creates=%d reheals=%d, want 1 and 0", creates, reheals)
		}
	})

	t.Run("success first try, no reheal", func(t *testing.T) {
		var creates, reheals int
		create := func() error { creates++; return nil }
		reheal := func() error { reheals++; return nil }
		if err := retryOnPortCollision(create, reheal); err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		if creates != 1 || reheals != 0 {
			t.Errorf("creates=%d reheals=%d, want 1 and 0", creates, reheals)
		}
	})

	t.Run("reheal failure is returned", func(t *testing.T) {
		rehealErr := errors.New("could not delete partial registry")
		create := func() error { return portErr }
		reheal := func() error { return rehealErr }
		if err := retryOnPortCollision(create, reheal); !errors.Is(err, rehealErr) {
			t.Fatalf("err = %v, want %v", err, rehealErr)
		}
	})

	t.Run("retry still colliding returns the second error", func(t *testing.T) {
		create := func() error { return portErr } // always collides
		reheal := func() error { return nil }
		if err := retryOnPortCollision(create, reheal); !errors.Is(err, portErr) {
			t.Fatalf("err = %v, want %v", err, portErr)
		}
	})
}
