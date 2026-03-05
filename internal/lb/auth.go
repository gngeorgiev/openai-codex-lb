package lb

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type AuthInfo struct {
	AccessToken      string
	ChatGPTAccountID string
	UserEmail        string
}

type codexAuthFile struct {
	Tokens struct {
		AccessToken string `json:"access_token"`
		AccountID   string `json:"account_id"`
	} `json:"tokens"`
}

func LoadAuth(homeDir string) (AuthInfo, error) {
	path := filepath.Join(homeDir, "auth.json")
	bytes, err := os.ReadFile(path)
	if err != nil {
		return AuthInfo{}, fmt.Errorf("read %s: %w", path, err)
	}

	var auth codexAuthFile
	if err := json.Unmarshal(bytes, &auth); err != nil {
		return AuthInfo{}, fmt.Errorf("parse %s: %w", path, err)
	}
	if strings.TrimSpace(auth.Tokens.AccessToken) == "" {
		return AuthInfo{}, fmt.Errorf("missing tokens.access_token in %s", path)
	}

	info := AuthInfo{AccessToken: auth.Tokens.AccessToken}
	if auth.Tokens.AccountID != "" {
		info.ChatGPTAccountID = auth.Tokens.AccountID
	}

	claims, _ := decodeJWTPayload(auth.Tokens.AccessToken)
	if info.ChatGPTAccountID == "" {
		info.ChatGPTAccountID = nestedString(claims, "https://api.openai.com/auth", "chatgpt_account_id")
	}
	info.UserEmail = nestedString(claims, "https://api.openai.com/profile", "email")

	return info, nil
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
