package lb

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"
)

type Store struct {
	root string
	path string

	mu   sync.Mutex
	data StoreFile
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
	return cloneStore(s.data)
}

func (s *Store) Update(fn func(*StoreFile) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := fn(&s.data); err != nil {
		return err
	}
	s.data.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	return writeJSONAtomic(s.path, s.data)
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

	settings, err := loadOrCreateSettingsConfig(s.root, s.data.Settings)
	if err != nil {
		return err
	}
	s.data.Settings = settings

	return writeJSONAtomic(s.path, s.data)
}

func (s *Store) PersistSettingsToConfig() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return WriteSettingsConfig(s.root, s.data.Settings)
}

func (s *Store) ReloadSettingsFromConfig() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	settings, err := loadOrCreateSettingsConfig(s.root, s.data.Settings)
	if err != nil {
		return err
	}
	s.data.Settings = settings
	return writeJSONAtomic(s.path, s.data)
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
	return out
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
