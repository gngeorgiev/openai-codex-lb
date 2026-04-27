package lb

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProxyAdminImportListPinUnpinRemove(t *testing.T) {
	root := t.TempDir()
	store, err := OpenStore(root)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	srv := httptest.NewServer(NewProxyServer(store, nil, nil))
	defer srv.Close()

	source := filepath.Join(root, "source")
	if err := os.MkdirAll(source, 0o700); err != nil {
		t.Fatalf("mkdir source: %v", err)
	}
	token := testAdminJWT(map[string]any{"https://api.openai.com/auth": map[string]any{
		"chatgpt_account_id": "acct-a",
		"chatgpt_plan_type":  "plus",
	}})
	auth := map[string]any{"tokens": map[string]any{"access_token": token, "account_id": "acct-a"}}
	b, _ := json.Marshal(auth)
	if err := os.WriteFile(filepath.Join(source, "auth.json"), b, 0o600); err != nil {
		t.Fatalf("write source auth: %v", err)
	}
	if err := os.WriteFile(filepath.Join(source, "config.toml"), []byte("model = \"gpt-5\"\n"), 0o600); err != nil {
		t.Fatalf("write source config: %v", err)
	}

	callAdminJSON(t, http.MethodPost, srv.URL+"/admin/account/import", AdminImportRequest{
		Alias:    "alice",
		FromHome: source,
	}, nil)

	var listResp AdminAccountsResponse
	callAdminJSON(t, http.MethodGet, srv.URL+"/admin/accounts", nil, &listResp)
	if len(listResp.Accounts) != 1 || listResp.Accounts[0].Alias != "alice" {
		t.Fatalf("unexpected admin account list: %+v", listResp.Accounts)
	}

	var runtimeAuthResp AdminRuntimeAuthResponse
	callAdminJSON(t, http.MethodGet, srv.URL+"/admin/runtime-auth", nil, &runtimeAuthResp)
	if !json.Valid(runtimeAuthResp.Auth) {
		t.Fatalf("expected valid runtime auth payload, got: %s", string(runtimeAuthResp.Auth))
	}
	var runtimeAuthPayload struct {
		Tokens struct {
			AccountID string `json:"account_id"`
			IDToken   string `json:"id_token"`
			Access    string `json:"access_token"`
			Refresh   string `json:"refresh_token"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal(runtimeAuthResp.Auth, &runtimeAuthPayload); err != nil {
		t.Fatalf("unmarshal runtime auth payload: %v", err)
	}
	if runtimeAuthPayload.Tokens.AccountID != "acct-a" {
		t.Fatalf("expected runtime auth account id acct-a, got %q", runtimeAuthPayload.Tokens.AccountID)
	}
	claims, err := decodeJWTPayload(runtimeAuthPayload.Tokens.IDToken)
	if err != nil {
		t.Fatalf("decode runtime id_token: %v", err)
	}
	if got := nestedString(claims, "https://api.openai.com/auth", "chatgpt_plan_type"); got != "plus" {
		t.Fatalf("expected runtime plan type plus, got %q", got)
	}
	if got := stringField(claims["email"]); got != "proxy-only@codexlb.internal" {
		t.Fatalf("expected runtime id_token email to be proxy-only, got %q", got)
	}
	if runtimeAuthPayload.Tokens.Refresh != proxyRuntimeRefreshToken {
		t.Fatalf("expected runtime refresh_token %q, got %q", proxyRuntimeRefreshToken, runtimeAuthPayload.Tokens.Refresh)
	}
	if runtimeAuthResp.SourceAlias != "alice" {
		t.Fatalf("expected source alias alice, got %q", runtimeAuthResp.SourceAlias)
	}
	if runtimeAuthResp.Config != "model = \"gpt-5\"\n" {
		t.Fatalf("expected runtime config payload, got %q", runtimeAuthResp.Config)
	}

	callAdminJSON(t, http.MethodPost, srv.URL+"/admin/account/pin", AdminAliasRequest{Alias: "alice"}, nil)
	if got := store.Snapshot().State.PinnedAccountID; got != "openai:alice" {
		t.Fatalf("expected pinned id openai:alice, got %q", got)
	}

	callAdminJSON(t, http.MethodPost, srv.URL+"/admin/account/unpin", map[string]any{}, nil)
	if got := store.Snapshot().State.PinnedAccountID; got != "" {
		t.Fatalf("expected unpinned id, got %q", got)
	}

	callAdminJSON(t, http.MethodPost, srv.URL+"/admin/account/rm", AdminAliasRequest{Alias: "alice"}, nil)
	if got := len(store.Snapshot().Accounts); got != 0 {
		t.Fatalf("expected 0 accounts, got %d", got)
	}
}

func TestProxyAdminImportFromUploadedData(t *testing.T) {
	root := t.TempDir()
	store, err := OpenStore(root)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	srv := httptest.NewServer(NewProxyServer(store, nil, nil))
	defer srv.Close()

	token := testAdminJWT(map[string]any{
		"https://api.openai.com/auth":    map[string]any{"chatgpt_account_id": "acct-b"},
		"https://api.openai.com/profile": map[string]any{"email": "bob@example.com"},
	})
	auth := map[string]any{"tokens": map[string]any{"access_token": token, "account_id": "acct-b"}}
	b, _ := json.Marshal(auth)

	callAdminJSON(t, http.MethodPost, srv.URL+"/admin/account/import", AdminImportRequest{
		Alias:  "bob",
		Auth:   b,
		Config: "model = \"gpt-5\"\n",
	}, nil)

	snap := store.Snapshot()
	if len(snap.Accounts) != 1 || snap.Accounts[0].Alias != "bob" {
		t.Fatalf("unexpected accounts after uploaded import: %+v", snap.Accounts)
	}
	if got := snap.Accounts[0].UserEmail; got != "bob@example.com" {
		t.Fatalf("unexpected email after uploaded import: %q", got)
	}
	if _, err := os.Stat(filepath.Join(root, "accounts", "bob", "config.toml")); err != nil {
		t.Fatalf("expected uploaded config.toml to persist: %v", err)
	}
}

func TestProxyAdminListForwardsToNamedChildProxy(t *testing.T) {
	childRoot := t.TempDir()
	childStore, err := OpenStore(childRoot)
	if err != nil {
		t.Fatalf("open child store: %v", err)
	}
	if err := childStore.Update(func(sf *StoreFile) error {
		sf.Settings.Proxy.Name = "edge-vpn"
		return nil
	}); err != nil {
		t.Fatalf("set child proxy name: %v", err)
	}
	importTestAccount(t, childStore, "alice", "acct-alice", "alice@example.com")
	childSrv := httptest.NewServer(NewProxyServer(childStore, nil, nil))
	defer childSrv.Close()

	mainRoot := t.TempDir()
	mainStore, err := OpenStore(mainRoot)
	if err != nil {
		t.Fatalf("open main store: %v", err)
	}
	if err := mainStore.Update(func(sf *StoreFile) error {
		sf.Settings.Proxy.Name = "edge-main"
		sf.Settings.Proxy.ChildProxyURLs = []string{childSrv.URL}
		return nil
	}); err != nil {
		t.Fatalf("configure main proxy: %v", err)
	}
	importTestAccount(t, mainStore, "bob", "acct-bob", "bob@example.com")
	mainSrv := httptest.NewServer(NewProxyServer(mainStore, nil, nil))
	defer mainSrv.Close()

	var listResp AdminAccountsResponse
	callAdminJSONWithHeaders(t, http.MethodGet, mainSrv.URL+"/admin/accounts", nil, &listResp, map[string]string{
		adminTargetProxyNameHeader: "edge-vpn",
	})
	if len(listResp.Accounts) != 1 || listResp.Accounts[0].Alias != "alice" {
		t.Fatalf("unexpected forwarded admin list: %+v", listResp.Accounts)
	}
}

func TestProxyAdminRemoveForwardsRecursivelyToNamedGrandchildProxy(t *testing.T) {
	grandchildRoot := t.TempDir()
	grandchildStore, err := OpenStore(grandchildRoot)
	if err != nil {
		t.Fatalf("open grandchild store: %v", err)
	}
	if err := grandchildStore.Update(func(sf *StoreFile) error {
		sf.Settings.Proxy.Name = "edge-leaf"
		return nil
	}); err != nil {
		t.Fatalf("set grandchild proxy name: %v", err)
	}
	importTestAccount(t, grandchildStore, "alice", "acct-alice", "alice@example.com")
	grandchildSrv := httptest.NewServer(NewProxyServer(grandchildStore, nil, nil))
	defer grandchildSrv.Close()

	childRoot := t.TempDir()
	childStore, err := OpenStore(childRoot)
	if err != nil {
		t.Fatalf("open child store: %v", err)
	}
	if err := childStore.Update(func(sf *StoreFile) error {
		sf.Settings.Proxy.Name = "edge-vpn"
		sf.Settings.Proxy.ChildProxyURLs = []string{grandchildSrv.URL}
		return nil
	}); err != nil {
		t.Fatalf("configure child proxy: %v", err)
	}
	childSrv := httptest.NewServer(NewProxyServer(childStore, nil, nil))
	defer childSrv.Close()

	mainRoot := t.TempDir()
	mainStore, err := OpenStore(mainRoot)
	if err != nil {
		t.Fatalf("open main store: %v", err)
	}
	if err := mainStore.Update(func(sf *StoreFile) error {
		sf.Settings.Proxy.Name = "edge-main"
		sf.Settings.Proxy.ChildProxyURLs = []string{childSrv.URL}
		return nil
	}); err != nil {
		t.Fatalf("configure main proxy: %v", err)
	}
	mainSrv := httptest.NewServer(NewProxyServer(mainStore, nil, nil))
	defer mainSrv.Close()

	callAdminJSONWithHeaders(t, http.MethodPost, mainSrv.URL+"/admin/account/rm", AdminAliasRequest{Alias: "alice"}, nil, map[string]string{
		adminTargetProxyNameHeader: "edge-leaf",
	})

	if got := len(grandchildStore.Snapshot().Accounts); got != 0 {
		t.Fatalf("expected grandchild account to be removed, got %d", got)
	}
}

func TestProxyAdminLoginStreamForwardsToNamedChildProxy(t *testing.T) {
	codexBin := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(codexBin, []byte(`#!/usr/bin/env bash
set -euo pipefail
echo "device code: ABC-123"
mkdir -p "$CODEX_HOME"
cat > "$CODEX_HOME/auth.json" <<JSON
{"tokens":{"access_token":"`+testAdminJWT(map[string]any{"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "acct-stream"}})+`","account_id":"acct-stream"}}
JSON
`), 0o755); err != nil {
		t.Fatalf("write fake codex: %v", err)
	}

	childRoot := t.TempDir()
	childStore, err := OpenStore(childRoot)
	if err != nil {
		t.Fatalf("open child store: %v", err)
	}
	if err := childStore.Update(func(sf *StoreFile) error {
		sf.Settings.Proxy.Name = "edge-vpn"
		return nil
	}); err != nil {
		t.Fatalf("set child proxy name: %v", err)
	}
	childSrv := httptest.NewServer(NewProxyServer(childStore, nil, nil))
	defer childSrv.Close()

	mainRoot := t.TempDir()
	mainStore, err := OpenStore(mainRoot)
	if err != nil {
		t.Fatalf("open main store: %v", err)
	}
	if err := mainStore.Update(func(sf *StoreFile) error {
		sf.Settings.Proxy.Name = "edge-main"
		sf.Settings.Proxy.ChildProxyURLs = []string{childSrv.URL}
		return nil
	}); err != nil {
		t.Fatalf("configure main proxy: %v", err)
	}
	mainSrv := httptest.NewServer(NewProxyServer(mainStore, nil, nil))
	defer mainSrv.Close()

	reqBody, err := json.Marshal(AdminLoginRequest{Alias: "alice", CodexBin: codexBin})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, mainSrv.URL+"/admin/account/login?stream=1", bytes.NewReader(reqBody))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(adminTargetProxyNameHeader, "edge-vpn")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unexpected status %d body=%s", resp.StatusCode, string(body))
	}
	if got := strings.TrimSpace(resp.Trailer.Get(adminLoginStreamExitCodeTrailer)); got != "0" {
		t.Fatalf("expected zero login exit trailer, got %q", got)
	}
	text := string(body)
	if !strings.Contains(text, "device code: ABC-123") {
		t.Fatalf("expected streamed device code, got %q", text)
	}
	if !strings.Contains(text, "registered account alice") {
		t.Fatalf("expected streamed success line, got %q", text)
	}
	if got := len(childStore.Snapshot().Accounts); got != 1 || childStore.Snapshot().Accounts[0].Alias != "alice" {
		t.Fatalf("expected child proxy to register alice, got %+v", childStore.Snapshot().Accounts)
	}
}

func TestEffectiveAdminLoginArgsDefaultsDeviceAuthOnlyWhenNeeded(t *testing.T) {
	snapshot := defaultStore()
	if got := effectiveAdminLoginArgs(snapshot, nil); len(got) != 1 || got[0] != "--device-auth" {
		t.Fatalf("expected default device auth args, got %#v", got)
	}

	snapshot.Settings.Commands.Login = []string{"login", "--device-auth"}
	if got := effectiveAdminLoginArgs(snapshot, nil); len(got) != 0 {
		t.Fatalf("expected no extra args when config already includes device auth, got %#v", got)
	}

	if got := effectiveAdminLoginArgs(snapshot, []string{"--foo"}); len(got) != 1 || got[0] != "--foo" {
		t.Fatalf("expected explicit args to win, got %#v", got)
	}
}

func callAdminJSON(t *testing.T, method, url string, reqBody any, respBody any) {
	callAdminJSONWithHeaders(t, method, url, reqBody, respBody, nil)
}

func callAdminJSONWithHeaders(t *testing.T, method, url string, reqBody any, respBody any, headers map[string]string) {
	t.Helper()
	var body *bytes.Reader
	if reqBody == nil {
		body = bytes.NewReader(nil)
	} else {
		b, err := json.Marshal(reqBody)
		if err != nil {
			t.Fatalf("marshal request: %v", err)
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if reqBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var msg map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&msg)
		t.Fatalf("unexpected status=%d body=%v", resp.StatusCode, msg)
	}
	if respBody != nil {
		if err := json.NewDecoder(resp.Body).Decode(respBody); err != nil {
			t.Fatalf("decode response: %v", err)
		}
	}
}

func importTestAccount(t *testing.T, store *Store, alias, accountID, email string) {
	t.Helper()
	token := testAdminJWT(map[string]any{
		"https://api.openai.com/auth":    map[string]any{"chatgpt_account_id": accountID},
		"https://api.openai.com/profile": map[string]any{"email": email},
	})
	auth := map[string]any{"tokens": map[string]any{"access_token": token, "account_id": accountID}}
	b, _ := json.Marshal(auth)
	if err := ImportAccountData(store, alias, b, nil); err != nil {
		t.Fatalf("import test account %s: %v", alias, err)
	}
}

func testAdminJWT(payload map[string]any) string {
	head := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	b, _ := json.Marshal(payload)
	body := base64.RawURLEncoding.EncodeToString(b)
	return head + "." + body + ".sig"
}
