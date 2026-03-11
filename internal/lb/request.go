package lb

import (
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strings"
)

var hopByHopHeaderNames = []string{
	"Connection",
	"Proxy-Connection",
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"Te",
	"Trailer",
	"Transfer-Encoding",
	"Upgrade",
}

var forwardedHeaderNames = []string{
	"Forwarded",
	"Via",
	"X-Forwarded-For",
	"X-Forwarded-Host",
	"X-Forwarded-Proto",
	"X-Real-Ip",
}

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

func sanitizeForwardHeaders(src http.Header) http.Header {
	out := cloneHeaders(src)
	removeConnectionHeaders(out)
	for _, name := range forwardedHeaderNames {
		out.Del(name)
	}
	return out
}

func sanitizeResponseHeaders(src http.Header) http.Header {
	out := cloneHeaders(src)
	removeConnectionHeaders(out)
	return out
}

func removeConnectionHeaders(headers http.Header) {
	connectionTokens := append([]string(nil), headers.Values("Connection")...)
	for _, name := range hopByHopHeaderNames {
		headers.Del(name)
	}
	for _, value := range connectionTokens {
		for _, token := range strings.Split(value, ",") {
			if token = strings.TrimSpace(token); token != "" {
				headers.Del(token)
			}
		}
	}
}

func isControlPlanePath(path string) bool {
	return path == "/healthz" || path == "/status" || path == "/logs" || strings.HasPrefix(path, "/admin/")
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
