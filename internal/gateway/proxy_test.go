package gateway

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"aliyun-api2api/internal/config"
)

const testGatewayKey = "sk-gateway-0123456789abcdef0123456789"

func testConfig(t *testing.T, upstreamURL string, keys ...string) config.Config {
	t.Helper()
	cfg := config.Defaults()
	cfg.ClientAPIKeys = []string{testGatewayKey}
	cfg.AdminAPIKey = "sk-admin-0123456789abcdef0123456789"
	cfg.AllowInsecureUpstream = true
	cfg.MaxAttempts = len(keys)
	cfg.BaseCooldown = config.Duration{Duration: 10 * time.Millisecond}
	cfg.CredentialCooldown = config.Duration{Duration: time.Minute}
	for i, key := range keys {
		cfg.Upstreams = append(cfg.Upstreams, config.Upstream{
			ID:               "key-" + string(rune('a'+i)),
			APIKey:           key,
			OpenAIBaseURL:    upstreamURL + "/compatible-mode/v1",
			AnthropicBaseURL: upstreamURL + "/apps/anthropic",
			Weight:           1,
		})
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("invalid test config: %v", err)
	}
	return cfg
}

func newTestProxy(cfg config.Config, client *http.Client) *Proxy {
	return NewProxyWithClient(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), client)
}

func TestOpenAIFailoverRewritesCredentialAndPreservesPath(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got, want := r.URL.RequestURI(), "/compatible-mode/v1/chat/completions?preview=1"; got != want {
			t.Errorf("request URI = %q, want %q", got, want)
		}
		if r.Header.Get("X-Api-Key") != "" {
			t.Errorf("client x-api-key leaked upstream")
		}
		if r.Header.Get("X-Secret-Hop") != "" {
			t.Errorf("Connection-nominated hop-by-hop header leaked upstream")
		}
		auth := r.Header.Get("Authorization")
		mu.Lock()
		seen = append(seen, auth)
		mu.Unlock()
		if auth == "Bearer real-key-a" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"message":"rate limit"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	cfg := testConfig(t, upstream.URL, "real-key-a", "real-key-b")
	proxy := newTestProxy(cfg, upstream.Client())
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions?preview=1", strings.NewReader(`{"model":"qwen-plus"}`))
	req.Header.Set("Authorization", "Bearer "+testGatewayKey)
	req.Header.Set("X-Api-Key", testGatewayKey)
	req.Header.Set("Connection", "X-Secret-Hop")
	req.Header.Set("X-Secret-Hop", "must-not-forward")
	rec := httptest.NewRecorder()

	proxy.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("X-API2API-Attempts") != "2" {
		t.Fatalf("attempt header = %q", rec.Header().Get("X-API2API-Attempts"))
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 || seen[0] != "Bearer real-key-a" || seen[1] != "Bearer real-key-b" {
		t.Fatalf("upstream credentials = %#v", seen)
	}
}

func TestFailoverMetricCountsOnlyRealAdditionalAttempts(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"rate limit"}}`))
	}))
	defer upstream.Close()

	cfg := testConfig(t, upstream.URL, "real-key-a", "real-key-b")
	cfg.MaxAttempts = 3 // More attempts than available distinct upstreams.
	proxy := newTestProxy(cfg, upstream.Client())
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer "+testGatewayKey)
	rec := httptest.NewRecorder()
	proxy.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("X-API2API-Attempts") != "2" {
		t.Fatalf("attempts = %q", rec.Header().Get("X-API2API-Attempts"))
	}
	if got := proxy.totalFailovers.Load(); got != 1 {
		t.Fatalf("failovers = %d, want 1", got)
	}
}

func TestAnthropicPathAndCredential(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got, want := r.URL.Path, "/apps/anthropic/v1/messages"; got != want {
			t.Errorf("path = %q, want %q", got, want)
		}
		if got := r.Header.Get("X-Api-Key"); got != "real-anthropic-key" {
			t.Errorf("x-api-key = %q", got)
		}
		if got := r.Header.Get("Authorization"); got != "" {
			t.Errorf("authorization should be removed, got %q", got)
		}
		if got := r.Header.Get("Anthropic-Version"); got != "2023-06-01" {
			t.Errorf("anthropic-version = %q", got)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: message_stop\ndata: {}\n\n"))
	}))
	defer upstream.Close()

	cfg := testConfig(t, upstream.URL, "real-anthropic-key")
	proxy := newTestProxy(cfg, upstream.Client())
	req := httptest.NewRequest(http.MethodPost, "/apps/anthropic/v1/messages", strings.NewReader(`{"model":"qwen-plus"}`))
	req.Header.Set("X-Api-Key", testGatewayKey)
	rec := httptest.NewRecorder()
	proxy.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "message_stop") {
		t.Fatalf("stream body = %q", rec.Body.String())
	}
}

func TestV1MessagesUsesAnthropicProtocol(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got, want := r.URL.Path, "/apps/anthropic/v1/messages/count_tokens"; got != want {
			t.Errorf("path = %q, want %q", got, want)
		}
		_, _ = w.Write([]byte(`{"input_tokens":1}`))
	}))
	defer upstream.Close()
	cfg := testConfig(t, upstream.URL, "real-key")
	proxy := newTestProxy(cfg, upstream.Client())
	req := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer "+testGatewayKey)
	rec := httptest.NewRecorder()
	proxy.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func TestUnauthorizedRequestNeverReachesUpstream(t *testing.T) {
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++ }))
	defer upstream.Close()
	cfg := testConfig(t, upstream.URL, "real-key")
	proxy := newTestProxy(cfg, upstream.Client())
	rec := httptest.NewRecorder()
	proxy.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`)))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d", rec.Code)
	}
	if calls != 0 {
		t.Fatalf("upstream calls = %d", calls)
	}
}

func TestConcurrencyLimitsRejectWithoutCallingUpstream(t *testing.T) {
	tests := []struct {
		name        string
		globalLimit int
		perKeyLimit int
		errorCode   string
	}{
		{name: "global", globalLimit: 1, perKeyLimit: 1, errorCode: "concurrency_limit"},
		{name: "per_upstream", globalLimit: 2, perKeyLimit: 1, errorCode: "upstream_concurrency_limit"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			started := make(chan struct{}, 1)
			release := make(chan struct{})
			var calls atomic.Int64
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				started <- struct{}{}
				<-release
				_, _ = w.Write([]byte(`{"ok":true}`))
			}))
			defer upstream.Close()

			cfg := testConfig(t, upstream.URL, "real-key")
			cfg.MaxConcurrentRequests = tt.globalLimit
			cfg.MaxConcurrentPerUpstream = tt.perKeyLimit
			proxy := newTestProxy(cfg, upstream.Client())

			firstReq := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
			firstReq.Header.Set("Authorization", "Bearer "+testGatewayKey)
			firstRec := httptest.NewRecorder()
			firstDone := make(chan struct{})
			go func() {
				proxy.Handler().ServeHTTP(firstRec, firstReq)
				close(firstDone)
			}()
			<-started

			secondReq := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
			secondReq.Header.Set("Authorization", "Bearer "+testGatewayKey)
			secondRec := httptest.NewRecorder()
			proxy.Handler().ServeHTTP(secondRec, secondReq)
			if secondRec.Code != http.StatusTooManyRequests || !strings.Contains(secondRec.Body.String(), tt.errorCode) {
				t.Fatalf("status=%d body=%s", secondRec.Code, secondRec.Body.String())
			}
			if secondRec.Header().Get("Retry-After") != "1" {
				t.Fatalf("Retry-After=%q", secondRec.Header().Get("Retry-After"))
			}
			if calls.Load() != 1 {
				t.Fatalf("upstream calls=%d, want 1", calls.Load())
			}

			close(release)
			<-firstDone
			if firstRec.Code != http.StatusOK {
				t.Fatalf("first status=%d body=%s", firstRec.Code, firstRec.Body.String())
			}
		})
	}
}

func TestAdminStatusDoesNotExposeSecrets(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer upstream.Close()
	cfg := testConfig(t, upstream.URL, "top-secret-real-key")
	proxy := newTestProxy(cfg, upstream.Client())
	req := httptest.NewRequest(http.MethodGet, "/admin/status", nil)
	req.Header.Set("Authorization", "Bearer "+cfg.AdminAPIKey)
	rec := httptest.NewRecorder()
	proxy.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "top-secret-real-key") || strings.Contains(rec.Body.String(), testGatewayKey) {
		t.Fatalf("status response leaked a secret: %s", rec.Body.String())
	}
	var decoded map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
}

func TestUpstreamRedirectDoesNotLeakCredential(t *testing.T) {
	leakCalls := 0
	leakTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leakCalls++
		if r.Header.Get("Authorization") != "" || r.Header.Get("X-Api-Key") != "" {
			t.Error("credential leaked to redirect target")
		}
	}))
	defer leakTarget.Close()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, leakTarget.URL, http.StatusFound)
	}))
	defer upstream.Close()

	cfg := testConfig(t, upstream.URL, "real-key")
	proxy := NewProxy(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer "+testGatewayKey)
	rec := httptest.NewRecorder()
	proxy.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", rec.Code)
	}
	if leakCalls != 0 {
		t.Fatalf("redirect target received %d requests", leakCalls)
	}
}

func TestClassifyBalanceExhaustion(t *testing.T) {
	retry, kind, _ := classifyUpstreamError(http.StatusBadRequest, []byte(`{"code":"Arrearage","message":"Account balance is insufficient"}`))
	if !retry || kind != FailureCredential {
		t.Fatalf("retry=%v kind=%v", retry, kind)
	}
	retry, kind, _ = classifyUpstreamError(http.StatusBadRequest, []byte(`{"message":"账户余额不足，请充值"}`))
	if !retry || kind != FailureCredential {
		t.Fatalf("Chinese balance error: retry=%v kind=%v", retry, kind)
	}
	retry, kind, _ = classifyUpstreamError(http.StatusBadRequest, []byte(`{"message":"账户已欠费"}`))
	if !retry || kind != FailureCredential {
		t.Fatalf("Chinese arrears error: retry=%v kind=%v", retry, kind)
	}
	retry, _, _ = classifyUpstreamError(http.StatusBadRequest, []byte(`{"error":"invalid parameter model"}`))
	if retry {
		t.Fatal("ordinary 400 response must not be retried")
	}
}

func TestRoute(t *testing.T) {
	tests := []struct {
		path     string
		protocol Protocol
		suffix   string
	}{
		{"/v1/chat/completions", ProtocolOpenAI, "/chat/completions"},
		{"/compatible-mode/v1/models", ProtocolOpenAI, "/models"},
		{"/apps/anthropic/v1/messages", ProtocolAnthropic, "/v1/messages"},
		{"/v1/messages", ProtocolAnthropic, "/v1/messages"},
	}
	for _, tt := range tests {
		gotProtocol, gotSuffix, ok := route(tt.path)
		if !ok || gotProtocol != tt.protocol || gotSuffix != tt.suffix {
			t.Errorf("route(%q) = %q, %q, %v", tt.path, gotProtocol, gotSuffix, ok)
		}
	}
}
