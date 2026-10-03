package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/example/load-balancer/internal/config"
)

func TestDefaultsAreValid(t *testing.T) {
	if err := config.Default().Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestLoadFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.yaml")
	content := `# comment
listen_addr: ":9000"
backends:
  - http://localhost:1   # inline comment
  - "http://localhost:2"
health_check_interval: 3s
health_check_timeout: 1s
shutdown_timeout: 7s
failure_threshold: 3
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ListenAddr != ":9000" || len(cfg.Backends) != 2 || cfg.Backends[1] != "http://localhost:2" ||
		cfg.HealthCheckInterval != 3*time.Second || cfg.HealthCheckTimeout != time.Second ||
		cfg.ShutdownTimeout != 7*time.Second || cfg.FailureThreshold != 3 ||
		cfg.RecoveryThreshold != config.Default().RecoveryThreshold {
		t.Fatalf("unexpected config: %+v", cfg)
	}
}

func TestLoadFileRejectsUnknownKeysAndBadValues(t *testing.T) {
	for name, content := range map[string]string{
		"unknown key":  "nope: 1\n",
		"bad duration": "health_check_interval: soon\n",
		"orphan item":  "- http://x\n",
		"no colon":     "garbage\n",
	} {
		path := filepath.Join(t.TempDir(), "c.yaml")
		_ = os.WriteFile(path, []byte(content), 0o600)
		if _, err := config.LoadFile(path); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestExampleConfigFileLoads(t *testing.T) {
	cfg, err := config.LoadFile("../../examples/config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestApplyEnvOverrides(t *testing.T) {
	env := map[string]string{
		"LB_LISTEN_ADDR":     ":1234",
		"LB_BACKENDS":        "http://a:1, http://b:2",
		"LB_HEALTH_INTERVAL": "250ms",
	}
	cfg := config.Default()
	if err := cfg.ApplyEnv(func(k string) string { return env[k] }); err != nil {
		t.Fatal(err)
	}
	if cfg.ListenAddr != ":1234" || len(cfg.Backends) != 2 || cfg.HealthCheckInterval != 250*time.Millisecond {
		t.Fatalf("unexpected config: %+v", cfg)
	}

	bad := config.Default()
	err := bad.ApplyEnv(func(k string) string {
		if k == "LB_HEALTH_TIMEOUT" {
			return "x"
		}
		return ""
	})
	if err == nil || !strings.Contains(err.Error(), "LB_HEALTH_TIMEOUT") {
		t.Fatalf("expected an error naming the variable, got %v", err)
	}
}

func TestValidate(t *testing.T) {
	mutations := map[string]func(*config.Config){
		"no backends":           func(c *config.Config) { c.Backends = nil },
		"timeout over interval": func(c *config.Config) { c.HealthCheckTimeout = c.HealthCheckInterval + 1 },
		"zero interval":         func(c *config.Config) { c.HealthCheckInterval = 0 },
		"bad path":              func(c *config.Config) { c.HealthCheckPath = "health" },
		"zero threshold":        func(c *config.Config) { c.FailureThreshold = 0 },
	}
	for name, mutate := range mutations {
		cfg := config.Default()
		mutate(&cfg)
		if cfg.Validate() == nil {
			t.Errorf("%s: expected validation error", name)
		}
	}
}
