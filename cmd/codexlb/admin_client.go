package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/gngeorgiev/openai-codex-lb/internal/lb"
)

func remoteAdminListAccounts(proxyURL string) ([]lb.Account, error) {
	var resp lb.AdminAccountsResponse
	if err := callRemoteAdminJSON(http.MethodGet, strings.TrimRight(proxyURL, "/")+"/admin/accounts", nil, &resp); err != nil {
		return nil, err
	}
	return resp.Accounts, nil
}

func remoteAdminLogin(proxyURL, alias, codexBin string, loginArgs []string) (lb.AdminMutationResponse, error) {
	req := lb.AdminLoginRequest{
		Alias:     alias,
		CodexBin:  codexBin,
		LoginArgs: loginArgs,
	}
	var resp lb.AdminMutationResponse
	err := callRemoteAdminJSON(http.MethodPost, strings.TrimRight(proxyURL, "/")+"/admin/account/login", req, &resp)
	return resp, err
}

func remoteAdminImport(proxyURL, alias, from string) (lb.AdminMutationResponse, error) {
	req := lb.AdminImportRequest{Alias: alias, FromHome: from}
	var resp lb.AdminMutationResponse
	err := callRemoteAdminJSON(http.MethodPost, strings.TrimRight(proxyURL, "/")+"/admin/account/import", req, &resp)
	return resp, err
}

func remoteAdminRemove(proxyURL, alias string) (lb.AdminMutationResponse, error) {
	req := lb.AdminAliasRequest{Alias: alias}
	var resp lb.AdminMutationResponse
	err := callRemoteAdminJSON(http.MethodPost, strings.TrimRight(proxyURL, "/")+"/admin/account/rm", req, &resp)
	return resp, err
}

func remoteAdminPin(proxyURL, alias string) (lb.AdminMutationResponse, error) {
	req := lb.AdminAliasRequest{Alias: alias}
	var resp lb.AdminMutationResponse
	err := callRemoteAdminJSON(http.MethodPost, strings.TrimRight(proxyURL, "/")+"/admin/account/pin", req, &resp)
	return resp, err
}

func remoteAdminUnpin(proxyURL string) (lb.AdminMutationResponse, error) {
	var resp lb.AdminMutationResponse
	err := callRemoteAdminJSON(http.MethodPost, strings.TrimRight(proxyURL, "/")+"/admin/account/unpin", map[string]any{}, &resp)
	return resp, err
}

func callRemoteAdminJSON(method, url string, reqBody any, respBody any) error {
	var body io.Reader
	if reqBody != nil {
		b, err := json.Marshal(reqBody)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		return err
	}
	if reqBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg := strings.TrimSpace(string(raw))
		var apiErr map[string]any
		if err := json.Unmarshal(raw, &apiErr); err == nil {
			if s, ok := apiErr["error"].(string); ok && s != "" {
				msg = s
			}
		}
		if msg == "" {
			msg = fmt.Sprintf("http %d", resp.StatusCode)
		}
		return fmt.Errorf("remote admin error: %s", msg)
	}
	if respBody == nil {
		return nil
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, respBody); err != nil {
		return fmt.Errorf("parse admin response: %w", err)
	}
	return nil
}
