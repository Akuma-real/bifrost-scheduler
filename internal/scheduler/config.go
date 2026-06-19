package scheduler

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

type Config struct {
	Mode            string       `json:"mode"`
	API             APIConfig    `json:"api"`
	Window          string       `json:"window"`
	QualityWindows  int          `json:"quality_windows"`
	MinimumAttempts int          `json:"minimum_attempts"`
	Cooldown        string       `json:"cooldown"`
	Pools           []PoolConfig `json:"pools"`
}

type APIConfig struct {
	BaseURL string   `json:"base_url"`
	Paths   APIPaths `json:"-"`
}

type APIPaths struct {
	VirtualKeys string `json:"virtual_keys"`
	Logs        string `json:"logs"`
	Login       string `json:"login"`
	ProviderKey string `json:"provider_key"`
}

type PoolConfig struct {
	ID                 string           `json:"id"`
	VirtualKey         string           `json:"virtual_key"`
	Kind               string           `json:"kind"`
	MinActiveProviders int              `json:"min_active_providers"`
	Rules              *PoolRules       `json:"rules"`
	Providers          []ProviderConfig `json:"providers"`
}

type PoolRules struct {
	MaxErrorRate              float64 `json:"max_error_rate"`
	DisableErrorRate          float64 `json:"disable_error_rate"`
	MaxTimeoutOrIdle          int     `json:"max_timeout_or_idle"`
	MaxP95LatencyMS           float64 `json:"max_p95_latency_ms"`
	MinSuccessRateForRecovery float64 `json:"min_success_rate_for_recovery"`
	MinErrors                 int     `json:"min_errors"`
	CriticalErrorThreshold    int     `json:"critical_error_threshold"`
	MinLatencySamples         int     `json:"min_latency_samples"`
	RequiredBadWindows        int     `json:"required_bad_windows"`
	DefaultCostWeight         float64 `json:"default_cost_weight"`
	MinWeight                 float64 `json:"min_weight"`
}

type ProviderConfig struct {
	Name       string  `json:"name"`
	Role       string  `json:"role"`
	Allowed    *bool   `json:"allowed,omitempty"`
	CostWeight float64 `json:"cost_weight"`
	MinWeight  float64 `json:"min_weight"`
}

type RuntimeConfig struct {
	Config
	WindowDuration   time.Duration
	CooldownDuration time.Duration
}

func LoadConfig(path string) (RuntimeConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return RuntimeConfig{}, fmt.Errorf("read config: %w", err)
	}

	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return RuntimeConfig{}, fmt.Errorf("parse config: %w", err)
	}
	return normalizeConfig(cfg)
}

func normalizeConfig(cfg Config) (RuntimeConfig, error) {
	if cfg.Mode == "" {
		cfg.Mode = "read_only"
	}
	if cfg.API.Paths.VirtualKeys == "" {
		cfg.API.Paths.VirtualKeys = "/api/governance/virtual-keys"
	}
	if cfg.API.Paths.Logs == "" {
		cfg.API.Paths.Logs = "/api/logs"
	}
	if cfg.API.Paths.Login == "" {
		cfg.API.Paths.Login = "/api/session/login"
	}
	if cfg.API.Paths.ProviderKey == "" {
		cfg.API.Paths.ProviderKey = "/api/providers/{provider}/keys/{key_id}"
	}
	if cfg.Window == "" {
		cfg.Window = "15m"
	}
	if cfg.Cooldown == "" {
		cfg.Cooldown = "30m"
	}
	if cfg.QualityWindows <= 0 {
		cfg.QualityWindows = 3
	}
	if cfg.MinimumAttempts <= 0 {
		cfg.MinimumAttempts = 10
	}
	if len(cfg.Pools) == 0 {
		return RuntimeConfig{}, fmt.Errorf("at least one pool is required")
	}

	window, err := time.ParseDuration(cfg.Window)
	if err != nil {
		return RuntimeConfig{}, fmt.Errorf("parse window: %w", err)
	}
	cooldown, err := time.ParseDuration(cfg.Cooldown)
	if err != nil {
		return RuntimeConfig{}, fmt.Errorf("parse cooldown: %w", err)
	}
	if window <= 0 {
		return RuntimeConfig{}, fmt.Errorf("window must be positive")
	}
	if cooldown < 0 {
		return RuntimeConfig{}, fmt.Errorf("cooldown cannot be negative")
	}

	seenPools := map[string]bool{}
	for i := range cfg.Pools {
		pool := &cfg.Pools[i]
		if pool.ID == "" {
			return RuntimeConfig{}, fmt.Errorf("pool id is required")
		}
		if seenPools[pool.ID] {
			return RuntimeConfig{}, fmt.Errorf("duplicate pool id %q", pool.ID)
		}
		seenPools[pool.ID] = true
		if pool.VirtualKey == "" {
			return RuntimeConfig{}, fmt.Errorf("pool %s virtual_key is required", pool.ID)
		}
		if pool.Kind == "" {
			pool.Kind = "text"
		}
		if pool.MinActiveProviders <= 0 {
			pool.MinActiveProviders = defaultMinActiveProviders()
		}
		if pool.Rules == nil {
			rules := defaultPoolRules()
			pool.Rules = &rules
		} else {
			normalizePoolRules(pool.Rules)
		}
		if len(pool.Providers) == 0 {
			return RuntimeConfig{}, fmt.Errorf("pool %s must define providers", pool.ID)
		}
		seenProviders := map[string]bool{}
		for j := range pool.Providers {
			provider := &pool.Providers[j]
			if provider.Name == "" {
				return RuntimeConfig{}, fmt.Errorf("pool %s provider name is required", pool.ID)
			}
			if seenProviders[provider.Name] {
				return RuntimeConfig{}, fmt.Errorf("pool %s has duplicate provider %q", pool.ID, provider.Name)
			}
			seenProviders[provider.Name] = true
			if provider.Role == "" {
				provider.Role = "fallback"
			}
			if provider.CostWeight < 0 {
				return RuntimeConfig{}, fmt.Errorf("pool %s provider %s cost_weight cannot be negative", pool.ID, provider.Name)
			}
			if provider.MinWeight < 0 {
				return RuntimeConfig{}, fmt.Errorf("pool %s provider %s min_weight cannot be negative", pool.ID, provider.Name)
			}
		}
	}

	return RuntimeConfig{
		Config:           cfg,
		WindowDuration:   window,
		CooldownDuration: cooldown,
	}, nil
}

func defaultMinActiveProviders() int {
	return 1
}

func defaultPoolRules() PoolRules {
	return PoolRules{
		MaxErrorRate:              0.5,
		DisableErrorRate:          0.8,
		MaxTimeoutOrIdle:          10,
		MaxP95LatencyMS:           0,
		MinSuccessRateForRecovery: 0.95,
		MinErrors:                 3,
		CriticalErrorThreshold:    2,
		MinLatencySamples:         5,
		RequiredBadWindows:        2,
		DefaultCostWeight:         1,
		MinWeight:                 0.05,
	}
}

func normalizePoolRules(rules *PoolRules) {
	defaults := defaultPoolRules()
	if rules.MaxErrorRate <= 0 {
		rules.MaxErrorRate = defaults.MaxErrorRate
	}
	if rules.DisableErrorRate <= 0 {
		rules.DisableErrorRate = defaults.DisableErrorRate
	}
	if rules.MinSuccessRateForRecovery <= 0 {
		rules.MinSuccessRateForRecovery = defaults.MinSuccessRateForRecovery
	}
	if rules.MinErrors <= 0 {
		rules.MinErrors = defaults.MinErrors
	}
	if rules.CriticalErrorThreshold <= 0 {
		rules.CriticalErrorThreshold = defaults.CriticalErrorThreshold
	}
	if rules.MinLatencySamples <= 0 {
		rules.MinLatencySamples = defaults.MinLatencySamples
	}
	if rules.RequiredBadWindows <= 0 {
		rules.RequiredBadWindows = defaults.RequiredBadWindows
	}
	if rules.DefaultCostWeight <= 0 {
		rules.DefaultCostWeight = defaults.DefaultCostWeight
	}
	if rules.MinWeight <= 0 {
		rules.MinWeight = defaults.MinWeight
	}
}

func (p PoolConfig) EffectiveRules() PoolRules {
	if p.Rules == nil {
		return defaultPoolRules()
	}
	return *p.Rules
}

func (c RuntimeConfig) Provider(poolID, providerName string) (ProviderConfig, bool) {
	for _, pool := range c.Pools {
		if pool.ID != poolID {
			continue
		}
		for _, provider := range pool.Providers {
			if provider.Name == providerName {
				return provider, true
			}
		}
	}
	return ProviderConfig{}, false
}

func (p ProviderConfig) AllowedInPool() bool {
	if strings.EqualFold(p.Role, "quarantine") {
		return false
	}
	if p.Allowed == nil {
		return true
	}
	return *p.Allowed
}
