package lb

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"time"
)

type ProxyServer struct {
	store *Store

	requestClient *http.Client
	usageClient   *http.Client
	logger        *log.Logger
	events        *EventLogger

	refreshInFlight atomic.Bool
	requestSeq      atomic.Uint64
}

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
		logger: logger,
		events: events,
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

	now := time.Now()
	p.expireCooldowns(now)
	p.maybeRefreshQuota(r.Context(), now)

	if isWebSocketUpgrade(r) {
		p.handleWebsocket(w, r, now, reqID)
		return
	}
	p.handleHTTP(w, r, now, reqID)
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

		if isAuthStatus(resp.StatusCode) {
			_ = resp.Body.Close()
			p.markDisabled(account.ID, fmt.Sprintf("http-%d", resp.StatusCode))
			lastResp = resp
			p.logEvent("account.disabled", map[string]any{
				"req_id":     reqID,
				"attempt":    attempt,
				"account_id": account.ID,
				"status":     resp.StatusCode,
			})
			continue
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
		if isAuthStatus(resp.StatusCode) {
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

func (p *ProxyServer) maybeRefreshQuota(ctx context.Context, now time.Time) {
	if !p.refreshInFlight.CompareAndSwap(false, true) {
		return
	}
	defer p.refreshInFlight.Store(false)

	snapshot := p.store.Snapshot()
	for _, account := range snapshot.Accounts {
		if !account.Enabled || account.DisabledReason != "" {
			continue
		}
		if !dueForQuotaRefresh(account, snapshot.State, snapshot.Settings.Quota, now) {
			continue
		}
		auth, err := LoadAuth(account.HomeDir)
		if err != nil {
			p.markDisabled(account.ID, "missing-auth")
			continue
		}
		timeout := time.Duration(max(1, snapshot.Settings.Proxy.UsageTimeoutMS)) * time.Millisecond
		refreshCtx, cancel := context.WithTimeout(ctx, timeout)
		updated := account
		err = refreshQuotaForAccount(refreshCtx, p.usageClient, &updated, auth, now)
		cancel()
		if err != nil {
			p.logEvent("quota.refresh_failed", map[string]any{
				"account_id": account.ID,
				"error":      err.Error(),
			})
			if p.logger != nil {
				p.logger.Printf("quota refresh failed for %s: %v", account.ID, err)
			}
			continue
		}
		updated.Quota.LastSyncMessageCounter = snapshot.State.MessageCounter
		updated.ChatGPTAccountID = auth.ChatGPTAccountID
		updated.UserEmail = auth.UserEmail

		_ = p.store.Update(func(sf *StoreFile) error {
			idx := slices.IndexFunc(sf.Accounts, func(a Account) bool { return a.ID == account.ID })
			if idx < 0 {
				return nil
			}
			sf.Accounts[idx].Quota = updated.Quota
			sf.Accounts[idx].ChatGPTAccountID = updated.ChatGPTAccountID
			sf.Accounts[idx].UserEmail = updated.UserEmail
			return nil
		})
		p.logEvent("quota.refreshed", map[string]any{
			"account_id":   account.ID,
			"daily_limit":  updated.Quota.DailyLimit,
			"daily_used":   updated.Quota.DailyUsed,
			"weekly_limit": updated.Quota.WeeklyLimit,
			"weekly_used":  updated.Quota.WeeklyUsed,
		})
	}
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
