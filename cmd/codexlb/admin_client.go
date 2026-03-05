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

type remoteAdminUnexpectedResponseError struct {
	statusCode int
	body       string
}

func (e *remoteAdminUnexpectedResponseError) Error() string {
	body := strings.TrimSpace(e.body)
	if body == "" {
		return fmt.Sprintf("remote admin unexpected response: status=%d", e.statusCode)
	}
	if len(body) > 200 {
		body = body[:200] + "..."
	}
	return fmt.Sprintf("remote admin unexpected response: status=%d body=%s", e.statusCode, body)
}

func remoteAdminListAccounts(proxyURL string) ([]lb.Account, error) {
	return remoteAdminListAccountsWithClient(http.DefaultClient, proxyURL)
}

func remoteAdminListAccountsWithClient(client *http.Client, proxyURL string) ([]lb.Account, error) {
	var resp lb.AdminAccountsResponse
	if err := callRemoteAdminJSONWithClient(client, http.MethodGet, strings.TrimRight(proxyURL, "/")+"/admin/accounts", nil, &resp); err != nil {
		return nil, err
	}
	return resp.Accounts, nil
}

func remoteAdminLogin(proxyURL, alias, codexBin string, loginArgs []string) (lb.AdminMutationResponse, error) {
	return remoteAdminLoginWithClient(http.DefaultClient, proxyURL, alias, codexBin, loginArgs)
}

func remoteAdminLoginWithClient(client *http.Client, proxyURL, alias, codexBin string, loginArgs []string) (lb.AdminMutationResponse, error) {
	req := lb.AdminLoginRequest{
		Alias:     alias,
		CodexBin:  codexBin,
		LoginArgs: loginArgs,
	}
	var resp lb.AdminMutationResponse
	err := callRemoteAdminJSONWithClient(client, http.MethodPost, strings.TrimRight(proxyURL, "/")+"/admin/account/login", req, &resp)
	return resp, err
}

func remoteAdminImport(proxyURL, alias, from string) (lb.AdminMutationResponse, error) {
	return remoteAdminImportWithClient(http.DefaultClient, proxyURL, alias, from)
}

func remoteAdminImportWithClient(client *http.Client, proxyURL, alias, from string) (lb.AdminMutationResponse, error) {
	req := lb.AdminImportRequest{Alias: alias, FromHome: from}
	var resp lb.AdminMutationResponse
	err := callRemoteAdminJSONWithClient(client, http.MethodPost, strings.TrimRight(proxyURL, "/")+"/admin/account/import", req, &resp)
	return resp, err
}

func remoteAdminRemove(proxyURL, alias string) (lb.AdminMutationResponse, error) {
	return remoteAdminRemoveWithClient(http.DefaultClient, proxyURL, alias)
}

func remoteAdminRemoveWithClient(client *http.Client, proxyURL, alias string) (lb.AdminMutationResponse, error) {
	req := lb.AdminAliasRequest{Alias: alias}
	var resp lb.AdminMutationResponse
	err := callRemoteAdminJSONWithClient(client, http.MethodPost, strings.TrimRight(proxyURL, "/")+"/admin/account/rm", req, &resp)
	return resp, err
}

func remoteAdminPin(proxyURL, alias string) (lb.AdminMutationResponse, error) {
	return remoteAdminPinWithClient(http.DefaultClient, proxyURL, alias)
}

func remoteAdminPinWithClient(client *http.Client, proxyURL, alias string) (lb.AdminMutationResponse, error) {
	req := lb.AdminAliasRequest{Alias: alias}
	var resp lb.AdminMutationResponse
	err := callRemoteAdminJSONWithClient(client, http.MethodPost, strings.TrimRight(proxyURL, "/")+"/admin/account/pin", req, &resp)
	return resp, err
}

func remoteAdminUnpin(proxyURL string) (lb.AdminMutationResponse, error) {
	return remoteAdminUnpinWithClient(http.DefaultClient, proxyURL)
}

func remoteAdminUnpinWithClient(client *http.Client, proxyURL string) (lb.AdminMutationResponse, error) {
	var resp lb.AdminMutationResponse
	err := callRemoteAdminJSONWithClient(client, http.MethodPost, strings.TrimRight(proxyURL, "/")+"/admin/account/unpin", map[string]any{}, &resp)
	return resp, err
}

func callRemoteAdminJSON(method, url string, reqBody any, respBody any) error {
	return callRemoteAdminJSONWithClient(http.DefaultClient, method, url, reqBody, respBody)
}

func callRemoteAdminJSONWithClient(client *http.Client, method, url string, reqBody any, respBody any) error {
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
	resp, err := client.Do(req)
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
		if msg == "" || strings.HasPrefix(msg, "<") {
			return &remoteAdminUnexpectedResponseError{
				statusCode: resp.StatusCode,
				body:       msg,
			}
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
