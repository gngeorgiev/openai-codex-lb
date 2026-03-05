package lb

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

var aliasRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

func ValidateAlias(alias string) error {
	if !aliasRe.MatchString(alias) {
		return fmt.Errorf("invalid alias %q: must match %s", alias, aliasRe.String())
	}
	return nil
}

func AccountHomeDir(store *Store, alias string) string {
	return filepath.Join(store.AccountsDir(), alias)
}

func LoginAccount(store *Store, alias, codexBin string, loginArgs []string) error {
	if err := ValidateAlias(alias); err != nil {
		return err
	}
	home := AccountHomeDir(store, alias)
	if err := os.MkdirAll(home, 0o700); err != nil {
		return fmt.Errorf("create account home: %w", err)
	}
	if codexBin == "" {
		codexBin = "codex"
	}
	args := []string{"login"}
	args = append(args, loginArgs...)
	cmd := exec.Command(codexBin, args...)
	cmd.Env = withEnv(os.Environ(), map[string]string{"CODEX_HOME": home})
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("run %s %s: %w", codexBin, strings.Join(args, " "), err)
	}
	return RegisterAccount(store, alias, home)
}

func ImportAccount(store *Store, alias, fromHome string) error {
	if err := ValidateAlias(alias); err != nil {
		return err
	}
	if fromHome == "" {
		return fmt.Errorf("source home is required")
	}
	srcAuth := filepath.Join(fromHome, "auth.json")
	if _, err := os.Stat(srcAuth); err != nil {
		return fmt.Errorf("source auth missing at %s: %w", srcAuth, err)
	}
	home := AccountHomeDir(store, alias)
	if err := os.MkdirAll(home, 0o700); err != nil {
		return fmt.Errorf("create account home: %w", err)
	}
	if err := copyFile(srcAuth, filepath.Join(home, "auth.json"), 0o600); err != nil {
		return err
	}
	for _, opt := range []string{"config.toml"} {
		src := filepath.Join(fromHome, opt)
		if _, err := os.Stat(src); err == nil {
			_ = copyFile(src, filepath.Join(home, opt), 0o600)
		}
	}
	return RegisterAccount(store, alias, home)
}

func RegisterAccount(store *Store, alias, home string) error {
	auth, err := LoadAuth(home)
	if err != nil {
		return err
	}
	snapshot := store.Snapshot()
	accountID := fmt.Sprintf("openai:%s", strings.ToLower(alias))
	base := snapshot.Settings.Proxy.UpstreamBaseURL
	if base == "" {
		base = "https://chatgpt.com/backend-api"
	}
	return store.UpsertAccount(Account{
		ID:               accountID,
		Alias:            alias,
		HomeDir:          home,
		BaseURL:          base,
		Enabled:          true,
		DisabledReason:   "",
		ChatGPTAccountID: auth.ChatGPTAccountID,
		UserEmail:        auth.UserEmail,
	})
}

func ListAccounts(store *Store) []Account {
	snapshot := store.Snapshot()
	accounts := append([]Account(nil), snapshot.Accounts...)
	sort.Slice(accounts, func(i, j int) bool {
		return accounts[i].Alias < accounts[j].Alias
	})
	return accounts
}

func RemoveAccount(store *Store, alias string) error {
	if alias == "" {
		return fmt.Errorf("alias is required")
	}
	home := AccountHomeDir(store, alias)
	_ = os.RemoveAll(home)
	return store.RemoveAccountByAlias(alias)
}

func RunCodex(store *Store, codexBin, proxyURL, codexHome string, args []string) (int, error) {
	if codexBin == "" {
		codexBin = "codex"
	}
	snapshot := store.Snapshot()
	if proxyURL == "" {
		proxyURL = "http://" + snapshot.Settings.Proxy.Listen
	}
	if codexHome == "" {
		codexHome = store.RuntimeDir()
	}
	if err := os.MkdirAll(codexHome, 0o700); err != nil {
		return 1, fmt.Errorf("create runtime CODEX_HOME: %w", err)
	}

	env := map[string]string{
		"OPENAI_BASE_URL": proxyURL,
		"CODEX_HOME":      codexHome,
	}
	if os.Getenv("OPENAI_API_KEY") == "" {
		env["OPENAI_API_KEY"] = "codex-lb-local-key"
	}
	cmd := exec.Command(codexBin, args...)
	cmd.Env = withEnv(os.Environ(), env)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err := cmd.Run()
	if err == nil {
		return 0, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), nil
	}
	return 1, err
}

func withEnv(base []string, updates map[string]string) []string {
	out := append([]string(nil), base...)
	for key, value := range updates {
		prefix := key + "="
		replaced := false
		for i := range out {
			if strings.HasPrefix(out[i], prefix) {
				out[i] = prefix + value
				replaced = true
				break
			}
		}
		if !replaced {
			out = append(out, prefix+value)
		}
	}
	return out
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open %s: %w", src, err)
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return fmt.Errorf("open %s: %w", dst, err)
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return fmt.Errorf("copy %s -> %s: %w", src, dst, err)
	}
	return out.Close()
}
