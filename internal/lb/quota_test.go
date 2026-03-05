package lb

import (
	"net/http"
	"testing"
	"time"
)

func TestParseWindowWithUsedPercentFallback(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_700_000_000, 0)
	limit, used, reset := parseWindow(now, usageWindow{UsedPercent: 42, ResetAfterSeconds: 60})
	if limit != 100 || used != 42 {
		t.Fatalf("unexpected fallback parse: limit=%v used=%v", limit, used)
	}
	if reset != now.Unix()+60 {
		t.Fatalf("unexpected reset: %d", reset)
	}
}

func TestParseRetryAfterSeconds(t *testing.T) {
	t.Parallel()
	h := http.Header{}
	h.Set("Retry-After", "7")
	if got := parseRetryAfterSeconds(h); got != 7 {
		t.Fatalf("expected 7, got %d", got)
	}
}

func TestUsageURLBackendAPI(t *testing.T) {
	t.Parallel()
	u, err := usageURL("https://chatgpt.com/backend-api")
	if err != nil {
		t.Fatalf("usageURL: %v", err)
	}
	if want := "https://chatgpt.com/backend-api/wham/usage"; u != want {
		t.Fatalf("expected %s, got %s", want, u)
	}
}
