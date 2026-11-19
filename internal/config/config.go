package config

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	DefaultOpenAIBaseURL    = "https://dashscope.aliyuncs.com/compatible-mode/v1"
	DefaultAnthropicBaseURL = "https://dashscope.aliyuncs.com/apps/anthropic"
)

type Config struct {
	Listen                    string     `json:"listen"`
	ClientAPIKeys             []string   `json:"client_api_keys"`
	AdminAPIKey               string     `json:"admin_api_key"`
	Strategy                  string     `json:"strategy"`
	DefaultOpenAIBaseURL      string     `json:"default_openai_base_url"`
	DefaultAnthropicBaseURL   string     `json:"default_anthropic_base_url"`
	Upstreams                 []Upstream `json:"upstreams"`
	MaxAttempts               int        `json:"max_attempts"`
	MaxConcurrentRequests     int        `json:"max_concurrent_requests"`
	MaxConcurrentPerUpstream  int        `json:"max_concurrent_per_upstream"`
	MaxRequestBodyBytes       int64      `json:"max_request_body_bytes"`
	MaxErrorBodyBytes         int64      `json:"max_error_body_bytes"`
	ResponseHeaderTimeout     Duration   `json:"response_header_timeout"`
	IdleConnTimeout           Duration   `json:"idle_conn_timeout"`
	BaseCooldown              Duration   `json:"base_cooldown"`
	CredentialCooldown        Duration   `json:"credential_cooldown"`
	AllowInsecureUpstream     bool       `json:"allow_insecure_upstream"`
	AllowedUpstreamHostSuffix []string   `json:"allowed_upstream_host_suffixes"`
}

type Upstream struct {
	ID               string `json:"id"`
	APIKey           string `json:"api_key"`
	OpenAIBaseURL    string `json:"openai_base_url,omitempty"`
	AnthropicBaseURL string `json:"anthropic_base_url,omitempty"`
	Weight           int    `json:"weight,omitempty"`
	Enabled          *bool  `json:"enabled,omitempty"`
}

type Duration struct{ time.Duration }

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("duration must be a string: %w", err)
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	d.Duration = v
	return nil
}

func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(d.String()) }

func Defaults() Config {
	return Config{
		Listen:                    ":8080",
		Strategy:                  "least_inflight",
		DefaultOpenAIBaseURL:      DefaultOpenAIBaseURL,
		DefaultAnthropicBaseURL:   DefaultAnthropicBaseURL,
		MaxAttempts:               3,
		MaxConcurrentRequests:     100,
		MaxConcurrentPerUpstream:  50,
		MaxRequestBodyBytes:       32 << 20,
		MaxErrorBodyBytes:         1 << 20,
		ResponseHeaderTimeout:     Duration{90 * time.Second},
		IdleConnTimeout:           Duration{90 * time.Second},
		BaseCooldown:              Duration{30 * time.Second},
		CredentialCooldown:        Duration{30 * time.Minute},
		AllowedUpstreamHostSuffix: []string{"aliyuncs.com"},
	}
}

func Load(path string) (Config, error) {
	cfg := Defaults()
	b, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("read config %q: %w", path, err)
	}
	// Secrets can stay in environment variables or in root-managed files named
	// by VAR_FILE: "api_key": "${ALIYUN_API_KEY_1}".
	expanded, err := expandEnvironment(string(b))
	if err != nil {
		return cfg, err
	}
	dec := json.NewDecoder(strings.NewReader(expanded))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return cfg, fmt.Errorf("parse config %q: %w", path, err)
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("multiple JSON values")
		}
		return cfg, fmt.Errorf("parse config %q: unexpected trailing content: %w", path, err)
	}
	if err := applyEnv(&cfg); err != nil {
		return cfg, err
	}
	if err := cfg.Validate(); err != nil {
		return cfg, err
	}
	return cfg, nil
}

func applyEnv(cfg *Config) error {
	if v := strings.TrimSpace(os.Getenv("LISTEN_ADDR")); v != "" {
		cfg.Listen = v
	}
	clientKeys, err := environmentValue("CLIENT_API_KEYS")
	if err != nil {
		return err
	}
	if v := splitCSV(clientKeys); len(v) > 0 {
		cfg.ClientAPIKeys = v
	}
	adminKey, err := environmentValue("ADMIN_API_KEY")
	if err != nil {
		return err
	}
	if v := strings.TrimSpace(adminKey); v != "" {
		cfg.AdminAPIKey = v
	}
	if v := strings.TrimSpace(os.Getenv("BALANCER_STRATEGY")); v != "" {
		cfg.Strategy = v
	}
	if v := strings.TrimSpace(os.Getenv("OPENAI_UPSTREAM_BASE_URL")); v != "" {
		cfg.DefaultOpenAIBaseURL = v
	}
	if v := strings.TrimSpace(os.Getenv("ANTHROPIC_UPSTREAM_BASE_URL")); v != "" {
		cfg.DefaultAnthropicBaseURL = v
	}
	aliyunKeys, err := environmentValue("ALIYUN_API_KEYS")
	if err != nil {
		return err
	}
	if v := splitCSV(aliyunKeys); len(v) > 0 {
		cfg.Upstreams = make([]Upstream, 0, len(v))
		for i, key := range v {
			cfg.Upstreams = append(cfg.Upstreams, Upstream{ID: fmt.Sprintf("aliyun-%d", i+1), APIKey: key, Weight: 1})
		}
	}
	if v := strings.TrimSpace(os.Getenv("MAX_ATTEMPTS")); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.MaxAttempts = n
		}
	}
	if v := strings.TrimSpace(os.Getenv("MAX_CONCURRENT_REQUESTS")); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.MaxConcurrentRequests = n
		}
	}
	if v := strings.TrimSpace(os.Getenv("MAX_CONCURRENT_PER_UPSTREAM")); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.MaxConcurrentPerUpstream = n
		}
	}
	return nil
}

func expandEnvironment(s string) (string, error) {
	var expansionErr error
	expanded := os.Expand(s, func(name string) string {
		if expansionErr != nil {
			return ""
		}
		value, err := environmentValue(name)
		if err != nil {
			expansionErr = err
			return ""
		}
		return value
	})
	if expansionErr != nil {
		return "", expansionErr
	}
	return expanded, nil
}

func environmentValue(name string) (string, error) {
	if value := os.Getenv(name); value != "" {
		return value, nil
	}
	path := strings.TrimSpace(os.Getenv(name + "_FILE"))
	if path == "" {
		return "", nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("read secret file for %s: %w", name, err)
	}
	if !info.Mode().IsRegular() || info.Size() > 1<<20 {
		return "", fmt.Errorf("secret file for %s must be a regular file no larger than 1 MiB", name)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read secret file for %s: %w", name, err)
	}
	return strings.TrimSpace(string(b)), nil
}

func splitCSV(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

func (c Config) Validate() error {
	if strings.TrimSpace(c.Listen) == "" {
		return errors.New("listen must not be empty")
	}
	if len(c.ClientAPIKeys) == 0 {
		return errors.New("at least one client_api_keys entry is required")
	}
	for _, key := range c.ClientAPIKeys {
		lower := strings.ToLower(key)
		if len(strings.TrimSpace(key)) < 16 || strings.Contains(lower, "change-me") || strings.Contains(lower, "replace") {
			return errors.New("client_api_keys must contain strong non-placeholder keys (minimum 16 characters)")
		}
	}
	adminLower := strings.ToLower(c.AdminAPIKey)
	if c.AdminAPIKey != "" && (len(c.AdminAPIKey) < 16 || strings.Contains(adminLower, "change-me") || strings.Contains(adminLower, "replace")) {
		return errors.New("admin_api_key must be empty or a strong non-placeholder key")
	}
	if c.Strategy != "least_inflight" && c.Strategy != "round_robin" {
		return fmt.Errorf("unsupported strategy %q", c.Strategy)
	}
	if c.MaxAttempts < 1 || c.MaxAttempts > 20 {
		return errors.New("max_attempts must be between 1 and 20")
	}
	if c.MaxConcurrentRequests < 1 || c.MaxConcurrentRequests > 10000 {
		return errors.New("max_concurrent_requests must be between 1 and 10000")
	}
	if c.MaxConcurrentPerUpstream < 1 || c.MaxConcurrentPerUpstream > c.MaxConcurrentRequests {
		return errors.New("max_concurrent_per_upstream must be between 1 and max_concurrent_requests")
	}
	if c.MaxRequestBodyBytes < 1024 || c.MaxRequestBodyBytes > 1<<30 {
		return errors.New("max_request_body_bytes must be between 1 KiB and 1 GiB")
	}
	if c.MaxErrorBodyBytes < 1024 || c.MaxErrorBodyBytes > 16<<20 {
		return errors.New("max_error_body_bytes must be between 1 KiB and 16 MiB")
	}
	if c.ResponseHeaderTimeout.Duration <= 0 || c.IdleConnTimeout.Duration <= 0 || c.BaseCooldown.Duration <= 0 || c.CredentialCooldown.Duration <= 0 {
		return errors.New("timeouts and cooldowns must be positive")
	}
	if len(c.Upstreams) == 0 {
		return errors.New("at least one upstream is required")
	}
	seen := make(map[string]struct{}, len(c.Upstreams))
	for i := range c.Upstreams {
		u := &c.Upstreams[i]
		if strings.TrimSpace(u.ID) == "" {
			return fmt.Errorf("upstreams[%d].id must not be empty", i)
		}
		if _, ok := seen[u.ID]; ok {
			return fmt.Errorf("duplicate upstream id %q", u.ID)
		}
		seen[u.ID] = struct{}{}
		upstreamKeyLower := strings.ToLower(u.APIKey)
		if strings.TrimSpace(u.APIKey) == "" || strings.Contains(upstreamKeyLower, "your-real") || strings.Contains(upstreamKeyLower, "replace") {
			return fmt.Errorf("upstream %q has an empty or placeholder api_key (check environment expansion)", u.ID)
		}
		if u.Weight == 0 {
			u.Weight = 1
		}
		if u.Weight < 1 || u.Weight > 100 {
			return fmt.Errorf("upstream %q weight must be between 1 and 100", u.ID)
		}
		if u.OpenAIBaseURL == "" {
			u.OpenAIBaseURL = c.DefaultOpenAIBaseURL
		}
		if u.AnthropicBaseURL == "" {
			u.AnthropicBaseURL = c.DefaultAnthropicBaseURL
		}
		if err := c.validateUpstreamURL(u.OpenAIBaseURL); err != nil {
			return fmt.Errorf("upstream %q openai_base_url: %w", u.ID, err)
		}
		if err := c.validateUpstreamURL(u.AnthropicBaseURL); err != nil {
			return fmt.Errorf("upstream %q anthropic_base_url: %w", u.ID, err)
		}
	}
	return nil
}

func (c Config) validateUpstreamURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || u.User != nil {
		return errors.New("must be an absolute URL without userinfo")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return errors.New("query strings and fragments are not allowed")
	}
	if u.Scheme != "https" && !(c.AllowInsecureUpstream && u.Scheme == "http") {
		return errors.New("HTTPS is required (set allow_insecure_upstream only for local tests)")
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if ip := net.ParseIP(host); ip != nil && !c.AllowInsecureUpstream {
		return errors.New("IP literal upstreams are not allowed")
	}
	if c.AllowInsecureUpstream {
		return nil
	}
	for _, suffix := range c.AllowedUpstreamHostSuffix {
		suffix = strings.ToLower(strings.Trim(strings.TrimSpace(suffix), "."))
		if suffix != "" && (host == suffix || strings.HasSuffix(host, "."+suffix)) {
			return nil
		}
	}
	return fmt.Errorf("host %q is outside allowed_upstream_host_suffixes", host)
}

func ConstantTimeKeyMatch(candidate string, allowed []string) bool {
	match := 0
	for _, key := range allowed {
		match |= subtle.ConstantTimeCompare([]byte(candidate), []byte(key))
	}
	return match == 1
}
