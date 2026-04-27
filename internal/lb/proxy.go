package lb

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	adminTargetProxyNameHeader      = "X-CodexLB-Target-Proxy-Name"
	adminErrorCodeTargetNotFound    = "target_proxy_not_found"
	adminErrorCodeTargetAmbiguous   = "target_proxy_ambiguous"
	adminErrorCodeTargetUnavailable = "target_proxy_unavailable"
	adminLoginStreamExitCodeTrailer = "X-CodexLB-Login-Exit-Code"
	adminLoginStreamErrorTrailer    = "X-CodexLB-Login-Error"
)

type ProxyServer struct {
	store *Store

	requestClient *http.Client
	usageClient   *http.Client
	logger        *log.Logger
	events        *EventLogger

	authRefreshMu sync.Mutex
	authTokenURL  string
	authClientID  string

	refreshInFlight           atomic.Bool
	requestSeq                atomic.Uint64
	childProxyRefreshInFlight atomic.Bool

	childProxyMu        sync.Mutex
	childProxyStates    map[string]childProxyRuntime
	childProxyActiveURL string
	activeRouteID       string
}

const maxDisableBodyLogBytes = 2048

func NewProxyServer(store *Store, logger *log.Logger, events *EventLogger) *ProxyServer {
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          200,
		MaxIdleConnsPerHost:   50,
		IdleConnTimeout:       120 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
	usageTransport := transport.Clone()

	return &ProxyServer{
		store: store,
		requestClient: &http.Client{
			Transport: transport,
		},
		usageClient: &http.Client{
			Transport: usageTransport,
		},
		logger:           logger,
		events:           events,
		authTokenURL:     defaultAuthTokenURL,
		authClientID:     defaultAuthClientID,
		childProxyStates: make(map[string]childProxyRuntime),
	}
}

func (p *ProxyServer) StartMaintenanceLoop(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = time.Minute
	}
	go func() {
		p.maybeRefreshQuota(ctx, time.Now(), false)

		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case now := <-ticker.C:
				p.maybeRefreshQuota(ctx, now, false)
			}
		}
	}()
}

func (p *ProxyServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	reqID := p.requestSeq.Add(1)
	p.logEvent("request.received", map[string]any{
		"req_id": reqID,
		"method": r.Method,
		"path":   r.URL.Path,
	})

	if r.URL.Path == "/healthz" {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
		p.logEvent("request.completed", map[string]any{
			"req_id": reqID,
			"status": http.StatusOK,
			"path":   r.URL.Path,
		})
		return
	}

	if r.URL.Path == "/status" {
		now := time.Now()
		p.expireCooldowns(now)
		p.expireChildProxyCooldowns(now)
		forceRefresh := r.URL.Query().Get("refresh") == "1"
		if forceRefresh {
			p.maybeRefreshQuota(context.WithoutCancel(r.Context()), now, true)
		}
		snapshot := p.store.Snapshot()
		if !forceRefresh {
			p.triggerStatusRefresh(snapshot, now)
		}
		status := p.buildStatus(context.WithoutCancel(r.Context()), snapshot, now, forceRefresh)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(status)
		p.logEvent("request.completed", map[string]any{
			"req_id": reqID,
			"status": http.StatusOK,
			"path":   r.URL.Path,
		})
		return
	}
	if strings.HasPrefix(r.URL.Path, "/admin/") {
		status := p.handleAdmin(w, r)
		p.logEvent("request.completed", map[string]any{
			"req_id": reqID,
			"status": status,
			"path":   r.URL.Path,
		})
		return
	}
	if r.URL.Path == "/" && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true,"service":"codexlb-proxy"}`)
		p.logEvent("request.completed", map[string]any{
			"req_id": reqID,
			"status": http.StatusOK,
			"path":   r.URL.Path,
		})
		return
	}
	if r.URL.Path == "/logs" {
		p.handleLogs(w, r)
		p.logEvent("request.completed", map[string]any{
			"req_id": reqID,
			"status": http.StatusOK,
			"path":   r.URL.Path,
		})
		return
	}

	now := time.Now()
	p.expireCooldowns(now)
	p.expireChildProxyCooldowns(now)
	p.maybeRefreshQuota(r.Context(), now, false)
	if p.handleAggregatedUsage(w, r, now, reqID) {
		return
	}

	if isWebSocketUpgrade(r) {
		p.handleWebsocket(w, r, now, reqID)
		return
	}
	p.handleHTTP(w, r, now, reqID)
}

func (p *ProxyServer) handleAggregatedUsage(w http.ResponseWriter, r *http.Request, now time.Time, reqID uint64) bool {
	if r.Method != http.MethodGet {
		return false
	}
	if r.URL.Path != "/backend-api/wham/usage" && r.URL.Path != "/api/codex/usage" && r.URL.Path != "/usage" {
		return false
	}

	snapshot := p.store.Snapshot()
	if hasChildProxyRouting(snapshot) {
		go p.refreshChildProxyStatusCache(snapshot, now)
	}
	status := p.buildStatus(context.WithoutCancel(r.Context()), snapshot, now, false)
	payload := aggregateUsageResponse(status, now)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(payload)
	p.logEvent("request.completed", map[string]any{
		"req_id": reqID,
		"status": http.StatusOK,
		"path":   r.URL.Path,
		"mode":   "aggregated-usage",
	})
	return true
}

func (p *ProxyServer) handleAdmin(w http.ResponseWriter, r *http.Request) int {
	targetProxyName := strings.TrimSpace(r.Header.Get(adminTargetProxyNameHeader))
	if targetProxyName != "" {
		snapshot := p.store.Snapshot()
		if !strings.EqualFold(targetProxyName, snapshot.Settings.Proxy.Name) {
			return p.forwardAdminToNamedProxy(w, r, snapshot, targetProxyName)
		}
		r.Header.Del(adminTargetProxyNameHeader)
	}

	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/admin/accounts":
		snapshot := p.store.Snapshot()
		writeJSON(w, http.StatusOK, AdminAccountsResponse{Accounts: snapshot.Accounts})
		return http.StatusOK
	case r.Method == http.MethodGet && r.URL.Path == "/admin/runtime-auth":
		snapshot := p.store.Snapshot()
		sel, err := selectAccount(&snapshot, time.Now().UnixMilli())
		if err != nil {
			writeJSONError(w, http.StatusNotFound, "no available account for runtime auth")
			return http.StatusNotFound
		}
		if sel.Index < 0 || sel.Index >= len(snapshot.Accounts) {
			writeJSONError(w, http.StatusInternalServerError, "selected account index is out of range")
			return http.StatusInternalServerError
		}
		account := snapshot.Accounts[sel.Index]
		authPayload, err := normalizedRuntimeAuthPayloadFromHome(account.HomeDir, account.ChatGPTAccountID)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, fmt.Sprintf("load runtime auth for %s: %v", account.Alias, err))
			return http.StatusInternalServerError
		}
		var rawConfig []byte
		configPath := filepath.Join(account.HomeDir, "config.toml")
		if data, err := os.ReadFile(configPath); err == nil {
			rawConfig = data
		}
		writeJSON(w, http.StatusOK, AdminRuntimeAuthResponse{
			Auth:        json.RawMessage(authPayload),
			Config:      string(rawConfig),
			SourceAlias: account.Alias,
		})
		return http.StatusOK
	case r.Method == http.MethodPost && r.URL.Path == "/admin/account/login":
		var req AdminLoginRequest
		if err := decodeJSONBody(r, &req); err != nil {
			writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("invalid login request: %v", err))
			return http.StatusBadRequest
		}
		if strings.TrimSpace(req.Alias) == "" {
			writeJSONError(w, http.StatusBadRequest, "alias is required")
			return http.StatusBadRequest
		}
		req.LoginArgs = effectiveAdminLoginArgs(p.store.Snapshot(), req.LoginArgs)
		if isStreamingAdminLoginRequest(r) {
			return p.handleStreamingAdminLogin(w, req)
		}
		if err := LoginAccount(p.store, req.Alias, req.CodexBin, req.LoginArgs); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return http.StatusBadRequest
		}
		total := len(p.store.Snapshot().Accounts)
		writeJSON(w, http.StatusOK, AdminMutationResponse{
			OK:      true,
			Message: fmt.Sprintf("registered account %s", req.Alias),
			Total:   total,
		})
		return http.StatusOK
	case r.Method == http.MethodPost && r.URL.Path == "/admin/account/import":
		var req AdminImportRequest
		if err := decodeJSONBody(r, &req); err != nil {
			writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("invalid import request: %v", err))
			return http.StatusBadRequest
		}
		if strings.TrimSpace(req.Alias) == "" {
			writeJSONError(w, http.StatusBadRequest, "alias is required")
			return http.StatusBadRequest
		}
		var err error
		switch {
		case len(req.Auth) > 0:
			err = ImportAccountData(p.store, req.Alias, req.Auth, []byte(req.Config))
		case strings.TrimSpace(req.FromHome) != "":
			err = ImportAccount(p.store, req.Alias, req.FromHome)
		default:
			writeJSONError(w, http.StatusBadRequest, "from_home or auth is required")
			return http.StatusBadRequest
		}
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return http.StatusBadRequest
		}
		total := len(p.store.Snapshot().Accounts)
		writeJSON(w, http.StatusOK, AdminMutationResponse{
			OK:      true,
			Message: fmt.Sprintf("imported account %s", req.Alias),
			Total:   total,
		})
		return http.StatusOK
	case r.Method == http.MethodPost && r.URL.Path == "/admin/account/rm":
		var req AdminAliasRequest
		if err := decodeJSONBody(r, &req); err != nil {
			writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("invalid rm request: %v", err))
			return http.StatusBadRequest
		}
		if strings.TrimSpace(req.Alias) == "" {
			writeJSONError(w, http.StatusBadRequest, "alias is required")
			return http.StatusBadRequest
		}
		if err := RemoveAccount(p.store, req.Alias); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return http.StatusBadRequest
		}
		total := len(p.store.Snapshot().Accounts)
		writeJSON(w, http.StatusOK, AdminMutationResponse{
			OK:      true,
			Message: fmt.Sprintf("removed account %s", req.Alias),
			Total:   total,
		})
		return http.StatusOK
	case r.Method == http.MethodPost && r.URL.Path == "/admin/account/pin":
		var req AdminAliasRequest
		if err := decodeJSONBody(r, &req); err != nil {
			writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("invalid pin request: %v", err))
			return http.StatusBadRequest
		}
		if strings.TrimSpace(req.Alias) == "" {
			writeJSONError(w, http.StatusBadRequest, "alias is required")
			return http.StatusBadRequest
		}
		snapshot := p.store.Snapshot()
		pinnedID := ""
		for _, account := range snapshot.Accounts {
			if account.Alias == req.Alias {
				pinnedID = account.ID
				break
			}
		}
		if pinnedID == "" {
			writeJSONError(w, http.StatusNotFound, fmt.Sprintf("alias not found: %s", req.Alias))
			return http.StatusNotFound
		}
		if err := p.store.Update(func(sf *StoreFile) error {
			sf.State.PinnedAccountID = pinnedID
			return nil
		}); err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return http.StatusInternalServerError
		}
		total := len(p.store.Snapshot().Accounts)
		writeJSON(w, http.StatusOK, AdminMutationResponse{
			OK:      true,
			Message: fmt.Sprintf("pinned account %s", req.Alias),
			Total:   total,
		})
		return http.StatusOK
	case r.Method == http.MethodPost && r.URL.Path == "/admin/account/unpin":
		if err := p.store.Update(func(sf *StoreFile) error {
			sf.State.PinnedAccountID = ""
			return nil
		}); err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return http.StatusInternalServerError
		}
		total := len(p.store.Snapshot().Accounts)
		writeJSON(w, http.StatusOK, AdminMutationResponse{
			OK:      true,
			Message: "unpinned account selection",
			Total:   total,
		})
		return http.StatusOK
	default:
		writeJSONError(w, http.StatusNotFound, "admin endpoint not found")
		return http.StatusNotFound
	}
}

func (p *ProxyServer) triggerStatusRefresh(snapshot StoreFile, now time.Time) {
	go p.maybeRefreshQuota(context.Background(), now, true)
	if !hasChildProxyRouting(snapshot) {
		return
	}
	go p.refreshChildProxyStatusCache(snapshot, now)
}

func decodeJSONBody(r *http.Request, dst any) error {
	defer r.Body.Close()
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	return dec.Decode(dst)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeJSONError(w http.ResponseWriter, status int, message string) {
	writeJSONErrorCode(w, status, "", message)
}

func writeJSONErrorCode(w http.ResponseWriter, status int, code, message string) {
	payload := map[string]any{"error": message}
	if strings.TrimSpace(code) != "" {
		payload["code"] = code
	}
	writeJSON(w, status, payload)
}

type adminForwardResponse struct {
	StatusCode int
	Body       []byte
	Header     http.Header
	ErrorCode  string
}

type adminErrorPayload struct {
	Error string `json:"error"`
	Code  string `json:"code,omitempty"`
}

func (p *ProxyServer) forwardAdminToNamedProxy(w http.ResponseWriter, r *http.Request, snapshot StoreFile, targetProxyName string) int {
	if !hasChildProxyRouting(snapshot) {
		writeJSONErrorCode(w, http.StatusNotFound, adminErrorCodeTargetNotFound, fmt.Sprintf("target proxy not found: %s", targetProxyName))
		return http.StatusNotFound
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("read admin request body: %v", err))
		return http.StatusBadRequest
	}
	_ = r.Body.Close()
	if isStreamingAdminLoginRequest(r) {
		return p.forwardStreamingAdminToNamedProxy(w, r, snapshot, targetProxyName, body)
	}

	matchURLs, searchErr := p.findDirectChildProxyMatches(r.Context(), snapshot, targetProxyName)
	if searchErr != nil {
		writeJSONErrorCode(w, http.StatusBadGateway, adminErrorCodeTargetUnavailable, searchErr.Error())
		return http.StatusBadGateway
	}
	switch len(matchURLs) {
	case 0:
	case 1:
		resp, err := p.forwardAdminRequest(r.Context(), matchURLs[0], r, body, targetProxyName)
		if err != nil {
			writeJSONErrorCode(w, http.StatusBadGateway, adminErrorCodeTargetUnavailable, err.Error())
			return http.StatusBadGateway
		}
		return writeForwardedAdminResponse(w, resp)
	default:
		writeJSONErrorCode(w, http.StatusConflict, adminErrorCodeTargetAmbiguous, fmt.Sprintf("target proxy name %q matched multiple direct child proxies", targetProxyName))
		return http.StatusConflict
	}

	var matched *adminForwardResponse
	searchErrors := []string{}
	for _, childURL := range snapshot.Settings.Proxy.ChildProxyURLs {
		resp, err := p.forwardAdminRequest(r.Context(), childURL, r, body, targetProxyName)
		if err != nil {
			searchErrors = append(searchErrors, err.Error())
			continue
		}
		if resp.StatusCode == http.StatusNotFound && resp.ErrorCode == adminErrorCodeTargetNotFound {
			continue
		}
		if matched != nil {
			writeJSONErrorCode(w, http.StatusConflict, adminErrorCodeTargetAmbiguous, fmt.Sprintf("target proxy name %q matched multiple proxies", targetProxyName))
			return http.StatusConflict
		}
		copyResp := *resp
		copyResp.Header = resp.Header.Clone()
		copyResp.Body = append([]byte(nil), resp.Body...)
		matched = &copyResp
	}
	if matched != nil {
		return writeForwardedAdminResponse(w, matched)
	}
	if len(searchErrors) > 0 {
		writeJSONErrorCode(w, http.StatusBadGateway, adminErrorCodeTargetUnavailable, fmt.Sprintf("search target proxy %q: %s", targetProxyName, searchErrors[0]))
		return http.StatusBadGateway
	}
	writeJSONErrorCode(w, http.StatusNotFound, adminErrorCodeTargetNotFound, fmt.Sprintf("target proxy not found: %s", targetProxyName))
	return http.StatusNotFound
}

func isStreamingAdminLoginRequest(r *http.Request) bool {
	return r.Method == http.MethodPost && r.URL.Path == "/admin/account/login" && r.URL.Query().Get("stream") == "1"
}

func effectiveAdminLoginArgs(snapshot StoreFile, args []string) []string {
	args = append([]string(nil), args...)
	if len(args) > 0 {
		return args
	}
	baseLogin := sanitizeCommand(snapshot.Settings.Commands.Login)
	for _, part := range baseLogin {
		if strings.TrimSpace(part) == "--device-auth" {
			return args
		}
	}
	return []string{"--device-auth"}
}

func (p *ProxyServer) handleStreamingAdminLogin(w http.ResponseWriter, req AdminLoginRequest) int {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Add("Trailer", adminLoginStreamExitCodeTrailer)
	w.Header().Add("Trailer", adminLoginStreamErrorTrailer)
	w.WriteHeader(http.StatusOK)

	fw := newFlushWriter(w)
	if err := LoginAccountWithIO(p.store, req.Alias, req.CodexBin, req.LoginArgs, nil, fw, fw); err != nil {
		w.Header().Set(adminLoginStreamExitCodeTrailer, strconv.Itoa(exitCodeFromError(err)))
		w.Header().Set(adminLoginStreamErrorTrailer, err.Error())
		return http.StatusOK
	}
	total := len(p.store.Snapshot().Accounts)
	_, _ = fmt.Fprintf(fw, "registered account %s (total=%d)\n", req.Alias, total)
	w.Header().Set(adminLoginStreamExitCodeTrailer, "0")
	return http.StatusOK
}

func (p *ProxyServer) findDirectChildProxyMatches(ctx context.Context, snapshot StoreFile, targetProxyName string) ([]string, error) {
	targetProxyName = strings.TrimSpace(targetProxyName)
	if targetProxyName == "" {
		return nil, nil
	}
	matches := []string{}
	errorsSeen := []string{}
	for _, childURL := range snapshot.Settings.Proxy.ChildProxyURLs {
		status, err := p.fetchChildProxyStatus(ctx, childURL, true)
		if err != nil {
			errorsSeen = append(errorsSeen, fmt.Sprintf("query child proxy %s: %v", childURL, err))
			continue
		}
		if strings.EqualFold(strings.TrimSpace(status.ProxyName), targetProxyName) {
			matches = append(matches, childURL)
		}
	}
	if len(matches) == 0 && len(errorsSeen) > 0 {
		return nil, errors.New(errorsSeen[0])
	}
	return matches, nil
}

func (p *ProxyServer) forwardStreamingAdminToNamedProxy(w http.ResponseWriter, r *http.Request, snapshot StoreFile, targetProxyName string, body []byte) int {
	matchURLs, searchErr := p.findDirectChildProxyMatches(r.Context(), snapshot, targetProxyName)
	if searchErr != nil {
		writeJSONErrorCode(w, http.StatusBadGateway, adminErrorCodeTargetUnavailable, searchErr.Error())
		return http.StatusBadGateway
	}
	switch len(matchURLs) {
	case 0:
	case 1:
		return p.streamForwardAdminRequest(w, r, matchURLs[0], body, targetProxyName)
	default:
		writeJSONErrorCode(w, http.StatusConflict, adminErrorCodeTargetAmbiguous, fmt.Sprintf("target proxy name %q matched multiple direct child proxies", targetProxyName))
		return http.StatusConflict
	}

	searchErrors := []string{}
	for _, childURL := range snapshot.Settings.Proxy.ChildProxyURLs {
		resp, err := p.openForwardAdminRequest(r.Context(), childURL, r, body, targetProxyName)
		if err != nil {
			searchErrors = append(searchErrors, err.Error())
			continue
		}
		targetNotFound, checkErr := isTargetNotFoundAdminResponse(resp)
		if checkErr != nil {
			searchErrors = append(searchErrors, checkErr.Error())
			continue
		}
		if targetNotFound {
			continue
		}
		return writeStreamingForwardedAdminResponse(w, resp)
	}
	if len(searchErrors) > 0 {
		writeJSONErrorCode(w, http.StatusBadGateway, adminErrorCodeTargetUnavailable, fmt.Sprintf("search target proxy %q: %s", targetProxyName, searchErrors[0]))
		return http.StatusBadGateway
	}
	writeJSONErrorCode(w, http.StatusNotFound, adminErrorCodeTargetNotFound, fmt.Sprintf("target proxy not found: %s", targetProxyName))
	return http.StatusNotFound
}

func (p *ProxyServer) forwardAdminRequest(ctx context.Context, proxyURL string, original *http.Request, body []byte, targetProxyName string) (*adminForwardResponse, error) {
	resp, err := p.openForwardAdminRequest(ctx, proxyURL, original, body, targetProxyName)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var payload adminErrorPayload
	if len(respBody) > 0 {
		_ = json.Unmarshal(respBody, &payload)
	}
	return &adminForwardResponse{
		StatusCode: resp.StatusCode,
		Body:       respBody,
		Header:     resp.Header.Clone(),
		ErrorCode:  strings.TrimSpace(payload.Code),
	}, nil
}

func (p *ProxyServer) openForwardAdminRequest(ctx context.Context, proxyURL string, original *http.Request, body []byte, targetProxyName string) (*http.Response, error) {
	targetURL := strings.TrimRight(strings.TrimSpace(proxyURL), "/") + original.URL.Path
	if rawQuery := strings.TrimSpace(original.URL.RawQuery); rawQuery != "" {
		targetURL += "?" + rawQuery
	}
	req, err := http.NewRequestWithContext(ctx, original.Method, targetURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header = original.Header.Clone()
	if strings.TrimSpace(targetProxyName) != "" {
		req.Header.Set(adminTargetProxyNameHeader, targetProxyName)
	} else {
		req.Header.Del(adminTargetProxyNameHeader)
	}
	return p.requestClient.Do(req)
}

func writeForwardedAdminResponse(w http.ResponseWriter, resp *adminForwardResponse) int {
	for name, values := range resp.Header {
		lower := strings.ToLower(name)
		if lower == "content-length" || lower == "transfer-encoding" || lower == "connection" {
			continue
		}
		for _, value := range values {
			w.Header().Add(name, value)
		}
	}
	w.WriteHeader(resp.StatusCode)
	if len(resp.Body) > 0 {
		_, _ = w.Write(resp.Body)
	}
	return resp.StatusCode
}

func (p *ProxyServer) streamForwardAdminRequest(w http.ResponseWriter, original *http.Request, proxyURL string, body []byte, targetProxyName string) int {
	resp, err := p.openForwardAdminRequest(original.Context(), proxyURL, original, body, targetProxyName)
	if err != nil {
		writeJSONErrorCode(w, http.StatusBadGateway, adminErrorCodeTargetUnavailable, err.Error())
		return http.StatusBadGateway
	}
	return writeStreamingForwardedAdminResponse(w, resp)
}

func isTargetNotFoundAdminResponse(resp *http.Response) (bool, error) {
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		return false, nil
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return false, err
	}
	var payload adminErrorPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return false, nil
	}
	return strings.TrimSpace(payload.Code) == adminErrorCodeTargetNotFound, nil
}

func writeStreamingForwardedAdminResponse(w http.ResponseWriter, resp *http.Response) int {
	defer resp.Body.Close()
	copyForwardHeaders(w.Header(), resp.Header)
	for name := range resp.Trailer {
		w.Header().Add("Trailer", name)
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(newFlushWriter(w), resp.Body)
	for name, values := range resp.Trailer {
		for _, value := range values {
			w.Header().Add(name, value)
		}
	}
	return resp.StatusCode
}

func copyForwardHeaders(dst, src http.Header) {
	for name, values := range src {
		lower := strings.ToLower(name)
		if lower == "content-length" || lower == "transfer-encoding" || lower == "connection" {
			continue
		}
		for _, value := range values {
			dst.Add(name, value)
		}
	}
}

type flushWriter struct {
	w       http.ResponseWriter
	flusher http.Flusher
	mu      sync.Mutex
}

func newFlushWriter(w http.ResponseWriter) *flushWriter {
	fw := &flushWriter{w: w}
	if flusher, ok := w.(http.Flusher); ok {
		fw.flusher = flusher
	}
	return fw
}

func (w *flushWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n, err := w.w.Write(p)
	if w.flusher != nil {
		w.flusher.Flush()
	}
	return n, err
}

func exitCodeFromError(err error) int {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return 1
}

func (p *ProxyServer) handleHTTP(w http.ResponseWriter, r *http.Request, now time.Time, reqID uint64) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, fmt.Sprintf("read request body: %v", err), http.StatusBadRequest)
		return
	}
	_ = r.Body.Close()

	snapshot := p.store.Snapshot()
	maxAttempts := max(1, snapshot.Settings.Proxy.MaxAttempts)

	var lastResp *http.Response
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		snapshot = p.store.Snapshot()
		if hasChildProxyRouting(snapshot) {
			route, err := p.selectRoute(r.Context(), snapshot, now, attempt == 1)
			if err != nil {
				lastErr = err
				p.logEvent("request.selection_failed", map[string]any{
					"req_id":  reqID,
					"attempt": attempt,
					"error":   err.Error(),
				})
				break
			}
			if route.Kind == routeKindChild {
				if p.handleHTTPViaSelectedChildRoute(w, r, body, snapshot, now, reqID, attempt, maxAttempts, route, &lastResp, &lastErr) {
					return
				}
				continue
			}
			if p.handleHTTPViaSelectedLocalRoute(w, r, body, snapshot, now, reqID, attempt, maxAttempts, route, &lastResp, &lastErr) {
				return
			}
			continue
		}
		sel, account, auth, err := p.pickAccount(now)
		if err != nil {
			lastErr = err
			p.logEvent("request.selection_failed", map[string]any{
				"req_id":  reqID,
				"attempt": attempt,
				"error":   err.Error(),
			})
			break
		}
		if p.handleHTTPViaPickedAccount(w, r, body, snapshot, now, reqID, attempt, maxAttempts, sel, account, auth, &lastResp, &lastErr) {
			return
		}
	}

	if lastResp != nil {
		p.logEvent("request.completed", map[string]any{
			"req_id": reqID,
			"status": lastResp.StatusCode,
		})
		writeResponse(w, r.URL.Path, lastResp)
		return
	}
	if lastErr != nil {
		p.logEvent("request.failed", map[string]any{
			"req_id": reqID,
			"error":  lastErr.Error(),
		})
		http.Error(w, lastErr.Error(), http.StatusServiceUnavailable)
		return
	}
	p.logEvent("request.failed", map[string]any{
		"req_id": reqID,
		"error":  "no account available",
	})
	http.Error(w, "no account available", http.StatusServiceUnavailable)
}

func (p *ProxyServer) handleHTTPViaPickedAccount(w http.ResponseWriter, r *http.Request, body []byte, snapshot StoreFile, now time.Time, reqID uint64, attempt, maxAttempts int, sel Selection, account Account, auth AuthInfo, lastResp **http.Response, lastErr *error) bool {
	p.logEvent("request.account_selected", map[string]any{
		"req_id":        reqID,
		"attempt":       attempt,
		"account_id":    account.ID,
		"account_alias": account.Alias,
		"switch_reason": sel.SwitchReason,
		"switched":      sel.Switched,
	})

	targetURL, err := rewriteForAccount(r.URL, account.BaseURL)
	if err != nil {
		*lastErr = err
		p.logEvent("request.rewrite_failed", map[string]any{
			"req_id":  reqID,
			"attempt": attempt,
			"error":   err.Error(),
		})
		return false
	}

	upstreamReq, err := http.NewRequestWithContext(r.Context(), r.Method, targetURL.String(), bytes.NewReader(body))
	if err != nil {
		*lastErr = err
		return false
	}
	upstreamReq.Header = cloneHeaders(r.Header)
	setAccountHeaders(upstreamReq.Header, auth)
	upstreamReq.Host = targetURL.Host

	resp, err := p.requestClient.Do(upstreamReq)
	if err != nil {
		if isCanceledRequest(r.Context(), err) {
			p.logEvent("request.canceled", map[string]any{
				"req_id":     reqID,
				"attempt":    attempt,
				"account_id": account.ID,
				"method":     r.Method,
				"path":       r.URL.Path,
				"error":      err.Error(),
			})
			return true
		}
		p.markCooldown(account.ID, 0, snapshot.Settings.Proxy.CooldownDefaultS, "transport-error")
		*lastErr = err
		p.logEvent("request.transport_error", map[string]any{
			"req_id":     reqID,
			"attempt":    attempt,
			"account_id": account.ID,
			"error":      err.Error(),
		})
		return false
	}

	var respBody []byte
	bodySnippet := ""
	loadBodySnippet := func() string {
		if bodySnippet != "" || resp == nil || resp.Body == nil {
			return bodySnippet
		}
		respBody, err = bufferResponseBody(resp)
		if err != nil {
			return ""
		}
		bodySnippet = responseBodySnippet(respBody, maxDisableBodyLogBytes)
		return bodySnippet
	}

	if resp.StatusCode == http.StatusForbidden && isUsageLimitResponse(resp.StatusCode, r.URL.Path, loadBodySnippet()) {
		retryAfter := parseRetryAfterSeconds(resp.Header)
		cooldown := max(snapshot.Settings.Proxy.CooldownDefaultS, retryAfter)
		p.markCooldown(account.ID, resp.StatusCode, cooldown, "usage-limit")
		p.logEvent("account.cooldown", map[string]any{
			"req_id":           reqID,
			"attempt":          attempt,
			"account_id":       account.ID,
			"status":           resp.StatusCode,
			"cooldown_seconds": cooldown,
			"retry_after":      retryAfter,
			"reason":           "usage-limit",
			"error_body":       bodySnippet,
		})
		if attempt < maxAttempts {
			*lastResp = resp
			return false
		}
	}

	if shouldDisableForAuthFailure(resp.StatusCode, r.URL.Path) {
		bodySnippet = loadBodySnippet()
		refreshedAuth, refreshed, refreshErr := p.tryRefreshAccountAuth(r.Context(), account, auth)
		if refreshErr == nil && refreshed {
			upstreamReq, err = http.NewRequestWithContext(r.Context(), r.Method, targetURL.String(), bytes.NewReader(body))
			if err != nil {
				*lastErr = err
				return false
			}
			upstreamReq.Header = cloneHeaders(r.Header)
			setAccountHeaders(upstreamReq.Header, refreshedAuth)
			upstreamReq.Host = targetURL.Host
			resp, err = p.requestClient.Do(upstreamReq)
			if err != nil {
				if isCanceledRequest(r.Context(), err) {
					p.logEvent("request.canceled", map[string]any{
						"req_id":     reqID,
						"attempt":    attempt,
						"account_id": account.ID,
						"method":     r.Method,
						"path":       r.URL.Path,
						"error":      err.Error(),
					})
					return true
				}
				p.markCooldown(account.ID, 0, snapshot.Settings.Proxy.CooldownDefaultS, "transport-error")
				*lastErr = err
				p.logEvent("request.transport_error", map[string]any{
					"req_id":     reqID,
					"attempt":    attempt,
					"account_id": account.ID,
					"error":      err.Error(),
				})
				return false
			}
			if !shouldDisableForAuthFailure(resp.StatusCode, r.URL.Path) {
				auth = refreshedAuth
			}
		}
		if refreshErr != nil || shouldDisableForAuthFailure(resp.StatusCode, r.URL.Path) {
			p.markDisabled(account.ID, disabledReasonForAuthFailure(resp.StatusCode, refreshErr))
			*lastResp = resp
			fields := map[string]any{
				"req_id":     reqID,
				"attempt":    attempt,
				"account_id": account.ID,
				"status":     resp.StatusCode,
				"path":       r.URL.Path,
				"method":     r.Method,
				"error_body": bodySnippet,
			}
			if refreshErr != nil {
				fields["refresh_error"] = refreshErr.Error()
			}
			p.logEvent("account.disabled", fields)
			if p.logger != nil {
				if refreshErr != nil {
					p.logger.Printf("account disabled id=%s status=%d method=%s path=%s body=%q refresh_error=%v", account.ID, resp.StatusCode, r.Method, r.URL.Path, bodySnippet, refreshErr)
				} else {
					p.logger.Printf("account disabled id=%s status=%d method=%s path=%s body=%q", account.ID, resp.StatusCode, r.Method, r.URL.Path, bodySnippet)
				}
			}
			return false
		}
	}

	if isRetryableStatus(resp.StatusCode) {
		retryAfter := parseRetryAfterSeconds(resp.Header)
		cooldown := defaultBackoffSeconds(resp.StatusCode, retryAfter)
		p.markCooldown(account.ID, resp.StatusCode, cooldown, fmt.Sprintf("http-%d", resp.StatusCode))
		p.logEvent("account.cooldown", map[string]any{
			"req_id":             reqID,
			"attempt":            attempt,
			"account_id":         account.ID,
			"status":             resp.StatusCode,
			"cooldown_seconds":   cooldown,
			"retry_after_second": retryAfter,
		})
		if attempt < maxAttempts {
			_ = resp.Body.Close()
			*lastResp = resp
			return false
		}
	}

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		increment := r.Method == http.MethodPost && strings.Contains(r.URL.Path, "responses")
		p.markSuccess(sel, account.ID, now, increment)
		p.logEvent("request.completed", map[string]any{
			"req_id":     reqID,
			"status":     resp.StatusCode,
			"account_id": account.ID,
			"attempt":    attempt,
		})
		if sel.Switched {
			p.logEvent("request.switched", map[string]any{
				"req_id":        reqID,
				"account_id":    account.ID,
				"switch_reason": sel.SwitchReason,
				"score_current": sel.Score,
				"score_best":    sel.BestScore,
			})
		}
	} else {
		p.logEvent("request.completed", map[string]any{
			"req_id":     reqID,
			"status":     resp.StatusCode,
			"account_id": account.ID,
			"attempt":    attempt,
		})
	}
	writeResponse(w, r.URL.Path, resp)
	return true
}

func isCanceledRequest(ctx context.Context, err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil
}

func (p *ProxyServer) handleWebsocket(w http.ResponseWriter, r *http.Request, now time.Time, reqID uint64) {
	snapshot := p.store.Snapshot()
	if hasChildProxyRouting(snapshot) {
		route, err := p.selectRoute(r.Context(), snapshot, now, true)
		if err != nil {
			p.logEvent("websocket.selection_failed", map[string]any{
				"req_id": reqID,
				"error":  err.Error(),
			})
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		if route.Kind == routeKindChild {
			p.handleWebsocketViaSelectedChildProxy(w, r, snapshot, now, reqID, route)
			return
		}
		auth, err := p.loadAuthForAccount(route.Account)
		if err != nil {
			p.logEvent("websocket.selection_failed", map[string]any{
				"req_id":     reqID,
				"account_id": route.Account.ID,
				"error":      err.Error(),
			})
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		p.handleWebsocketViaSelectedAccount(w, r, now, reqID, route.Selection, route.Account, auth)
		return
	}

	sel, account, auth, err := p.pickAccount(now)
	if err != nil {
		p.logEvent("websocket.selection_failed", map[string]any{
			"req_id": reqID,
			"error":  err.Error(),
		})
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	p.handleWebsocketViaSelectedAccount(w, r, now, reqID, sel, account, auth)
}

func (p *ProxyServer) handleWebsocketViaSelectedAccount(w http.ResponseWriter, r *http.Request, now time.Time, reqID uint64, sel Selection, account Account, auth AuthInfo) {
	p.logEvent("websocket.account_selected", map[string]any{
		"req_id":        reqID,
		"account_id":    account.ID,
		"account_alias": account.Alias,
		"switch_reason": sel.SwitchReason,
		"switched":      sel.Switched,
	})

	targetURL, err := rewriteForAccount(r.URL, account.BaseURL)
	if err != nil {
		p.logEvent("websocket.rewrite_failed", map[string]any{
			"req_id": reqID,
			"error":  err.Error(),
		})
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	base := &url.URL{Scheme: targetURL.Scheme, Host: targetURL.Host}

	proxy := httputil.NewSingleHostReverseProxy(base)
	proxy.Transport = p.requestClient.Transport
	proxy.FlushInterval = -1
	proxy.Director = func(req *http.Request) {
		req.URL.Scheme = targetURL.Scheme
		req.URL.Host = targetURL.Host
		req.URL.Path = targetURL.Path
		req.URL.RawPath = targetURL.RawPath
		req.URL.RawQuery = targetURL.RawQuery
		req.Host = targetURL.Host
		req.Header = cloneHeaders(r.Header)
		setAccountHeaders(req.Header, auth)
	}
	proxy.ModifyResponse = func(resp *http.Response) error {
		if shouldDisableForAuthFailure(resp.StatusCode, r.URL.Path) {
			if _, refreshed, err := p.tryRefreshAccountAuth(r.Context(), account, auth); err == nil && refreshed {
				return nil
			}
			p.markDisabled(account.ID, disabledReasonForAuthFailure(resp.StatusCode, nil))
			p.logEvent("account.disabled", map[string]any{
				"req_id":     reqID,
				"status":     resp.StatusCode,
				"account_id": account.ID,
			})
		} else if isRetryableStatus(resp.StatusCode) {
			retryAfter := parseRetryAfterSeconds(resp.Header)
			cooldown := defaultBackoffSeconds(resp.StatusCode, retryAfter)
			p.markCooldown(account.ID, resp.StatusCode, cooldown, fmt.Sprintf("http-%d", resp.StatusCode))
			p.logEvent("account.cooldown", map[string]any{
				"req_id":           reqID,
				"status":           resp.StatusCode,
				"account_id":       account.ID,
				"cooldown_seconds": cooldown,
			})
		} else if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			p.markSuccess(sel, account.ID, now, false)
			p.logEvent("websocket.completed", map[string]any{
				"req_id":     reqID,
				"status":     resp.StatusCode,
				"account_id": account.ID,
			})
		}
		return nil
	}
	proxy.ErrorHandler = func(rw http.ResponseWriter, req *http.Request, proxyErr error) {
		p.markCooldown(account.ID, 0, 2, "websocket-proxy-error")
		p.logEvent("websocket.failed", map[string]any{
			"req_id":     reqID,
			"account_id": account.ID,
			"error":      proxyErr.Error(),
		})
		http.Error(rw, proxyErr.Error(), http.StatusBadGateway)
	}
	proxy.ServeHTTP(w, r)
}

func (p *ProxyServer) pickAccount(now time.Time) (Selection, Account, AuthInfo, error) {
	snapshot := p.store.Snapshot()
	sel, err := selectAccount(&snapshot, now.UnixMilli())
	if err != nil {
		return Selection{}, Account{}, AuthInfo{}, err
	}
	if sel.Index < 0 || sel.Index >= len(snapshot.Accounts) {
		return Selection{}, Account{}, AuthInfo{}, fmt.Errorf("invalid selected account index")
	}
	account := snapshot.Accounts[sel.Index]
	auth, err := p.loadAuthForAccount(account)
	if err != nil {
		return Selection{}, Account{}, AuthInfo{}, err
	}
	return sel, account, auth, nil
}

func (p *ProxyServer) loadAuthForAccount(account Account) (AuthInfo, error) {
	auth, err := LoadAuth(account.HomeDir)
	if err != nil {
		p.markDisabled(account.ID, "missing-auth")
		return AuthInfo{}, err
	}

	if auth.ChatGPTAccountID != "" && auth.ChatGPTAccountID != account.ChatGPTAccountID {
		_ = p.store.Update(func(sf *StoreFile) error {
			idx := slices.IndexFunc(sf.Accounts, func(a Account) bool { return a.ID == account.ID })
			if idx < 0 {
				return nil
			}
			sf.Accounts[idx].ChatGPTAccountID = auth.ChatGPTAccountID
			sf.Accounts[idx].UserEmail = auth.UserEmail
			sf.Accounts[idx].Enabled = true
			sf.Accounts[idx].DisabledReason = ""
			return nil
		})
	}
	return auth, nil
}

func (p *ProxyServer) markSuccess(sel Selection, accountID string, now time.Time, incrementMessage bool) {
	_ = p.store.Update(func(sf *StoreFile) error {
		idx := slices.IndexFunc(sf.Accounts, func(a Account) bool { return a.ID == accountID })
		if idx < 0 {
			return nil
		}
		sf.Accounts[idx].LastUsedAtMS = now.UnixMilli()
		sf.Accounts[idx].LastSwitchReason = sel.SwitchReason
		sf.Accounts[idx].CooldownUntilMS = 0
		sf.Accounts[idx].Enabled = true
		sf.Accounts[idx].DisabledReason = ""
		sf.State.ActiveIndex = idx
		if sel.Switched {
			sf.State.LastRotationAtMS = now.UnixMilli()
		}
		if incrementMessage {
			sf.State.MessageCounter++
		}
		return nil
	})
	p.markLocalRouteActive(accountID)
}

func (p *ProxyServer) markCooldown(accountID string, _ int, cooldownSeconds int, reason string) {
	_ = p.store.Update(func(sf *StoreFile) error {
		idx := slices.IndexFunc(sf.Accounts, func(a Account) bool { return a.ID == accountID })
		if idx < 0 {
			return nil
		}
		if cooldownSeconds <= 0 {
			cooldownSeconds = 1
		}
		sf.Accounts[idx].CooldownUntilMS = time.Now().Add(time.Duration(cooldownSeconds) * time.Second).UnixMilli()
		sf.Accounts[idx].LastSwitchReason = reason
		return nil
	})
}

func (p *ProxyServer) markDisabled(accountID, reason string) {
	_ = p.store.Update(func(sf *StoreFile) error {
		idx := slices.IndexFunc(sf.Accounts, func(a Account) bool { return a.ID == accountID })
		if idx < 0 {
			return nil
		}
		sf.Accounts[idx].Enabled = false
		sf.Accounts[idx].DisabledReason = reason
		sf.Accounts[idx].LastSwitchReason = reason
		if shouldClearPinOnDisable(reason) && sf.State.PinnedAccountID == accountID {
			sf.State.PinnedAccountID = ""
		}
		return nil
	})
}

func (p *ProxyServer) markAuthRecovered(accountID string, auth AuthInfo) {
	_ = p.store.Update(func(sf *StoreFile) error {
		idx := slices.IndexFunc(sf.Accounts, func(a Account) bool { return a.ID == accountID })
		if idx < 0 {
			return nil
		}
		sf.Accounts[idx].Enabled = true
		sf.Accounts[idx].DisabledReason = ""
		sf.Accounts[idx].LastSwitchReason = "auth-refresh-recovered"
		if auth.ChatGPTAccountID != "" {
			sf.Accounts[idx].ChatGPTAccountID = auth.ChatGPTAccountID
		}
		if auth.UserEmail != "" {
			sf.Accounts[idx].UserEmail = auth.UserEmail
		}
		return nil
	})
}

func (p *ProxyServer) expireCooldowns(now time.Time) {
	_ = p.store.Update(func(sf *StoreFile) error {
		for i := range sf.Accounts {
			if sf.Accounts[i].CooldownUntilMS > 0 && sf.Accounts[i].CooldownUntilMS <= now.UnixMilli() {
				sf.Accounts[i].CooldownUntilMS = 0
			}
		}
		return nil
	})
}

func (p *ProxyServer) maybeRefreshQuota(ctx context.Context, now time.Time, force bool) {
	if !p.refreshInFlight.CompareAndSwap(false, true) {
		return
	}
	defer p.refreshInFlight.Store(false)

	snapshot := p.store.Snapshot()
	for _, account := range snapshot.Accounts {
		if !force && !dueForQuotaRefresh(account, snapshot.State, snapshot.Settings.Quota, now) {
			continue
		}
		auth, err := LoadAuth(account.HomeDir)
		if err != nil {
			if !force {
				p.markDisabled(account.ID, "missing-auth")
			}
			continue
		}
		// A reused refresh token can be transient when the same account home is
		// synchronized across machines and another node has already rotated auth.
		// Keep forced refreshes cheap, but allow background maintenance to retry
		// later so a newly synced auth.json can recover the account automatically.
		if force && isTerminalAuthFailureReason(account.DisabledReason) {
			continue
		}
		if isAuthFailureReason(account.DisabledReason) {
			refreshedAuth, refreshed, refreshErr := p.tryRefreshAccountAuth(ctx, account, auth)
			if refreshErr != nil {
				p.logEvent("account.auth_refresh_failed", map[string]any{
					"account_id": account.ID,
					"error":      refreshErr.Error(),
					"force":      force,
				})
				if p.logger != nil {
					p.logger.Printf("auth refresh failed account=%s force=%t error=%v", account.ID, force, refreshErr)
				}
				continue
			}
			if refreshed {
				auth = refreshedAuth
			}
		}
		timeout := time.Duration(max(1, snapshot.Settings.Proxy.UsageTimeoutMS)) * time.Millisecond
		refreshCtx, cancel := context.WithTimeout(ctx, timeout)
		updated := account
		err = refreshQuotaForAccount(refreshCtx, p.usageClient, &updated, auth, now)
		cancel()
		if status := authFailureStatusFromError(err); status != 0 {
			refreshedAuth, refreshed, refreshErr := p.tryRefreshAccountAuth(ctx, account, auth)
			if refreshErr == nil && refreshed {
				auth = refreshedAuth
				refreshCtx, cancel = context.WithTimeout(ctx, timeout)
				err = refreshQuotaForAccount(refreshCtx, p.usageClient, &updated, auth, now)
				cancel()
			}
			if refreshErr != nil {
				p.logEvent("account.auth_refresh_failed", map[string]any{
					"account_id": account.ID,
					"error":      refreshErr.Error(),
					"force":      force,
					"status":     status,
				})
				if p.logger != nil {
					p.logger.Printf("auth refresh failed account=%s force=%t status=%d error=%v", account.ID, force, status, refreshErr)
				}
			}
		}
		if err != nil {
			p.logEvent("quota.refresh_failed", map[string]any{
				"account_id": account.ID,
				"error":      err.Error(),
				"force":      force,
			})
			if p.logger != nil {
				p.logger.Printf("quota refresh failed account=%s force=%t error=%v", account.ID, force, err)
			}
			continue
		}
		updated.Quota.LastSyncMessageCounter = snapshot.State.MessageCounter
		updated.ChatGPTAccountID = auth.ChatGPTAccountID
		updated.UserEmail = auth.UserEmail
		updated.Enabled = true
		updated.DisabledReason = ""
		if account.DisabledReason != "" {
			updated.LastSwitchReason = "quota-refresh-recovered"
		}

		_ = p.store.Update(func(sf *StoreFile) error {
			idx := slices.IndexFunc(sf.Accounts, func(a Account) bool { return a.ID == account.ID })
			if idx < 0 {
				return nil
			}
			sf.Accounts[idx].Quota = updated.Quota
			sf.Accounts[idx].ChatGPTAccountID = updated.ChatGPTAccountID
			sf.Accounts[idx].UserEmail = updated.UserEmail
			sf.Accounts[idx].Enabled = updated.Enabled
			sf.Accounts[idx].DisabledReason = updated.DisabledReason
			if updated.LastSwitchReason != "" {
				sf.Accounts[idx].LastSwitchReason = updated.LastSwitchReason
			}
			return nil
		})
		p.logEvent("quota.refreshed", map[string]any{
			"account_id":      account.ID,
			"daily_limit":     updated.Quota.DailyLimit,
			"daily_used":      updated.Quota.DailyUsed,
			"weekly_limit":    updated.Quota.WeeklyLimit,
			"weekly_used":     updated.Quota.WeeklyUsed,
			"force":           force,
			"recovered_from":  account.DisabledReason,
			"last_sync_at_ms": updated.Quota.LastSyncAt,
		})
	}
}

func (p *ProxyServer) tryRefreshAccountAuth(ctx context.Context, account Account, failedAuth AuthInfo) (AuthInfo, bool, error) {
	p.authRefreshMu.Lock()
	defer p.authRefreshMu.Unlock()

	current, err := LoadAuth(account.HomeDir)
	if err != nil {
		return AuthInfo{}, false, err
	}
	if failedAuth.AccessToken != "" && current.AccessToken != "" && current.AccessToken != failedAuth.AccessToken {
		p.markAuthRecovered(account.ID, current)
		p.logEvent("account.auth_refreshed", map[string]any{
			"account_id": account.ID,
			"mode":       "guarded-reload",
		})
		return current, true, nil
	}

	timeout := time.Duration(max(1, p.store.Snapshot().Settings.Proxy.UsageTimeoutMS)) * time.Millisecond
	refreshCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	refreshed, err := RefreshAuth(refreshCtx, p.requestClient, account.HomeDir, p.authTokenURL, p.authClientID, failedAuth.AccessToken)
	if err != nil {
		return AuthInfo{}, false, err
	}
	p.markAuthRecovered(account.ID, refreshed)
	p.logEvent("account.auth_refreshed", map[string]any{
		"account_id": account.ID,
		"mode":       "token-refresh",
	})
	return refreshed, true, nil
}

func (p *ProxyServer) handleLogs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	offset := int64(-1)
	if raw := strings.TrimSpace(q.Get("offset")); raw != "" {
		if v, err := strconv.ParseInt(raw, 10, 64); err == nil && v >= 0 {
			offset = v
		}
	}
	tailN := 100
	if raw := strings.TrimSpace(q.Get("tail")); raw != "" {
		if v, err := strconv.Atoi(raw); err == nil && v > 0 {
			tailN = v
		}
	}
	limit := 500
	if raw := strings.TrimSpace(q.Get("limit")); raw != "" {
		if v, err := strconv.Atoi(raw); err == nil && v > 0 {
			limit = min(2000, v)
		}
	}

	logPath := filepath.Join(p.store.RootDir(), "logs", "proxy.current.jsonl")
	info, err := os.Stat(logPath)
	if err != nil {
		if os.IsNotExist(err) {
			w.Header().Set("Content-Type", "application/x-ndjson")
			w.Header().Set("X-Next-Offset", "0")
			w.WriteHeader(http.StatusOK)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if offset >= 0 {
		if offset > info.Size() {
			offset = info.Size()
		}
		file, err := os.Open(logPath)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer file.Close()
		if _, err := file.Seek(offset, io.SeekStart); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		scanner := bufio.NewScanner(file)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		nextOffset := offset
		lines := 0
		var b strings.Builder
		for scanner.Scan() {
			line := scanner.Text()
			b.WriteString(line)
			b.WriteByte('\n')
			nextOffset += int64(len(line) + 1)
			lines++
			if lines >= limit {
				break
			}
		}
		if err := scanner.Err(); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.Header().Set("X-Next-Offset", strconv.FormatInt(nextOffset, 10))
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, b.String())
		return
	}

	data, err := os.ReadFile(logPath)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	text := strings.TrimRight(string(data), "\n")
	if text == "" {
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.Header().Set("X-Next-Offset", strconv.FormatInt(info.Size(), 10))
		w.WriteHeader(http.StatusOK)
		return
	}
	lines := strings.Split(text, "\n")
	if tailN > len(lines) {
		tailN = len(lines)
	}
	if tailN > limit {
		tailN = limit
	}
	selected := strings.Join(lines[len(lines)-tailN:], "\n") + "\n"
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("X-Next-Offset", strconv.FormatInt(info.Size(), 10))
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, selected)
}

func readBodySnippet(r io.Reader, maxBytes int) string {
	if r == nil || maxBytes <= 0 {
		return ""
	}
	body, err := io.ReadAll(io.LimitReader(r, int64(maxBytes)))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(body))
}

func bufferResponseBody(resp *http.Response) ([]byte, error) {
	if resp == nil || resp.Body == nil {
		return nil, nil
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	_ = resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	if len(body) == 0 {
		resp.Header.Del("Content-Length")
	} else {
		resp.Header.Set("Content-Length", strconv.Itoa(len(body)))
	}
	return body, nil
}

func responseBodySnippet(body []byte, maxBytes int) string {
	if len(body) == 0 || maxBytes <= 0 {
		return ""
	}
	if len(body) > maxBytes {
		body = body[:maxBytes]
	}
	return strings.TrimSpace(string(body))
}

func isAuthFailureReason(reason string) bool {
	return reason == "http-401"
}

func disabledReasonForAuthFailure(statusCode int, refreshErr error) string {
	if statusCode == http.StatusUnauthorized && isTerminalRefreshError(refreshErr) {
		return "refresh-token-reused"
	}
	return fmt.Sprintf("http-%d", statusCode)
}

func shouldClearPinOnDisable(reason string) bool {
	switch reason {
	case "missing-auth", "http-401", "refresh-token-reused":
		return true
	default:
		return false
	}
}

func isTerminalAuthFailureReason(reason string) bool {
	return reason == "refresh-token-reused"
}

func (p *ProxyServer) logEvent(event string, fields map[string]any) {
	if p.events != nil {
		p.events.Log(event, fields)
	}
}

func writeResponse(w http.ResponseWriter, requestPath string, resp *http.Response) {
	defer resp.Body.Close()
	if err := maybeBackfillModelsDisplayNames(requestPath, resp); err != nil {
		resp.Body = io.NopCloser(strings.NewReader(`{"error":"invalid models response"}`))
		resp.StatusCode = http.StatusBadGateway
		resp.Header = make(http.Header)
		resp.Header.Set("Content-Type", "application/json")
	}
	copyHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func cloneHeaders(src http.Header) http.Header {
	out := make(http.Header, len(src))
	for k, v := range src {
		copied := make([]string, len(v))
		copy(copied, v)
		out[k] = copied
	}
	return out
}

func copyHeaders(dst, src http.Header) {
	for k, vv := range src {
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}

func isWebSocketUpgrade(r *http.Request) bool {
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		return false
	}
	for _, token := range strings.Split(r.Header.Get("Connection"), ",") {
		if strings.EqualFold(strings.TrimSpace(token), "upgrade") {
			return true
		}
	}
	return false
}
