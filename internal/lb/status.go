package lb

import (
	"sort"
	"time"
)

type ProxyStatus struct {
	ProxyName         string             `json:"proxy_name"`
	GeneratedAt       string             `json:"generated_at"`
	Policy            PolicyConfig       `json:"policy"`
	State             RuntimeState       `json:"state"`
	SelectedAccountID string             `json:"selected_account_id,omitempty"`
	SelectedProxyURL  string             `json:"selected_proxy_url,omitempty"`
	SelectedProxyName string             `json:"selected_proxy_name,omitempty"`
	SelectionReason   string             `json:"selection_reason,omitempty"`
	Accounts          []AccountStatus    `json:"accounts"`
	ChildProxies      []ChildProxyStatus `json:"child_proxies,omitempty"`
}

type AccountStatus struct {
	ProxyName        string  `json:"proxy_name,omitempty"`
	Alias            string  `json:"alias"`
	ID               string  `json:"id"`
	Email            string  `json:"email,omitempty"`
	Active           bool    `json:"active"`
	Pinned           bool    `json:"pinned"`
	Healthy          bool    `json:"healthy"`
	Enabled          bool    `json:"enabled"`
	DisabledReason   string  `json:"disabled_reason,omitempty"`
	CooldownSeconds  int64   `json:"cooldown_seconds"`
	DailyLeftPct     float64 `json:"daily_left_pct"`
	DailyResetAt     int64   `json:"daily_reset_at"`
	WeeklyLeftPct    float64 `json:"weekly_left_pct"`
	WeeklyResetAt    int64   `json:"weekly_reset_at"`
	Score            float64 `json:"score"`
	LastUsedAtMS     int64   `json:"last_used_at_ms"`
	LastSwitchReason string  `json:"last_switch_reason,omitempty"`
	QuotaSource      string  `json:"quota_source,omitempty"`
}

type ChildProxyStatus struct {
	Name             string  `json:"name,omitempty"`
	URL              string  `json:"url"`
	Active           bool    `json:"active"`
	Healthy          bool    `json:"healthy"`
	Reachable        bool    `json:"reachable"`
	CooldownSeconds  int64   `json:"cooldown_seconds"`
	Score            float64 `json:"score"`
	SelectedTarget   string  `json:"selected_target,omitempty"`
	SelectionReason  string  `json:"selection_reason,omitempty"`
	LastSwitchReason string  `json:"last_switch_reason,omitempty"`
	LastError        string  `json:"last_error,omitempty"`
}

func BuildProxyStatus(sf StoreFile, now time.Time) ProxyStatus {
	status := ProxyStatus{
		ProxyName:   sf.Settings.Proxy.Name,
		GeneratedAt: now.UTC().Format(time.RFC3339Nano),
		Policy:      sf.Settings.Policy,
		State:       sf.State,
		Accounts:    make([]AccountStatus, 0, len(sf.Accounts)),
	}

	nowMS := now.UnixMilli()
	healthySet := map[int]bool{}
	for _, idx := range healthyIndexes(sf.Accounts, nowMS) {
		healthySet[idx] = true
	}

	if sel, err := selectAccount(&sf, nowMS); err == nil && sel.Index >= 0 && sel.Index < len(sf.Accounts) {
		status.SelectedAccountID = sf.Accounts[sel.Index].ID
		status.SelectionReason = sel.SwitchReason
	}

	for i, a := range sf.Accounts {
		dailyLeft := -1.0
		if a.Quota.DailyLimit > 0 {
			dailyLeft = clamp01((a.Quota.DailyLimit-a.Quota.DailyUsed)/a.Quota.DailyLimit) * 100
		}
		weeklyLeft := -1.0
		if a.Quota.WeeklyLimit > 0 {
			weeklyLeft = clamp01((a.Quota.WeeklyLimit-a.Quota.WeeklyUsed)/a.Quota.WeeklyLimit) * 100
		}
		cooldownSec := int64(0)
		if a.CooldownUntilMS > nowMS {
			cooldownSec = (a.CooldownUntilMS - nowMS + 999) / 1000
		}
		status.Accounts = append(status.Accounts, AccountStatus{
			ProxyName:        sf.Settings.Proxy.Name,
			Alias:            a.Alias,
			ID:               a.ID,
			Email:            a.UserEmail,
			Active:           i == sf.State.ActiveIndex,
			Pinned:           sf.State.PinnedAccountID != "" && sf.State.PinnedAccountID == a.ID,
			Healthy:          healthySet[i],
			Enabled:          a.Enabled,
			DisabledReason:   a.DisabledReason,
			CooldownSeconds:  cooldownSec,
			DailyLeftPct:     dailyLeft,
			DailyResetAt:     a.Quota.DailyResetAt,
			WeeklyLeftPct:    weeklyLeft,
			WeeklyResetAt:    a.Quota.WeeklyResetAt,
			Score:            score(a, sf.Settings.Policy),
			LastUsedAtMS:     a.LastUsedAtMS,
			LastSwitchReason: a.LastSwitchReason,
			QuotaSource:      a.Quota.Source,
		})
	}

	sortAccountStatuses(status.Accounts)

	return status
}

func sortAccountStatuses(accounts []AccountStatus) {
	sort.Slice(accounts, func(i, j int) bool {
		if accounts[i].Active != accounts[j].Active {
			return accounts[i].Active
		}
		if accounts[i].ProxyName != accounts[j].ProxyName {
			return accounts[i].ProxyName < accounts[j].ProxyName
		}
		return accounts[i].Alias < accounts[j].Alias
	})
}
