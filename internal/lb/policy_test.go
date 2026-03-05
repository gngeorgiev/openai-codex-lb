package lb

import "testing"

func TestSelectAccountUsageBalancedSwitchesToHigherScore(t *testing.T) {
	t.Parallel()
	sf := defaultStore()
	sf.Settings.Policy.Mode = PolicyUsageBalanced
	sf.Settings.Policy.DeltaPercent = 10
	sf.Accounts = []Account{
		{
			ID:      "a",
			Enabled: true,
			Quota: QuotaState{
				DailyLimit:  100,
				DailyUsed:   90,
				WeeklyLimit: 100,
				WeeklyUsed:  90,
			},
		},
		{
			ID:      "b",
			Enabled: true,
			Quota: QuotaState{
				DailyLimit:  100,
				DailyUsed:   10,
				WeeklyLimit: 100,
				WeeklyUsed:  20,
			},
		},
	}
	sf.State.ActiveIndex = 0

	sel, err := selectAccount(&sf, 0)
	if err != nil {
		t.Fatalf("selectAccount: %v", err)
	}
	if sel.Index != 1 {
		t.Fatalf("expected account index 1, got %d", sel.Index)
	}
	if !sel.Switched {
		t.Fatalf("expected switched=true")
	}
}

func TestSelectAccountStickyFallsBackWhenActiveCoolingDown(t *testing.T) {
	t.Parallel()
	sf := defaultStore()
	sf.Settings.Policy.Mode = PolicySticky
	sf.Accounts = []Account{
		{ID: "a", Enabled: true, CooldownUntilMS: 1000},
		{ID: "b", Enabled: true},
	}
	sf.State.ActiveIndex = 0

	sel, err := selectAccount(&sf, 10)
	if err != nil {
		t.Fatalf("selectAccount: %v", err)
	}
	if sel.Index != 1 {
		t.Fatalf("expected fallback to 1, got %d", sel.Index)
	}
	if sel.SwitchReason != "sticky-fallback" {
		t.Fatalf("unexpected reason: %s", sel.SwitchReason)
	}
}
