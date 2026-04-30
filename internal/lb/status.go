package lb

import (
	"sort"
	"strings"
	"time"
)

type ProxyStatus struct {
	ProxyName         string                  `json:"proxy_name"`
	GeneratedAt       string                  `json:"generated_at"`
	Policy            PolicyConfig            `json:"policy"`
	State             RuntimeState            `json:"state"`
	SelectedAccountID string                  `json:"selected_account_id,omitempty"`
	SelectedProxyURL  string                  `json:"selected_proxy_url,omitempty"`
	SelectedProxyName string                  `json:"selected_proxy_name,omitempty"`
	SelectionReason   string                  `json:"selection_reason,omitempty"`
	Accounts          []AccountStatus         `json:"accounts"`
	AdditionalLimits  []AdditionalLimitStatus `json:"additional_limits,omitempty"`
	ChildProxies      []ChildProxyStatus      `json:"child_proxies,omitempty"`
}

type AccountStatus struct {
	ProxyName        string                  `json:"proxy_name,omitempty"`
	Alias            string                  `json:"alias"`
	ID               string                  `json:"id"`
	Email            string                  `json:"email,omitempty"`
	Active           bool                    `json:"active"`
	Pinned           bool                    `json:"pinned"`
	Healthy          bool                    `json:"healthy"`
	Enabled          bool                    `json:"enabled"`
	DisabledReason   string                  `json:"disabled_reason,omitempty"`
	CooldownSeconds  int64                   `json:"cooldown_seconds"`
	DailyLeftPct     float64                 `json:"daily_left_pct"`
	DailyResetAt     int64                   `json:"daily_reset_at"`
	WeeklyLeftPct    float64                 `json:"weekly_left_pct"`
	WeeklyResetAt    int64                   `json:"weekly_reset_at"`
	AdditionalLimits []AdditionalLimitStatus `json:"additional_limits,omitempty"`
	Score            float64                 `json:"score"`
	LastUsedAtMS     int64                   `json:"last_used_at_ms"`
	LastSwitchReason string                  `json:"last_switch_reason,omitempty"`
	QuotaSource      string                  `json:"quota_source,omitempty"`
}

type AdditionalLimitStatus struct {
	LimitID                string  `json:"limit_id"`
	LimitName              string  `json:"limit_name,omitempty"`
	PrimaryLeftPct         float64 `json:"primary_left_pct"`
	PrimaryResetAt         int64   `json:"primary_reset_at"`
	PrimaryWindowSeconds   int64   `json:"primary_window_seconds"`
	SecondaryLeftPct       float64 `json:"secondary_left_pct"`
	SecondaryResetAt       int64   `json:"secondary_reset_at"`
	SecondaryWindowSeconds int64   `json:"secondary_window_seconds"`
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
			AdditionalLimits: quotaAdditionalStatuses(a.Quota.AdditionalLimits),
			Score:            score(a, sf.Settings.Policy),
			LastUsedAtMS:     a.LastUsedAtMS,
			LastSwitchReason: a.LastSwitchReason,
			QuotaSource:      a.Quota.Source,
		})
	}

	sortAccountStatuses(status.Accounts)
	status.AdditionalLimits = aggregateAdditionalLimitStatuses(status.Accounts)

	return status
}

func quotaAdditionalStatuses(limits []AdditionalQuotaState) []AdditionalLimitStatus {
	if len(limits) == 0 {
		return nil
	}
	out := make([]AdditionalLimitStatus, 0, len(limits))
	for _, limit := range limits {
		out = append(out, AdditionalLimitStatus{
			LimitID:                strings.TrimSpace(limit.LimitID),
			LimitName:              strings.TrimSpace(limit.LimitName),
			PrimaryLeftPct:         quotaLeftPercent(limit.PrimaryLimit, limit.PrimaryUsed),
			PrimaryResetAt:         limit.PrimaryResetAt,
			PrimaryWindowSeconds:   limit.PrimaryWindowSeconds,
			SecondaryLeftPct:       quotaLeftPercent(limit.SecondaryLimit, limit.SecondaryUsed),
			SecondaryResetAt:       limit.SecondaryResetAt,
			SecondaryWindowSeconds: limit.SecondaryWindowSeconds,
		})
	}
	sortAdditionalLimitStatuses(out)
	return out
}

func aggregateAdditionalLimitStatuses(accounts []AccountStatus) []AdditionalLimitStatus {
	type aggregate struct {
		limitID                string
		limitName              string
		primaryTotal           float64
		primaryCount           int
		primaryResetAt         int64
		primaryWindowSeconds   int64
		secondaryTotal         float64
		secondaryCount         int
		secondaryResetAt       int64
		secondaryWindowSeconds int64
	}
	byID := map[string]*aggregate{}
	for _, account := range accounts {
		for _, limit := range account.AdditionalLimits {
			id := strings.TrimSpace(limit.LimitID)
			if id == "" {
				continue
			}
			entry := byID[id]
			if entry == nil {
				entry = &aggregate{limitID: id, limitName: strings.TrimSpace(limit.LimitName)}
				byID[id] = entry
			}
			if entry.limitName == "" {
				entry.limitName = strings.TrimSpace(limit.LimitName)
			}
			if limit.PrimaryLeftPct >= 0 {
				entry.primaryTotal += limit.PrimaryLeftPct
				entry.primaryCount++
				entry.primaryWindowSeconds = limit.PrimaryWindowSeconds
				if limit.PrimaryResetAt > 0 && (entry.primaryResetAt == 0 || limit.PrimaryResetAt < entry.primaryResetAt) {
					entry.primaryResetAt = limit.PrimaryResetAt
				}
			}
			if limit.SecondaryLeftPct >= 0 {
				entry.secondaryTotal += limit.SecondaryLeftPct
				entry.secondaryCount++
				entry.secondaryWindowSeconds = limit.SecondaryWindowSeconds
				if limit.SecondaryResetAt > 0 && (entry.secondaryResetAt == 0 || limit.SecondaryResetAt < entry.secondaryResetAt) {
					entry.secondaryResetAt = limit.SecondaryResetAt
				}
			}
		}
	}
	if len(byID) == 0 {
		return nil
	}
	out := make([]AdditionalLimitStatus, 0, len(byID))
	for _, entry := range byID {
		primaryLeft := -1.0
		if entry.primaryCount > 0 {
			primaryLeft = entry.primaryTotal / float64(entry.primaryCount)
		}
		secondaryLeft := -1.0
		if entry.secondaryCount > 0 {
			secondaryLeft = entry.secondaryTotal / float64(entry.secondaryCount)
		}
		out = append(out, AdditionalLimitStatus{
			LimitID:                entry.limitID,
			LimitName:              entry.limitName,
			PrimaryLeftPct:         primaryLeft,
			PrimaryResetAt:         entry.primaryResetAt,
			PrimaryWindowSeconds:   entry.primaryWindowSeconds,
			SecondaryLeftPct:       secondaryLeft,
			SecondaryResetAt:       entry.secondaryResetAt,
			SecondaryWindowSeconds: entry.secondaryWindowSeconds,
		})
	}
	sortAdditionalLimitStatuses(out)
	return out
}

func quotaLeftPercent(limit, used float64) float64 {
	if limit <= 0 {
		return -1
	}
	return clamp01((limit-used)/limit) * 100
}

func sortAdditionalLimitStatuses(limits []AdditionalLimitStatus) {
	sort.Slice(limits, func(i, j int) bool {
		if limits[i].LimitID != limits[j].LimitID {
			return limits[i].LimitID < limits[j].LimitID
		}
		return limits[i].LimitName < limits[j].LimitName
	})
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
