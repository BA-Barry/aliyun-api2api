package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func validConfig() Config {
	c := Defaults()
	c.ClientAPIKeys = []string{"sk-gateway-0123456789abcdef"}
	c.Upstreams = []Upstream{{ID: "one", APIKey: "real-key", Weight: 1}}
	return c
}

func TestValidateAppliesDefaultBaseURLs(t *testing.T) {
	c := validConfig()
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if c.Upstreams[0].OpenAIBaseURL != DefaultOpenAIBaseURL || c.Upstreams[0].AnthropicBaseURL != DefaultAnthropicBaseURL {
		t.Fatalf("defaults not applied: %#v", c.Upstreams[0])
	}
}

func TestLegacyPublicBaseURLsAreDefaults(t *testing.T) {
	if DefaultOpenAIBaseURL != "https://dashscope.aliyuncs.com/compatible-mode/v1" {
		t.Fatalf("unexpected OpenAI default: %s", DefaultOpenAIBaseURL)
	}
	if DefaultAnthropicBaseURL != "https://dashscope.aliyuncs.com/apps/anthropic" {
		t.Fatalf("unexpected Anthropic default: %s", DefaultAnthropicBaseURL)
	}
}

func TestValidateRejectsUnexpectedUpstreamHost(t *testing.T) {
	c := validConfig()
	c.Upstreams[0].OpenAIBaseURL = "https://attacker.example/v1"
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "outside allowed") {
		t.Fatalf("error = %v", err)
	}
}

func TestValidateRejectsPlaceholderSecrets(t *testing.T) {
	c := validConfig()
	c.ClientAPIKeys = []string{"sk-gw-replace-with-random-value"}
	if err := c.Validate(); err == nil {
		t.Fatal("placeholder gateway key must be rejected")
	}
	c = validConfig()
	c.Upstreams[0].APIKey = "sk-your-real-aliyun-key-1"
	if err := c.Validate(); err == nil {
		t.Fatal("placeholder upstream key must be rejected")
	}
}

func TestLoadExpandsEnvironmentSecrets(t *testing.T) {
	t.Setenv("TEST_GATEWAY_KEY", "sk-gateway-environment-0123456789")
	t.Setenv("TEST_ALIYUN_KEY", "sk-real-environment-key")
	data := `{
  "client_api_keys":["${TEST_GATEWAY_KEY}"],
  "strategy":"least_inflight",
  "upstreams":[{"id":"one","api_key":"${TEST_ALIYUN_KEY}","weight":1}]
}`
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ClientAPIKeys[0] != os.Getenv("TEST_GATEWAY_KEY") || cfg.Upstreams[0].APIKey != os.Getenv("TEST_ALIYUN_KEY") {
		t.Fatal("environment variables were not expanded")
	}
}

func TestLoadExpandsFileSecrets(t *testing.T) {
	dir := t.TempDir()
	gatewayPath := filepath.Join(dir, "gateway-key")
	aliyunPath := filepath.Join(dir, "aliyun-key")
	if err := os.WriteFile(gatewayPath, []byte("sk-gateway-file-0123456789abcdef\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(aliyunPath, []byte("sk-real-file-key\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TEST_FILE_GATEWAY_KEY_FILE", gatewayPath)
	t.Setenv("TEST_FILE_ALIYUN_KEY_FILE", aliyunPath)
	data := `{
  "client_api_keys":["${TEST_FILE_GATEWAY_KEY}"],
  "upstreams":[{"id":"one","api_key":"${TEST_FILE_ALIYUN_KEY}"}]
}`
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ClientAPIKeys[0] != "sk-gateway-file-0123456789abcdef" || cfg.Upstreams[0].APIKey != "sk-real-file-key" {
		t.Fatal("file-backed secrets were not expanded")
	}
}

func TestLoadRejectsMissingSecretFile(t *testing.T) {
	t.Setenv("TEST_MISSING_KEY_FILE", filepath.Join(t.TempDir(), "missing"))
	data := `{
  "client_api_keys":["${TEST_MISSING_KEY}"],
  "upstreams":[{"id":"one","api_key":"real-key"}]
}`
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "read secret file") {
		t.Fatalf("expected secret file error, got %v", err)
	}
}

func TestClientAPIKeysFileOverridesMissingSingleKeyPlaceholder(t *testing.T) {
	dir := t.TempDir()
	keysPath := filepath.Join(dir, "client-keys")
	if err := os.WriteFile(keysPath, []byte("client-one-0123456789,client-two-0123456789\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLIENT_API_KEYS_FILE", keysPath)
	data := `{
  "client_api_keys":["${GATEWAY_API_KEY}"],
  "upstreams":[{"id":"one","api_key":"real-key"}]
}`
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(cfg.ClientAPIKeys, ","); got != "client-one-0123456789,client-two-0123456789" {
		t.Fatalf("client keys = %q", got)
	}
}

func TestLoadRejectsTrailingJSON(t *testing.T) {
	data := `{
  "client_api_keys":["sk-gateway-0123456789abcdef"],
  "upstreams":[{"id":"one","api_key":"real-key"}]
} {"ignored":true}`
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "trailing") {
		t.Fatalf("expected trailing JSON error, got %v", err)
	}
}

func TestLoadEnvironmentListsOverrideJSON(t *testing.T) {
	t.Setenv("CLIENT_API_KEYS", "client-one-0123456789, client-two-0123456789")
	t.Setenv("ALIYUN_API_KEYS", "upstream-one, upstream-two, upstream-three")
	data := `{
  "client_api_keys":["sk-gateway-0123456789abcdef"],
  "upstreams":[{"id":"one","api_key":"real-key"}]
}`
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(cfg.ClientAPIKeys, ","); got != "client-one-0123456789,client-two-0123456789" {
		t.Fatalf("client keys = %q", got)
	}
	if len(cfg.Upstreams) != 3 || cfg.Upstreams[2].ID != "aliyun-3" || cfg.Upstreams[2].APIKey != "upstream-three" {
		t.Fatalf("upstreams = %#v", cfg.Upstreams)
	}
}

func TestConstantTimeKeyMatch(t *testing.T) {
	if !ConstantTimeKeyMatch("bravo", []string{"alpha", "bravo"}) {
		t.Fatal("expected match")
	}
	if ConstantTimeKeyMatch("charlie", []string{"alpha", "bravo"}) {
		t.Fatal("unexpected match")
	}
}
