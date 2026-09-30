package bot

import (
	"testing"
	"time"
)

// TestRetryBackoff — пауза между попытками растёт и упирается в потолок.
func TestRetryBackoff(t *testing.T) {
	if got := retryBackoff(0); got != 2*time.Minute {
		t.Errorf("retryBackoff(0) = %v, ожидали 2m", got)
	}
	if got := retryBackoff(4); got != 30*time.Minute {
		t.Errorf("retryBackoff(4) = %v, ожидали 30m", got)
	}
	if got := retryBackoff(100); got != 30*time.Minute {
		t.Errorf("retryBackoff(100) = %v, ожидали 30m (потолок)", got)
	}
	prev := time.Duration(0)
	for i := 0; i < 6; i++ {
		if got := retryBackoff(i); got < prev {
			t.Errorf("retryBackoff не должен уменьшаться: %v < %v на i=%d", got, prev, i)
		} else {
			prev = got
		}
	}
}
