package lb

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type usageWindow struct {
	Limit             float64 `json:"limit"`
	Used              float64 `json:"used"`
	UsedPercent       float64 `json:"used_percent"`
	ResetAfterSeconds int64   `json:"reset_after_seconds"`
	ResetAt           int64   `json:"reset_at"`
	ResetsAt          int64   `json:"resets_at"`
}

type usageResponse struct {
	RateLimit struct {
		PrimaryWindow   usageWindow `json:"primary_window"`
		SecondaryWindow usageWindow `json:"secondary_window"`
	} `json:"rate_limit"`
}

func usageURL(base string) (string, error) {
	u, err := url.Parse(base)
	if err != nil {
		return "", fmt.Errorf("parse base url: %w", err)
	}
	if strings.Contains(u.Path, "/backend-api") {
		u.Path = "/backend-api/wham/usage"
	} else {
		u.Path = "/api/codex/usage"
	}
	return u.String(), nil
}

func parseWindow(now time.Time, w usageWindow) (limit float64, used float64, resetAt int64) {
	resetAt = w.ResetsAt
	if resetAt <= 0 {
		resetAt = w.ResetAt
	}
	if resetAt <= 0 && w.ResetAfterSeconds > 0 {
		resetAt = now.Unix() + w.ResetAfterSeconds
	}

	if w.Limit > 0 {
		return w.Limit, math.Max(0, w.Used), resetAt
	}
	if w.UsedPercent >= 0 {
		usedPct := math.Max(0, math.Min(100, w.UsedPercent))
		return 100, usedPct, resetAt
	}
	return 0, 0, resetAt
}

func refreshQuotaForAccount(ctx context.Context, client *http.Client, account *Account, auth AuthInfo, now time.Time) error {
	url, err := usageURL(account.BaseURL)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("build usage request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+auth.AccessToken)
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("fetch usage: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("usage status %d", resp.StatusCode)
	}
	var payload usageResponse
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return fmt.Errorf("decode usage payload: %w", err)
	}

	dailyLimit, dailyUsed, dailyReset := parseWindow(now, payload.RateLimit.PrimaryWindow)
	weeklyLimit, weeklyUsed, weeklyReset := parseWindow(now, payload.RateLimit.SecondaryWindow)
	account.Quota.DailyLimit = dailyLimit
	account.Quota.DailyUsed = dailyUsed
	account.Quota.DailyResetAt = dailyReset
	account.Quota.WeeklyLimit = weeklyLimit
	account.Quota.WeeklyUsed = weeklyUsed
	account.Quota.WeeklyResetAt = weeklyReset
	account.Quota.LastSyncAt = now.UnixMilli()
	account.Quota.Source = "openai_usage_api"
	return nil
}

func dueForQuotaRefresh(account Account, state RuntimeState, quotaCfg QuotaConfig, now time.Time) bool {
	refreshMS := int64(max(1, quotaCfg.RefreshIntervalMinutes) * int(time.Minute/time.Millisecond))
	refreshMessages := int64(max(1, quotaCfg.RefreshIntervalMessages))

	if account.Quota.LastSyncAt <= 0 {
		return true
	}
	if now.UnixMilli()-account.Quota.LastSyncAt >= refreshMS {
		return true
	}
	msgsSince := state.MessageCounter - account.Quota.LastSyncMessageCounter
	if msgsSince >= refreshMessages {
		return true
	}
	return account.Quota.DailyLimit <= 0 || account.Quota.WeeklyLimit <= 0
}

func parseRetryAfterSeconds(headers http.Header) int {
	raw := strings.TrimSpace(headers.Get("Retry-After"))
	if raw == "" {
		return 0
	}
	if n, err := strconv.Atoi(raw); err == nil && n > 0 {
		return n
	}
	if t, err := http.ParseTime(raw); err == nil {
		delta := int(time.Until(t).Seconds())
		if delta > 0 {
			return delta
		}
	}
	return 0
}
