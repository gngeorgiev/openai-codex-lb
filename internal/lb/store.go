package lb

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

type Store struct {
	root string
	path string

	mu               sync.Mutex
	data             StoreFile
	runtimeOverrides RuntimeSettingsOverrides
}

type RuntimeSettingsOverrides struct {
	ProxyName               *string
	Listen                  *string
	UpstreamBaseURL         *string
	ChildProxyURLs          *[]string
	MaxAttempts             *int
	UsageTimeoutMS          *int
	CooldownDefaultSeconds  *int
	RefreshIntervalMinutes  *int
	RefreshIntervalMessages *int
	CacheTTLMinutes         *int
}

type SettingsReloadSummary struct {
	Changed               bool
	UpstreamChanged       bool
	ListenChangeIgnored   bool
	Previous              Settings
	Current               Settings
	UpdatedAccountBaseURL int
}

type storeFilePersisted struct {
	Version   int          `json:"version"`
	UpdatedAt string       `json:"updated_at"`
	State     RuntimeState `json:"state"`
	Accounts  []Account    `json:"accounts"`
}

func DefaultRootDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home dir: %w", err)
	}
	return filepath.Join(home, ".codex-lb"), nil
}

func OpenStore(root string) (*Store, error) {
	if root == "" {
		var err error
		root, err = DefaultRootDir()
		if err != nil {
			return nil, err
		}
	}

	if err := os.MkdirAll(filepath.Join(root, "accounts"), 0o700); err != nil {
		return nil, fmt.Errorf("create accounts dir: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(root, "runtime"), 0o700); err != nil {
		return nil, fmt.Errorf("create runtime dir: %w", err)
	}

	path := filepath.Join(root, "store.json")
	st := &Store{root: root, path: path}

	if err := st.loadOrInit(); err != nil {
		return nil, err
	}
	return st, nil
}

func (s *Store) RootDir() string {
	return s.root
}

func (s *Store) AccountsDir() string {
	return filepath.Join(s.root, "accounts")
}

func (s *Store) RuntimeDir() string {
	return filepath.Join(s.root, "runtime")
}

func (s *Store) Snapshot() StoreFile {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := cloneStore(s.data)
	applyRuntimeOverrides(&out, s.runtimeOverrides)
	return out
}

func (s *Store) SetRuntimeSettingsOverrides(overrides RuntimeSettingsOverrides) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.runtimeOverrides = overrides
}

func (s *Store) Update(fn func(*StoreFile) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := fn(&s.data); err != nil {
		return err
	}
	s.data.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	return writeJSONAtomic(s.path, storeFileForPersistence(s.data))
}

func (s *Store) loadOrInit() error {
	bytes, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		s.data = defaultStore()
	} else {
		if err != nil {
			return fmt.Errorf("read store file: %w", err)
		}

		var sf StoreFile
		if err := json.Unmarshal(bytes, &sf); err != nil {
			return fmt.Errorf("parse store file %s: %w", s.path, err)
		}
		s.data = mergeDefaults(sf)
	}

	prev := s.data.Settings
	settings, err := loadOrCreateSettingsConfig(s.root)
	if err != nil {
		return err
	}
	if settings.Proxy.UpstreamBaseURL != prev.Proxy.UpstreamBaseURL {
		for i := range s.data.Accounts {
			if s.data.Accounts[i].BaseURL == "" || s.data.Accounts[i].BaseURL == prev.Proxy.UpstreamBaseURL {
				s.data.Accounts[i].BaseURL = settings.Proxy.UpstreamBaseURL
			}
		}
	}
	s.data.Settings = settings
	if err := s.reconcileAccountsFromDisk(); err != nil {
		return err
	}

	return writeJSONAtomic(s.path, storeFileForPersistence(s.data))
}

func (s *Store) PersistSettingsToConfig() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return WriteSettingsConfig(s.root, s.data.Settings)
}

func (s *Store) ReloadSettingsFromConfig() (SettingsReloadSummary, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	prev := s.data.Settings
	settings, err := loadOrCreateSettingsConfig(s.root)
	if err != nil {
		return SettingsReloadSummary{}, err
	}
	summary := SettingsReloadSummary{
		Previous: prev,
		Current:  settings,
		Changed:  !settingsEqual(settings, prev),
	}

	// Listen address is bound by the running HTTP server and needs a restart.
	if settings.Proxy.Listen != prev.Proxy.Listen {
		summary.ListenChangeIgnored = true
		settings.Proxy.Listen = prev.Proxy.Listen
		summary.Current.Proxy.Listen = prev.Proxy.Listen
	}
	if settings.Proxy.UpstreamBaseURL != prev.Proxy.UpstreamBaseURL {
		summary.UpstreamChanged = true
		for i := range s.data.Accounts {
			if s.data.Accounts[i].BaseURL == "" || s.data.Accounts[i].BaseURL == prev.Proxy.UpstreamBaseURL {
				s.data.Accounts[i].BaseURL = settings.Proxy.UpstreamBaseURL
				summary.UpdatedAccountBaseURL++
			}
		}
	}

	summary.Changed = summary.Changed || summary.ListenChangeIgnored || summary.UpdatedAccountBaseURL > 0
	s.data.Settings = settings
	if !summary.Changed {
		return summary, nil
	}
	return summary, writeJSONAtomic(s.path, storeFileForPersistence(s.data))
}

func writeJSONAtomic(path string, v any) error {
	tmp := fmt.Sprintf("%s.tmp.%d", path, os.Getpid())
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("serialize json: %w", err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write temp json: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename temp json: %w", err)
	}
	return nil
}

func cloneStore(in StoreFile) StoreFile {
	out := in
	out.Accounts = append([]Account(nil), in.Accounts...)
	for i := range out.Accounts {
		out.Accounts[i].Quota.AdditionalLimits = append([]AdditionalQuotaState(nil), in.Accounts[i].Quota.AdditionalLimits...)
	}
	out.Settings.Proxy.ChildProxyURLs = append([]string(nil), in.Settings.Proxy.ChildProxyURLs...)
	out.Settings.Commands.Login = append([]string(nil), in.Settings.Commands.Login...)
	out.Settings.Commands.Run = append([]string(nil), in.Settings.Commands.Run...)
	return out
}

func settingsEqual(a, b Settings) bool {
	if !proxyConfigEqual(a.Proxy, b.Proxy) || a.Policy != b.Policy || a.Quota != b.Quota || a.Run != b.Run {
		return false
	}
	if !slices.Equal(a.Commands.Login, b.Commands.Login) {
		return false
	}
	if !slices.Equal(a.Commands.Run, b.Commands.Run) {
		return false
	}
	return true
}

func proxyConfigEqual(a, b ProxyConfig) bool {
	if a.Name != b.Name ||
		a.Listen != b.Listen ||
		a.UpstreamBaseURL != b.UpstreamBaseURL ||
		a.MaxAttempts != b.MaxAttempts ||
		a.UsageTimeoutMS != b.UsageTimeoutMS ||
		a.CooldownDefaultS != b.CooldownDefaultS {
		return false
	}
	return slices.Equal(a.ChildProxyURLs, b.ChildProxyURLs)
}

func (s *Store) UpsertAccount(account Account) error {
	return s.Update(func(sf *StoreFile) error {
		idx := slices.IndexFunc(sf.Accounts, func(a Account) bool {
			return a.ID == account.ID || a.Alias == account.Alias
		})
		if idx >= 0 {
			prevQuota := sf.Accounts[idx].Quota
			prevCooldown := sf.Accounts[idx].CooldownUntilMS
			prevLastUsed := sf.Accounts[idx].LastUsedAtMS
			prevReason := sf.Accounts[idx].LastSwitchReason
			sf.Accounts[idx] = account
			sf.Accounts[idx].Quota = prevQuota
			sf.Accounts[idx].CooldownUntilMS = prevCooldown
			sf.Accounts[idx].LastUsedAtMS = prevLastUsed
			sf.Accounts[idx].LastSwitchReason = prevReason
			return nil
		}
		sf.Accounts = append(sf.Accounts, account)
		return nil
	})
}

func (s *Store) RemoveAccountByAlias(alias string) error {
	return s.Update(func(sf *StoreFile) error {
		idx := slices.IndexFunc(sf.Accounts, func(a Account) bool { return a.Alias == alias || a.ID == alias })
		if idx < 0 {
			return fmt.Errorf("account not found: %s", alias)
		}
		removed := sf.Accounts[idx]
		sf.Accounts = append(sf.Accounts[:idx], sf.Accounts[idx+1:]...)
		if sf.State.ActiveIndex >= len(sf.Accounts) {
			sf.State.ActiveIndex = max(0, len(sf.Accounts)-1)
		}
		if sf.State.PinnedAccountID == alias || sf.State.PinnedAccountID == removed.ID {
			sf.State.PinnedAccountID = ""
		}
		return nil
	})
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func applyRuntimeOverrides(sf *StoreFile, overrides RuntimeSettingsOverrides) {
	if overrides.ProxyName != nil {
		sf.Settings.Proxy.Name = strings.TrimSpace(*overrides.ProxyName)
	}
	if overrides.Listen != nil {
		sf.Settings.Proxy.Listen = *overrides.Listen
	}
	if overrides.UpstreamBaseURL != nil {
		prevUpstream := sf.Settings.Proxy.UpstreamBaseURL
		sf.Settings.Proxy.UpstreamBaseURL = *overrides.UpstreamBaseURL
		for i := range sf.Accounts {
			if sf.Accounts[i].BaseURL == "" || sf.Accounts[i].BaseURL == prevUpstream {
				sf.Accounts[i].BaseURL = sf.Settings.Proxy.UpstreamBaseURL
			}
		}
	}
	if overrides.ChildProxyURLs != nil {
		sf.Settings.Proxy.ChildProxyURLs = normalizeProxyURLList(*overrides.ChildProxyURLs)
	}
	if overrides.MaxAttempts != nil {
		sf.Settings.Proxy.MaxAttempts = *overrides.MaxAttempts
	}
	if overrides.UsageTimeoutMS != nil {
		sf.Settings.Proxy.UsageTimeoutMS = *overrides.UsageTimeoutMS
	}
	if overrides.CooldownDefaultSeconds != nil {
		sf.Settings.Proxy.CooldownDefaultS = *overrides.CooldownDefaultSeconds
	}
	if overrides.RefreshIntervalMinutes != nil {
		sf.Settings.Quota.RefreshIntervalMinutes = *overrides.RefreshIntervalMinutes
	}
	if overrides.RefreshIntervalMessages != nil {
		sf.Settings.Quota.RefreshIntervalMessages = *overrides.RefreshIntervalMessages
	}
	if overrides.CacheTTLMinutes != nil {
		sf.Settings.Quota.CacheTTLMinutes = *overrides.CacheTTLMinutes
	}
}

func storeFileForPersistence(in StoreFile) storeFilePersisted {
	out := cloneStore(in)
	return storeFilePersisted{
		Version:   out.Version,
		UpdatedAt: out.UpdatedAt,
		State:     out.State,
		Accounts:  out.Accounts,
	}
}

func (s *Store) reconcileAccountsFromDisk() error {
	entries, err := os.ReadDir(s.AccountsDir())
	if err != nil {
		return fmt.Errorf("read accounts dir: %w", err)
	}

	existingByAlias := make(map[string]Account, len(s.data.Accounts))
	existingByID := make(map[string]Account, len(s.data.Accounts))
	for _, account := range s.data.Accounts {
		existingByAlias[account.Alias] = account
		existingByID[account.ID] = account
	}

	base := s.data.Settings.Proxy.UpstreamBaseURL
	if base == "" {
		base = "https://chatgpt.com/backend-api"
	}

	discovered := make(map[string]Account)
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		alias := entry.Name()
		if err := ValidateAlias(alias); err != nil {
			continue
		}

		home := filepath.Join(s.AccountsDir(), alias)
		auth, err := LoadAuth(home)
		if err != nil {
			continue
		}

		id := fmt.Sprintf("openai:%s", strings.ToLower(alias))
		account, ok := existingByAlias[alias]
		if !ok {
			account, ok = existingByID[id]
		}
		if !ok {
			account = Account{
				ID:             id,
				Alias:          alias,
				HomeDir:        home,
				BaseURL:        base,
				Enabled:        true,
				DisabledReason: "",
			}
		}
		account.ID = id
		account.Alias = alias
		account.HomeDir = home
		if account.BaseURL == "" {
			account.BaseURL = base
		}
		account.ChatGPTAccountID = auth.ChatGPTAccountID
		account.UserEmail = auth.UserEmail
		discovered[alias] = account
	}

	// Preserve existing order for still-present accounts, then append new ones by alias.
	reconciled := make([]Account, 0, len(discovered))
	for _, existing := range s.data.Accounts {
		account, ok := discovered[existing.Alias]
		if !ok {
			continue
		}
		reconciled = append(reconciled, account)
		delete(discovered, existing.Alias)
	}
	newAliases := make([]string, 0, len(discovered))
	for alias := range discovered {
		newAliases = append(newAliases, alias)
	}
	slices.Sort(newAliases)
	for _, alias := range newAliases {
		reconciled = append(reconciled, discovered[alias])
	}

	s.data.Accounts = reconciled
	if s.data.State.ActiveIndex >= len(s.data.Accounts) {
		s.data.State.ActiveIndex = max(0, len(s.data.Accounts)-1)
	}
	if s.data.State.PinnedAccountID != "" {
		found := false
		for _, account := range s.data.Accounts {
			if account.ID == s.data.State.PinnedAccountID {
				found = true
				break
			}
		}
		if !found {
			s.data.State.PinnedAccountID = ""
		}
	}

	return nil
}
