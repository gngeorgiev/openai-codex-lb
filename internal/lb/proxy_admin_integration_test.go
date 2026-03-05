package lb

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
	token := testAdminJWT(map[string]any{"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "acct-a"}})
	auth := map[string]any{"tokens": map[string]any{"access_token": token, "account_id": "acct-a"}}
	b, _ := json.Marshal(auth)
	if err := os.WriteFile(filepath.Join(source, "auth.json"), b, 0o600); err != nil {
		t.Fatalf("write source auth: %v", err)
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

func callAdminJSON(t *testing.T, method, url string, reqBody any, respBody any) {
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

func testAdminJWT(payload map[string]any) string {
	head := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	b, _ := json.Marshal(payload)
	body := base64.RawURLEncoding.EncodeToString(b)
	return head + "." + body + ".sig"
}
