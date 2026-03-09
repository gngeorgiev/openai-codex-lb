package lb

import "encoding/json"

type AdminAccountsResponse struct {
	Accounts []Account `json:"accounts"`
}

type AdminLoginRequest struct {
	Alias     string   `json:"alias"`
	CodexBin  string   `json:"codex_bin,omitempty"`
	LoginArgs []string `json:"login_args,omitempty"`
}

type AdminImportRequest struct {
	Alias    string          `json:"alias"`
	FromHome string          `json:"from_home,omitempty"`
	Auth     json.RawMessage `json:"auth,omitempty"`
	Config   string          `json:"config,omitempty"`
}

type AdminAliasRequest struct {
	Alias string `json:"alias"`
}

type AdminMutationResponse struct {
	OK      bool   `json:"ok"`
	Message string `json:"message,omitempty"`
	Total   int    `json:"total"`
}

type AdminRuntimeAuthResponse struct {
	Auth        json.RawMessage `json:"auth"`
	SourceAlias string          `json:"source_alias,omitempty"`
}
