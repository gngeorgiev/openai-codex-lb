package lb

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
)

func rewriteForAccount(src *url.URL, baseURL string) (*url.URL, error) {
	base, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("parse base url %q: %w", baseURL, err)
	}

	next := *src
	next.Scheme = base.Scheme
	next.Host = base.Host

	basePath := strings.TrimSuffix(base.Path, "/")
	sourcePath := src.Path

	if strings.Contains(basePath, "/backend-api") {
		codexPrefix := basePath
		if !strings.Contains(codexPrefix, "/codex") {
			codexPrefix = codexPrefix + "/codex"
		}
		if responsePathSuffix, ok := accountResponsesSuffix(sourcePath); ok {
			next.Path = codexPrefix + "/responses" + responsePathSuffix
		} else if appsPathSuffix, ok := accountAppsSuffix(sourcePath); ok {
			next.Path = basePath + "/wham/apps" + appsPathSuffix
		} else if strings.Contains(sourcePath, "/chat/completions") {
			next.Path = codexPrefix + "/responses"
		} else {
			next.Path = joinPath(basePath, sourcePath)
		}
	} else if basePath != "" && basePath != "/" {
		next.Path = joinPath(basePath, sourcePath)
	}

	return &next, nil
}

func joinPath(prefix, p string) string {
	prefix = strings.TrimSuffix(prefix, "/")
	if p == "" {
		return prefix
	}
	if strings.HasPrefix(p, prefix+"/") || p == prefix {
		return p
	}
	return path.Clean(prefix + "/" + strings.TrimPrefix(p, "/"))
}

func setAccountHeaders(headers http.Header, info AuthInfo) {
	headers.Del("Authorization")
	headers.Del("authorization")
	headers.Set("Authorization", "Bearer "+info.AccessToken)
	if info.ChatGPTAccountID != "" {
		headers.Set("ChatGPT-Account-Id", info.ChatGPTAccountID)
	}
}

func isRetryableStatus(status int) bool {
	return status == http.StatusTooManyRequests || (status >= 500 && status <= 599)
}

func shouldDisableForAuthFailure(status int, requestPath string) bool {
	if status == http.StatusUnauthorized {
		return true
	}
	if status != http.StatusForbidden {
		return false
	}
	return isAccountScopedPath(requestPath)
}

func isAccountScopedPath(requestPath string) bool {
	requestPath = strings.ToLower(requestPath)
	_, ok := accountResponsesSuffix(requestPath)
	return ok ||
		strings.Contains(requestPath, "/chat/completions") ||
		strings.Contains(requestPath, "/backend-api/codex/responses")
}

func isUsageLimitResponse(status int, requestPath, body string) bool {
	if status != http.StatusForbidden || !isAccountScopedPath(requestPath) {
		return false
	}
	body = strings.ToLower(strings.ReplaceAll(strings.TrimSpace(body), "’", "'"))
	if body == "" {
		return false
	}
	return strings.Contains(body, "you've hit your usage limit") ||
		(strings.Contains(body, "usage limit") && strings.Contains(body, "purchase more credits")) ||
		strings.Contains(body, "chatgpt.com/codex/settings/usage")
}

func accountResponsesSuffix(requestPath string) (string, bool) {
	requestPath = strings.ToLower(requestPath)
	switch {
	case requestPath == "/responses":
		return "", true
	case strings.HasPrefix(requestPath, "/responses/"):
		return strings.TrimPrefix(requestPath, "/responses"), true
	case strings.Contains(requestPath, "/v1/responses"):
		suffix, ok := strings.CutPrefix(requestPath, "/v1/responses")
		return suffix, ok
	default:
		return "", false
	}
}

func accountAppsSuffix(requestPath string) (string, bool) {
	requestPath = strings.ToLower(requestPath)
	switch {
	case requestPath == "/api/codex/apps":
		return "", true
	case requestPath == "/api/codex/apps/":
		return "/", true
	case strings.HasPrefix(requestPath, "/api/codex/apps/"):
		return strings.TrimPrefix(requestPath, "/api/codex/apps"), true
	default:
		return "", false
	}
}

func defaultBackoffSeconds(status int, fallback int) int {
	if status == http.StatusTooManyRequests {
		if fallback > 0 {
			return fallback
		}
		return 5
	}
	if fallback > 0 {
		return fallback
	}
	return 2
}

func maybeBackfillModelsDisplayNames(requestPath string, resp *http.Response) error {
	if !isModelsPath(requestPath) || resp == nil || resp.Body == nil {
		return nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil
	}
	if !strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "json") {
		return nil
	}

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()

	updated, changed, err := backfillModelsDisplayNamesJSON(raw)
	if err != nil || !changed {
		resp.Body = io.NopCloser(strings.NewReader(string(raw)))
		return err
	}

	resp.Body = io.NopCloser(strings.NewReader(string(updated)))
	resp.ContentLength = int64(len(updated))
	resp.Header.Del("Content-Length")
	return nil
}

func backfillModelsDisplayNamesJSON(raw []byte) ([]byte, bool, error) {
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, false, err
	}

	models, _ := payload["models"].([]any)
	changed := false
	for _, entry := range models {
		model, _ := entry.(map[string]any)
		if model == nil {
			continue
		}
		if strings.TrimSpace(stringField(model["display_name"])) != "" {
			continue
		}
		if title := strings.TrimSpace(stringField(model["title"])); title != "" {
			model["display_name"] = title
			changed = true
		}
	}
	if !changed {
		return raw, false, nil
	}
	updated, err := json.Marshal(payload)
	if err != nil {
		return nil, false, err
	}
	return updated, true, nil
}

func isModelsPath(requestPath string) bool {
	requestPath = strings.ToLower(requestPath)
	return requestPath == "/models" || strings.HasSuffix(requestPath, "/models")
}
