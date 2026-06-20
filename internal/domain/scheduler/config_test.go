// package scheduler 表示这个测试文件属于领域层 scheduler 包。
package scheduler

// testing 是 Go 标准测试包。
import "testing"

// TestNormalizeConfigDefaultsAPIPaths 验证最小配置也会自动补齐 Bifrost API 路径。
func TestNormalizeConfigDefaultsAPIPaths(t *testing.T) {
	// 这里故意只写 pools，不写 mode/api paths/window/rules。
	// 目的就是确认 NormalizeConfig 会补默认值。
	cfg, err := NormalizeConfig(Config{
		Pools: []PoolConfig{
			{
				ID:         "gpt_low",
				VirtualKey: "vk_low_text",
				Providers:  []ProviderConfig{{Name: "provider_a"}},
			},
		},
	})
	if err != nil {
		t.Fatalf("NormalizeConfig returned error: %v", err)
	}

	// 下面每个 if 都是在检查一个默认值。
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

// TestNormalizeConfigMinimalDefaultsArePoolAgnostic 验证默认规则不区分 low/stable。
//
// 这对应你的要求：调度器是无人值守，不要因为池子名字不同就内置区别待遇。
func TestNormalizeConfigMinimalDefaultsArePoolAgnostic(t *testing.T) {
	cfg, err := NormalizeConfig(Config{
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
		t.Fatalf("NormalizeConfig returned error: %v", err)
	}

	// 两个 pool 都应该得到同一套默认保护规则。
	for _, pool := range cfg.Pools {
		if pool.MinActiveProviders != 1 {
			t.Fatalf("%s min_active_providers = %d, want 1", pool.ID, pool.MinActiveProviders)
		}
		rules := pool.EffectiveRules()
		if rules.MaxErrorRate != 0.5 || rules.DisableErrorRate != 0.8 || rules.MaxTimeoutOrIdle != 10 || rules.MaxP95LatencyMS != 0 || rules.MinWeight != 0.05 {
			t.Fatalf("%s rules = %+v", pool.ID, rules)
		}
	}

	// 普通 provider 默认允许。
	if !cfg.Pools[0].Providers[0].AllowedInPool() {
		t.Fatalf("plain provider should be allowed by default")
	}
	// quarantine provider 默认不允许。
	if cfg.Pools[0].Providers[1].AllowedInPool() {
		t.Fatalf("quarantine provider should not be allowed")
	}
}

// TestCostWeightValidation 验证 cost_weight 不能是负数。
func TestCostWeightValidation(t *testing.T) {
	_, err := NormalizeConfig(Config{
		Pools: []PoolConfig{
			{
				ID:         "gpt_low",
				VirtualKey: "vk_low_text",
				Providers:  []ProviderConfig{{Name: "provider_a", CostWeight: -0.1}},
			},
		},
	})
	if err == nil {
		// err == nil 表示没有错误，这里反而是测试失败。
		t.Fatalf("NormalizeConfig returned nil error, want cost_weight validation error")
	}
}

// TestDefaultCostWeight 验证 provider 没写 cost_weight 时，规则里默认目标权重是 1。
func TestDefaultCostWeight(t *testing.T) {
	cfg, err := NormalizeConfig(Config{
		Pools: []PoolConfig{
			{
				ID:         "gpt_low",
				VirtualKey: "vk_low_text",
				Providers:  []ProviderConfig{{Name: "provider_a"}},
			},
		},
	})
	if err != nil {
		t.Fatalf("NormalizeConfig returned error: %v", err)
	}

	rules := cfg.Pools[0].EffectiveRules()
	if rules.DefaultCostWeight != 1 {
		t.Fatalf("default_cost_weight = %.2f, want 1", rules.DefaultCostWeight)
	}
}

// TestPartialRulesKeepDefaults 验证只写一部分 rules 时，其他默认保护规则不会丢。
func TestPartialRulesKeepDefaults(t *testing.T) {
	cfg, err := NormalizeConfig(Config{
		Pools: []PoolConfig{
			{
				ID:         "gpt_low",
				VirtualKey: "vk_low_text",
				Providers:  []ProviderConfig{{Name: "provider_a"}},
				Rules:      &PoolRules{MinWeightChange: 0.05},
			},
		},
	})
	if err != nil {
		t.Fatalf("NormalizeConfig returned error: %v", err)
	}

	rules := cfg.Pools[0].EffectiveRules()
	if rules.MaxTimeoutOrIdle != 10 {
		t.Fatalf("max_timeout_or_idle = %d, want default 10", rules.MaxTimeoutOrIdle)
	}
	if rules.MaxWeightStep != 0.2 {
		t.Fatalf("max_weight_step = %.2f, want default 0.2", rules.MaxWeightStep)
	}
	if rules.MinWeightChange != 0.05 {
		t.Fatalf("min_weight_change = %.2f, want configured 0.05", rules.MinWeightChange)
	}
}
