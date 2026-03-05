package lb

import (
	"strings"
	"time"
)

type PolicyMode string

const (
	PolicyUsageBalanced PolicyMode = "usage_balanced"
	PolicyRoundRobin    PolicyMode = "round_robin"
	PolicySticky        PolicyMode = "sticky"
)

type ProxyConfig struct {
	Listen           string `json:"listen"`
	UpstreamBaseURL  string `json:"upstream_base_url"`
	MaxAttempts      int    `json:"max_attempts"`
	UsageTimeoutMS   int    `json:"usage_timeout_ms"`
	CooldownDefaultS int    `json:"cooldown_default_seconds"`
}

type PolicyWeights struct {
	Daily  float64 `json:"daily"`
	Weekly float64 `json:"weekly"`
}

type PolicyConfig struct {
	Mode         PolicyMode    `json:"mode"`
	DeltaPercent float64       `json:"delta_percent"`
	Weights      PolicyWeights `json:"weights"`
}

type QuotaConfig struct {
	RefreshIntervalMinutes  int `json:"refresh_interval_minutes"`
	RefreshIntervalMessages int `json:"refresh_interval_messages"`
	CacheTTLMinutes         int `json:"cache_ttl_minutes"`
}

type CommandConfig struct {
	Login []string `json:"login"`
	Run   []string `json:"run"`
}

type RunConfig struct {
	InheritShell bool `json:"inherit_shell"`
}

type Settings struct {
	Proxy    ProxyConfig   `json:"proxy"`
	ProxyURL string        `json:"proxy_url"`
	Policy   PolicyConfig  `json:"policy"`
	Quota    QuotaConfig   `json:"quota"`
	Commands CommandConfig `json:"commands"`
	Run      RunConfig     `json:"run"`
}

type QuotaState struct {
	DailyLimit             float64 `json:"daily_limit"`
	DailyUsed              float64 `json:"daily_used"`
	DailyResetAt           int64   `json:"daily_reset_at"`
	WeeklyLimit            float64 `json:"weekly_limit"`
	WeeklyUsed             float64 `json:"weekly_used"`
	WeeklyResetAt          int64   `json:"weekly_reset_at"`
	LastSyncAt             int64   `json:"last_sync_at"`
	LastSyncMessageCounter int64   `json:"last_sync_message_counter"`
	Source                 string  `json:"source"`
}

type Account struct {
	ID               string     `json:"id"`
	Alias            string     `json:"alias"`
	HomeDir          string     `json:"home_dir"`
	BaseURL          string     `json:"base_url"`
	Enabled          bool       `json:"enabled"`
	DisabledReason   string     `json:"disabled_reason,omitempty"`
	CooldownUntilMS  int64      `json:"cooldown_until_ms"`
	LastUsedAtMS     int64      `json:"last_used_at_ms"`
	LastSwitchReason string     `json:"last_switch_reason,omitempty"`
	ChatGPTAccountID string     `json:"chatgpt_account_id,omitempty"`
	UserEmail        string     `json:"user_email,omitempty"`
	Quota            QuotaState `json:"quota"`
}

type RuntimeState struct {
	ActiveIndex      int    `json:"active_index"`
	MessageCounter   int64  `json:"message_counter"`
	LastRotationAtMS int64  `json:"last_rotation_at_ms"`
	PinnedAccountID  string `json:"pinned_account_id,omitempty"`
}

type StoreFile struct {
	Version   int          `json:"version"`
	UpdatedAt string       `json:"updated_at"`
	Settings  Settings     `json:"settings,omitempty"`
	State     RuntimeState `json:"state"`
	Accounts  []Account    `json:"accounts"`
}

func defaultStore() StoreFile {
	return StoreFile{
		Version:   1,
		UpdatedAt: time.Now().UTC().Format(time.RFC3339),
		Settings: Settings{
			Proxy: ProxyConfig{
				Listen:           "127.0.0.1:8765",
				UpstreamBaseURL:  "https://chatgpt.com/backend-api",
				MaxAttempts:      3,
				UsageTimeoutMS:   15000,
				CooldownDefaultS: 5,
			},
			ProxyURL: "",
			Policy: PolicyConfig{
				Mode:         PolicyUsageBalanced,
				DeltaPercent: 10,
				Weights: PolicyWeights{
					Daily:  60,
					Weekly: 40,
				},
			},
			Quota: QuotaConfig{
				RefreshIntervalMinutes:  10,
				RefreshIntervalMessages: 10,
				CacheTTLMinutes:         30,
			},
			Commands: CommandConfig{
				Login: []string{"login"},
				Run:   []string{},
			},
			Run: RunConfig{
				InheritShell: true,
			},
		},
		State:    RuntimeState{ActiveIndex: 0},
		Accounts: []Account{},
	}
}

func mergeDefaults(in StoreFile) StoreFile {
	def := defaultStore()
	out := in
	if out.Version == 0 {
		out.Version = def.Version
	}
	if out.UpdatedAt == "" {
		out.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	}

	if out.Settings.Proxy.Listen == "" {
		out.Settings.Proxy.Listen = def.Settings.Proxy.Listen
	}
	if out.Settings.Proxy.UpstreamBaseURL == "" {
		out.Settings.Proxy.UpstreamBaseURL = def.Settings.Proxy.UpstreamBaseURL
	}
	if out.Settings.Proxy.MaxAttempts <= 0 {
		out.Settings.Proxy.MaxAttempts = def.Settings.Proxy.MaxAttempts
	}
	if out.Settings.Proxy.UsageTimeoutMS <= 0 {
		out.Settings.Proxy.UsageTimeoutMS = def.Settings.Proxy.UsageTimeoutMS
	}
	if out.Settings.Proxy.CooldownDefaultS <= 0 {
		out.Settings.Proxy.CooldownDefaultS = def.Settings.Proxy.CooldownDefaultS
	}

	if out.Settings.Policy.Mode == "" {
		out.Settings.Policy.Mode = def.Settings.Policy.Mode
	}
	if out.Settings.Policy.DeltaPercent < 0 {
		out.Settings.Policy.DeltaPercent = def.Settings.Policy.DeltaPercent
	}
	if out.Settings.Policy.Weights.Daily < 0 {
		out.Settings.Policy.Weights.Daily = 0
	}
	if out.Settings.Policy.Weights.Weekly < 0 {
		out.Settings.Policy.Weights.Weekly = 0
	}
	if out.Settings.Policy.Weights.Daily == 0 && out.Settings.Policy.Weights.Weekly == 0 {
		out.Settings.Policy.Weights = def.Settings.Policy.Weights
	}

	if out.Settings.Quota.RefreshIntervalMinutes <= 0 {
		out.Settings.Quota.RefreshIntervalMinutes = def.Settings.Quota.RefreshIntervalMinutes
	}
	if out.Settings.Quota.RefreshIntervalMessages <= 0 {
		out.Settings.Quota.RefreshIntervalMessages = def.Settings.Quota.RefreshIntervalMessages
	}
	if out.Settings.Quota.CacheTTLMinutes <= 0 {
		out.Settings.Quota.CacheTTLMinutes = def.Settings.Quota.CacheTTLMinutes
	}
	if len(out.Settings.Commands.Login) == 0 {
		out.Settings.Commands.Login = append([]string(nil), def.Settings.Commands.Login...)
	} else {
		out.Settings.Commands.Login = sanitizeCommand(out.Settings.Commands.Login)
		if len(out.Settings.Commands.Login) == 0 {
			out.Settings.Commands.Login = append([]string(nil), def.Settings.Commands.Login...)
		}
	}
	out.Settings.Commands.Run = sanitizeCommand(out.Settings.Commands.Run)
	out.Settings.ProxyURL = strings.TrimSpace(out.Settings.ProxyURL)

	for i := range out.Accounts {
		if out.Accounts[i].BaseURL == "" {
			out.Accounts[i].BaseURL = out.Settings.Proxy.UpstreamBaseURL
		}
		if !out.Accounts[i].Enabled && out.Accounts[i].DisabledReason == "" {
			// Keep disabled state if explicitly disabled in older versions.
			out.Accounts[i].Enabled = true
		}
	}
	return out
}

func sanitizeCommand(parts []string) []string {
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		out = append(out, part)
	}
	return out
}
