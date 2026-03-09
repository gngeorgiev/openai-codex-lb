package lb

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
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

	refreshInFlight atomic.Bool
	requestSeq      atomic.Uint64
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
		logger:       logger,
		events:       events,
		authTokenURL: defaultAuthTokenURL,
		authClientID: defaultAuthClientID,
	}
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
		p.maybeRefreshQuota(r.Context(), now, true)
		snapshot := p.store.Snapshot()
		status := BuildProxyStatus(snapshot, now)
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
	p.maybeRefreshQuota(r.Context(), now, false)

	if isWebSocketUpgrade(r) {
		p.handleWebsocket(w, r, now, reqID)
		return
	}
	p.handleHTTP(w, r, now, reqID)
}

func (p *ProxyServer) handleAdmin(w http.ResponseWriter, r *http.Request) int {
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
		rawAuth, err := os.ReadFile(filepath.Join(account.HomeDir, "auth.json"))
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("read auth for %s: %v", account.Alias, err))
			return http.StatusBadRequest
		}
		if !json.Valid(rawAuth) {
			writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("invalid auth.json for %s", account.Alias))
			return http.StatusBadRequest
		}
		writeJSON(w, http.StatusOK, AdminRuntimeAuthResponse{
			Auth:        json.RawMessage(rawAuth),
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
		if strings.TrimSpace(req.Alias) == "" || strings.TrimSpace(req.FromHome) == "" {
			writeJSONError(w, http.StatusBadRequest, "alias and from_home are required")
			return http.StatusBadRequest
		}
		if err := ImportAccount(p.store, req.Alias, req.FromHome); err != nil {
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
	writeJSON(w, status, map[string]any{"error": message})
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
			lastErr = err
			p.logEvent("request.rewrite_failed", map[string]any{
				"req_id":  reqID,
				"attempt": attempt,
				"error":   err.Error(),
			})
			break
		}

		upstreamReq, err := http.NewRequestWithContext(r.Context(), r.Method, targetURL.String(), bytes.NewReader(body))
		if err != nil {
			lastErr = err
			break
		}
		upstreamReq.Header = cloneHeaders(r.Header)
		setAccountHeaders(upstreamReq.Header, auth)
		upstreamReq.Host = targetURL.Host

		resp, err := p.requestClient.Do(upstreamReq)
		if err != nil {
			p.markCooldown(account.ID, 0, snapshot.Settings.Proxy.CooldownDefaultS, "transport-error")
			lastErr = err
			p.logEvent("request.transport_error", map[string]any{
				"req_id":     reqID,
				"attempt":    attempt,
				"account_id": account.ID,
				"error":      err.Error(),
			})
			continue
		}

		if shouldDisableForAuthFailure(resp.StatusCode, r.URL.Path) {
			bodySnippet := readBodySnippet(resp.Body, maxDisableBodyLogBytes)
			_ = resp.Body.Close()
			refreshedAuth, refreshed, refreshErr := p.tryRefreshAccountAuth(r.Context(), account, auth)
			if refreshErr == nil && refreshed {
				upstreamReq, err = http.NewRequestWithContext(r.Context(), r.Method, targetURL.String(), bytes.NewReader(body))
				if err != nil {
					lastErr = err
					break
				}
				upstreamReq.Header = cloneHeaders(r.Header)
				setAccountHeaders(upstreamReq.Header, refreshedAuth)
				upstreamReq.Host = targetURL.Host
				resp, err = p.requestClient.Do(upstreamReq)
				if err != nil {
					p.markCooldown(account.ID, 0, snapshot.Settings.Proxy.CooldownDefaultS, "transport-error")
					lastErr = err
					p.logEvent("request.transport_error", map[string]any{
						"req_id":     reqID,
						"attempt":    attempt,
						"account_id": account.ID,
						"error":      err.Error(),
					})
					continue
				}
				if !shouldDisableForAuthFailure(resp.StatusCode, r.URL.Path) {
					auth = refreshedAuth
				}
			}
			if refreshErr != nil || shouldDisableForAuthFailure(resp.StatusCode, r.URL.Path) {
				p.markDisabled(account.ID, fmt.Sprintf("http-%d", resp.StatusCode))
				lastResp = resp
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
				continue
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
				lastResp = resp
				continue
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
		writeResponse(w, resp)
		return
	}

	if lastResp != nil {
		p.logEvent("request.completed", map[string]any{
			"req_id": reqID,
			"status": lastResp.StatusCode,
		})
		writeResponse(w, lastResp)
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

func (p *ProxyServer) handleWebsocket(w http.ResponseWriter, r *http.Request, now time.Time, reqID uint64) {
	sel, account, auth, err := p.pickAccount(now)
	if err != nil {
		p.logEvent("websocket.selection_failed", map[string]any{
			"req_id": reqID,
			"error":  err.Error(),
		})
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
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
			p.markDisabled(account.ID, fmt.Sprintf("http-%d", resp.StatusCode))
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
	auth, err := LoadAuth(account.HomeDir)
	if err != nil {
		p.markDisabled(account.ID, "missing-auth")
		return Selection{}, Account{}, AuthInfo{}, err
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
	return sel, account, auth, nil
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

func isAuthFailureReason(reason string) bool {
	return reason == "http-401"
}

func (p *ProxyServer) logEvent(event string, fields map[string]any) {
	if p.events != nil {
		p.events.Log(event, fields)
	}
}

func writeResponse(w http.ResponseWriter, resp *http.Response) {
	defer resp.Body.Close()
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
