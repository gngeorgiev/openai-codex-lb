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

func TestAggregateUsageResponseMatchesStatusFooterAccountAverage(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_700_000_000, 0)
	status := ProxyStatus{
		Accounts: []AccountStatus{
			{Alias: "ready", Enabled: true, Healthy: true, DailyLeftPct: 80, WeeklyLeftPct: 70, DailyResetAt: now.Add(3 * time.Hour).Unix(), WeeklyResetAt: now.Add(5 * 24 * time.Hour).Unix()},
			{Alias: "disabled", Enabled: false, Healthy: false, DisabledReason: "auth", DailyLeftPct: 20, WeeklyLeftPct: 10, DailyResetAt: now.Add(2 * time.Hour).Unix(), WeeklyResetAt: now.Add(4 * 24 * time.Hour).Unix()},
		},
	}

	payload := aggregateUsageResponse(status, now)

	if got := payload.RateLimit.PrimaryWindow.UsedPercent; got != 50 {
		t.Fatalf("expected primary used_percent 50, got %v", got)
	}
	if got := payload.RateLimit.SecondaryWindow.UsedPercent; got != 60 {
		t.Fatalf("expected secondary used_percent 60, got %v", got)
	}
	if got := payload.RateLimit.PrimaryWindow.ResetAt; got != now.Add(2*time.Hour).Unix() {
		t.Fatalf("expected earliest primary reset from all status accounts, got %d", got)
	}
}
