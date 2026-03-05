package lb

type AdminAccountsResponse struct {
	Accounts []Account `json:"accounts"`
}

type AdminLoginRequest struct {
	Alias     string   `json:"alias"`
	CodexBin  string   `json:"codex_bin,omitempty"`
	LoginArgs []string `json:"login_args,omitempty"`
}

type AdminImportRequest struct {
	Alias    string `json:"alias"`
	FromHome string `json:"from_home"`
}

type AdminAliasRequest struct {
	Alias string `json:"alias"`
}

type AdminMutationResponse struct {
	OK      bool   `json:"ok"`
	Message string `json:"message,omitempty"`
	Total   int    `json:"total"`
}
