// Package config loads load balancer settings from defaults, an optional
// YAML-subset file, and environment variables, in that order of precedence
// (later wins). It uses only the standard library.
package config

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds all load balancer settings.
type Config struct {
	ListenAddr          string
	Backends            []string
	HealthCheckPath     string
	HealthCheckInterval time.Duration
	HealthCheckTimeout  time.Duration
	ShutdownTimeout     time.Duration
	FailureThreshold    int
	RecoveryThreshold   int
}

// Default returns a configuration that works with the local demo.
func Default() Config {
	return Config{
		ListenAddr:          ":8080",
		Backends:            []string{"http://localhost:8081", "http://localhost:8082", "http://localhost:8083"},
		HealthCheckPath:     "/health",
		HealthCheckInterval: 5 * time.Second,
		HealthCheckTimeout:  2 * time.Second,
		ShutdownTimeout:     15 * time.Second,
		// Eject quickly-ish, readmit cautiously.
		FailureThreshold:  2,
		RecoveryThreshold: 2,
	}
}

// Validate reports the first invalid setting.
func (c Config) Validate() error {
	switch {
	case c.ListenAddr == "":
		return errors.New("config: listen address is required")
	case len(c.Backends) == 0:
		return errors.New("config: at least one backend is required")
	case !strings.HasPrefix(c.HealthCheckPath, "/"):
		return errors.New("config: health check path must start with /")
	case c.HealthCheckInterval <= 0:
		return errors.New("config: health check interval must be positive")
	case c.HealthCheckTimeout <= 0:
		return errors.New("config: health check timeout must be positive")
	case c.HealthCheckTimeout > c.HealthCheckInterval:
		return errors.New("config: health check timeout must not exceed the interval")
	case c.ShutdownTimeout <= 0:
		return errors.New("config: shutdown timeout must be positive")
	case c.FailureThreshold < 1 || c.RecoveryThreshold < 1:
		return errors.New("config: thresholds must be at least 1")
	}
	return nil
}

// LoadFile reads a configuration file on top of the defaults.
//
// Supported syntax is a small YAML subset: "key: value" lines, one "backends:"
// key followed by "- url" list items, and "#" comments. This keeps the project
// free of third-party dependencies.
func LoadFile(path string) (Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return Config{}, fmt.Errorf("config: %w", err)
	}
	defer f.Close()

	cfg := Default()
	var backends []string
	inBackends := false

	scanner := bufio.NewScanner(f)
	for lineNo := 1; scanner.Scan(); lineNo++ {
		line := strings.TrimSpace(stripComment(scanner.Text()))
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "- ") {
			if !inBackends {
				return Config{}, fmt.Errorf("config: line %d: list item outside backends", lineNo)
			}
			backends = append(backends, unquote(strings.TrimPrefix(line, "- ")))
			continue
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			return Config{}, fmt.Errorf("config: line %d: expected key: value", lineNo)
		}
		key, value = strings.TrimSpace(key), unquote(strings.TrimSpace(value))
		inBackends = key == "backends"
		if inBackends {
			continue
		}
		if err := cfg.set(key, value); err != nil {
			return Config{}, fmt.Errorf("config: line %d: %w", lineNo, err)
		}
	}
	if err := scanner.Err(); err != nil {
		return Config{}, fmt.Errorf("config: %w", err)
	}
	if backends != nil {
		cfg.Backends = backends
	}
	return cfg, nil
}

// ApplyEnv overrides settings from LB_* environment variables. getenv is
// injected (pass os.Getenv) so tests do not touch the process environment.
func (c *Config) ApplyEnv(getenv func(string) string) error {
	if v := getenv("LB_BACKENDS"); v != "" {
		var list []string
		for _, s := range strings.Split(v, ",") {
			if s = strings.TrimSpace(s); s != "" {
				list = append(list, s)
			}
		}
		c.Backends = list
	}
	for env, key := range map[string]string{
		"LB_LISTEN_ADDR":        "listen_addr",
		"LB_HEALTH_PATH":        "health_check_path",
		"LB_HEALTH_INTERVAL":    "health_check_interval",
		"LB_HEALTH_TIMEOUT":     "health_check_timeout",
		"LB_SHUTDOWN_TIMEOUT":   "shutdown_timeout",
		"LB_FAILURE_THRESHOLD":  "failure_threshold",
		"LB_RECOVERY_THRESHOLD": "recovery_threshold",
	} {
		if v := getenv(env); v != "" {
			if err := c.set(key, v); err != nil {
				return fmt.Errorf("config: %s: %w", env, err)
			}
		}
	}
	return nil
}

func (c *Config) set(key, value string) error {
	var err error
	switch key {
	case "listen_addr":
		c.ListenAddr = value
	case "health_check_path":
		c.HealthCheckPath = value
	case "health_check_interval":
		c.HealthCheckInterval, err = time.ParseDuration(value)
	case "health_check_timeout":
		c.HealthCheckTimeout, err = time.ParseDuration(value)
	case "shutdown_timeout":
		c.ShutdownTimeout, err = time.ParseDuration(value)
	case "failure_threshold":
		c.FailureThreshold, err = strconv.Atoi(value)
	case "recovery_threshold":
		c.RecoveryThreshold, err = strconv.Atoi(value)
	default:
		return fmt.Errorf("unknown key %q", key)
	}
	if err != nil {
		return fmt.Errorf("invalid value for %s: %w", key, err)
	}
	return nil
}

func stripComment(s string) string {
	if i := strings.Index(s, " #"); i >= 0 {
		return s[:i]
	}
	if strings.HasPrefix(strings.TrimSpace(s), "#") {
		return ""
	}
	return s
}

func unquote(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && (s[0] == '"' && s[len(s)-1] == '"' || s[0] == '\'' && s[len(s)-1] == '\'') {
		return s[1 : len(s)-1]
	}
	return s
}
