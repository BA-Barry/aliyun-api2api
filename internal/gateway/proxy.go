package gateway

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"aliyun-api2api/internal/config"
)

type Protocol string

const (
	ProtocolOpenAI    Protocol = "openai"
	ProtocolAnthropic Protocol = "anthropic"
)

type Proxy struct {
	cfg         config.Config
	pool        *Pool
	client      *http.Client
	log         *slog.Logger
	globalSlots chan struct{}

	totalRequests  atomic.Uint64
	totalFailures  atomic.Uint64
	totalFailovers atomic.Uint64
	totalRejected  atomic.Uint64
	activeRequests atomic.Int64
}

type savedResponse struct {
	status int
	header http.Header
	body   []byte
}

func NewProxy(cfg config.Config, logger *slog.Logger) *Proxy {
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          256,
		MaxIdleConnsPerHost:   64,
		IdleConnTimeout:       cfg.IdleConnTimeout.Duration,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: cfg.ResponseHeaderTimeout.Duration,
		ExpectContinueTimeout: 1 * time.Second,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
	}
	return &Proxy{
		cfg:  cfg,
		pool: NewPool(cfg),
		client: &http.Client{
			Transport: transport,
			// Do not let a redirect forward a real upstream key to another host.
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		log:         logger,
		globalSlots: make(chan struct{}, cfg.MaxConcurrentRequests),
	}
}

func NewProxyWithClient(cfg config.Config, logger *slog.Logger, client *http.Client) *Proxy {
	p := NewProxy(cfg, logger)
	p.client = client
	return p
}

func (p *Proxy) Handler() http.Handler {
	return http.HandlerFunc(p.serveHTTP)
}

func (p *Proxy) serveHTTP(w http.ResponseWriter, r *http.Request) {
	requestID := requestID(r)
	w.Header().Set("X-Request-ID", requestID)
	switch r.URL.Path {
	case "/healthz":
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
		return
	case "/readyz":
		if p.ready() {
			writeJSON(w, http.StatusOK, map[string]any{"status": "ready"})
		} else {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"status": "unavailable"})
		}
		return
	case "/admin/status":
		p.serveStatus(w, r)
		return
	case "/metrics":
		p.serveMetrics(w, r)
		return
	}

	protocol, suffix, ok := route(r.URL.Path)
	if !ok {
		writeAPIError(w, http.StatusNotFound, "unsupported_path", "supported routes: /v1/*, /compatible-mode/v1/*, /apps/anthropic/*")
		return
	}
	if !p.authorized(r, p.cfg.ClientAPIKeys) {
		w.Header().Set("WWW-Authenticate", "Bearer")
		writeAPIError(w, http.StatusUnauthorized, "invalid_api_key", "invalid gateway API key")
		return
	}
	if r.Method == http.MethodConnect || r.Method == http.MethodTrace {
		writeAPIError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		return
	}
	select {
	case p.globalSlots <- struct{}{}:
		defer func() { <-p.globalSlots }()
	case <-r.Context().Done():
		return
	default:
		p.totalRejected.Add(1)
		w.Header().Set("Retry-After", "1")
		writeAPIError(w, http.StatusTooManyRequests, "concurrency_limit", "gateway concurrency limit reached")
		return
	}
	p.activeRequests.Add(1)
	defer p.activeRequests.Add(-1)

	body, err := readRequestBody(r.Body, p.cfg.MaxRequestBodyBytes)
	if err != nil {
		writeAPIError(w, http.StatusRequestEntityTooLarge, "request_too_large", err.Error())
		return
	}

	p.totalRequests.Add(1)
	started := time.Now()
	status, attempts, accountID := p.forward(w, r, requestID, protocol, suffix, body)
	p.log.Info("request completed",
		"request_id", requestID,
		"method", r.Method,
		"path", r.URL.Path,
		"protocol", protocol,
		"status", status,
		"attempts", attempts,
		"upstream_id", accountID,
		"duration_ms", time.Since(started).Milliseconds(),
	)
}

func route(path string) (Protocol, string, bool) {
	if path == "/apps/anthropic" || strings.HasPrefix(path, "/apps/anthropic/") {
		return ProtocolAnthropic, strings.TrimPrefix(path, "/apps/anthropic"), true
	}
	if path == "/anthropic" || strings.HasPrefix(path, "/anthropic/") {
		return ProtocolAnthropic, strings.TrimPrefix(path, "/anthropic"), true
	}
	if path == "/v1/messages" || strings.HasPrefix(path, "/v1/messages/") {
		return ProtocolAnthropic, path, true
	}
	if path == "/compatible-mode/v1" || strings.HasPrefix(path, "/compatible-mode/v1/") {
		return ProtocolOpenAI, strings.TrimPrefix(path, "/compatible-mode/v1"), true
	}
	if path == "/v1" || strings.HasPrefix(path, "/v1/") {
		return ProtocolOpenAI, strings.TrimPrefix(path, "/v1"), true
	}
	return "", "", false
}

func (p *Proxy) forward(w http.ResponseWriter, incoming *http.Request, requestID string, protocol Protocol, suffix string, body []byte) (int, int, string) {
	excluded := make(map[string]struct{})
	var last *savedResponse
	var lastErr error
	lastAccount := ""
	attempts := 0
	allBusy := false

	for attempts < p.cfg.MaxAttempts {
		lease, err := p.pool.Acquire(excluded)
		if err != nil {
			allBusy = errors.Is(err, ErrAllUpstreamsBusy)
			break
		}
		if attempts > 0 {
			// Count only a failover that actually acquired another upstream and
			// will make an additional attempt.
			p.totalFailovers.Add(1)
		}
		attempts++
		a := lease.Account
		lastAccount = a.ID
		excluded[a.ID] = struct{}{}

		resp, err := p.doAttempt(incoming.Context(), incoming, requestID, protocol, suffix, body, a)
		if err != nil {
			lease.Release()
			lastErr = err
			p.pool.MarkFailure(a, FailureTransient, "network_error")
			continue
		}

		if resp.StatusCode < 400 {
			p.pool.MarkSuccess(a)
			copyResponseHeaders(w.Header(), resp.Header)
			w.Header().Set("X-API2API-Attempts", strconv.Itoa(attempts))
			w.WriteHeader(resp.StatusCode)
			err = copyStream(w, resp.Body)
			resp.Body.Close()
			lease.Release()
			if err != nil && !errors.Is(err, context.Canceled) {
				p.totalFailures.Add(1)
			}
			return resp.StatusCode, attempts, lastAccount
		}

		errBody, readErr := io.ReadAll(io.LimitReader(resp.Body, p.cfg.MaxErrorBodyBytes+1))
		resp.Body.Close()
		lease.Release()
		if readErr != nil {
			lastErr = readErr
			p.pool.MarkFailure(a, FailureTransient, "error_body_read")
			continue
		}
		if int64(len(errBody)) > p.cfg.MaxErrorBodyBytes {
			errBody = errBody[:p.cfg.MaxErrorBodyBytes]
		}
		last = &savedResponse{status: resp.StatusCode, header: resp.Header.Clone(), body: errBody}
		retry, kind, reason := classifyUpstreamError(resp.StatusCode, errBody)
		if !retry {
			p.pool.MarkSuccess(a)
			writeSavedResponse(w, last, attempts)
			return resp.StatusCode, attempts, lastAccount
		}
		p.pool.MarkFailure(a, kind, reason)
	}

	if allBusy && last == nil && lastErr == nil {
		p.totalRejected.Add(1)
		w.Header().Set("Retry-After", "1")
		writeAPIError(w, http.StatusTooManyRequests, "upstream_concurrency_limit", "all upstream keys reached their concurrency limit")
		return http.StatusTooManyRequests, attempts, lastAccount
	}
	p.totalFailures.Add(1)
	if last != nil {
		writeSavedResponse(w, last, attempts)
		return last.status, attempts, lastAccount
	}
	message := "all upstream keys are unavailable or cooling down"
	if lastErr != nil {
		message = "upstream connection failed after failover"
	}
	writeAPIError(w, http.StatusServiceUnavailable, "upstream_unavailable", message)
	return http.StatusServiceUnavailable, attempts, lastAccount
}

func (p *Proxy) doAttempt(ctx context.Context, incoming *http.Request, requestID string, protocol Protocol, suffix string, body []byte, account *Account) (*http.Response, error) {
	base := account.OpenAIBaseURL
	if protocol == ProtocolAnthropic {
		base = account.AnthropicBaseURL
	}
	target, err := joinURL(base, suffix, incoming.URL.RawQuery)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, incoming.Method, target, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	copyRequestHeaders(req.Header, incoming.Header)
	req.Header.Set("X-Request-ID", requestID)
	req.Header.Del("Authorization")
	req.Header.Del("X-Api-Key")
	if protocol == ProtocolAnthropic {
		req.Header.Set("X-Api-Key", account.APIKey)
		if req.Header.Get("Anthropic-Version") == "" {
			req.Header.Set("Anthropic-Version", "2023-06-01")
		}
	} else {
		req.Header.Set("Authorization", "Bearer "+account.APIKey)
	}
	return p.client.Do(req)
}

func joinURL(base, suffix, rawQuery string) (string, error) {
	u, err := url.Parse(strings.TrimRight(base, "/"))
	if err != nil {
		return "", err
	}
	if suffix != "" && suffix != "/" {
		u.Path = strings.TrimRight(u.Path, "/") + "/" + strings.TrimLeft(suffix, "/")
	}
	u.RawQuery = rawQuery
	return u.String(), nil
}

func classifyUpstreamError(status int, body []byte) (bool, FailureKind, string) {
	lower := strings.ToLower(string(body))
	credentialMarkers := []string{
		"invalid_api_key", "invalidapikey", "invalid api key", "authentication", "unauthorized",
		"arrearage", "quota_exhausted", "quota exhausted", "insufficient balance",
		"insufficient credit", "account balance", "余额不足", "欠费",
	}
	for _, marker := range credentialMarkers {
		if strings.Contains(lower, marker) {
			return true, FailureCredential, "credential_or_balance"
		}
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden || status == http.StatusPaymentRequired {
		return true, FailureCredential, "credential_rejected"
	}
	if status == http.StatusTooManyRequests || status == http.StatusRequestTimeout || status >= 500 {
		return true, FailureTransient, "upstream_" + strconv.Itoa(status)
	}
	return false, FailureTransient, ""
}

func (p *Proxy) authorized(r *http.Request, keys []string) bool {
	candidates := []string{strings.TrimSpace(r.Header.Get("X-Api-Key"))}
	if h := strings.TrimSpace(r.Header.Get("Authorization")); len(h) > 7 && strings.EqualFold(h[:7], "Bearer ") {
		candidates = append(candidates, strings.TrimSpace(h[7:]))
	}
	for _, candidate := range candidates {
		if candidate != "" && config.ConstantTimeKeyMatch(candidate, keys) {
			return true
		}
	}
	return false
}

func (p *Proxy) serveStatus(w http.ResponseWriter, r *http.Request) {
	if p.cfg.AdminAPIKey == "" {
		http.NotFound(w, r)
		return
	}
	if !p.authorized(r, []string{p.cfg.AdminAPIKey}) {
		writeAPIError(w, http.StatusUnauthorized, "invalid_admin_key", "invalid admin API key")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"strategy":  p.cfg.Strategy,
		"upstreams": p.pool.Status(),
		"metrics": map[string]any{
			"requests_total":  p.totalRequests.Load(),
			"failures_total":  p.totalFailures.Load(),
			"failovers_total": p.totalFailovers.Load(),
			"rejected_total":  p.totalRejected.Load(),
			"active_requests": p.activeRequests.Load(),
		},
	})
}

func (p *Proxy) serveMetrics(w http.ResponseWriter, r *http.Request) {
	if p.cfg.AdminAPIKey == "" {
		http.NotFound(w, r)
		return
	}
	if !p.authorized(r, []string{p.cfg.AdminAPIKey}) {
		writeAPIError(w, http.StatusUnauthorized, "invalid_admin_key", "invalid admin API key")
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	fmt.Fprintf(w, "api2api_requests_total %d\n", p.totalRequests.Load())
	fmt.Fprintf(w, "api2api_failures_total %d\n", p.totalFailures.Load())
	fmt.Fprintf(w, "api2api_failovers_total %d\n", p.totalFailovers.Load())
	fmt.Fprintf(w, "api2api_rejected_total %d\n", p.totalRejected.Load())
	fmt.Fprintf(w, "api2api_active_requests %d\n", p.activeRequests.Load())
	for _, s := range p.pool.Status() {
		fmt.Fprintf(w, "api2api_upstream_inflight{upstream=%q} %d\n", s.ID, s.Inflight)
		fmt.Fprintf(w, "api2api_upstream_failures_total{upstream=%q} %d\n", s.ID, s.Failures)
	}
}

func (p *Proxy) ready() bool {
	now := time.Now()
	for _, s := range p.pool.Status() {
		if s.Enabled && !now.Before(s.CooldownUntil) {
			return true
		}
	}
	return false
}

func requestID(r *http.Request) string {
	if v := strings.TrimSpace(r.Header.Get("X-Request-ID")); v != "" && len(v) <= 128 {
		return v
	}
	b := make([]byte, 12)
	if _, err := rand.Read(b); err == nil {
		return hex.EncodeToString(b)
	}
	return strconv.FormatInt(time.Now().UnixNano(), 36)
}

func readRequestBody(body io.ReadCloser, max int64) ([]byte, error) {
	if body == nil {
		return nil, nil
	}
	defer body.Close()
	b, err := io.ReadAll(io.LimitReader(body, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, fmt.Errorf("request body exceeds %d bytes", max)
	}
	return b, nil
}

var hopByHopHeaders = map[string]struct{}{
	"Connection": {}, "Proxy-Connection": {}, "Keep-Alive": {},
	"Proxy-Authenticate": {}, "Proxy-Authorization": {}, "Te": {},
	"Trailer": {}, "Transfer-Encoding": {}, "Upgrade": {},
}

func copyRequestHeaders(dst, src http.Header) {
	blocked := blockedHeaders(src)
	for key, values := range src {
		if _, skip := blocked[http.CanonicalHeaderKey(key)]; skip {
			continue
		}
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

func copyResponseHeaders(dst, src http.Header) {
	blocked := blockedHeaders(src)
	for key, values := range src {
		if _, skip := blocked[http.CanonicalHeaderKey(key)]; skip {
			continue
		}
		dst.Del(key)
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

func blockedHeaders(src http.Header) map[string]struct{} {
	blocked := make(map[string]struct{}, len(hopByHopHeaders)+4)
	for key := range hopByHopHeaders {
		blocked[key] = struct{}{}
	}
	// RFC 9110: Connection may nominate additional hop-by-hop fields.
	for _, value := range src.Values("Connection") {
		for _, token := range strings.Split(value, ",") {
			if token = strings.TrimSpace(token); token != "" {
				blocked[http.CanonicalHeaderKey(token)] = struct{}{}
			}
		}
	}
	return blocked
}

func copyStream(w http.ResponseWriter, src io.Reader) error {
	buf := make([]byte, 32<<10)
	flusher, canFlush := w.(http.Flusher)
	for {
		n, readErr := src.Read(buf)
		if n > 0 {
			if _, err := w.Write(buf[:n]); err != nil {
				return err
			}
			if canFlush {
				flusher.Flush()
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return nil
			}
			return readErr
		}
	}
}

func writeSavedResponse(w http.ResponseWriter, resp *savedResponse, attempts int) {
	copyResponseHeaders(w.Header(), resp.header)
	// The gateway caps error bodies, so an upstream Content-Length may no longer be valid.
	w.Header().Del("Content-Length")
	w.Header().Set("X-API2API-Attempts", strconv.Itoa(attempts))
	w.WriteHeader(resp.status)
	_, _ = w.Write(resp.body)
}

func writeAPIError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{"type": code, "message": message}})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
