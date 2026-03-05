package lb

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadAuthExtractsClaims(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	token := testJWT(map[string]any{
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": "acct-123",
		},
		"https://api.openai.com/profile": map[string]any{
			"email": "alice@example.com",
		},
	})

	auth := map[string]any{
		"tokens": map[string]any{
			"access_token": token,
		},
	}
	b, _ := json.Marshal(auth)
	if err := os.WriteFile(filepath.Join(home, "auth.json"), b, 0o600); err != nil {
		t.Fatalf("write auth.json: %v", err)
	}

	info, err := LoadAuth(home)
	if err != nil {
		t.Fatalf("LoadAuth: %v", err)
	}
	if info.ChatGPTAccountID != "acct-123" {
		t.Fatalf("expected account id acct-123, got %q", info.ChatGPTAccountID)
	}
	if info.UserEmail != "alice@example.com" {
		t.Fatalf("expected email alice@example.com, got %q", info.UserEmail)
	}
}

func TestLoadAuthPrefersExplicitAccountID(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	token := testJWT(map[string]any{
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": "acct-from-claim",
		},
	})

	auth := map[string]any{
		"tokens": map[string]any{
			"access_token": token,
			"account_id":   "acct-explicit",
		},
	}
	b, _ := json.Marshal(auth)
	if err := os.WriteFile(filepath.Join(home, "auth.json"), b, 0o600); err != nil {
		t.Fatalf("write auth.json: %v", err)
	}

	info, err := LoadAuth(home)
	if err != nil {
		t.Fatalf("LoadAuth: %v", err)
	}
	if info.ChatGPTAccountID != "acct-explicit" {
		t.Fatalf("expected explicit account id, got %q", info.ChatGPTAccountID)
	}
}

func testJWT(payload map[string]any) string {
	head := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	b, _ := json.Marshal(payload)
	body := base64.RawURLEncoding.EncodeToString(b)
	return head + "." + body + ".signature"
}
