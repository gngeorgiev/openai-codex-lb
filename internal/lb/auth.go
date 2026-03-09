package lb

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type AuthInfo struct {
	AccessToken      string
	RefreshToken     string
	IDToken          string
	ChatGPTAccountID string
	UserEmail        string
}

const (
	defaultAuthTokenURL = "https://auth.openai.com/oauth/token"
	defaultAuthClientID = "app_EMoamEEZ73f0CkXaXp7hrann"
)

type oauthTokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
}

func LoadAuth(homeDir string) (AuthInfo, error) {
	_, _, info, err := loadAuthDocument(homeDir)
	return info, err
}

func RefreshAuth(ctx context.Context, client *http.Client, homeDir, tokenURL, clientID, failedAccessToken string) (AuthInfo, error) {
	path, payload, current, err := loadAuthDocument(homeDir)
	if err != nil {
		return AuthInfo{}, err
	}
	if failedAccessToken != "" && current.AccessToken != "" && current.AccessToken != failedAccessToken {
		return current, nil
	}
	if strings.TrimSpace(current.RefreshToken) == "" {
		return AuthInfo{}, fmt.Errorf("missing tokens.refresh_token in %s", path)
	}
	if strings.TrimSpace(tokenURL) == "" {
		tokenURL = defaultAuthTokenURL
	}
	if strings.TrimSpace(clientID) == "" {
		clientID = defaultAuthClientID
	}

	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("client_id", clientID)
	form.Set("refresh_token", current.RefreshToken)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return AuthInfo{}, fmt.Errorf("build token refresh request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := client.Do(req)
	if err != nil {
		return AuthInfo{}, fmt.Errorf("refresh auth tokens: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body := readBodySnippet(resp.Body, 1024)
		if body != "" {
			return AuthInfo{}, fmt.Errorf("refresh auth tokens status %d: %s", resp.StatusCode, body)
		}
		return AuthInfo{}, fmt.Errorf("refresh auth tokens status %d", resp.StatusCode)
	}

	var refreshed oauthTokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&refreshed); err != nil {
		return AuthInfo{}, fmt.Errorf("decode token refresh response: %w", err)
	}
	if strings.TrimSpace(refreshed.AccessToken) == "" {
		return AuthInfo{}, fmt.Errorf("token refresh response missing access_token")
	}

	tokens, _ := payload["tokens"].(map[string]any)
	if tokens == nil {
		tokens = map[string]any{}
	}
	tokens["access_token"] = refreshed.AccessToken
	if strings.TrimSpace(refreshed.RefreshToken) != "" {
		tokens["refresh_token"] = refreshed.RefreshToken
	} else if current.RefreshToken != "" {
		tokens["refresh_token"] = current.RefreshToken
	}
	if strings.TrimSpace(refreshed.IDToken) != "" {
		tokens["id_token"] = refreshed.IDToken
	} else if current.IDToken != "" {
		tokens["id_token"] = current.IDToken
	}
	if refreshedClaims, err := decodeJWTPayload(refreshed.AccessToken); err == nil {
		if refreshedAccountID := nestedString(refreshedClaims, "https://api.openai.com/auth", "chatgpt_account_id"); refreshedAccountID != "" {
			tokens["account_id"] = refreshedAccountID
		}
	}
	if strings.TrimSpace(stringField(tokens["account_id"])) == "" && current.ChatGPTAccountID != "" {
		tokens["account_id"] = current.ChatGPTAccountID
	}
	payload["tokens"] = tokens
	if strings.TrimSpace(stringField(payload["auth_mode"])) == "" {
		payload["auth_mode"] = "chatgpt"
	}
	payload["last_refresh"] = time.Now().UTC().Format(time.RFC3339Nano)

	if err := writeJSONAtomic(path, payload); err != nil {
		return AuthInfo{}, fmt.Errorf("persist refreshed auth tokens: %w", err)
	}
	return LoadAuth(homeDir)
}

func loadAuthDocument(homeDir string) (string, map[string]any, AuthInfo, error) {
	path := filepath.Join(homeDir, "auth.json")
	bytes, err := os.ReadFile(path)
	if err != nil {
		return "", nil, AuthInfo{}, fmt.Errorf("read %s: %w", path, err)
	}

	var payload map[string]any
	if err := json.Unmarshal(bytes, &payload); err != nil {
		return "", nil, AuthInfo{}, fmt.Errorf("parse %s: %w", path, err)
	}
	tokens, _ := payload["tokens"].(map[string]any)
	if tokens == nil {
		return "", nil, AuthInfo{}, fmt.Errorf("missing tokens object in %s", path)
	}

	info := AuthInfo{
		AccessToken:  strings.TrimSpace(stringField(tokens["access_token"])),
		RefreshToken: strings.TrimSpace(stringField(tokens["refresh_token"])),
		IDToken:      strings.TrimSpace(stringField(tokens["id_token"])),
	}
	if info.AccessToken == "" {
		return "", nil, AuthInfo{}, fmt.Errorf("missing tokens.access_token in %s", path)
	}
	if accountID := strings.TrimSpace(stringField(tokens["account_id"])); accountID != "" {
		info.ChatGPTAccountID = accountID
	}

	claims, _ := decodeJWTPayload(info.AccessToken)
	if info.ChatGPTAccountID == "" {
		info.ChatGPTAccountID = nestedString(claims, "https://api.openai.com/auth", "chatgpt_account_id")
	}
	info.UserEmail = nestedString(claims, "https://api.openai.com/profile", "email")

	return path, payload, info, nil
}

func decodeJWTPayload(token string) (map[string]any, error) {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return nil, fmt.Errorf("invalid jwt token")
	}
	payload := parts[1]
	pad := len(payload) % 4
	if pad != 0 {
		payload += strings.Repeat("=", 4-pad)
	}
	decoded, err := base64.URLEncoding.DecodeString(payload)
	if err != nil {
		return nil, fmt.Errorf("decode jwt payload: %w", err)
	}
	var out map[string]any
	if err := json.Unmarshal(decoded, &out); err != nil {
		return nil, fmt.Errorf("parse jwt payload: %w", err)
	}
	return out, nil
}

func nestedString(m map[string]any, keys ...string) string {
	if len(keys) == 0 {
		return ""
	}
	cur := any(m)
	for _, k := range keys[:len(keys)-1] {
		next, ok := cur.(map[string]any)
		if !ok {
			return ""
		}
		cur, ok = next[k]
		if !ok {
			return ""
		}
	}
	last, ok := cur.(map[string]any)
	if !ok {
		return ""
	}
	v, _ := last[keys[len(keys)-1]].(string)
	return v
}
