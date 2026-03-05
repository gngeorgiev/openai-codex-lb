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
	snapshot := store.Snapshot()
	baseLogin := sanitizeCommand(snapshot.Settings.Commands.Login)
	if len(baseLogin) == 0 {
		baseLogin = []string{"login"}
	}
	args := append(append([]string(nil), baseLogin...), loginArgs...)
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
	codexBin, args, codexHome, env := resolveCodexInvocation(store, codexBin, proxyURL, codexHome, args)
	if err := os.MkdirAll(codexHome, 0o700); err != nil {
		return 1, fmt.Errorf("create runtime CODEX_HOME: %w", err)
	}
	if err := seedRuntimeAuthIfMissing(store, codexHome); err != nil {
		return 1, err
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

func FormatRunCodexCommand(store *Store, codexBin, proxyURL, codexHome string, args []string) string {
	codexBin, args, _, env := resolveCodexInvocation(store, codexBin, proxyURL, codexHome, args)
	return formatShellCommand(codexBin, args, env)
}

func resolveCodexInvocation(store *Store, codexBin, proxyURL, codexHome string, args []string) (string, []string, string, map[string]string) {
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
	runPrefix := sanitizeCommand(snapshot.Settings.Commands.Run)
	fullArgs := make([]string, 0, len(runPrefix)+len(args))
	fullArgs = append(fullArgs, runPrefix...)
	fullArgs = append(fullArgs, args...)

	env := map[string]string{
		"OPENAI_BASE_URL": proxyURL,
		"CODEX_HOME":      codexHome,
	}
	if os.Getenv("OPENAI_API_KEY") == "" {
		env["OPENAI_API_KEY"] = "codex-lb-local-key"
	}
	return codexBin, fullArgs, codexHome, env
}

func seedRuntimeAuthIfMissing(store *Store, codexHome string) error {
	targetAuth := filepath.Join(codexHome, "auth.json")
	if _, err := os.Stat(targetAuth); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat runtime auth.json: %w", err)
	}

	snapshot := store.Snapshot()
	if len(snapshot.Accounts) == 0 {
		return nil
	}

	candidates := make([]int, 0, len(snapshot.Accounts))
	if snapshot.State.ActiveIndex >= 0 && snapshot.State.ActiveIndex < len(snapshot.Accounts) {
		candidates = append(candidates, snapshot.State.ActiveIndex)
	}
	for i := range snapshot.Accounts {
		if i == snapshot.State.ActiveIndex {
			continue
		}
		candidates = append(candidates, i)
	}

	for _, idx := range candidates {
		home := snapshot.Accounts[idx].HomeDir
		sourceAuth := filepath.Join(home, "auth.json")
		if _, err := os.Stat(sourceAuth); err != nil {
			continue
		}
		if err := copyFile(sourceAuth, targetAuth, 0o600); err != nil {
			return fmt.Errorf("seed runtime auth.json from account %s: %w", snapshot.Accounts[idx].Alias, err)
		}
		sourceConfig := filepath.Join(home, "config.toml")
		targetConfig := filepath.Join(codexHome, "config.toml")
		if _, err := os.Stat(sourceConfig); err == nil {
			_ = copyFile(sourceConfig, targetConfig, 0o600)
		}
		return nil
	}
	return nil
}

func formatShellCommand(bin string, args []string, env map[string]string) string {
	keys := make([]string, 0, len(env))
	for key := range env {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	parts := make([]string, 0, len(keys)+1+len(args))
	for _, key := range keys {
		parts = append(parts, key+"="+shellQuote(env[key]))
	}
	parts = append(parts, shellQuote(bin))
	for _, arg := range args {
		parts = append(parts, shellQuote(arg))
	}
	return strings.Join(parts, " ")
}

func shellQuote(value string) string {
	if value == "" {
		return "''"
	}
	if isSafeShellWord(value) {
		return value
	}
	return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'"
}

func isSafeShellWord(value string) bool {
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case strings.ContainsRune("_-./:@%+=,", r):
		default:
			return false
		}
	}
	return true
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
