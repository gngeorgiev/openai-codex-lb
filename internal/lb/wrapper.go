package lb

import (
	"bufio"
	"bytes"
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

	toml "github.com/pelletier/go-toml/v2"
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
	if err := ensureRuntimeAuth(store, codexHome, env["OPENAI_BASE_URL"]); err != nil {
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
	if refreshURL := runtimeRefreshTokenURLOverride(proxyURL); refreshURL != "" {
		env["CODEX_REFRESH_TOKEN_URL_OVERRIDE"] = refreshURL
	}
	if os.Getenv("OPENAI_API_KEY") == "" {
		env["OPENAI_API_KEY"] = "codex-lb-local-key"
	}
	return codexBin, fullArgs, codexHome, env, snapshot.Settings.Run.InheritShell
}

func EnsureRuntimeAuth(store *Store, proxyURL string) error {
	return ensureRuntimeAuth(store, store.RuntimeDir(), proxyURL)
}

func EnsureRuntimeAuthAt(store *Store, codexHome, proxyURL string) error {
	return ensureRuntimeAuth(store, codexHome, proxyURL)
}

func ensureRuntimeAuth(store *Store, codexHome, proxyURL string) error {
	if err := os.MkdirAll(codexHome, 0o700); err != nil {
		return fmt.Errorf("create runtime CODEX_HOME: %w", err)
	}
	if err := seedRuntimeAuthIfMissing(store, codexHome, proxyURL); err != nil {
		return err
	}
	if _, err := SanitizeRuntimeRateLimitState(codexHome, false); err != nil {
		return fmt.Errorf("sanitize runtime rate limits: %w", err)
	}
	if strings.TrimSpace(proxyURL) != "" {
		if status := CheckRuntimeAuth(codexHome); !status.OK {
			return fmt.Errorf("runtime auth self-check failed: %s", status.Issue)
		}
	}
	return nil
}

type RuntimeAuthStatus struct {
	OK                  bool   `json:"ok"`
	Path                string `json:"path"`
	Exists              bool   `json:"exists"`
	AccountID           string `json:"account_id,omitempty"`
	AccessSubject       string `json:"access_subject,omitempty"`
	AccessAccountID     string `json:"access_account_id,omitempty"`
	AccessEmail         string `json:"access_email,omitempty"`
	ProfileEmail        string `json:"profile_email,omitempty"`
	RefreshTokenProxy   bool   `json:"refresh_token_proxy"`
	AccessEqualsIDToken bool   `json:"access_equals_id_token"`
	Issue               string `json:"issue,omitempty"`
}

func CheckRuntimeAuth(codexHome string) RuntimeAuthStatus {
	path := filepath.Join(codexHome, "auth.json")
	status := RuntimeAuthStatus{Path: path}
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			status.Issue = "missing auth.json"
			return status
		}
		status.Issue = fmt.Sprintf("read auth.json: %v", err)
		return status
	}
	status.Exists = true

	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		status.Issue = fmt.Sprintf("parse auth.json: %v", err)
		return status
	}
	tokens, _ := payload["tokens"].(map[string]any)
	if tokens == nil {
		status.Issue = "missing tokens object"
		return status
	}

	accessToken := strings.TrimSpace(stringField(tokens["access_token"]))
	idToken := strings.TrimSpace(stringField(tokens["id_token"]))
	refreshToken := strings.TrimSpace(stringField(tokens["refresh_token"]))
	status.AccountID = strings.TrimSpace(stringField(tokens["account_id"]))
	status.RefreshTokenProxy = refreshToken == proxyRuntimeRefreshToken
	status.AccessEqualsIDToken = accessToken != "" && accessToken == idToken

	claims, err := decodeJWTPayload(accessToken)
	if err != nil {
		status.Issue = fmt.Sprintf("decode access token: %v", err)
		return status
	}
	status.AccessSubject = strings.TrimSpace(stringField(claims["sub"]))
	status.AccessAccountID = nestedString(claims, "https://api.openai.com/auth", "chatgpt_account_id")
	status.AccessEmail = strings.TrimSpace(stringField(claims["email"]))
	status.ProfileEmail = nestedString(claims, "https://api.openai.com/profile", "email")

	checks := map[string]bool{
		"tokens.account_id is proxy-only":    status.AccountID == "proxy-only",
		"access token account is proxy-only": status.AccessAccountID == "proxy-only",
		"access token subject is proxy-only": status.AccessSubject == "codexlb-proxy-only",
		"access token email is proxy-only":   status.AccessEmail == "proxy-only@codexlb.internal",
		"profile email is proxy-only":        status.ProfileEmail == "proxy-only@codexlb.internal",
		"refresh token is not proxy token":   status.RefreshTokenProxy,
		"access and id token match":          status.AccessEqualsIDToken,
	}
	for label, ok := range checks {
		if !ok {
			status.Issue = label
			return status
		}
	}
	status.OK = true
	return status
}

type RuntimeRateLimitSanitizeResult struct {
	RolloutFilesScanned int  `json:"rollout_files_scanned"`
	RolloutFilesChanged int  `json:"rollout_files_changed"`
	RolloutLinesChanged int  `json:"rollout_lines_changed"`
	LogArtifactsRotated int  `json:"log_artifacts_rotated"`
	HistoryFilesChanged int  `json:"history_files_changed"`
	HistoryLinesChanged int  `json:"history_lines_changed"`
	Aggressive          bool `json:"aggressive"`
}

func SanitizeRuntimeRateLimitState(codexHome string, aggressive bool) (RuntimeRateLimitSanitizeResult, error) {
	result := RuntimeRateLimitSanitizeResult{Aggressive: aggressive}
	sessionsDir := filepath.Join(codexHome, "sessions")
	info, err := os.Stat(sessionsDir)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return result, err
		}
	} else if info.IsDir() {
		if err := filepath.WalkDir(sessionsDir, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !d.Type().IsRegular() || !strings.HasSuffix(strings.ToLower(d.Name()), ".jsonl") {
				return nil
			}
			result.RolloutFilesScanned++
			changed, lines, err := stripPersistedRateLimitsFromRollout(path)
			if changed {
				result.RolloutFilesChanged++
				result.RolloutLinesChanged += lines
			}
			return err
		}); err != nil {
			return result, err
		}
	}

	historyPath := filepath.Join(codexHome, "history.jsonl")
	if isRegularFile(historyPath) {
		changed, lines, err := stripPersistedRateLimitsFromRollout(historyPath)
		if err != nil {
			return result, err
		}
		if changed {
			result.HistoryFilesChanged++
			result.HistoryLinesChanged += lines
		}
	}

	if aggressive {
		rotated, err := rotateRuntimeRateLimitLogArtifacts(codexHome)
		if err != nil {
			return result, err
		}
		result.LogArtifactsRotated = rotated
	}
	return result, nil
}

func stripPersistedRateLimitsFromRollout(path string) (bool, int, error) {
	in, err := os.Open(path)
	if err != nil {
		return false, 0, err
	}
	defer in.Close()

	tmpPath := path + ".tmp"
	out, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return false, 0, err
	}

	reader := bufio.NewReader(in)
	changed := false
	changedLines := 0
	writeErr := func(err error) error {
		_ = out.Close()
		_ = os.Remove(tmpPath)
		return err
	}

	for {
		line, readErr := reader.ReadBytes('\n')
		if len(line) > 0 {
			updated, lineChanged, err := stripRateLimitsFromRolloutLine(line)
			if err != nil {
				return false, 0, writeErr(err)
			}
			if lineChanged {
				changed = true
				changedLines++
				line = updated
			}
			if _, err := out.Write(line); err != nil {
				return false, 0, writeErr(err)
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return false, 0, writeErr(readErr)
		}
	}

	if err := out.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return false, 0, err
	}
	if !changed {
		_ = os.Remove(tmpPath)
		return false, 0, nil
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return false, 0, err
	}
	return true, changedLines, nil
}

func stripRateLimitsFromRolloutLine(line []byte) ([]byte, bool, error) {
	trimmed := strings.TrimSpace(string(line))
	if trimmed == "" ||
		!strings.Contains(trimmed, `"type":"event_msg"`) ||
		!strings.Contains(trimmed, `"type":"token_count"`) ||
		!strings.Contains(trimmed, `"rate_limits"`) {
		return line, false, nil
	}

	var entry map[string]any
	if err := json.Unmarshal([]byte(trimmed), &entry); err != nil {
		return line, false, nil
	}
	if stringField(entry["type"]) != "event_msg" {
		return line, false, nil
	}
	payload, _ := entry["payload"].(map[string]any)
	if payload == nil || stringField(payload["type"]) != "token_count" {
		return line, false, nil
	}
	if _, ok := payload["rate_limits"]; !ok {
		return line, false, nil
	}

	payload["rate_limits"] = nil
	entry["payload"] = payload
	updated, err := json.Marshal(entry)
	if err != nil {
		return nil, false, err
	}
	if len(line) > 0 && line[len(line)-1] == '\n' {
		updated = append(updated, '\n')
	}
	return updated, true, nil
}

func rotateRuntimeRateLimitLogArtifacts(codexHome string) (int, error) {
	rotated := 0
	suffix := ".codexlb-sanitized-" + time.Now().UTC().Format("20060102T150405Z")
	seenSQLite := map[string]struct{}{}

	if err := filepath.WalkDir(codexHome, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		name := d.Name()
		lower := strings.ToLower(name)
		if strings.Contains(lower, ".codexlb-sanitized-") {
			return nil
		}

		if strings.HasSuffix(lower, ".log") {
			contains, err := fileContainsAny(path, []string{`"rate_limits"`, "rateLimits", "account/rateLimits"})
			if err != nil {
				return err
			}
			if contains {
				if err := os.Rename(path, path+suffix); err != nil {
					return err
				}
				rotated++
			}
			return nil
		}

		sqliteBase, ok := runtimeLogSQLiteBase(path)
		if !ok {
			return nil
		}
		if _, done := seenSQLite[sqliteBase]; done {
			return nil
		}
		seenSQLite[sqliteBase] = struct{}{}
		if runtimeSQLiteLogSetContainsRateLimit(sqliteBase) {
			for _, candidate := range []string{sqliteBase, sqliteBase + "-wal", sqliteBase + "-shm"} {
				if isRegularFile(candidate) {
					if err := os.Rename(candidate, candidate+suffix); err != nil {
						return err
					}
					rotated++
				}
			}
		}
		return nil
	}); err != nil {
		return rotated, err
	}

	return rotated, nil
}

func runtimeLogSQLiteBase(path string) (string, bool) {
	base := path
	switch {
	case strings.HasSuffix(base, ".sqlite-wal"):
		base = strings.TrimSuffix(base, "-wal")
	case strings.HasSuffix(base, ".sqlite-shm"):
		base = strings.TrimSuffix(base, "-shm")
	case strings.HasSuffix(base, ".sqlite"):
	default:
		return "", false
	}
	if !strings.Contains(strings.ToLower(filepath.Base(base)), "log") {
		return "", false
	}
	return base, true
}

func runtimeSQLiteLogSetContainsRateLimit(base string) bool {
	for _, candidate := range []string{base, base + "-wal"} {
		if !isRegularFile(candidate) {
			continue
		}
		contains, err := fileContainsAny(candidate, []string{`"rate_limits"`, "rateLimits", "account/rateLimits"})
		if err == nil && contains {
			return true
		}
	}
	return false
}

func fileContainsAny(path string, needles []string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()

	chunks := make([]byte, 0, 64*1024)
	buf := make([]byte, 32*1024)
	for {
		n, readErr := f.Read(buf)
		if n > 0 {
			chunks = append(chunks, buf[:n]...)
			for _, needle := range needles {
				if bytes.Contains(chunks, []byte(needle)) {
					return true, nil
				}
			}
			if len(chunks) > 128*1024 {
				chunks = append([]byte(nil), chunks[len(chunks)-64*1024:]...)
			}
		}
		if errors.Is(readErr, io.EOF) {
			return false, nil
		}
		if readErr != nil {
			return false, readErr
		}
	}
}

func seedRuntimeAuthIfMissing(store *Store, codexHome, proxyURL string) error {
	targetAuth := filepath.Join(codexHome, "auth.json")
	if _, err := os.Stat(targetAuth); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat runtime auth.json: %w", err)
	}

	snapshot := store.Snapshot()
	if len(snapshot.Accounts) > 0 {
		candidates := runtimeAuthCandidateIndexes(snapshot, time.Now().UnixMilli())
		var lastErr error

		for _, idx := range candidates {
			account := snapshot.Accounts[idx]
			payload, err := normalizedRuntimeAuthPayloadFromHome(account.HomeDir, account.ChatGPTAccountID, runtimeRefreshTokenOverride(proxyURL))
			if err != nil {
				lastErr = err
				continue
			}
			if err := os.WriteFile(targetAuth, payload, 0o600); err != nil {
				return fmt.Errorf("write runtime auth.json from account %s: %w", account.Alias, err)
			}
			if err := syncRuntimeConfigForAccount(snapshot, idx, codexHome, proxyURL); err != nil {
				return err
			}
			return nil
		}
		if lastErr != nil {
			return lastErr
		}
	}

	if remoteAuth, err := fetchRemoteRuntimeAuth(proxyURL); err == nil {
		payload, err := normalizeRuntimeAuthPayload(remoteAuth.Auth, "", runtimeRefreshTokenOverride(proxyURL))
		if err != nil {
			return fmt.Errorf("normalize remote runtime auth payload: %w", err)
		}
		if err := os.WriteFile(targetAuth, payload, 0o600); err != nil {
			return fmt.Errorf("write runtime auth.json from remote runtime auth: %w", err)
		}
		if err := syncRuntimeConfigFromRemote(remoteAuth, codexHome, proxyURL); err != nil {
			return err
		}
		return nil
	}

	if err := syncDefaultRuntimeConfig(codexHome, proxyURL); err != nil {
		return err
	}
	if err := writeProxyOnlyRuntimeAuth(targetAuth, runtimeRefreshTokenOverride(proxyURL)); err != nil {
		return fmt.Errorf("write proxy-only runtime auth.json: %w", err)
	}
	return nil
}

func syncRuntimeConfigForAccount(snapshot StoreFile, accountIdx int, codexHome, proxyURL string) error {
	if accountIdx >= 0 && accountIdx < len(snapshot.Accounts) {
		sourceConfig := filepath.Join(snapshot.Accounts[accountIdx].HomeDir, "config.toml")
		if isRegularFile(sourceConfig) {
			return copyRuntimeConfigFile(sourceConfig, codexHome, fmt.Sprintf("account %s", snapshot.Accounts[accountIdx].Alias), proxyURL)
		}
	}
	return syncDefaultRuntimeConfig(codexHome, proxyURL)
}

func syncRuntimeConfigFromRemote(runtimeAuth AdminRuntimeAuthResponse, codexHome, proxyURL string) error {
	if strings.TrimSpace(runtimeAuth.Config) != "" {
		sourceDesc := "remote runtime auth"
		if runtimeAuth.SourceAlias != "" {
			sourceDesc = fmt.Sprintf("remote account %s", runtimeAuth.SourceAlias)
		}
		return writeRuntimeConfigBytes([]byte(runtimeAuth.Config), codexHome, sourceDesc, proxyURL)
	}
	return syncDefaultRuntimeConfig(codexHome, proxyURL)
}

func syncDefaultRuntimeConfig(codexHome, proxyURL string) error {
	if sourceConfig := defaultCodexConfigPath(codexHome); sourceConfig != "" {
		return copyRuntimeConfigFile(sourceConfig, codexHome, "default Codex home", proxyURL)
	}
	return writeRuntimeConfigBytes(nil, codexHome, "default runtime config", proxyURL)
}

func copyRuntimeConfigFile(sourceConfig, codexHome, sourceDesc, proxyURL string) error {
	data, err := os.ReadFile(sourceConfig)
	if err != nil {
		return fmt.Errorf("seed runtime config.toml from %s: %w", sourceDesc, err)
	}
	return writeRuntimeConfigBytes(data, codexHome, sourceDesc, proxyURL)
}

func writeRuntimeConfigBytes(source []byte, codexHome, sourceDesc, proxyURL string) error {
	targetConfig := filepath.Join(codexHome, "config.toml")
	var existing []byte
	if isRegularFile(targetConfig) {
		data, err := os.ReadFile(targetConfig)
		if err != nil {
			return fmt.Errorf("seed runtime config.toml from %s: read existing runtime config: %w", sourceDesc, err)
		}
		existing = data
	}

	normalized, err := normalizeRuntimeConfigData(source, existing, proxyURL)
	if err != nil {
		return fmt.Errorf("seed runtime config.toml from %s: %w", sourceDesc, err)
	}
	if len(normalized) == 0 {
		return nil
	}
	if err := os.WriteFile(targetConfig, normalized, 0o600); err != nil {
		return fmt.Errorf("seed runtime config.toml from %s: %w", sourceDesc, err)
	}
	return nil
}

func normalizeRuntimeConfigData(source, existing []byte, proxyURL string) ([]byte, error) {
	trimmedProxyURL := strings.TrimRight(strings.TrimSpace(proxyURL), "/")
	if len(source) == 0 && len(existing) == 0 && trimmedProxyURL == "" {
		return nil, nil
	}

	cfg := map[string]any{}
	if len(source) > 0 {
		if err := toml.Unmarshal(source, &cfg); err != nil {
			return nil, err
		}
	}
	if len(existing) > 0 {
		existingCfg := map[string]any{}
		if err := toml.Unmarshal(existing, &existingCfg); err != nil {
			return nil, err
		}
		preserveRuntimeConfigSelections(cfg, existingCfg)
	}
	if trimmedProxyURL != "" {
		cfg["chatgpt_base_url"] = trimmedProxyURL
	}
	return toml.Marshal(cfg)
}

func preserveRuntimeConfigSelections(cfg, existing map[string]any) {
	for _, key := range []string{"model", "model_reasoning_effort", "check_for_update_on_startup"} {
		if value, ok := existing[key]; ok {
			cfg[key] = cloneConfigValue(value)
		}
	}
	for _, key := range []string{"notice", "projects"} {
		if value, ok := existing[key]; ok {
			cfg[key] = cloneConfigValue(value)
		}
	}

	existingTUI, _ := existing["tui"].(map[string]any)
	if existingTUI == nil {
		return
	}
	modelAvailability, ok := existingTUI["model_availability_nux"]
	if !ok {
		return
	}

	targetTUI, _ := cfg["tui"].(map[string]any)
	if targetTUI == nil {
		targetTUI = map[string]any{}
		cfg["tui"] = targetTUI
	}
	targetTUI["model_availability_nux"] = cloneConfigValue(modelAvailability)
}

func cloneConfigValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		cloned := make(map[string]any, len(typed))
		for key, inner := range typed {
			cloned[key] = cloneConfigValue(inner)
		}
		return cloned
	case []any:
		cloned := make([]any, len(typed))
		for i, inner := range typed {
			cloned[i] = cloneConfigValue(inner)
		}
		return cloned
	default:
		return typed
	}
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
	if _, err := normalizeRuntimeAuthPayload(payload.Auth, "", ""); err != nil {
		return AdminRuntimeAuthResponse{}, fmt.Errorf("runtime auth payload is invalid: %w", err)
	}
	return payload, nil
}

type proxyOnlyRuntimeProfile struct {
	PlanType string
}

const proxyRuntimeRefreshToken = "codexlb-runtime-refresh"

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

func writeProxyOnlyRuntimeAuth(path, refreshToken string) error {
	payload, err := proxyOnlyRuntimeAuthPayloadWithRefreshToken(proxyOnlyRuntimeProfile{}, refreshToken)
	if err != nil {
		return err
	}
	return os.WriteFile(path, payload, 0o600)
}

func normalizedRuntimeAuthPayloadFromHome(homeDir, fallbackAccountID, refreshTokenOverride string) ([]byte, error) {
	path := filepath.Join(homeDir, "auth.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	payload, err := normalizeRuntimeAuthPayload(raw, fallbackAccountID, refreshTokenOverride)
	if err != nil {
		return nil, fmt.Errorf("normalize %s: %w", path, err)
	}
	return payload, nil
}

func runtimeRefreshTokenResponseFromHome(homeDir, fallbackAccountID string) (oauthTokenResponse, error) {
	payload, err := normalizedRuntimeAuthPayloadFromHome(homeDir, fallbackAccountID, proxyRuntimeRefreshToken)
	if err != nil {
		return oauthTokenResponse{}, err
	}
	var auth struct {
		Tokens struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
			IDToken      string `json:"id_token"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal(payload, &auth); err != nil {
		return oauthTokenResponse{}, fmt.Errorf("parse normalized runtime auth payload: %w", err)
	}
	return oauthTokenResponse{
		AccessToken:  auth.Tokens.AccessToken,
		RefreshToken: auth.Tokens.RefreshToken,
		IDToken:      auth.Tokens.IDToken,
	}, nil
}

func proxyOnlyRuntimeAuthPayload(profile proxyOnlyRuntimeProfile) ([]byte, error) {
	return proxyOnlyRuntimeAuthPayloadWithRefreshToken(profile, "")
}

func proxyOnlyRuntimeAuthPayloadWithRefreshToken(profile proxyOnlyRuntimeProfile, refreshToken string) ([]byte, error) {
	token := buildProxyOnlyAccessToken(profile)
	if strings.TrimSpace(refreshToken) == "" {
		refreshToken = token
	}
	payload := map[string]any{
		"auth_mode":      "chatgpt",
		"OPENAI_API_KEY": nil,
		"last_refresh":   time.Now().UTC().Format(time.RFC3339),
		"tokens": map[string]any{
			"access_token":  token,
			"refresh_token": refreshToken,
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

func buildProxyOnlyAccessToken(profile proxyOnlyRuntimeProfile) string {
	head := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	planType := strings.TrimSpace(profile.PlanType)
	if planType == "" {
		planType = "plus"
	}
	payload := map[string]any{
		"email": "proxy-only@codexlb.internal",
		"sub":   "codexlb-proxy-only",
		"exp":   int64(4102444800),
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": "proxy-only",
			"chatgpt_plan_type":  planType,
		},
		"https://api.openai.com/profile": map[string]any{
			"email": "proxy-only@codexlb.internal",
		},
	}
	b, _ := json.Marshal(payload)
	body := base64.RawURLEncoding.EncodeToString(b)
	return head + "." + body + ".sig"
}

func planTypeForAccount(account Account) string {
	auth, err := LoadAuth(account.HomeDir)
	if err != nil {
		return "plus"
	}
	for _, token := range []string{auth.IDToken, auth.AccessToken} {
		if strings.TrimSpace(token) == "" {
			continue
		}
		claims, err := decodeJWTPayload(token)
		if err != nil {
			continue
		}
		if planType := nestedString(claims, "https://api.openai.com/auth", "chatgpt_plan_type"); strings.TrimSpace(planType) != "" {
			return planType
		}
	}
	return "plus"
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

func normalizeRuntimeAuthPayload(raw []byte, fallbackAccountID, refreshTokenOverride string) ([]byte, error) {
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
	if strings.TrimSpace(refreshTokenOverride) != "" {
		return proxyOnlyRuntimeAuthPayloadWithRefreshToken(runtimeProxyOnlyProfileFromTokens(tokens), refreshTokenOverride)
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
	if err := rewriteRuntimeDisplayIDToken(tokens); err != nil {
		return nil, fmt.Errorf("rewrite runtime display id_token: %w", err)
	}
	payload["tokens"] = tokens

	normalized, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("serialize runtime auth payload: %w", err)
	}
	return normalized, nil
}

func runtimeProxyOnlyProfileFromTokens(tokens map[string]any) proxyOnlyRuntimeProfile {
	for _, key := range []string{"id_token", "access_token"} {
		claims, err := decodeJWTPayload(strings.TrimSpace(stringField(tokens[key])))
		if err != nil {
			continue
		}
		if planType := nestedString(claims, "https://api.openai.com/auth", "chatgpt_plan_type"); planType != "" {
			return proxyOnlyRuntimeProfile{PlanType: planType}
		}
	}
	return proxyOnlyRuntimeProfile{}
}

func runtimeRefreshTokenOverride(proxyURL string) string {
	if strings.TrimSpace(proxyURL) == "" {
		return ""
	}
	return proxyRuntimeRefreshToken
}

func runtimeRefreshTokenURLOverride(proxyURL string) string {
	proxyURL = strings.TrimSpace(proxyURL)
	if proxyURL == "" {
		return ""
	}
	return strings.TrimRight(proxyURL, "/") + "/oauth/token"
}

func stringField(v any) string {
	s, _ := v.(string)
	return s
}

func rewriteRuntimeDisplayIDToken(tokens map[string]any) error {
	accountID := strings.TrimSpace(stringField(tokens["account_id"]))
	if accountID == "proxy-only" {
		return nil
	}

	sourceToken := strings.TrimSpace(stringField(tokens["id_token"]))
	if sourceToken == "" {
		sourceToken = strings.TrimSpace(stringField(tokens["access_token"]))
	}
	if sourceToken == "" {
		return fmt.Errorf("missing source token")
	}

	claims, err := decodeJWTPayload(sourceToken)
	if err != nil {
		return err
	}
	authClaims := nestedMap(claims, "https://api.openai.com/auth")
	if accountID == "" {
		accountID = strings.TrimSpace(stringField(authClaims["chatgpt_account_id"]))
	}
	if accountID == "proxy-only" {
		return nil
	}
	if accountID != "" {
		authClaims["chatgpt_account_id"] = accountID
	}
	claims["https://api.openai.com/auth"] = authClaims

	const proxyDisplayEmail = "proxy-only@codexlb.internal"
	claims["email"] = proxyDisplayEmail
	profileClaims := nestedMap(claims, "https://api.openai.com/profile")
	profileClaims["email"] = proxyDisplayEmail
	claims["https://api.openai.com/profile"] = profileClaims

	idToken, err := buildUnsignedJWT(claims)
	if err != nil {
		return err
	}
	tokens["id_token"] = idToken
	return nil
}

func nestedMap(root map[string]any, key string) map[string]any {
	if root == nil {
		return map[string]any{}
	}
	if out, _ := root[key].(map[string]any); out != nil {
		return out
	}
	return map[string]any{}
}

func buildUnsignedJWT(claims map[string]any) (string, error) {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	bodyBytes, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	body := base64.RawURLEncoding.EncodeToString(bodyBytes)
	return header + "." + body + ".sig", nil
}
