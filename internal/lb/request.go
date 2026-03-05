package lb

import (
	"fmt"
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
		if sourcePath == "/responses" || strings.Contains(sourcePath, "/v1/responses") || strings.Contains(sourcePath, "/chat/completions") {
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

func isAuthStatus(status int) bool {
	return status == http.StatusUnauthorized || status == http.StatusForbidden
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
