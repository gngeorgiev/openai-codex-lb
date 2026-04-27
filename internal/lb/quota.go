package lb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type usageWindow struct {
	Limit              float64 `json:"limit"`
	Used               float64 `json:"used"`
	UsedPercent        float64 `json:"used_percent"`
	LimitWindowSeconds int64   `json:"limit_window_seconds,omitempty"`
	ResetAfterSeconds  int64   `json:"reset_after_seconds"`
	ResetAt            int64   `json:"reset_at"`
	ResetsAt           int64   `json:"resets_at"`
}

type usageRateLimit struct {
	Allowed         bool        `json:"allowed"`
	LimitReached    bool        `json:"limit_reached"`
	PrimaryWindow   usageWindow `json:"primary_window"`
	SecondaryWindow usageWindow `json:"secondary_window"`
}

type usageCredits struct {
	HasCredits          bool   `json:"has_credits"`
	Unlimited           bool   `json:"unlimited"`
	OverageLimitReached bool   `json:"overage_limit_reached"`
	Balance             string `json:"balance"`
	ApproxLocalMessages [2]int `json:"approx_local_messages"`
	ApproxCloudMessages [2]int `json:"approx_cloud_messages"`
}

type usageSpendControl struct {
	Reached bool `json:"reached"`
}

type usageResponse struct {
	UserID               string            `json:"user_id,omitempty"`
	AccountID            string            `json:"account_id,omitempty"`
	Email                string            `json:"email,omitempty"`
	PlanType             string            `json:"plan_type,omitempty"`
	RateLimit            usageRateLimit    `json:"rate_limit"`
	CodeReviewRateLimit  any               `json:"code_review_rate_limit"`
	AdditionalRateLimits []any             `json:"additional_rate_limits"`
	Credits              usageCredits      `json:"credits"`
	SpendControl         usageSpendControl `json:"spend_control"`
	RateLimitReachedType any               `json:"rate_limit_reached_type"`
	Promo                any               `json:"promo"`
}

type upstreamStatusError struct {
	Operation  string
	StatusCode int
}

func (e *upstreamStatusError) Error() string {
	return fmt.Sprintf("%s status %d", e.Operation, e.StatusCode)
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
		return &upstreamStatusError{Operation: "usage", StatusCode: resp.StatusCode}
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

func aggregateUsageResponse(status ProxyStatus, now time.Time) usageResponse {
	dailyUsedPercent, dailyResetAt := aggregateUsageWindow(status.Accounts, now, func(a AccountStatus) (float64, int64) {
		if a.DailyLeftPct < 0 {
			return -1, 0
		}
		return clampUsagePercent(100 - a.DailyLeftPct), a.DailyResetAt
	})
	weeklyUsedPercent, weeklyResetAt := aggregateUsageWindow(status.Accounts, now, func(a AccountStatus) (float64, int64) {
		if a.WeeklyLeftPct < 0 {
			return -1, 0
		}
		return clampUsagePercent(100 - a.WeeklyLeftPct), a.WeeklyResetAt
	})

	var payload usageResponse
	payload.UserID = "proxy-only"
	payload.AccountID = "proxy-only"
	payload.Email = "proxy-only@codexlb.internal"
	payload.PlanType = "plus"
	payload.RateLimit.Allowed = dailyUsedPercent < 100 && weeklyUsedPercent < 100
	payload.RateLimit.LimitReached = !payload.RateLimit.Allowed
	payload.RateLimit.PrimaryWindow.Limit = 100
	payload.RateLimit.PrimaryWindow.Used = dailyUsedPercent
	payload.RateLimit.PrimaryWindow.UsedPercent = dailyUsedPercent
	payload.RateLimit.PrimaryWindow.LimitWindowSeconds = 5 * 60 * 60
	payload.RateLimit.PrimaryWindow.ResetAt = dailyResetAt
	payload.RateLimit.PrimaryWindow.ResetsAt = dailyResetAt
	if dailyResetAt > 0 {
		payload.RateLimit.PrimaryWindow.ResetAfterSeconds = maxInt64(0, dailyResetAt-now.Unix())
	}

	payload.RateLimit.SecondaryWindow.Limit = 100
	payload.RateLimit.SecondaryWindow.Used = weeklyUsedPercent
	payload.RateLimit.SecondaryWindow.UsedPercent = weeklyUsedPercent
	payload.RateLimit.SecondaryWindow.LimitWindowSeconds = 7 * 24 * 60 * 60
	payload.RateLimit.SecondaryWindow.ResetAt = weeklyResetAt
	payload.RateLimit.SecondaryWindow.ResetsAt = weeklyResetAt
	if weeklyResetAt > 0 {
		payload.RateLimit.SecondaryWindow.ResetAfterSeconds = maxInt64(0, weeklyResetAt-now.Unix())
	}

	return payload
}

func aggregateUsageWindow(accounts []AccountStatus, now time.Time, extract func(AccountStatus) (usedPercent float64, resetAt int64)) (float64, int64) {
	total := 0.0
	count := 0
	earliestReset := int64(0)
	for _, account := range accounts {
		usedPercent, resetAt := extract(account)
		if usedPercent < 0 {
			continue
		}
		total += usedPercent
		count++
		if resetAt > now.Unix() && (earliestReset == 0 || resetAt < earliestReset) {
			earliestReset = resetAt
		}
	}
	if count == 0 {
		return 0, 0
	}
	return total / float64(count), earliestReset
}

func clampUsagePercent(v float64) float64 {
	return math.Max(0, math.Min(100, v))
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func authFailureStatusFromError(err error) int {
	var statusErr *upstreamStatusError
	if !errors.As(err, &statusErr) {
		return 0
	}
	if statusErr.StatusCode == http.StatusUnauthorized || statusErr.StatusCode == http.StatusForbidden {
		return statusErr.StatusCode
	}
	return 0
}
