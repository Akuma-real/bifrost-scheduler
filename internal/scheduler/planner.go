package scheduler

import (
	"context"
	"fmt"
	"math"
	"sort"
	"time"
)

type Planner struct {
	cfg  RuntimeConfig
	repo Store
	now  time.Time
}

type Store interface {
	LoadProviderStates(ctx context.Context, pools []PoolConfig) ([]PoolProviderState, error)
	LoadMetrics(ctx context.Context, pools []PoolConfig, windowStart, windowEnd time.Time, windowDuration time.Duration) ([]ProviderMetric, error)
	SetProviderWeight(ctx context.Context, state PoolProviderState, weight float64) error
	SetProviderKeysEnabled(ctx context.Context, state PoolProviderState, enabled bool) error
}

func NewPlanner(cfg RuntimeConfig, repo Store, now time.Time) Planner {
	if now.IsZero() {
		now = time.Now()
	}
	return Planner{cfg: cfg, repo: repo, now: now}
}

func (p Planner) BuildPlan(ctx context.Context, apply bool) (Plan, error) {
	windowStart := p.now.Add(-p.cfg.WindowDuration * time.Duration(p.cfg.QualityWindows))
	windowLength := p.cfg.WindowDuration * time.Duration(p.cfg.QualityWindows)
	states, err := p.repo.LoadProviderStates(ctx, p.cfg.Pools)
	if err != nil {
		return Plan{}, err
	}
	metrics, err := p.repo.LoadMetrics(ctx, p.cfg.Pools, windowStart, p.now, p.cfg.WindowDuration)
	if err != nil {
		return Plan{}, err
	}
	metrics = p.annotateBadWindows(metrics)

	plan := Plan{
		GeneratedAt:   p.now,
		WindowStart:   windowStart,
		Window:        windowLength.String(),
		Mode:          p.cfg.Mode,
		ApplyEnabled:  apply && p.cfg.Mode == "guarded_write",
		Pools:         poolSnapshots(p.cfg.Pools),
		Metrics:       metrics,
		CurrentStates: states,
	}
	plan.Decisions = p.decide(states, metrics, plan.ApplyEnabled)

	if apply {
		if p.cfg.Mode != "guarded_write" {
			for i := range plan.Decisions {
				plan.Decisions[i].Apply = &ApplyResult{
					Applied: false,
					Message: "apply requested but config mode is not guarded_write",
				}
			}
			return plan, nil
		}
		for i := range plan.Decisions {
			result := p.applyDecision(ctx, plan.Decisions[i], states)
			plan.Decisions[i].Apply = &result
		}
	}

	return plan, nil
}

func (p Planner) annotateBadWindows(metrics []ProviderMetric) []ProviderMetric {
	out := make([]ProviderMetric, 0, len(metrics))
	for _, metric := range metrics {
		pool, ok := p.pool(metric.PoolID)
		if !ok || len(metric.Windows) == 0 {
			out = append(out, metric)
			continue
		}
		rules := pool.EffectiveRules()
		badWindows := 0
		consecutive := 0
		maxConsecutive := 0
		slowWindows := 0
		slowConsecutive := 0
		maxSlowConsecutive := 0
		for i := range metric.Windows {
			window := &metric.Windows[i]
			window.Bad = badWindow(*window, rules, p.cfg.MinimumAttempts)
			if window.Bad {
				badWindows++
				consecutive++
				if consecutive > maxConsecutive {
					maxConsecutive = consecutive
				}
			} else {
				consecutive = 0
			}

			window.Slow = slowWindow(*window, rules)
			if window.Slow {
				slowWindows++
				slowConsecutive++
				if slowConsecutive > maxSlowConsecutive {
					maxSlowConsecutive = slowConsecutive
				}
			} else {
				slowConsecutive = 0
			}
		}
		metric.BadWindows = badWindows
		metric.ConsecutiveBadWindows = maxConsecutive
		metric.SlowWindows = slowWindows
		metric.ConsecutiveSlowWindows = maxSlowConsecutive
		out = append(out, metric)
	}
	return out
}

func (p Planner) pool(poolID string) (PoolConfig, bool) {
	for _, pool := range p.cfg.Pools {
		if pool.ID == poolID {
			return pool, true
		}
	}
	return PoolConfig{}, false
}

func (p Planner) decide(states []PoolProviderState, metrics []ProviderMetric, applyEnabled bool) []Decision {
	statesByPoolProvider := map[string]PoolProviderState{}
	for _, state := range states {
		statesByPoolProvider[key(state.PoolID, state.Provider)] = state
	}
	metricsByPoolProvider := map[string]ProviderMetric{}
	for _, metric := range metrics {
		metricsByPoolProvider[key(metric.PoolID, metric.Provider)] = metric
	}

	var decisions []Decision
	for _, pool := range p.cfg.Pools {
		activeNow := activeProviderCount(pool.ID, statesByPoolProvider)
		projectedActive := activeNow
		for _, providerCfg := range pool.Providers {
			state := statesByPoolProvider[key(pool.ID, providerCfg.Name)]
			metric := metricsByPoolProvider[key(pool.ID, providerCfg.Name)]
			decision := p.decideProvider(pool, providerCfg, state, metric, projectedActive)
			decision.DryRun = !applyEnabled
			if decisionNoops(decision) {
				continue
			}
			projectedActive = projectedActiveProviderCount(projectedActive, state, decision)
			decisions = append(decisions, decision)
		}
		for _, state := range states {
			if state.PoolID != pool.ID {
				continue
			}
			if _, ok := p.cfg.Provider(pool.ID, state.Provider); ok {
				continue
			}
			metric := metricsByPoolProvider[key(pool.ID, state.Provider)]
			targetWeight := 0.0
			action := "set_weight_zero"
			reason := "provider is present in Bifrost VK but missing from scheduler config"
			if projectedActive <= pool.MinActiveProviders && state.CurrentWeight > 0 {
				targetWeight = pool.EffectiveRules().MinWeight
				action = "set_weight"
				reason = "provider is missing from scheduler config, but keeping minimum weight to avoid dropping below min active providers"
			}
			decision := Decision{
				PoolID:        pool.ID,
				VirtualKey:    pool.VirtualKey,
				Provider:      state.Provider,
				Action:        action,
				CurrentWeight: state.CurrentWeight,
				TargetWeight:  targetWeight,
				Severity:      "warning",
				Reason:        reason,
				DryRun:        !applyEnabled,
				Inputs:        decisionInputs(metric),
			}
			if decisionNoops(decision) {
				continue
			}
			decisions = append(decisions, decision)
			projectedActive = projectedActiveProviderCount(projectedActive, state, Decision{Action: action, TargetWeight: targetWeight})
		}
	}

	sort.Slice(decisions, func(i, j int) bool {
		if decisions[i].Severity != decisions[j].Severity {
			return severityRank(decisions[i].Severity) > severityRank(decisions[j].Severity)
		}
		if decisions[i].PoolID != decisions[j].PoolID {
			return decisions[i].PoolID < decisions[j].PoolID
		}
		return decisions[i].Provider < decisions[j].Provider
	})
	return decisions
}

func (p Planner) decideProvider(pool PoolConfig, provider ProviderConfig, state PoolProviderState, metric ProviderMetric, activeNow int) Decision {
	rules := pool.EffectiveRules()
	currentWeight := state.CurrentWeight
	targetWeight := provider.CostWeight
	if targetWeight <= 0 {
		targetWeight = rules.DefaultCostWeight
	}
	minWeight := provider.MinWeight
	if minWeight == 0 {
		minWeight = rules.MinWeight
	}

	base := Decision{
		PoolID:        pool.ID,
		VirtualKey:    pool.VirtualKey,
		Provider:      provider.Name,
		Action:        "keep",
		CurrentWeight: currentWeight,
		TargetWeight:  currentWeight,
		Severity:      "info",
		Inputs:        decisionInputs(metric),
	}

	if !state.CurrentInBifrost {
		base.Action = "review_missing_provider"
		base.TargetWeight = targetWeight
		base.Severity = "warning"
		base.Reason = "provider is configured for pool but missing from Bifrost virtual key"
		return base
	}

	if !provider.AllowedInPool() {
		if currentWeight == 0 {
			return base
		}
		base.Action = "set_weight_zero"
		base.TargetWeight = 0
		base.Severity = "critical"
		base.Reason = "provider is not allowed in this pool"
		return base
	}

	if metric.Total < p.cfg.MinimumAttempts {
		if currentWeight == 0 && metric.CriticalErrors >= rules.CriticalErrorThreshold {
			base.Action = "disable_provider_keys"
			base.TargetWeight = 0
			base.Severity = "warning"
			base.Reason = "provider already has zero weight, but critical errors remain; retry disabling bound keys"
			return base
		}
		if currentWeight == 0 && targetWeight > 0 {
			base.Action = "set_weight"
			base.TargetWeight = minWeight
			base.Severity = "info"
			base.Reason = "provider has too little recent traffic; keep a small probing weight"
			return base
		}
		return base
	}

	if metric.CriticalErrors >= rules.CriticalErrorThreshold &&
		metric.ErrorRate >= rules.DisableErrorRate &&
		metric.ConsecutiveBadWindows >= rules.RequiredBadWindows {
		if activeNow <= pool.MinActiveProviders && currentWeight > 0 {
			base.Action = "set_weight"
			base.TargetWeight = minWeight
			base.Severity = "critical"
			base.Reason = "disable threshold reached, but keeping minimum weight to avoid dropping below min active providers"
			return base
		}
		base.Action = "disable_provider"
		base.TargetWeight = 0
		base.Severity = "critical"
		base.Reason = fmt.Sprintf("critical credential/quota errors observed across %d consecutive bad windows", metric.ConsecutiveBadWindows)
		return base
	}

	if metric.CriticalErrors >= rules.CriticalErrorThreshold &&
		metric.ErrorRate >= rules.DisableErrorRate &&
		metric.BadWindows > 0 {
		base.Action = "set_weight"
		base.TargetWeight = minWeight
		base.Severity = "warning"
		base.Reason = fmt.Sprintf("critical credential/quota errors observed: %d; not disabling until %d consecutive bad windows", metric.CriticalErrors, rules.RequiredBadWindows)
		return base
	}

	if metric.Errors >= rules.MinErrors && metric.ErrorRate >= rules.DisableErrorRate && metric.ConsecutiveBadWindows >= rules.RequiredBadWindows {
		if activeNow <= pool.MinActiveProviders && currentWeight > 0 {
			base.Action = "set_weight"
			base.TargetWeight = minWeight
			base.Severity = "critical"
			base.Reason = "disable threshold reached, but keeping minimum weight to avoid dropping below min active providers"
			return base
		}
		base.Action = "set_weight_zero"
		base.TargetWeight = 0
		base.Severity = "critical"
		base.Reason = fmt.Sprintf("error rate %.2f%% reached disable threshold %.2f%%", metric.ErrorRate*100, rules.DisableErrorRate*100)
		return base
	}

	if metric.Errors >= rules.MinErrors && metric.ErrorRate >= rules.DisableErrorRate && metric.BadWindows > 0 {
		base.Action = "set_weight"
		base.TargetWeight = minWeight
		base.Severity = "warning"
		base.Reason = fmt.Sprintf("error rate %.2f%% reached disable threshold, but only %d consecutive bad windows", metric.ErrorRate*100, metric.ConsecutiveBadWindows)
		return base
	}

	if rules.MaxTimeoutOrIdle > 0 && metric.TimeoutOrStreamIdle > rules.MaxTimeoutOrIdle && metric.Errors >= rules.MinErrors && metric.BadWindows > 0 {
		base.Action = "set_weight"
		base.TargetWeight = minWeight
		base.Severity = "warning"
		base.Reason = fmt.Sprintf("timeout/stream idle count %d exceeded %d", metric.TimeoutOrStreamIdle, rules.MaxTimeoutOrIdle)
		return base
	}

	if metric.Errors >= rules.MinErrors && metric.ErrorRate > rules.MaxErrorRate && metric.BadWindows > 0 {
		base.Action = "set_weight"
		base.TargetWeight = clampWeight(targetWeight*(1-metric.ErrorRate), minWeight, targetWeight)
		base.Severity = "warning"
		base.Reason = fmt.Sprintf("error rate %.2f%% exceeded %.2f%%", metric.ErrorRate*100, rules.MaxErrorRate*100)
		return base
	}

	if metric.P95LatencyMS != nil &&
		rules.MaxP95LatencyMS > 0 &&
		metric.Success >= rules.MinLatencySamples &&
		*metric.P95LatencyMS > rules.MaxP95LatencyMS &&
		metric.ConsecutiveSlowWindows >= rules.RequiredBadWindows {
		base.Action = "set_weight"
		base.TargetWeight = clampWeight(targetWeight*0.5, minWeight, targetWeight)
		base.Severity = "warning"
		base.Reason = fmt.Sprintf("p95 latency %.0fms exceeded %.0fms across %d consecutive slow windows", *metric.P95LatencyMS, rules.MaxP95LatencyMS, metric.ConsecutiveSlowWindows)
		return base
	}

	if currentWeight == 0 && metric.SuccessRate >= rules.MinSuccessRateForRecovery {
		base.Action = "set_weight"
		base.TargetWeight = minWeight
		base.Severity = "info"
		base.Reason = fmt.Sprintf("provider looks recovered with %.2f%% success; re-enter at minimum weight", metric.SuccessRate*100)
		return base
	}

	if currentWeight > 0 && math.Abs(currentWeight-targetWeight) > 0.001 && metric.SuccessRate >= rules.MinSuccessRateForRecovery {
		base.Action = "set_weight"
		base.TargetWeight = targetWeight
		base.Severity = "info"
		base.Reason = "provider is healthy; restore configured base weight"
		return base
	}

	return base
}

func badWindow(window WindowMetric, rules PoolRules, minimumAttempts int) bool {
	if window.Total < minimumAttempts {
		return false
	}
	if window.CriticalErrors >= rules.CriticalErrorThreshold && window.ErrorRate >= rules.DisableErrorRate {
		return true
	}
	if window.Errors >= rules.MinErrors && window.ErrorRate >= rules.DisableErrorRate {
		return true
	}
	if rules.MaxTimeoutOrIdle > 0 && window.TimeoutOrStreamIdle > rules.MaxTimeoutOrIdle && window.Errors >= rules.MinErrors {
		return true
	}
	return false
}

func slowWindow(window WindowMetric, rules PoolRules) bool {
	if rules.MaxP95LatencyMS <= 0 || window.P95LatencyMS == nil {
		return false
	}
	return window.Success >= rules.MinLatencySamples && *window.P95LatencyMS > rules.MaxP95LatencyMS
}

func (p Planner) applyDecision(ctx context.Context, decision Decision, states []PoolProviderState) ApplyResult {
	state, ok := stateFor(states, decision.PoolID, decision.Provider)
	if !ok || !state.CurrentInBifrost {
		return ApplyResult{Applied: false, Message: "provider config not found in Bifrost"}
	}

	switch decision.Action {
	case "set_weight", "set_weight_zero":
		if weightsEqual(state.CurrentWeight, decision.TargetWeight) {
			return ApplyResult{Skipped: true, Message: "provider weight already matches target"}
		}
		if err := p.repo.SetProviderWeight(ctx, state, decision.TargetWeight); err != nil {
			return ApplyResult{Applied: false, Message: err.Error()}
		}
		return ApplyResult{Applied: true, Message: "provider weight updated"}
	case "disable_provider_keys":
		if err := p.repo.SetProviderKeysEnabled(ctx, state, false); err != nil {
			return ApplyResult{Applied: false, Message: "key disable failed: " + err.Error()}
		}
		return ApplyResult{Applied: true, Message: "provider keys disabled"}
	case "disable_provider":
		if err := p.repo.SetProviderKeysEnabled(ctx, state, false); err != nil {
			return ApplyResult{Applied: false, Message: "key disable failed before weight update: " + err.Error()}
		}
		if err := p.repo.SetProviderWeight(ctx, state, 0); err != nil {
			return ApplyResult{Applied: false, Message: "keys disabled, but weight update failed: " + err.Error()}
		}
		return ApplyResult{Applied: true, Message: "provider weight set to zero and keys disabled"}
	case "review_missing_provider":
		return ApplyResult{Skipped: true, Message: "manual Bifrost provider-config creation is required"}
	default:
		return ApplyResult{Skipped: true, Message: "action is not applyable"}
	}
}

func decisionNoops(decision Decision) bool {
	if decision.Action == "keep" {
		return true
	}
	switch decision.Action {
	case "set_weight", "set_weight_zero":
		return weightsEqual(decision.CurrentWeight, decision.TargetWeight)
	default:
		return false
	}
}

func poolSnapshots(pools []PoolConfig) []PoolSnapshot {
	out := make([]PoolSnapshot, 0, len(pools))
	for _, pool := range pools {
		out = append(out, PoolSnapshot{ID: pool.ID, VirtualKey: pool.VirtualKey, Kind: pool.Kind})
	}
	return out
}

func activeProviderCount(poolID string, states map[string]PoolProviderState) int {
	count := 0
	for _, state := range states {
		if state.PoolID == poolID && state.CurrentInBifrost && state.CurrentWeight > 0 {
			count++
		}
	}
	return count
}

func projectedActiveProviderCount(current int, state PoolProviderState, decision Decision) int {
	switch decision.Action {
	case "set_weight", "set_weight_zero", "disable_provider":
	default:
		return current
	}
	if !state.CurrentInBifrost {
		return current
	}
	if state.CurrentWeight > 0 && decision.TargetWeight <= 0 {
		return current - 1
	}
	if state.CurrentWeight <= 0 && decision.TargetWeight > 0 {
		return current + 1
	}
	return current
}

func stateFor(states []PoolProviderState, poolID, provider string) (PoolProviderState, bool) {
	for _, state := range states {
		if state.PoolID == poolID && state.Provider == provider {
			return state, true
		}
	}
	return PoolProviderState{}, false
}

func key(poolID, provider string) string {
	return poolID + "\x00" + provider
}

func decisionInputs(metric ProviderMetric) DecisionInputs {
	return DecisionInputs{
		Total:                  metric.Total,
		Success:                metric.Success,
		Errors:                 metric.Errors,
		ErrorRate:              metric.ErrorRate,
		SuccessRate:            metric.SuccessRate,
		P95LatencyMS:           metric.P95LatencyMS,
		TimeoutOrStreamIdle:    metric.TimeoutOrStreamIdle,
		CriticalErrors:         metric.CriticalErrors,
		IgnoredErrors:          metric.IgnoredErrors,
		IgnoredErrorFamilies:   metric.IgnoredErrorFamilies,
		ErrorFamilies:          metric.ErrorFamilies,
		BadWindows:             metric.BadWindows,
		ConsecutiveBadWindows:  metric.ConsecutiveBadWindows,
		SlowWindows:            metric.SlowWindows,
		ConsecutiveSlowWindows: metric.ConsecutiveSlowWindows,
		WindowCount:            len(metric.Windows),
	}
}

func clampWeight(value, min, max float64) float64 {
	if max <= 0 {
		max = 1
	}
	if value < min {
		return min
	}
	if value > max {
		return max
	}
	return math.Round(value*10000) / 10000
}

func weightsEqual(a, b float64) bool {
	return math.Abs(a-b) <= 0.001
}

func severityRank(severity string) int {
	switch severity {
	case "critical":
		return 3
	case "warning":
		return 2
	case "info":
		return 1
	default:
		return 0
	}
}
