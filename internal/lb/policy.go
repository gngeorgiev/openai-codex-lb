package lb

import (
	"fmt"
	"math"
)

type Selection struct {
	Index        int
	Score        float64
	BestScore    float64
	Switched     bool
	SwitchReason string
}

func selectAccount(sf *StoreFile, nowMS int64) (Selection, error) {
	healthy := healthyIndexes(sf.Accounts, nowMS)
	if len(healthy) == 0 {
		return Selection{Index: -1, SwitchReason: "none-healthy"}, fmt.Errorf("no healthy accounts")
	}
	eligible := eligibleIndexes(sf.Accounts, healthy)
	if len(eligible) > 0 {
		healthy = eligible
	}

	if sf.State.PinnedAccountID != "" {
		for _, idx := range healthy {
			if sf.Accounts[idx].ID == sf.State.PinnedAccountID {
				s := score(sf.Accounts[idx], sf.Settings.Policy)
				return Selection{Index: idx, Score: s, BestScore: s, Switched: false, SwitchReason: "pinned"}, nil
			}
		}
	}

	active := sf.State.ActiveIndex
	if active < 0 || active >= len(sf.Accounts) {
		active = healthy[0]
	}

	switch sf.Settings.Policy.Mode {
	case PolicySticky:
		if contains(healthy, active) {
			s := score(sf.Accounts[active], sf.Settings.Policy)
			return Selection{Index: active, Score: s, BestScore: s, Switched: false, SwitchReason: "sticky"}, nil
		}
		idx := healthy[0]
		s := score(sf.Accounts[idx], sf.Settings.Policy)
		return Selection{Index: idx, Score: s, BestScore: s, Switched: true, SwitchReason: "sticky-fallback"}, nil
	case PolicyRoundRobin:
		if !contains(healthy, active) {
			idx := healthy[0]
			s := score(sf.Accounts[idx], sf.Settings.Policy)
			return Selection{Index: idx, Score: s, BestScore: s, Switched: true, SwitchReason: "rr-fallback"}, nil
		}
		idx := nextHealthy(healthy, active)
		s := score(sf.Accounts[idx], sf.Settings.Policy)
		if idx == active {
			return Selection{Index: idx, Score: s, BestScore: s, Switched: false, SwitchReason: "rr-stay"}, nil
		}
		return Selection{Index: idx, Score: s, BestScore: s, Switched: true, SwitchReason: "rr"}, nil
	default:
		bestIdx := healthy[0]
		bestScore := score(sf.Accounts[bestIdx], sf.Settings.Policy)
		for _, idx := range healthy[1:] {
			s := score(sf.Accounts[idx], sf.Settings.Policy)
			if s > bestScore {
				bestScore = s
				bestIdx = idx
			}
		}

		if !contains(healthy, active) {
			return Selection{Index: bestIdx, Score: bestScore, BestScore: bestScore, Switched: true, SwitchReason: "usage-fallback"}, nil
		}

		curScore := score(sf.Accounts[active], sf.Settings.Policy)
		delta := math.Max(0, sf.Settings.Policy.DeltaPercent) / 100.0
		if curScore < bestScore-delta {
			return Selection{Index: bestIdx, Score: curScore, BestScore: bestScore, Switched: true, SwitchReason: "usage-hysteresis"}, nil
		}
		return Selection{Index: active, Score: curScore, BestScore: bestScore, Switched: false, SwitchReason: "usage-stay"}, nil
	}
}

func healthyIndexes(accounts []Account, nowMS int64) []int {
	out := make([]int, 0, len(accounts))
	for i, account := range accounts {
		if !account.Enabled {
			continue
		}
		if account.DisabledReason != "" {
			continue
		}
		if account.CooldownUntilMS > nowMS {
			continue
		}
		out = append(out, i)
	}
	return out
}

func eligibleIndexes(accounts []Account, candidates []int) []int {
	out := make([]int, 0, len(candidates))
	for _, idx := range candidates {
		if idx < 0 || idx >= len(accounts) {
			continue
		}
		if accountQuotaExhausted(accounts[idx]) {
			continue
		}
		out = append(out, idx)
	}
	return out
}

func accountQuotaExhausted(account Account) bool {
	if account.Quota.DailyLimit > 0 && account.Quota.DailyUsed >= account.Quota.DailyLimit {
		return true
	}
	if account.Quota.WeeklyLimit > 0 && account.Quota.WeeklyUsed >= account.Quota.WeeklyLimit {
		return true
	}
	return false
}

func score(account Account, policy PolicyConfig) float64 {
	const unknown = 0.30

	daily := unknown
	if account.Quota.DailyLimit > 0 {
		daily = clamp01((account.Quota.DailyLimit - account.Quota.DailyUsed) / account.Quota.DailyLimit)
	}
	weekly := unknown
	if account.Quota.WeeklyLimit > 0 {
		weekly = clamp01((account.Quota.WeeklyLimit - account.Quota.WeeklyUsed) / account.Quota.WeeklyLimit)
	}

	dailyWeight := math.Max(0, policy.Weights.Daily)
	weeklyWeight := math.Max(0, policy.Weights.Weekly)
	total := dailyWeight + weeklyWeight
	if total <= 0 {
		return 0
	}
	return (dailyWeight/total)*daily + (weeklyWeight/total)*weekly
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

func contains(values []int, target int) bool {
	for _, v := range values {
		if v == target {
			return true
		}
	}
	return false
}

func nextHealthy(healthy []int, current int) int {
	if len(healthy) == 0 {
		return -1
	}
	at := -1
	for i, idx := range healthy {
		if idx == current {
			at = i
			break
		}
	}
	if at < 0 {
		return healthy[0]
	}
	return healthy[(at+1)%len(healthy)]
}
