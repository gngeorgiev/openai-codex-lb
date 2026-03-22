package lb

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"
)

const childProxyStatusCacheTTL = time.Second

type childProxyRuntime struct {
	CooldownUntilMS  int64
	LastUsedAtMS     int64
	LastSwitchReason string
	LastError        string
	Reachable        bool
	Status           ProxyStatus
	StatusFetchedAt  time.Time
}

type childProxyTarget struct {
	Name             string
	URL              string
	Active           bool
	Healthy          bool
	Reachable        bool
	CooldownUntilMS  int64
	LastUsedAtMS     int64
	Score            float64
	SelectedTarget   string
	SelectionReason  string
	LastSwitchReason string
	LastError        string
	Accounts         []AccountStatus
}

type proxyCapacitySummary struct {
	Score          float64
	SelectedTarget string
}

func hasChildProxyRouting(snapshot StoreFile) bool {
	return len(snapshot.Settings.Proxy.ChildProxyURLs) > 0
}

func (p *ProxyServer) expireChildProxyCooldowns(now time.Time) {
	p.childProxyMu.Lock()
	defer p.childProxyMu.Unlock()

	for url, state := range p.childProxyStates {
		if state.CooldownUntilMS > 0 && state.CooldownUntilMS <= now.UnixMilli() {
			state.CooldownUntilMS = 0
			p.childProxyStates[url] = state
		}
	}
}

func (p *ProxyServer) selectChildProxy(ctx context.Context, snapshot StoreFile, now time.Time, forceRefresh bool) (Selection, childProxyTarget, error) {
	targets := p.childProxyTargets(ctx, snapshot, now, forceRefresh)
	if len(targets) == 0 {
		return Selection{}, childProxyTarget{}, fmt.Errorf("no child proxies configured")
	}
	sel, target, ok := p.selectChildProxyFromTargets(snapshot, now, targets)
	if !ok {
		return Selection{}, childProxyTarget{}, fmt.Errorf("no healthy child proxies")
	}
	return sel, target, nil
}

func (p *ProxyServer) childProxyTargets(ctx context.Context, snapshot StoreFile, now time.Time, forceRefresh bool) []childProxyTarget {
	urls := snapshot.Settings.Proxy.ChildProxyURLs
	if len(urls) == 0 {
		return nil
	}

	p.childProxyMu.Lock()
	activeURL := p.childProxyActiveURL
	states := make(map[string]childProxyRuntime, len(p.childProxyStates))
	for url, state := range p.childProxyStates {
		states[url] = state
	}
	p.childProxyMu.Unlock()

	type result struct {
		idx    int
		target childProxyTarget
	}
	ch := make(chan result, len(urls))
	for idx, proxyURL := range urls {
		state := states[proxyURL]
		go func(idx int, proxyURL string, state childProxyRuntime) {
			target := childProxyTargetFromState(proxyURL, activeURL, state)

			status, err := p.fetchChildProxyStatus(ctx, proxyURL, forceRefresh)
			if err != nil {
				target.Healthy = false
				target.Reachable = false
				target.LastError = err.Error()
				ch <- result{idx: idx, target: target}
				return
			}
			target = applyChildProxyStatus(target, status, now)
			ch <- result{idx: idx, target: target}
		}(idx, proxyURL, state)
	}

	out := make([]childProxyTarget, len(urls))
	for range urls {
		res := <-ch
		out[res.idx] = res.target
	}
	return out
}

func childProxyTargetFromState(proxyURL, activeURL string, state childProxyRuntime) childProxyTarget {
	return childProxyTarget{
		URL:              proxyURL,
		Active:           proxyURL == activeURL,
		CooldownUntilMS:  state.CooldownUntilMS,
		LastUsedAtMS:     state.LastUsedAtMS,
		LastSwitchReason: state.LastSwitchReason,
		LastError:        state.LastError,
		Reachable:        state.Reachable,
	}
}

func applyChildProxyStatus(target childProxyTarget, status ProxyStatus, now time.Time) childProxyTarget {
	summary, ok := summarizeProxyCapacity(status)
	target.Name = strings.TrimSpace(status.ProxyName)
	target.Accounts = append([]AccountStatus(nil), status.Accounts...)
	target.Reachable = true
	target.SelectionReason = status.SelectionReason
	target.SelectedTarget = status.SelectedAccountID
	if target.SelectedTarget == "" {
		target.SelectedTarget = status.SelectedProxyURL
	}
	if ok {
		target.Score = summary.Score
		target.Healthy = target.CooldownUntilMS <= now.UnixMilli()
		target.LastError = ""
		return target
	}
	target.Healthy = false
	target.LastError = "child proxy has no healthy targets"
	return target
}

func (p *ProxyServer) fetchChildProxyStatus(ctx context.Context, proxyURL string, forceRefresh bool) (ProxyStatus, error) {
	now := time.Now()

	p.childProxyMu.Lock()
	state := p.childProxyStates[proxyURL]
	if !forceRefresh && state.Reachable && !state.StatusFetchedAt.IsZero() && now.Sub(state.StatusFetchedAt) < childProxyStatusCacheTTL {
		status := state.Status
		p.childProxyMu.Unlock()
		return status, nil
	}
	p.childProxyMu.Unlock()

	timeout := time.Duration(max(1, p.store.Snapshot().Settings.Proxy.UsageTimeoutMS)) * time.Millisecond
	reqCtx := ctx
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		reqCtx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, strings.TrimRight(proxyURL, "/")+"/status?refresh=1", nil)
	if err != nil {
		return ProxyStatus{}, err
	}
	resp, err := p.usageClient.Do(req)
	if err != nil {
		p.recordChildProxyStatus(proxyURL, ProxyStatus{}, time.Now(), false, err.Error())
		return ProxyStatus{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		p.recordChildProxyStatus(proxyURL, ProxyStatus{}, time.Now(), false, fmt.Sprintf("status %d", resp.StatusCode))
		return ProxyStatus{}, fmt.Errorf("query child proxy status %s: status %d", proxyURL, resp.StatusCode)
	}

	var status ProxyStatus
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		p.recordChildProxyStatus(proxyURL, ProxyStatus{}, time.Now(), false, err.Error())
		return ProxyStatus{}, err
	}
	p.recordChildProxyStatus(proxyURL, status, time.Now(), true, "")
	return status, nil
}

func (p *ProxyServer) recordChildProxyStatus(proxyURL string, status ProxyStatus, fetchedAt time.Time, reachable bool, lastError string) {
	p.childProxyMu.Lock()
	defer p.childProxyMu.Unlock()

	state := p.childProxyStates[proxyURL]
	state.Reachable = reachable
	state.LastError = strings.TrimSpace(lastError)
	state.StatusFetchedAt = fetchedAt
	if reachable {
		state.Status = status
	}
	p.childProxyStates[proxyURL] = state
}

func (p *ProxyServer) markChildProxySuccess(sel Selection, proxyURL string, now time.Time) {
	p.childProxyMu.Lock()
	defer p.childProxyMu.Unlock()

	state := p.childProxyStates[proxyURL]
	state.CooldownUntilMS = 0
	state.LastUsedAtMS = now.UnixMilli()
	state.LastSwitchReason = sel.SwitchReason
	state.LastError = ""
	state.Reachable = true
	p.childProxyStates[proxyURL] = state
	p.childProxyActiveURL = proxyURL
}

func (p *ProxyServer) markChildProxyCooldown(proxyURL string, cooldownSeconds int, reason, lastError string) {
	p.childProxyMu.Lock()
	defer p.childProxyMu.Unlock()

	if cooldownSeconds <= 0 {
		cooldownSeconds = 1
	}
	state := p.childProxyStates[proxyURL]
	state.CooldownUntilMS = time.Now().Add(time.Duration(cooldownSeconds) * time.Second).UnixMilli()
	state.LastSwitchReason = reason
	state.LastError = strings.TrimSpace(lastError)
	p.childProxyStates[proxyURL] = state
}

func summarizeProxyCapacity(status ProxyStatus) (proxyCapacitySummary, bool) {
	best := -1.0
	for _, account := range status.Accounts {
		if !account.Healthy || !account.Enabled || account.DisabledReason != "" {
			continue
		}
		if account.Score > best {
			best = account.Score
		}
	}
	for _, child := range status.ChildProxies {
		if !child.Healthy {
			continue
		}
		if child.Score > best {
			best = child.Score
		}
	}
	if best < 0 {
		return proxyCapacitySummary{}, false
	}

	selectedTarget := strings.TrimSpace(status.SelectedAccountID)
	if selectedTarget == "" {
		selectedTarget = strings.TrimSpace(status.SelectedProxyURL)
	}
	return proxyCapacitySummary{
		Score:          best,
		SelectedTarget: selectedTarget,
	}, true
}

func childProxyStatusView(target childProxyTarget, now time.Time) ChildProxyStatus {
	cooldownSeconds := int64(0)
	if target.CooldownUntilMS > now.UnixMilli() {
		cooldownSeconds = (target.CooldownUntilMS - now.UnixMilli() + 999) / 1000
	}
	name := target.Name
	if strings.TrimSpace(name) == "" {
		name = target.URL
	}
	return ChildProxyStatus{
		Name:             name,
		URL:              target.URL,
		Active:           target.Active,
		Healthy:          target.Healthy,
		Reachable:        target.Reachable,
		CooldownSeconds:  cooldownSeconds,
		Score:            target.Score,
		SelectedTarget:   target.SelectedTarget,
		SelectionReason:  target.SelectionReason,
		LastSwitchReason: target.LastSwitchReason,
		LastError:        target.LastError,
	}
}

func clearManagedProxyHeaders(headers http.Header) {
	headers.Del("Authorization")
	headers.Del("authorization")
	headers.Del("ChatGPT-Account-Id")
	headers.Del("chatgpt-account-id")
}

func rewriteForChildProxy(src *url.URL, proxyURL string) (*url.URL, error) {
	base, err := url.Parse(proxyURL)
	if err != nil {
		return nil, fmt.Errorf("parse child proxy url %q: %w", proxyURL, err)
	}

	next := *src
	next.Scheme = base.Scheme
	next.Host = base.Host
	basePath := strings.TrimSuffix(base.Path, "/")
	if basePath != "" && basePath != "/" {
		next.Path = joinPath(basePath, src.Path)
		if src.RawPath != "" {
			next.RawPath = joinPath(basePath, src.RawPath)
		}
	}
	return &next, nil
}

func (p *ProxyServer) buildStatus(ctx context.Context, snapshot StoreFile, now time.Time, refreshChildProxies bool) ProxyStatus {
	status := BuildProxyStatus(snapshot, now)
	if !hasChildProxyRouting(snapshot) {
		return status
	}
	status.SelectedAccountID = ""
	status.SelectionReason = ""
	for i := range status.Accounts {
		status.Accounts[i].Active = false
	}

	targets := p.cachedChildProxyTargets(snapshot, now)
	if refreshChildProxies {
		targets = p.childProxyTargets(ctx, snapshot, now, true)
	}
	sel, selectedTarget, hasSelected := p.selectChildProxyFromTargets(snapshot, now, targets)
	status.ChildProxies = make([]ChildProxyStatus, 0, len(targets))
	for _, target := range targets {
		for _, account := range target.Accounts {
			if strings.TrimSpace(account.ProxyName) == "" {
				account.ProxyName = target.Name
			}
			if !hasSelected || target.URL != selectedTarget.URL {
				account.Active = false
			}
			status.Accounts = append(status.Accounts, account)
		}
		status.ChildProxies = append(status.ChildProxies, childProxyStatusView(target, now))
	}
	sortAccountStatuses(status.Accounts)
	if hasSelected {
		status.SelectedProxyURL = selectedTarget.URL
		status.SelectedProxyName = selectedTarget.Name
		if status.SelectionReason == "" {
			status.SelectionReason = sel.SwitchReason
		}
	}
	return status
}

func (p *ProxyServer) cachedChildProxyTargets(snapshot StoreFile, now time.Time) []childProxyTarget {
	urls := snapshot.Settings.Proxy.ChildProxyURLs
	if len(urls) == 0 {
		return nil
	}

	p.childProxyMu.Lock()
	activeURL := p.childProxyActiveURL
	states := make(map[string]childProxyRuntime, len(p.childProxyStates))
	for url, state := range p.childProxyStates {
		states[url] = state
	}
	p.childProxyMu.Unlock()

	out := make([]childProxyTarget, 0, len(urls))
	for _, proxyURL := range urls {
		state := states[proxyURL]
		target := childProxyTargetFromState(proxyURL, activeURL, state)
		if state.Reachable && !state.StatusFetchedAt.IsZero() {
			target = applyChildProxyStatus(target, state.Status, now)
		}
		out = append(out, target)
	}
	return out
}

func (p *ProxyServer) refreshChildProxyStatusCache(snapshot StoreFile, now time.Time) {
	if !p.childProxyRefreshInFlight.CompareAndSwap(false, true) {
		return
	}
	defer p.childProxyRefreshInFlight.Store(false)

	_ = p.childProxyTargets(context.Background(), snapshot, now, true)
}

func (p *ProxyServer) selectChildProxyFromTargets(snapshot StoreFile, now time.Time, targets []childProxyTarget) (Selection, childProxyTarget, bool) {
	if len(targets) == 0 {
		return Selection{}, childProxyTarget{}, false
	}

	p.childProxyMu.Lock()
	activeURL := p.childProxyActiveURL
	p.childProxyMu.Unlock()

	activeIndex := 0
	fakeAccounts := make([]Account, 0, len(targets))
	nowMS := now.UnixMilli()
	for i, target := range targets {
		if target.URL == activeURL {
			activeIndex = i
		}
		cooldownUntil := target.CooldownUntilMS
		if !target.Reachable || !target.Healthy {
			if cooldownUntil <= nowMS {
				cooldownUntil = nowMS + 1
			}
		}
		remaining := clamp01(target.Score) * 100
		fakeAccounts = append(fakeAccounts, Account{
			ID:               target.URL,
			Alias:            target.URL,
			Enabled:          true,
			CooldownUntilMS:  cooldownUntil,
			LastUsedAtMS:     target.LastUsedAtMS,
			LastSwitchReason: target.LastSwitchReason,
			Quota: QuotaState{
				DailyLimit:  100,
				DailyUsed:   100 - remaining,
				WeeklyLimit: 100,
				WeeklyUsed:  100 - remaining,
				Source:      "child_proxy",
			},
		})
	}

	fake := StoreFile{
		Settings: Settings{
			Policy: snapshot.Settings.Policy,
		},
		State: RuntimeState{
			ActiveIndex: activeIndex,
		},
		Accounts: fakeAccounts,
	}
	sel, err := selectAccount(&fake, nowMS)
	if err != nil || sel.Index < 0 || sel.Index >= len(targets) {
		return Selection{}, childProxyTarget{}, false
	}
	return sel, targets[sel.Index], true
}

func (p *ProxyServer) handleHTTPViaChildProxies(w http.ResponseWriter, r *http.Request, body []byte, snapshot StoreFile, now time.Time, reqID uint64) {
	maxAttempts := max(1, snapshot.Settings.Proxy.MaxAttempts)

	var lastResp *http.Response
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		sel, child, err := p.selectChildProxy(r.Context(), snapshot, now, attempt == 1)
		if err != nil {
			lastErr = err
			p.logEvent("request.selection_failed", map[string]any{
				"req_id":  reqID,
				"attempt": attempt,
				"error":   err.Error(),
			})
			break
		}

		targetURL, err := rewriteForChildProxy(r.URL, child.URL)
		if err != nil {
			lastErr = err
			break
		}

		upstreamReq, err := http.NewRequestWithContext(r.Context(), r.Method, targetURL.String(), bytes.NewReader(body))
		if err != nil {
			lastErr = err
			break
		}
		upstreamReq.Header = cloneHeaders(r.Header)
		clearManagedProxyHeaders(upstreamReq.Header)
		upstreamReq.Host = targetURL.Host

		resp, err := p.requestClient.Do(upstreamReq)
		if err != nil {
			if isCanceledRequest(r.Context(), err) {
				p.logEvent("request.canceled", map[string]any{
					"req_id":          reqID,
					"attempt":         attempt,
					"child_proxy_url": child.URL,
					"method":          r.Method,
					"path":            r.URL.Path,
					"error":           err.Error(),
				})
				return
			}
			p.markChildProxyCooldown(child.URL, snapshot.Settings.Proxy.CooldownDefaultS, "transport-error", err.Error())
			lastErr = err
			p.logEvent("request.transport_error", map[string]any{
				"req_id":          reqID,
				"attempt":         attempt,
				"child_proxy_url": child.URL,
				"error":           err.Error(),
			})
			continue
		}

		if isRetryableChildProxyStatus(resp.StatusCode) {
			retryAfter := parseRetryAfterSeconds(resp.Header)
			cooldown := defaultBackoffSeconds(resp.StatusCode, retryAfter)
			p.markChildProxyCooldown(child.URL, cooldown, fmt.Sprintf("http-%d", resp.StatusCode), fmt.Sprintf("status %d", resp.StatusCode))
			p.logEvent("child_proxy.cooldown", map[string]any{
				"req_id":             reqID,
				"attempt":            attempt,
				"child_proxy_url":    child.URL,
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
			p.markChildProxySuccess(sel, child.URL, now)
		}
		p.logEvent("request.completed", map[string]any{
			"req_id":          reqID,
			"status":          resp.StatusCode,
			"child_proxy_url": child.URL,
			"attempt":         attempt,
		})
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
	http.Error(w, "no child proxy available", http.StatusServiceUnavailable)
}

func (p *ProxyServer) handleWebsocketViaChildProxy(w http.ResponseWriter, r *http.Request, snapshot StoreFile, now time.Time, reqID uint64) {
	sel, child, err := p.selectChildProxy(r.Context(), snapshot, now, true)
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}

	targetURL, err := rewriteForChildProxy(r.URL, child.URL)
	if err != nil {
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
		clearManagedProxyHeaders(req.Header)
	}
	proxy.ModifyResponse = func(resp *http.Response) error {
		if isRetryableChildProxyStatus(resp.StatusCode) {
			retryAfter := parseRetryAfterSeconds(resp.Header)
			cooldown := defaultBackoffSeconds(resp.StatusCode, retryAfter)
			p.markChildProxyCooldown(child.URL, cooldown, fmt.Sprintf("http-%d", resp.StatusCode), fmt.Sprintf("status %d", resp.StatusCode))
		} else if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			p.markChildProxySuccess(sel, child.URL, now)
		}
		return nil
	}
	proxy.ErrorHandler = func(rw http.ResponseWriter, req *http.Request, proxyErr error) {
		p.markChildProxyCooldown(child.URL, snapshot.Settings.Proxy.CooldownDefaultS, "websocket-proxy-error", proxyErr.Error())
		http.Error(rw, proxyErr.Error(), http.StatusBadGateway)
	}
	proxy.ServeHTTP(w, r)
}

func isRetryableChildProxyStatus(status int) bool {
	if status == http.StatusUnauthorized || status == http.StatusForbidden || status == http.StatusServiceUnavailable {
		return true
	}
	return isRetryableStatus(status)
}
