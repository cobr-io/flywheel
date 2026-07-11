package up

import "testing"

// TestWaitEnabled covers Options.Wait's *bool sentinel: nil defaults to
// waiting (matching --wait's true default), and explicit true/false are
// honored — the bug this guards against was opts.Wait's zero value (false)
// being silently coerced back to true, making --wait=false a no-op.
func TestWaitEnabled(t *testing.T) {
	cases := []struct {
		name string
		wait *bool
		want bool
	}{
		{"nil defaults to wait", nil, true},
		{"explicit true waits", boolPtr(true), true},
		{"explicit false skips", boolPtr(false), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := waitEnabled(Options{Wait: c.wait}); got != c.want {
				t.Errorf("waitEnabled(Wait: %v) = %v, want %v", c.wait, got, c.want)
			}
		})
	}
}
