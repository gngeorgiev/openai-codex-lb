package lb

import (
	"net/http"
	"net/url"
	"testing"
)

func TestRewriteForBackendResponses(t *testing.T) {
	t.Parallel()
	src, _ := url.Parse("http://127.0.0.1:8765/responses?stream=true")
	next, err := rewriteForAccount(src, "https://chatgpt.com/backend-api")
	if err != nil {
		t.Fatalf("rewriteForAccount: %v", err)
	}
	if next.Scheme != "https" || next.Host != "chatgpt.com" {
		t.Fatalf("unexpected host rewrite: %s", next.String())
	}
	if got, want := next.Path, "/backend-api/codex/responses"; got != want {
		t.Fatalf("path mismatch: got %q want %q", got, want)
	}
}

func TestSetAccountHeaders(t *testing.T) {
	t.Parallel()
	h := http.Header{}
	h.Set("Authorization", "Bearer old")
	setAccountHeaders(h, AuthInfo{AccessToken: "token-123", ChatGPTAccountID: "acct-1"})
	if got := h.Get("Authorization"); got != "Bearer token-123" {
		t.Fatalf("authorization mismatch: %q", got)
	}
	if got := h.Get("ChatGPT-Account-Id"); got != "acct-1" {
		t.Fatalf("chatgpt account header mismatch: %q", got)
	}
}
