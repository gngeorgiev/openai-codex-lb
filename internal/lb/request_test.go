package lb

import (
	"encoding/json"
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

func TestRewriteForBackendResponsesCompact(t *testing.T) {
	t.Parallel()
	src, _ := url.Parse("http://127.0.0.1:8765/responses/compact")
	next, err := rewriteForAccount(src, "https://chatgpt.com/backend-api")
	if err != nil {
		t.Fatalf("rewriteForAccount: %v", err)
	}
	if got, want := next.Path, "/backend-api/codex/responses/compact"; got != want {
		t.Fatalf("path mismatch: got %q want %q", got, want)
	}
}

func TestRewriteForBackendV1ResponsesCompact(t *testing.T) {
	t.Parallel()
	src, _ := url.Parse("http://127.0.0.1:8765/v1/responses/compact")
	next, err := rewriteForAccount(src, "https://chatgpt.com/backend-api")
	if err != nil {
		t.Fatalf("rewriteForAccount: %v", err)
	}
	if got, want := next.Path, "/backend-api/codex/responses/compact"; got != want {
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

func TestShouldDisableForAuthFailure(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		status      int
		path        string
		wantDisable bool
	}{
		{name: "401 always disables", status: http.StatusUnauthorized, path: "/", wantDisable: true},
		{name: "403 responses disables", status: http.StatusForbidden, path: "/responses", wantDisable: true},
		{name: "403 compact responses disables", status: http.StatusForbidden, path: "/responses/compact", wantDisable: true},
		{name: "403 v1 responses disables", status: http.StatusForbidden, path: "/v1/responses", wantDisable: true},
		{name: "403 v1 compact responses disables", status: http.StatusForbidden, path: "/v1/responses/compact", wantDisable: true},
		{name: "403 chat completions disables", status: http.StatusForbidden, path: "/chat/completions", wantDisable: true},
		{name: "403 models does not disable", status: http.StatusForbidden, path: "/models", wantDisable: false},
		{name: "403 root does not disable", status: http.StatusForbidden, path: "/", wantDisable: false},
		{name: "500 does not disable", status: http.StatusInternalServerError, path: "/responses", wantDisable: false},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := shouldDisableForAuthFailure(tt.status, tt.path); got != tt.wantDisable {
				t.Fatalf("shouldDisableForAuthFailure(%d, %q) = %t, want %t", tt.status, tt.path, got, tt.wantDisable)
			}
		})
	}
}

func TestIsUsageLimitResponse(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body string
		path string
		want bool
	}{
		{
			name: "matches codex usage limit message",
			path: "/responses",
			body: "You've hit your usage limit. Upgrade to Pro, visit https://chatgpt.com/codex/settings/usage to purchase more credits or try again later.",
			want: true,
		},
		{
			name: "ignores unrelated forbidden body",
			path: "/responses",
			body: `{"error":"forbidden"}`,
			want: false,
		},
		{
			name: "ignores non account scoped path",
			path: "/models",
			body: "You've hit your usage limit.",
			want: false,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := isUsageLimitResponse(http.StatusForbidden, tt.path, tt.body); got != tt.want {
				t.Fatalf("isUsageLimitResponse(403, %q, %q) = %t, want %t", tt.path, tt.body, got, tt.want)
			}
		})
	}
}

func TestBackfillModelsDisplayNamesJSON(t *testing.T) {
	t.Parallel()

	raw := []byte(`{"models":[{"slug":"gpt-5.3","title":"GPT-5.3"},{"slug":"already","title":"Already","display_name":"Keep Me"}]}`)
	updated, changed, err := backfillModelsDisplayNamesJSON(raw)
	if err != nil {
		t.Fatalf("backfillModelsDisplayNamesJSON: %v", err)
	}
	if !changed {
		t.Fatalf("expected changed=true")
	}

	var payload struct {
		Models []struct {
			Slug        string `json:"slug"`
			Title       string `json:"title"`
			DisplayName string `json:"display_name"`
		} `json:"models"`
	}
	if err := json.Unmarshal(updated, &payload); err != nil {
		t.Fatalf("unmarshal updated payload: %v", err)
	}
	if payload.Models[0].DisplayName != "GPT-5.3" {
		t.Fatalf("expected first display_name to be backfilled, got %q", payload.Models[0].DisplayName)
	}
	if payload.Models[1].DisplayName != "Keep Me" {
		t.Fatalf("expected existing display_name to be preserved, got %q", payload.Models[1].DisplayName)
	}
}
