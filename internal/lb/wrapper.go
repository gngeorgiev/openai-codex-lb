package lb

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

var aliasRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._@+-]{0,127}$`)

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
	if err := loginAccountHomeWithIO(store, home, codexBin, loginArgs, os.Stdin, os.Stdout, os.Stderr); err != nil {
		return err
	}
	return RegisterAccount(store, alias, home)
}

func LoginAccountToHome(store *Store, alias, home, codexBin string, loginArgs []string) error {
	if err := ValidateAlias(alias); err != nil {
		return err
	}
	return loginAccountHomeWithIO(store, home, codexBin, loginArgs, os.Stdin, os.Stdout, os.Stderr)
}

func LoginAccountWithIO(store *Store, alias, codexBin string, loginArgs []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if err := ValidateAlias(alias); err != nil {
		return err
	}
	home := AccountHomeDir(store, alias)
	if err := loginAccountHomeWithIO(store, home, codexBin, loginArgs, stdin, stdout, stderr); err != nil {
		return err
	}
	return RegisterAccount(store, alias, home)
}

func loginAccountHomeWithIO(store *Store, home, codexBin string, loginArgs []string, stdin io.Reader, stdout, stderr io.Writer) error {
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
	cmd.Stdin = stdin
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("run %s %s: %w", codexBin, strings.Join(args, " "), err)
	}
	return nil
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
	authData, err := os.ReadFile(srcAuth)
	if err != nil {
		return fmt.Errorf("read %s: %w", srcAuth, err)
	}
	var configData []byte
	srcConfig := filepath.Join(fromHome, "config.toml")
	if _, err := os.Stat(srcConfig); err == nil {
		configData, _ = os.ReadFile(srcConfig)
	}
	return ImportAccountData(store, alias, authData, configData)
}

func ImportAccountData(store *Store, alias string, authData, configData []byte) error {
	if err := ValidateAlias(alias); err != nil {
		return err
	}
	if len(authData) == 0 {
		return fmt.Errorf("source auth is required")
	}
	home := AccountHomeDir(store, alias)
	if err := os.MkdirAll(home, 0o700); err != nil {
		return fmt.Errorf("create account home: %w", err)
	}
	if err := os.WriteFile(filepath.Join(home, "auth.json"), authData, 0o600); err != nil {
		return fmt.Errorf("write auth.json: %w", err)
	}
	if len(configData) > 0 {
		if err := os.WriteFile(filepath.Join(home, "config.toml"), configData, 0o600); err != nil {
			return fmt.Errorf("write config.toml: %w", err)
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
	codexBin, args, codexHome, env, inheritShell := resolveCodexInvocation(store, codexBin, proxyURL, codexHome, args)
	if err := os.MkdirAll(codexHome, 0o700); err != nil {
		return 1, fmt.Errorf("create runtime CODEX_HOME: %w", err)
	}
	if err := seedRuntimeAuthIfMissing(store, codexHome, env["OPENAI_BASE_URL"]); err != nil {
		return 1, err
	}

	cmd := exec.Command(codexBin, args...)
	if inheritShell {
		shell := strings.TrimSpace(os.Getenv("SHELL"))
		if shell == "" {
			shell = "/bin/sh"
		}
		shellCmd := formatShellCommand(codexBin, args, nil)
		cmd = exec.Command(shell, "-lc", shellCmd)
	}
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
	codexBin, args, _, env, _ := resolveCodexInvocation(store, codexBin, proxyURL, codexHome, args)
	return formatShellCommand(codexBin, args, env)
}

func resolveCodexInvocation(store *Store, codexBin, proxyURL, codexHome string, args []string) (string, []string, string, map[string]string, bool) {
	if codexBin == "" {
		codexBin = "codex"
	}
	snapshot := store.Snapshot()
	if proxyURL == "" {
		if snapshot.Settings.ProxyURL != "" {
			proxyURL = snapshot.Settings.ProxyURL
		} else {
			proxyURL = "http://" + snapshot.Settings.Proxy.Listen
		}
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
	return codexBin, fullArgs, codexHome, env, snapshot.Settings.Run.InheritShell
}

func seedRuntimeAuthIfMissing(store *Store, codexHome, proxyURL string) error {
	targetAuth := filepath.Join(codexHome, "auth.json")
	if _, err := os.Stat(targetAuth); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat runtime auth.json: %w", err)
	}

	snapshot := store.Snapshot()
	if len(snapshot.Accounts) > 0 {
		candidates := runtimeAuthCandidateIndexes(snapshot, time.Now().UnixMilli())

		for _, idx := range candidates {
			if err := writeProxyOnlyRuntimeAuth(targetAuth); err != nil {
				return fmt.Errorf("write proxy-only runtime auth.json: %w", err)
			}
			if err := syncRuntimeConfigForAccount(snapshot, idx, codexHome); err != nil {
				return err
			}
			return nil
		}
	}

	if remoteAuth, err := fetchRemoteRuntimeAuth(proxyURL); err == nil {
		if err := writeProxyOnlyRuntimeAuth(targetAuth); err != nil {
			return fmt.Errorf("write proxy-only runtime auth.json: %w", err)
		}
		if err := syncRuntimeConfigFromRemote(remoteAuth, codexHome); err != nil {
			return err
		}
		return nil
	}

	if err := syncDefaultRuntimeConfig(codexHome); err != nil {
		return err
	}
	if err := writeProxyOnlyRuntimeAuth(targetAuth); err != nil {
		return fmt.Errorf("write proxy-only runtime auth.json: %w", err)
	}
	return nil
}

func syncRuntimeConfigForAccount(snapshot StoreFile, accountIdx int, codexHome string) error {
	if accountIdx >= 0 && accountIdx < len(snapshot.Accounts) {
		sourceConfig := filepath.Join(snapshot.Accounts[accountIdx].HomeDir, "config.toml")
		if isRegularFile(sourceConfig) {
			return copyRuntimeConfigFile(sourceConfig, codexHome, fmt.Sprintf("account %s", snapshot.Accounts[accountIdx].Alias))
		}
	}
	return syncDefaultRuntimeConfig(codexHome)
}

func syncRuntimeConfigFromRemote(runtimeAuth AdminRuntimeAuthResponse, codexHome string) error {
	if strings.TrimSpace(runtimeAuth.Config) != "" {
		targetConfig := filepath.Join(codexHome, "config.toml")
		if err := os.WriteFile(targetConfig, []byte(runtimeAuth.Config), 0o600); err != nil {
			sourceDesc := "remote runtime auth"
			if runtimeAuth.SourceAlias != "" {
				sourceDesc = fmt.Sprintf("remote account %s", runtimeAuth.SourceAlias)
			}
			return fmt.Errorf("seed runtime config.toml from %s: %w", sourceDesc, err)
		}
		return nil
	}
	return syncDefaultRuntimeConfig(codexHome)
}

func syncDefaultRuntimeConfig(codexHome string) error {
	if sourceConfig := defaultCodexConfigPath(codexHome); sourceConfig != "" {
		return copyRuntimeConfigFile(sourceConfig, codexHome, "default Codex home")
	}
	return nil
}

func copyRuntimeConfigFile(sourceConfig, codexHome, sourceDesc string) error {
	targetConfig := filepath.Join(codexHome, "config.toml")
	if sameCleanPath(sourceConfig, targetConfig) {
		return nil
	}
	if err := copyFile(sourceConfig, targetConfig, 0o600); err != nil {
		return fmt.Errorf("seed runtime config.toml from %s: %w", sourceDesc, err)
	}
	return nil
}

func defaultCodexConfigPath(codexHome string) string {
	candidates := []string{}
	if envHome := strings.TrimSpace(os.Getenv("CODEX_HOME")); envHome != "" {
		candidates = append(candidates, envHome)
	}
	if home, err := os.UserHomeDir(); err == nil && strings.TrimSpace(home) != "" {
		candidates = append(candidates, filepath.Join(home, ".codex"))
	}

	targetConfig := filepath.Join(codexHome, "config.toml")
	for _, candidateHome := range candidates {
		sourceConfig := filepath.Join(candidateHome, "config.toml")
		if sameCleanPath(sourceConfig, targetConfig) {
			continue
		}
		if isRegularFile(sourceConfig) {
			return sourceConfig
		}
	}
	return ""
}

func isRegularFile(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return info.Mode().IsRegular()
}

func sameCleanPath(a, b string) bool {
	return filepath.Clean(a) == filepath.Clean(b)
}

func fetchRemoteRuntimeAuth(proxyURL string) (AdminRuntimeAuthResponse, error) {
	url := strings.TrimSpace(proxyURL)
	if url == "" {
		return AdminRuntimeAuthResponse{}, fmt.Errorf("empty proxy URL")
	}
	url = strings.TrimRight(url, "/") + "/admin/runtime-auth"

	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return AdminRuntimeAuthResponse{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return AdminRuntimeAuthResponse{}, fmt.Errorf("runtime auth request failed with status=%d", resp.StatusCode)
	}

	var payload AdminRuntimeAuthResponse
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return AdminRuntimeAuthResponse{}, fmt.Errorf("decode runtime auth response: %w", err)
	}
	if len(payload.Auth) == 0 {
		return AdminRuntimeAuthResponse{}, fmt.Errorf("missing auth payload")
	}
	if _, err := normalizeRuntimeAuthPayload(payload.Auth, ""); err != nil {
		return AdminRuntimeAuthResponse{}, fmt.Errorf("runtime auth payload is invalid: %w", err)
	}
	return payload, nil
}

func runtimeAuthCandidateIndexes(snapshot StoreFile, nowMS int64) []int {
	candidates := make([]int, 0, len(snapshot.Accounts))
	seen := make(map[int]struct{}, len(snapshot.Accounts))
	appendUnique := func(idx int) {
		if idx < 0 || idx >= len(snapshot.Accounts) {
			return
		}
		if _, ok := seen[idx]; ok {
			return
		}
		seen[idx] = struct{}{}
		candidates = append(candidates, idx)
	}

	if sel, err := selectAccount(&snapshot, nowMS); err == nil {
		appendUnique(sel.Index)
	}
	appendUnique(snapshot.State.ActiveIndex)
	for i := range snapshot.Accounts {
		appendUnique(i)
	}
	return candidates
}

func writeProxyOnlyRuntimeAuth(path string) error {
	payload, err := proxyOnlyRuntimeAuthPayload()
	if err != nil {
		return err
	}
	return os.WriteFile(path, payload, 0o600)
}

func proxyOnlyRuntimeAuthPayload() ([]byte, error) {
	token := buildProxyOnlyAccessToken()
	payload := map[string]any{
		"auth_mode":      "chatgpt",
		"OPENAI_API_KEY": nil,
		"last_refresh":   time.Now().UTC().Format(time.RFC3339),
		"tokens": map[string]any{
			"access_token":  token,
			"refresh_token": token,
			"id_token":      token,
			"account_id":    "proxy-only",
		},
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return b, nil
}

func buildProxyOnlyAccessToken() string {
	head := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	payload := map[string]any{
		"sub": "codexlb-proxy-only",
		"exp": int64(4102444800),
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": "proxy-only",
		},
		"https://api.openai.com/profile": map[string]any{
			"email": "proxy-only@codexlb.internal",
		},
	}
	b, _ := json.Marshal(payload)
	body := base64.RawURLEncoding.EncodeToString(b)
	return head + "." + body + ".sig"
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

func normalizeRuntimeAuthPayload(raw []byte, fallbackAccountID string) ([]byte, error) {
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, fmt.Errorf("parse runtime auth payload: %w", err)
	}

	tokens, _ := payload["tokens"].(map[string]any)
	if tokens == nil {
		return nil, fmt.Errorf("missing tokens object")
	}

	accessToken := strings.TrimSpace(stringField(tokens["access_token"]))
	if accessToken == "" {
		return nil, fmt.Errorf("missing tokens.access_token")
	}
	if strings.TrimSpace(stringField(tokens["refresh_token"])) == "" {
		tokens["refresh_token"] = accessToken
	}
	if strings.TrimSpace(stringField(tokens["id_token"])) == "" {
		tokens["id_token"] = accessToken
	}
	if strings.TrimSpace(stringField(tokens["account_id"])) == "" && strings.TrimSpace(fallbackAccountID) != "" {
		tokens["account_id"] = fallbackAccountID
	}
	payload["tokens"] = tokens

	normalized, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("serialize runtime auth payload: %w", err)
	}
	return normalized, nil
}

func stringField(v any) string {
	s, _ := v.(string)
	return s
}
