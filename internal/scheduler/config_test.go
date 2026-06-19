package scheduler

import "testing"

func TestNormalizeConfigDefaultsAPIPaths(t *testing.T) {
	cfg, err := normalizeConfig(Config{
		Pools: []PoolConfig{
			{
				ID:         "gpt_low",
				VirtualKey: "vk_low_text",
				Providers:  []ProviderConfig{{Name: "provider_a"}},
			},
		},
	})
	if err != nil {
		t.Fatalf("normalizeConfig returned error: %v", err)
	}

	if cfg.Mode != "read_only" {
		t.Fatalf("mode = %q, want read_only", cfg.Mode)
	}
	if cfg.API.Paths.VirtualKeys != "/api/governance/virtual-keys" {
		t.Fatalf("virtual key path = %q", cfg.API.Paths.VirtualKeys)
	}
	if cfg.API.Paths.Logs != "/api/logs" {
		t.Fatalf("logs path = %q", cfg.API.Paths.Logs)
	}
	if cfg.API.Paths.Login != "/api/session/login" {
		t.Fatalf("login path = %q", cfg.API.Paths.Login)
	}
	if cfg.API.Paths.ProviderKey != "/api/providers/{provider}/keys/{key_id}" {
		t.Fatalf("provider key path = %q", cfg.API.Paths.ProviderKey)
	}
}

func TestNormalizeConfigMinimalDefaultsArePoolAgnostic(t *testing.T) {
	cfg, err := normalizeConfig(Config{
		Pools: []PoolConfig{
			{
				ID:         "gpt_low",
				VirtualKey: "vk_low_text",
				Providers: []ProviderConfig{
					{Name: "low_primary", CostWeight: 0.8},
					{Name: "low_quarantine", Role: "quarantine"},
				},
			},
			{
				ID:         "gpt_stable",
				VirtualKey: "vk_stable_text",
				Providers:  []ProviderConfig{{Name: "stable_primary"}},
			},
		},
	})
	if err != nil {
		t.Fatalf("normalizeConfig returned error: %v", err)
	}

	for _, pool := range cfg.Pools {
		if pool.MinActiveProviders != 1 {
			t.Fatalf("%s min_active_providers = %d, want 1", pool.ID, pool.MinActiveProviders)
		}
		rules := pool.EffectiveRules()
		if rules.MaxErrorRate != 0.5 || rules.DisableErrorRate != 0.8 || rules.MaxTimeoutOrIdle != 10 || rules.MaxP95LatencyMS != 0 || rules.MinWeight != 0.05 {
			t.Fatalf("%s rules = %+v", pool.ID, rules)
		}
	}

	if !cfg.Pools[0].Providers[0].AllowedInPool() {
		t.Fatalf("plain provider should be allowed by default")
	}
	if cfg.Pools[0].Providers[1].AllowedInPool() {
		t.Fatalf("quarantine provider should not be allowed")
	}
}

func TestCostWeightValidation(t *testing.T) {
	_, err := normalizeConfig(Config{
		Pools: []PoolConfig{
			{
				ID:         "gpt_low",
				VirtualKey: "vk_low_text",
				Providers:  []ProviderConfig{{Name: "provider_a", CostWeight: -0.1}},
			},
		},
	})
	if err == nil {
		t.Fatalf("normalizeConfig returned nil error, want cost_weight validation error")
	}
}

func TestDefaultCostWeight(t *testing.T) {
	cfg, err := normalizeConfig(Config{
		Pools: []PoolConfig{
			{
				ID:         "gpt_low",
				VirtualKey: "vk_low_text",
				Providers:  []ProviderConfig{{Name: "provider_a"}},
			},
		},
	})
	if err != nil {
		t.Fatalf("normalizeConfig returned error: %v", err)
	}

	rules := cfg.Pools[0].EffectiveRules()
	if rules.DefaultCostWeight != 1 {
		t.Fatalf("default_cost_weight = %.2f, want 1", rules.DefaultCostWeight)
	}
}
