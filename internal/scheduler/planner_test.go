package scheduler

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestCostWeightIsHealthyTargetWeight(t *testing.T) {
	cfg, err := normalizeConfig(Config{
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
		t.Fatalf("normalizeConfig returned error: %v", err)
	}

	planner := NewPlanner(cfg, nil, zeroTime())
	decision := planner.decideProvider(
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

func TestHighErrorRateNeedsConsecutiveBadWindowsBeforeZeroWeight(t *testing.T) {
	cfg, err := normalizeConfig(Config{
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
		t.Fatalf("normalizeConfig returned error: %v", err)
	}

	planner := NewPlanner(cfg, nil, zeroTime())
	state := PoolProviderState{Provider: "provider_a", CurrentWeight: 0.7, CurrentInBifrost: true}
	metric := ProviderMetric{
		Total:       20,
		Success:     1,
		Errors:      19,
		ErrorRate:   0.95,
		SuccessRate: 0.05,
	}
	decision := planner.decideProvider(cfg.Pools[0], cfg.Pools[0].Providers[0], state, metric, 2)
	if decision.Action != "keep" {
		t.Fatalf("decision = %+v, want keep without bad-window evidence", decision)
	}

	metric.BadWindows = 1
	metric.ConsecutiveBadWindows = 1
	decision = planner.decideProvider(cfg.Pools[0], cfg.Pools[0].Providers[0], state, metric, 2)
	if decision.Action != "set_weight" || decision.TargetWeight != cfg.Pools[0].EffectiveRules().MinWeight {
		t.Fatalf("decision = %+v, want cautious set_weight to min weight", decision)
	}

	metric.BadWindows = 2
	metric.ConsecutiveBadWindows = 2
	decision = planner.decideProvider(cfg.Pools[0], cfg.Pools[0].Providers[0], state, metric, 2)
	if decision.Action != "set_weight_zero" {
		t.Fatalf("decision = %+v, want set_weight_zero after consecutive bad windows", decision)
	}
}

func TestMinWeightProbeDoesNotCreateNoopDecision(t *testing.T) {
	cfg, err := normalizeConfig(Config{
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
		t.Fatalf("normalizeConfig returned error: %v", err)
	}

	store := &fakeStore{
		states: []PoolProviderState{
			{PoolID: "pool_a", VirtualKey: "vk_a", Provider: "provider_a", CurrentWeight: 0.05, CurrentInBifrost: true},
		},
		metrics: []ProviderMetric{
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
	}

	plan, err := NewPlanner(cfg, store, zeroTime()).BuildPlan(context.Background(), true)
	if err != nil {
		t.Fatalf("BuildPlan returned error: %v", err)
	}
	if len(plan.Decisions) != 0 {
		t.Fatalf("decisions = %+v, want no-op min-weight probe omitted", plan.Decisions)
	}
	if store.weightCalls != 0 {
		t.Fatalf("weightCalls = %d, want 0", store.weightCalls)
	}
}

func TestCriticalErrorsNeedConsecutiveBadWindowsBeforeDisablingKeys(t *testing.T) {
	cfg, err := normalizeConfig(Config{
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
		t.Fatalf("normalizeConfig returned error: %v", err)
	}

	planner := NewPlanner(cfg, nil, zeroTime())
	state := PoolProviderState{Provider: "provider_a", CurrentWeight: 0.7, CurrentInBifrost: true}
	metric := ProviderMetric{
		Total:          20,
		Success:        2,
		Errors:         18,
		ErrorRate:      0.9,
		SuccessRate:    0.1,
		CriticalErrors: 2,
	}
	decision := planner.decideProvider(cfg.Pools[0], cfg.Pools[0].Providers[0], state, metric, 2)
	if decision.Action != "keep" {
		t.Fatalf("decision = %+v, want keep without bad-window evidence", decision)
	}

	metric.BadWindows = 1
	metric.ConsecutiveBadWindows = 1
	decision = planner.decideProvider(cfg.Pools[0], cfg.Pools[0].Providers[0], state, metric, 2)
	if decision.Action != "set_weight" || decision.Severity != "warning" {
		t.Fatalf("decision = %+v, want warning set_weight before consecutive bad windows", decision)
	}

	metric.BadWindows = 2
	metric.ConsecutiveBadWindows = 2
	decision = planner.decideProvider(cfg.Pools[0], cfg.Pools[0].Providers[0], state, metric, 2)
	if decision.Action != "disable_provider" {
		t.Fatalf("decision = %+v, want disable_provider after consecutive bad windows", decision)
	}
}

func TestCriticalErrorsDoNotDisableHealthyProvider(t *testing.T) {
	cfg, err := normalizeConfig(Config{
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
		t.Fatalf("normalizeConfig returned error: %v", err)
	}

	decision := NewPlanner(cfg, nil, zeroTime()).decideProvider(
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

func TestProjectedMinActiveProvidersPreventsClearingWholePool(t *testing.T) {
	cfg, err := normalizeConfig(Config{
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
		t.Fatalf("normalizeConfig returned error: %v", err)
	}

	now := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)
	store := &fakeStore{
		states: []PoolProviderState{
			{PoolID: "pool_a", VirtualKey: "vk_a", Provider: "provider_a", CurrentWeight: 0.7, CurrentInBifrost: true},
			{PoolID: "pool_a", VirtualKey: "vk_a", Provider: "provider_b", CurrentWeight: 0.7, CurrentInBifrost: true},
		},
		metrics: []ProviderMetric{
			{PoolID: "pool_a", Provider: "provider_a", Total: 20, Errors: 20, ErrorRate: 1, ConsecutiveBadWindows: 2, BadWindows: 2},
			{PoolID: "pool_a", Provider: "provider_b", Total: 20, Errors: 20, ErrorRate: 1, ConsecutiveBadWindows: 2, BadWindows: 2},
		},
	}
	plan, err := NewPlanner(cfg, store, now).BuildPlan(context.Background(), false)
	if err != nil {
		t.Fatalf("BuildPlan returned error: %v", err)
	}
	if len(plan.Decisions) != 2 {
		t.Fatalf("decisions = %+v, want 2 decisions", plan.Decisions)
	}

	zeroed := 0
	minKept := 0
	for _, decision := range plan.Decisions {
		if decision.Action == "set_weight_zero" {
			zeroed++
		}
		if decision.Action == "set_weight" && decision.TargetWeight == cfg.Pools[0].EffectiveRules().MinWeight {
			minKept++
		}
	}
	if zeroed != 1 || minKept != 1 {
		t.Fatalf("decisions = %+v, want one zeroed provider and one min-weight provider", plan.Decisions)
	}
}

func TestZeroWeightCriticalProviderRetriesKeyDisable(t *testing.T) {
	cfg, err := normalizeConfig(Config{
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
		t.Fatalf("normalizeConfig returned error: %v", err)
	}

	decision := NewPlanner(cfg, nil, zeroTime()).decideProvider(
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

func TestDisableProviderDoesNotChangeWeightWhenKeyDisableFails(t *testing.T) {
	cfg, err := normalizeConfig(Config{
		Mode:            "guarded_write",
		MinimumAttempts: 10,
		Pools: []PoolConfig{
			{
				ID:         "pool_a",
				VirtualKey: "vk_a",
				Providers: []ProviderConfig{
					{Name: "provider_a", CostWeight: 0.7},
					{Name: "provider_b", CostWeight: 0.7},
				},
				Rules: &PoolRules{CriticalErrorThreshold: 2, RequiredBadWindows: 2},
			},
		},
	})
	if err != nil {
		t.Fatalf("normalizeConfig returned error: %v", err)
	}

	store := &fakeStore{
		states: []PoolProviderState{
			{PoolID: "pool_a", VirtualKey: "vk_a", Provider: "provider_a", CurrentWeight: 0.7, CurrentInBifrost: true},
			{PoolID: "pool_a", VirtualKey: "vk_a", Provider: "provider_b", CurrentWeight: 0.7, CurrentInBifrost: true},
		},
		metrics: []ProviderMetric{
			{
				PoolID:                "pool_a",
				Provider:              "provider_a",
				Total:                 20,
				Errors:                20,
				ErrorRate:             1,
				CriticalErrors:        2,
				BadWindows:            2,
				ConsecutiveBadWindows: 2,
			},
			{PoolID: "pool_a", Provider: "provider_b", Total: 20, Success: 20, SuccessRate: 1},
		},
		keyErr: fmt.Errorf("no bound keys"),
	}

	plan, err := NewPlanner(cfg, store, zeroTime()).BuildPlan(context.Background(), true)
	if err != nil {
		t.Fatalf("BuildPlan returned error: %v", err)
	}
	if len(plan.Decisions) != 1 {
		t.Fatalf("decisions = %+v, want one decision", plan.Decisions)
	}
	if plan.Decisions[0].Action != "disable_provider" || plan.Decisions[0].Apply == nil || plan.Decisions[0].Apply.Applied {
		t.Fatalf("decision = %+v, want failed disable_provider", plan.Decisions[0])
	}
	if store.keyDisableCalls != 1 {
		t.Fatalf("keyDisableCalls = %d, want 1", store.keyDisableCalls)
	}
	if store.weightCalls != 0 {
		t.Fatalf("weightCalls = %d, want 0", store.weightCalls)
	}
}

func TestApplyDecisionSkipsNoopWeightUpdate(t *testing.T) {
	cfg, err := normalizeConfig(Config{
		Mode: "guarded_write",
		Pools: []PoolConfig{{
			ID:         "pool_a",
			VirtualKey: "vk_a",
			Providers:  []ProviderConfig{{Name: "provider_a", CostWeight: 0.7}},
		}},
	})
	if err != nil {
		t.Fatalf("normalizeConfig returned error: %v", err)
	}

	store := &fakeStore{}
	result := NewPlanner(cfg, store, zeroTime()).applyDecision(
		context.Background(),
		Decision{PoolID: "pool_a", Provider: "provider_a", Action: "set_weight", TargetWeight: 0.05},
		[]PoolProviderState{{PoolID: "pool_a", Provider: "provider_a", CurrentWeight: 0.05, CurrentInBifrost: true}},
	)

	if !result.Skipped || result.Applied {
		t.Fatalf("result = %+v, want skipped no-op", result)
	}
	if store.weightCalls != 0 {
		t.Fatalf("weightCalls = %d, want 0", store.weightCalls)
	}
}

func TestMissingProviderDoesNotDropBelowMinActiveProviders(t *testing.T) {
	cfg, err := normalizeConfig(Config{
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
		t.Fatalf("normalizeConfig returned error: %v", err)
	}

	now := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)
	store := &fakeStore{
		states: []PoolProviderState{
			{PoolID: "pool_a", VirtualKey: "vk_a", Provider: "provider_a", CurrentWeight: 0, CurrentInBifrost: false},
			{PoolID: "pool_a", VirtualKey: "vk_a", Provider: "provider_b", CurrentWeight: 0.5, CurrentInBifrost: true},
		},
		metrics: []ProviderMetric{
			{PoolID: "pool_a", Provider: "provider_a"},
			{PoolID: "pool_a", Provider: "provider_b"},
		},
	}
	plan, err := NewPlanner(cfg, store, now).BuildPlan(context.Background(), false)
	if err != nil {
		t.Fatalf("BuildPlan returned error: %v", err)
	}
	if len(plan.Decisions) != 2 {
		t.Fatalf("decisions = %+v, want 2 decisions", plan.Decisions)
	}
	foundProtectedMissing := false
	for _, decision := range plan.Decisions {
		if decision.Provider == "provider_b" && decision.Action == "set_weight" && decision.TargetWeight == cfg.Pools[0].EffectiveRules().MinWeight {
			foundProtectedMissing = true
		}
		if decision.Provider == "provider_b" && decision.Action == "set_weight_zero" {
			t.Fatalf("decision = %+v, missing provider must not be zeroed when it is the last active provider", decision)
		}
	}
	if !foundProtectedMissing {
		t.Fatalf("decisions = %+v, want missing provider kept at min weight", plan.Decisions)
	}
}

func TestLatencyNeedsConsecutiveSlowWindowsBeforeWeightReduction(t *testing.T) {
	cfg, err := normalizeConfig(Config{
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
		t.Fatalf("normalizeConfig returned error: %v", err)
	}

	planner := NewPlanner(cfg, nil, zeroTime())
	state := PoolProviderState{Provider: "provider_a", CurrentWeight: 0.4, CurrentInBifrost: true}
	p95 := 85000.0
	metric := ProviderMetric{
		Total:        30,
		Success:      30,
		SuccessRate:  1,
		P95LatencyMS: &p95,
		SlowWindows:  1,
	}
	decision := planner.decideProvider(cfg.Pools[0], cfg.Pools[0].Providers[0], state, metric, 2)
	if decision.Action != "keep" {
		t.Fatalf("decision = %+v, want keep before consecutive slow windows", decision)
	}

	metric.ConsecutiveSlowWindows = 2
	decision = planner.decideProvider(cfg.Pools[0], cfg.Pools[0].Providers[0], state, metric, 2)
	if decision.Action != "set_weight" || decision.TargetWeight != 0.2 {
		t.Fatalf("decision = %+v, want set_weight to half after consecutive slow windows", decision)
	}
}

func TestAnnotateBadAndSlowWindows(t *testing.T) {
	cfg, err := normalizeConfig(Config{
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
		t.Fatalf("normalizeConfig returned error: %v", err)
	}
	p95 := 2000.0
	metrics := NewPlanner(cfg, nil, zeroTime()).annotateBadWindows([]ProviderMetric{{
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

func zeroTime() time.Time {
	return time.Time{}
}

type fakeStore struct {
	states          []PoolProviderState
	metrics         []ProviderMetric
	weightCalls     int
	keyDisableCalls int
	weightErr       error
	keyErr          error
}

func (s fakeStore) LoadProviderStates(_ context.Context, _ []PoolConfig) ([]PoolProviderState, error) {
	return s.states, nil
}

func (s fakeStore) LoadMetrics(_ context.Context, _ []PoolConfig, _, _ time.Time, _ time.Duration) ([]ProviderMetric, error) {
	return s.metrics, nil
}

func (s *fakeStore) SetProviderWeight(_ context.Context, _ PoolProviderState, _ float64) error {
	s.weightCalls++
	return s.weightErr
}

func (s *fakeStore) SetProviderKeysEnabled(_ context.Context, _ PoolProviderState, _ bool) error {
	s.keyDisableCalls++
	return s.keyErr
}
