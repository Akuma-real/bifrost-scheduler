// package scheduler 表示这个测试文件属于领域层 scheduler 包。
package scheduler

// testing 是 Go 标准测试包。
import (
	"testing"
)

// TestCostWeightIsHealthyTargetWeight 验证健康 provider 会恢复到 cost_weight。
func TestCostWeightIsHealthyTargetWeight(t *testing.T) {
	// MinimumAttempts=1 让测试里少量请求也能触发判断。
	cfg, err := NormalizeConfig(Config{
		MinimumAttempts: 1,
		Pools: []PoolConfig{
			{
				ID:         "pool_a",
				VirtualKey: "vk_a",
				Providers:  []ProviderConfig{{Name: "provider_a", CostWeight: 0.35}},
			},
		},
	})
	if err != nil {
		t.Fatalf("NormalizeConfig returned error: %v", err)
	}

	// decider 是领域决策服务。
	decider := NewDecisionService(cfg)
	// 当前权重 0.1，但配置 cost_weight 是 0.35，且最近 100% 成功。
	// 期望动作是恢复到 0.35。
	decision := decider.DecideProvider(
		cfg.Pools[0],
		cfg.Pools[0].Providers[0],
		PoolProviderState{Provider: "provider_a", CurrentWeight: 0.1, CurrentInBifrost: true},
		ProviderMetric{Total: 10, Success: 10, SuccessRate: 1},
		1,
	)

	if decision.Action != "set_weight" || decision.TargetWeight != 0.35 {
		t.Fatalf("decision = %+v, want set_weight to 0.35", decision)
	}
}

// TestHighErrorRateNeedsConsecutiveBadWindowsBeforeZeroWeight 验证高错误率不会立刻清零。
//
// 必须等连续坏窗口达到 RequiredBadWindows，才会 set_weight_zero。
func TestHighErrorRateNeedsConsecutiveBadWindowsBeforeZeroWeight(t *testing.T) {
	cfg, err := NormalizeConfig(Config{
		MinimumAttempts: 10,
		Pools: []PoolConfig{
			{
				ID:                 "pool_a",
				VirtualKey:         "vk_a",
				MinActiveProviders: 1,
				Providers:          []ProviderConfig{{Name: "provider_a", CostWeight: 0.7}},
				Rules:              &PoolRules{RequiredBadWindows: 2},
			},
		},
	})
	if err != nil {
		t.Fatalf("NormalizeConfig returned error: %v", err)
	}

	decider := NewDecisionService(cfg)
	state := PoolProviderState{Provider: "provider_a", CurrentWeight: 0.7, CurrentInBifrost: true}
	// 先构造一个错误率很高、但还没有坏窗口证据的指标。
	metric := ProviderMetric{
		Total:       20,
		Success:     1,
		Errors:      19,
		ErrorRate:   0.95,
		SuccessRate: 0.05,
	}
	// 没有坏窗口证据时保持不动。
	decision := decider.DecideProvider(cfg.Pools[0], cfg.Pools[0].Providers[0], state, metric, 2)
	if decision.Action != "keep" {
		t.Fatalf("decision = %+v, want keep without bad-window evidence", decision)
	}

	// 只有 1 个坏窗口时，先降到最小探测权重。
	metric.BadWindows = 1
	metric.ConsecutiveBadWindows = 1
	decision = decider.DecideProvider(cfg.Pools[0], cfg.Pools[0].Providers[0], state, metric, 2)
	if decision.Action != "set_weight" || decision.TargetWeight != cfg.Pools[0].EffectiveRules().MinWeight {
		t.Fatalf("decision = %+v, want cautious set_weight to min weight", decision)
	}

	// 连续 2 个坏窗口后，才清零。
	metric.BadWindows = 2
	metric.ConsecutiveBadWindows = 2
	decision = decider.DecideProvider(cfg.Pools[0], cfg.Pools[0].Providers[0], state, metric, 2)
	if decision.Action != "set_weight_zero" {
		t.Fatalf("decision = %+v, want set_weight_zero after consecutive bad windows", decision)
	}
}

// TestMinWeightProbeDoesNotCreateNoopDecision 验证“已经在最小探测权重”时不重复生成动作。
func TestMinWeightProbeDoesNotCreateNoopDecision(t *testing.T) {
	cfg, err := NormalizeConfig(Config{
		Mode:            "guarded_write",
		MinimumAttempts: 10,
		Pools: []PoolConfig{
			{
				ID:                 "pool_a",
				VirtualKey:         "vk_a",
				MinActiveProviders: 1,
				Providers:          []ProviderConfig{{Name: "provider_a", CostWeight: 0.7}},
				Rules:              &PoolRules{RequiredBadWindows: 2, MinWeight: 0.05},
			},
		},
	})
	if err != nil {
		t.Fatalf("NormalizeConfig returned error: %v", err)
	}

	// 当前权重已经是 0.05，决策也会想降到 0.05。
	// DecisionNoop 应该把这种无意义动作过滤掉。
	decisions := NewDecisionService(cfg).Decide(
		[]PoolProviderState{
			{PoolID: "pool_a", VirtualKey: "vk_a", Provider: "provider_a", CurrentWeight: 0.05, CurrentInBifrost: true},
		},
		[]ProviderMetric{
			{
				PoolID:                "pool_a",
				Provider:              "provider_a",
				Total:                 20,
				Errors:                20,
				ErrorRate:             1,
				BadWindows:            1,
				ConsecutiveBadWindows: 1,
			},
		},
		true,
	)
	if len(decisions) != 0 {
		t.Fatalf("decisions = %+v, want no-op min-weight probe omitted", decisions)
	}
}

// TestCriticalErrorsNeedConsecutiveBadWindowsBeforeDisablingKeys 验证关键错误也需要连续窗口证据。
func TestCriticalErrorsNeedConsecutiveBadWindowsBeforeDisablingKeys(t *testing.T) {
	cfg, err := NormalizeConfig(Config{
		MinimumAttempts: 10,
		Pools: []PoolConfig{
			{
				ID:         "pool_a",
				VirtualKey: "vk_a",
				Providers:  []ProviderConfig{{Name: "provider_a", CostWeight: 0.7}},
				Rules:      &PoolRules{CriticalErrorThreshold: 2, RequiredBadWindows: 2},
			},
		},
	})
	if err != nil {
		t.Fatalf("NormalizeConfig returned error: %v", err)
	}

	decider := NewDecisionService(cfg)
	state := PoolProviderState{Provider: "provider_a", CurrentWeight: 0.7, CurrentInBifrost: true}
	// 有关键错误，但还没有坏窗口证据。
	metric := ProviderMetric{
		Total:          20,
		Success:        2,
		Errors:         18,
		ErrorRate:      0.9,
		SuccessRate:    0.1,
		CriticalErrors: 2,
	}
	// 没有坏窗口证据，不禁用。
	decision := decider.DecideProvider(cfg.Pools[0], cfg.Pools[0].Providers[0], state, metric, 2)
	if decision.Action != "keep" {
		t.Fatalf("decision = %+v, want keep without bad-window evidence", decision)
	}

	// 只有一个坏窗口，先 warning 降权。
	metric.BadWindows = 1
	metric.ConsecutiveBadWindows = 1
	decision = decider.DecideProvider(cfg.Pools[0], cfg.Pools[0].Providers[0], state, metric, 2)
	if decision.Action != "set_weight" || decision.Severity != "warning" {
		t.Fatalf("decision = %+v, want warning set_weight before consecutive bad windows", decision)
	}

	// 连续坏窗口足够，才执行 disable_provider。
	metric.BadWindows = 2
	metric.ConsecutiveBadWindows = 2
	decision = decider.DecideProvider(cfg.Pools[0], cfg.Pools[0].Providers[0], state, metric, 2)
	if decision.Action != "disable_provider" {
		t.Fatalf("decision = %+v, want disable_provider after consecutive bad windows", decision)
	}
}

// TestCriticalErrorsDoNotDisableHealthyProvider 验证“关键错误数量多”不等于一定禁用。
//
// 如果总体错误率很低，说明大多数流量仍然健康，不能误伤 provider。
func TestCriticalErrorsDoNotDisableHealthyProvider(t *testing.T) {
	cfg, err := NormalizeConfig(Config{
		MinimumAttempts: 10,
		Pools: []PoolConfig{
			{
				ID:         "pool_a",
				VirtualKey: "vk_a",
				Providers:  []ProviderConfig{{Name: "provider_a", CostWeight: 0.7}},
				Rules:      &PoolRules{CriticalErrorThreshold: 2, RequiredBadWindows: 2, DisableErrorRate: 0.8},
			},
		},
	})
	if err != nil {
		t.Fatalf("NormalizeConfig returned error: %v", err)
	}

	decision := NewDecisionService(cfg).DecideProvider(
		cfg.Pools[0],
		cfg.Pools[0].Providers[0],
		PoolProviderState{Provider: "provider_a", CurrentWeight: 0.7, CurrentInBifrost: true},
		ProviderMetric{
			Total:                 100,
			Success:               90,
			Errors:                10,
			ErrorRate:             0.1,
			SuccessRate:           0.9,
			CriticalErrors:        10,
			BadWindows:            2,
			ConsecutiveBadWindows: 2,
		},
		2,
	)
	if decision.Action != "keep" {
		t.Fatalf("decision = %+v, want keep when critical errors are a minority of otherwise healthy traffic", decision)
	}
}

// TestProjectedMinActiveProvidersPreventsClearingWholePool 验证 min_active_providers 安全保护。
//
// 两个 provider 都坏时，最多清掉一个，另一个保留最小权重，避免整个池子归零。
func TestProjectedMinActiveProvidersPreventsClearingWholePool(t *testing.T) {
	cfg, err := NormalizeConfig(Config{
		MinimumAttempts: 10,
		Pools: []PoolConfig{
			{
				ID:                 "pool_a",
				VirtualKey:         "vk_a",
				MinActiveProviders: 1,
				Providers: []ProviderConfig{
					{Name: "provider_a", CostWeight: 0.7},
					{Name: "provider_b", CostWeight: 0.7},
				},
				Rules: &PoolRules{RequiredBadWindows: 2},
			},
		},
	})
	if err != nil {
		t.Fatalf("NormalizeConfig returned error: %v", err)
	}

	// 两个 provider 都连续坏窗口达到阈值。
	decisions := NewDecisionService(cfg).Decide(
		[]PoolProviderState{
			{PoolID: "pool_a", VirtualKey: "vk_a", Provider: "provider_a", CurrentWeight: 0.7, CurrentInBifrost: true},
			{PoolID: "pool_a", VirtualKey: "vk_a", Provider: "provider_b", CurrentWeight: 0.7, CurrentInBifrost: true},
		},
		[]ProviderMetric{
			{PoolID: "pool_a", Provider: "provider_a", Total: 20, Errors: 20, ErrorRate: 1, ConsecutiveBadWindows: 2, BadWindows: 2},
			{PoolID: "pool_a", Provider: "provider_b", Total: 20, Errors: 20, ErrorRate: 1, ConsecutiveBadWindows: 2, BadWindows: 2},
		},
		false,
	)
	if len(decisions) != 2 {
		t.Fatalf("decisions = %+v, want 2 decisions", decisions)
	}

	zeroed := 0
	minKept := 0
	// 统计最终决策：应该一个清零，一个保留最小权重。
	for _, decision := range decisions {
		if decision.Action == "set_weight_zero" {
			zeroed++
		}
		if decision.Action == "set_weight" && decision.TargetWeight == cfg.Pools[0].EffectiveRules().MinWeight {
			minKept++
		}
	}
	if zeroed != 1 || minKept != 1 {
		t.Fatalf("decisions = %+v, want one zeroed provider and one min-weight provider", decisions)
	}
}

// TestZeroWeightCriticalProviderRetriesKeyDisable 验证权重已经为 0 时仍可继续禁用 key。
//
// 这用于处理“权重为 0 但仍看到关键错误”的异常情况。
func TestZeroWeightCriticalProviderRetriesKeyDisable(t *testing.T) {
	cfg, err := NormalizeConfig(Config{
		MinimumAttempts: 10,
		Pools: []PoolConfig{
			{
				ID:         "pool_a",
				VirtualKey: "vk_a",
				Providers:  []ProviderConfig{{Name: "provider_a", CostWeight: 0.7}},
				Rules:      &PoolRules{CriticalErrorThreshold: 2},
			},
		},
	})
	if err != nil {
		t.Fatalf("NormalizeConfig returned error: %v", err)
	}

	decision := NewDecisionService(cfg).DecideProvider(
		cfg.Pools[0],
		cfg.Pools[0].Providers[0],
		PoolProviderState{Provider: "provider_a", CurrentWeight: 0, CurrentInBifrost: true},
		ProviderMetric{Total: 5, Errors: 5, CriticalErrors: 2},
		1,
	)
	if decision.Action != "disable_provider_keys" {
		t.Fatalf("decision = %+v, want disable_provider_keys", decision)
	}
}

// TestMissingProviderDoesNotDropBelowMinActiveProviders 验证配置缺失 provider 的安全处理。
//
// Bifrost 里有、配置里没有的 provider 通常应该清零；
// 但如果它是最后一个活跃 provider，就只降到最小权重。
func TestMissingProviderDoesNotDropBelowMinActiveProviders(t *testing.T) {
	cfg, err := NormalizeConfig(Config{
		Pools: []PoolConfig{
			{
				ID:                 "pool_a",
				VirtualKey:         "vk_a",
				MinActiveProviders: 1,
				Providers:          []ProviderConfig{{Name: "provider_a", CostWeight: 0.7}},
			},
		},
	})
	if err != nil {
		t.Fatalf("NormalizeConfig returned error: %v", err)
	}

	decisions := NewDecisionService(cfg).Decide(
		[]PoolProviderState{
			{PoolID: "pool_a", VirtualKey: "vk_a", Provider: "provider_a", CurrentWeight: 0, CurrentInBifrost: false},
			{PoolID: "pool_a", VirtualKey: "vk_a", Provider: "provider_b", CurrentWeight: 0.5, CurrentInBifrost: true},
		},
		[]ProviderMetric{
			{PoolID: "pool_a", Provider: "provider_a"},
			{PoolID: "pool_a", Provider: "provider_b"},
		},
		false,
	)
	if len(decisions) != 2 {
		t.Fatalf("decisions = %+v, want 2 decisions", decisions)
	}
	foundProtectedMissing := false
	for _, decision := range decisions {
		if decision.Provider == "provider_b" && decision.Action == "set_weight" && decision.TargetWeight == cfg.Pools[0].EffectiveRules().MinWeight {
			foundProtectedMissing = true
		}
		if decision.Provider == "provider_b" && decision.Action == "set_weight_zero" {
			t.Fatalf("decision = %+v, missing provider must not be zeroed when it is the last active provider", decision)
		}
	}
	if !foundProtectedMissing {
		t.Fatalf("decisions = %+v, want missing provider kept at min weight", decisions)
	}
}

// TestLatencyNeedsConsecutiveSlowWindowsBeforeWeightReduction 验证延迟过高也要连续慢窗口证据。
func TestLatencyNeedsConsecutiveSlowWindowsBeforeWeightReduction(t *testing.T) {
	cfg, err := NormalizeConfig(Config{
		MinimumAttempts: 10,
		Pools: []PoolConfig{
			{
				ID:         "pool_a",
				VirtualKey: "vk_a",
				Providers:  []ProviderConfig{{Name: "provider_a", CostWeight: 0.4}},
				Rules:      &PoolRules{MaxP95LatencyMS: 70000, MinLatencySamples: 5, RequiredBadWindows: 2},
			},
		},
	})
	if err != nil {
		t.Fatalf("NormalizeConfig returned error: %v", err)
	}

	decider := NewDecisionService(cfg)
	state := PoolProviderState{Provider: "provider_a", CurrentWeight: 0.4, CurrentInBifrost: true}
	// p95 是 float64 变量。
	// P95LatencyMS 字段需要 *float64，所以后面写 &p95 取地址。
	p95 := 85000.0
	metric := ProviderMetric{
		Total:        30,
		Success:      30,
		SuccessRate:  1,
		P95LatencyMS: &p95,
		SlowWindows:  1,
	}
	// 只有慢窗口总数，没有连续慢窗口，不降权。
	decision := decider.DecideProvider(cfg.Pools[0], cfg.Pools[0].Providers[0], state, metric, 2)
	if decision.Action != "keep" {
		t.Fatalf("decision = %+v, want keep before consecutive slow windows", decision)
	}

	// 连续慢窗口达到 2 后，降到目标权重的一半。
	metric.ConsecutiveSlowWindows = 2
	decision = decider.DecideProvider(cfg.Pools[0], cfg.Pools[0].Providers[0], state, metric, 2)
	if decision.Action != "set_weight" || decision.TargetWeight != 0.2 {
		t.Fatalf("decision = %+v, want set_weight to half after consecutive slow windows", decision)
	}
}

// TestAnnotateBadAndSlowWindows 验证 AnnotateWindows 能正确标记连续坏窗口和慢窗口。
func TestAnnotateBadAndSlowWindows(t *testing.T) {
	cfg, err := NormalizeConfig(Config{
		MinimumAttempts: 2,
		Pools: []PoolConfig{
			{
				ID:         "pool_a",
				VirtualKey: "vk_a",
				Providers:  []ProviderConfig{{Name: "provider_a"}},
				Rules: &PoolRules{
					DisableErrorRate:       0.8,
					MinErrors:              2,
					MaxP95LatencyMS:        1000,
					MinLatencySamples:      2,
					RequiredBadWindows:     2,
					CriticalErrorThreshold: 2,
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("NormalizeConfig returned error: %v", err)
	}
	p95 := 2000.0
	// 前两个窗口是坏窗口，后两个窗口是慢窗口。
	metrics := NewDecisionService(cfg).AnnotateWindows([]ProviderMetric{{
		PoolID:   "pool_a",
		Provider: "provider_a",
		Windows: []WindowMetric{
			{Total: 2, Errors: 2, ErrorRate: 1},
			{Total: 2, Errors: 2, ErrorRate: 1},
			{Total: 2, Success: 2, SuccessRate: 1, P95LatencyMS: &p95},
			{Total: 2, Success: 2, SuccessRate: 1, P95LatencyMS: &p95},
		},
	}})
	if metrics[0].ConsecutiveBadWindows != 2 || metrics[0].ConsecutiveSlowWindows != 2 {
		t.Fatalf("metric = %+v, want 2 bad and 2 slow consecutive windows", metrics[0])
	}
}
