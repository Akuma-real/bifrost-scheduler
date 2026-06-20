// package scheduler 表示这个文件属于“领域层调度器”这个包。
//
// 领域层只放业务判断：根据配置、当前状态、最近指标，决定要不要调权重。
package scheduler

// import 表示这个文件要用哪些包。
//
// fmt 用来拼接错误/原因文字，math 用来做浮点数计算，sort 用来排序决策。
import (
	"fmt"
	"math"
	"sort"
)

// DecisionService 是领域层的规则服务。
//
// service 在这里可以理解成“专门做一类判断的对象”。
// 它只保存 RuntimeConfig，因为所有判断都必须依据配置里的阈值。
type DecisionService struct {
	cfg RuntimeConfig
}

// NewDecisionService 创建一个“懂调度规则”的对象。
//
// Domain 可以先理解成“业务规则”。
// 这个对象不会调用 Bifrost，不会读文件，也不会打印输出。
// 它只回答一个问题：根据配置、状态和指标，我们应该做什么决策？
func NewDecisionService(cfg RuntimeConfig) DecisionService {
	// 这里返回的是值，不是指针。
	// 因为 DecisionService 里面只有配置，不需要在方法里修改自己。
	return DecisionService{cfg: cfg}
}

// AnnotateWindows 给每个 provider 的小时间窗口打标记：坏窗口或慢窗口。
//
// 为什么需要它：
// 单个 15 分钟窗口里的异常不应该立刻禁用 provider。
// 所以我们会看多个小窗口，并统计“连续坏了几次”或“连续慢了几次”，再做更强的决策。
func (s DecisionService) AnnotateWindows(metrics []ProviderMetric) []ProviderMetric {
	// make([]ProviderMetric, 0, len(metrics)) 创建一个空切片。
	// 容量预先设成 len(metrics)，避免 append 时频繁扩容。
	out := make([]ProviderMetric, 0, len(metrics))
	for _, metric := range metrics {
		// 先根据 metric.PoolID 找到对应 pool 配置。
		pool, ok := s.pool(metric.PoolID)
		// 找不到 pool，或者没有小窗口，就原样保留。
		if !ok || len(metric.Windows) == 0 {
			out = append(out, metric)
			continue
		}
		// EffectiveRules 返回这个 pool 最终使用的规则。
		rules := pool.EffectiveRules()

		// badWindows 是总坏窗口数量。
		// consecutive 是当前连续坏窗口计数。
		// maxConsecutive 是最大连续坏窗口计数。
		badWindows := 0
		consecutive := 0
		maxConsecutive := 0

		// slowWindows / slowConsecutive / maxSlowConsecutive 同理，只是用于慢窗口。
		slowWindows := 0
		slowConsecutive := 0
		maxSlowConsecutive := 0

		// 用下标遍历，才能拿到 &metric.Windows[i] 并修改这个窗口。
		for i := range metric.Windows {
			window := &metric.Windows[i]
			// badWindow 是纯函数：输入窗口和规则，返回 true/false。
			window.Bad = badWindow(*window, rules, s.cfg.MinimumAttempts)
			if window.Bad {
				badWindows++
				consecutive++
				if consecutive > maxConsecutive {
					maxConsecutive = consecutive
				}
			} else {
				// 一旦遇到非坏窗口，连续计数归零。
				consecutive = 0
			}

			// 慢窗口单独统计，因为错误和慢不是一回事。
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
		// 把统计结果写回 metric，报告和决策都会用到。
		metric.BadWindows = badWindows
		metric.ConsecutiveBadWindows = maxConsecutive
		metric.SlowWindows = slowWindows
		metric.ConsecutiveSlowWindows = maxSlowConsecutive
		out = append(out, metric)
	}
	return out
}

// Decide 比较三类东西，然后返回决策列表：
//   - 配置里写了哪些 provider。
//   - Bifrost 当前实际有哪些 provider 和权重。
//   - 最近日志统计出来的健康指标。
//
// 读这个函数时分三段看：
//  1. 先建立 map，方便快速找到某个 provider 的状态和指标。
//  2. 对配置里的每个 provider，调用 DecideProvider 判断。
//  3. 对“Bifrost 里有，但配置里没有”的 provider，建议人工检查或清零，
//     同时保护 min_active_providers，避免把池子清空。
func (s DecisionService) Decide(states []PoolProviderState, metrics []ProviderMetric, applyEnabled bool) []Decision {
	// 先把 states 列表转成 map，后面用 pool+provider 可以 O(1) 快速查找。
	statesByPoolProvider := map[string]PoolProviderState{}
	for _, state := range states {
		statesByPoolProvider[key(state.PoolID, state.Provider)] = state
	}
	// metrics 也同样转成 map。
	metricsByPoolProvider := map[string]ProviderMetric{}
	for _, metric := range metrics {
		metricsByPoolProvider[key(metric.PoolID, metric.Provider)] = metric
	}
	// 主动测速启用时，提前按 pool 计算每个 provider 的首字优先目标权重。
	// 这样 DecideProvider 仍然只处理单个 provider 的最终判断。
	probeTargets := s.probeAwareTargetWeights(metricsByPoolProvider)

	// var decisions []Decision 声明一个 Decision 切片。
	// 初始值是 nil，但可以直接 append。
	var decisions []Decision

	// 按配置里的 pool 逐个处理。
	for _, pool := range s.cfg.Pools {
		// activeNow 是当前 Bifrost 里这个 pool 有多少 provider 权重大于 0。
		activeNow := activeProviderCount(pool.ID, statesByPoolProvider)
		// projectedActive 是“如果按前面的决策执行后，预计还剩几个活跃 provider”。
		// 它用来防止连续几个清零动作把整个池子清空。
		projectedActive := activeNow
		for _, providerCfg := range pool.Providers {
			// 从 map 里取状态和指标。
			// 如果不存在，Go 会返回结构体零值。
			state := statesByPoolProvider[key(pool.ID, providerCfg.Name)]
			metric := metricsByPoolProvider[key(pool.ID, providerCfg.Name)]
			probeTarget, hasProbeTarget := probeTargets[key(pool.ID, providerCfg.Name)]

			// 单个 provider 的判断交给 DecideProvider。
			decision := s.decideProvider(pool, providerCfg, state, metric, projectedActive, probeTarget, hasProbeTarget)
			decision.DryRun = !applyEnabled
			// 没有实际动作的决策不放进报告，避免报告噪音太大。
			if DecisionNoop(decision) {
				continue
			}
			// 根据这个决策更新 projectedActive，供后续 provider 判断使用。
			projectedActive = projectedActiveProviderCount(projectedActive, state, decision)
			decisions = append(decisions, decision)
		}

		// 第二段：处理 Bifrost 里存在、但配置文件没写的 provider。
		// 这通常代表线上和配置漂移了，需要报告出来。
		for _, state := range states {
			if state.PoolID != pool.ID {
				continue
			}
			// 如果配置里能找到这个 provider，说明前面已经处理过。
			if _, ok := s.cfg.Provider(pool.ID, state.Provider); ok {
				continue
			}
			metric := metricsByPoolProvider[key(pool.ID, state.Provider)]

			// 默认策略：配置没有的 provider 应该被清零。
			targetWeight := 0.0
			action := "set_weight_zero"
			reason := "provider is present in Bifrost VK but missing from scheduler config"
			// 但如果这是最后一个活跃 provider，就只降到最小权重，避免池子不可用。
			if projectedActive <= pool.MinActiveProviders && state.CurrentWeight > 0 {
				targetWeight = degradedTargetWeight(state.CurrentWeight, pool.EffectiveRules().MinWeight)
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
				Inputs:        DecisionInputsFromMetric(metric),
			}
			if DecisionNoop(decision) {
				continue
			}
			decisions = append(decisions, decision)
			projectedActive = projectedActiveProviderCount(projectedActive, state, Decision{Action: action, TargetWeight: targetWeight})
		}
	}

	// 排序让报告稳定：严重级别高的在前，同级别再按 pool/provider 名称排序。
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

// MergeProbeMetrics 把主动测速结果合并进 Bifrost 日志指标。
//
// 为什么单独做这个函数：
// Bifrost 日志没有真实首字字段，主动测速是另一类数据源。
// 合并时只写 Probe*/P95TTFTMS 字段，避免把总耗时误当首字。
func MergeProbeMetrics(metrics []ProviderMetric, probes []ProbeMetric) []ProviderMetric {
	if len(probes) == 0 {
		return metrics
	}
	probesByPoolProvider := map[string]ProbeMetric{}
	for _, probe := range probes {
		probesByPoolProvider[key(probe.PoolID, probe.Provider)] = probe
	}
	out := make([]ProviderMetric, 0, len(metrics))
	for _, metric := range metrics {
		if probe, ok := probesByPoolProvider[key(metric.PoolID, metric.Provider)]; ok {
			metric.ProbeTotal = probe.Total
			metric.ProbeSuccess = probe.Success
			metric.ProbeErrors = probe.Errors
			metric.ProbeErrorRate = probe.ErrorRate
			metric.P95TTFTMS = probe.P95TTFTMS
			metric.P95ProbeLatencyMS = probe.P95LatencyMS
			metric.ProbeErrorFamilies = cloneStringSlice(probe.ErrorFamilies)
		}
		out = append(out, metric)
	}
	return out
}

// cloneStringSlice 复制字符串切片。
//
// 领域层自己保留这个小工具，避免为了复制切片去依赖 Bifrost HTTP 适配层。
func cloneStringSlice(values []string) []string {
	if values == nil {
		return nil
	}
	return append([]string(nil), values...)
}

// DecideProvider 是单个 provider 的核心健康判断规则。
//
// 这个函数是“纯判断”：只读取输入，返回一个 Decision。
// 它不会修改 Bifrost。
//
// 判断顺序从“强安全保护”到“正常恢复”：
//  1. Bifrost 里缺失：需要人工检查。
//  2. 配置不允许或 quarantine：权重清零。
//  3. 流量太少：只给 min_weight 探测。
//  4. 连续关键错误/坏窗口：禁用或清零。
//  5. 错误、超时、延迟告警：降低权重。
//  6. 恢复健康：恢复到配置里的 cost_weight。
func (s DecisionService) DecideProvider(pool PoolConfig, provider ProviderConfig, state PoolProviderState, metric ProviderMetric, activeNow int) Decision {
	return s.decideProvider(pool, provider, state, metric, activeNow, 0, false)
}

// decideProvider 是单个 provider 的核心实现。
//
// probeTarget/hasProbeTarget 来自主动测速的池内相对计算。
// 公共方法 DecideProvider 保留原签名，方便测试单个规则。
func (s DecisionService) decideProvider(pool PoolConfig, provider ProviderConfig, state PoolProviderState, metric ProviderMetric, activeNow int, probeTarget float64, hasProbeTarget bool) Decision {
	// 取出这个 pool 的最终规则。
	rules := pool.EffectiveRules()

	// currentWeight 是 Bifrost 当前权重。
	currentWeight := state.CurrentWeight

	// targetWeight 是健康时应该恢复到的目标权重。
	// 配置没写 cost_weight 时，用默认成本权重。
	targetWeight := provider.CostWeight
	if targetWeight <= 0 {
		targetWeight = rules.DefaultCostWeight
	}

	// minWeight 是探测权重。
	// provider 没单独写时，用 pool 规则里的默认值。
	minWeight := provider.MinWeight
	if minWeight == 0 {
		minWeight = rules.MinWeight
	}

	// base 是默认决策：保持不变。
	// 后面的每个分支只在需要动作时修改 base 并 return。
	base := Decision{
		PoolID:        pool.ID,
		VirtualKey:    pool.VirtualKey,
		Provider:      provider.Name,
		Action:        "keep",
		CurrentWeight: currentWeight,
		TargetWeight:  currentWeight,
		Severity:      "info",
		Inputs:        DecisionInputsFromMetric(metric),
	}

	// 配置里有，但 Bifrost VK 里找不到这个 provider。
	// 调度器不自动创建 provider config，只提醒人工检查。
	if !state.CurrentInBifrost {
		base.Action = "review_missing_provider"
		base.TargetWeight = targetWeight
		base.Severity = "warning"
		base.Reason = "provider is configured for pool but missing from Bifrost virtual key"
		return base
	}

	// quarantine 或 allowed=false：这个 provider 不应该在池子里有流量。
	if !provider.AllowedInPool() {
		// 当前已经是 0，就不用生成动作。
		if currentWeight == 0 {
			return base
		}
		base.Action = "set_weight_zero"
		base.TargetWeight = 0
		base.Severity = "critical"
		base.Reason = "provider is not allowed in this pool"
		return base
	}

	// 样本太少时不做强判断，避免误判。
	if metric.Total < s.cfg.MinimumAttempts {
		// 但如果权重已经为 0，仍然持续看到关键错误，就尝试继续禁用 key。
		if currentWeight == 0 && metric.CriticalErrors >= rules.CriticalErrorThreshold {
			base.Action = "disable_provider_keys"
			base.TargetWeight = 0
			base.Severity = "warning"
			base.Reason = "provider already has zero weight, but critical errors remain; retry disabling bound keys"
			return base
		}
		// 主动测速是调度器自己发出的流式请求。
		// 即使真实业务日志少，只要主动测速样本足够，也可以先按首字证据做温和调权。
		if decision, ok := s.probeDecision(base, rules, metric, currentWeight, targetWeight, minWeight, probeTarget, hasProbeTarget); ok {
			return decision
		}
		// 主动测速已经证明异常时，不能因为业务日志样本少就重新给权重。
		if s.hasDegradationEvidence(rules, metric) {
			return base
		}
		// 如果没有流量且权重为 0，给它一点点探测权重，让系统能重新收集证据。
		if currentWeight == 0 && targetWeight > 0 {
			base.Action = "set_weight"
			base.TargetWeight = minWeight
			base.Severity = "info"
			base.Reason = "provider has too little recent traffic; keep a small probing weight"
			return base
		}
		return base
	}

	// 关键错误 + 高错误率 + 连续坏窗口都满足时，才执行强禁用。
	// 这样可以避免一次短暂异常直接把 provider 永久打下去。
	if metric.CriticalErrors >= rules.CriticalErrorThreshold &&
		metric.ErrorRate >= rules.DisableErrorRate &&
		metric.ConsecutiveBadWindows >= rules.RequiredBadWindows {
		// 安全底线：不能把最后一个活跃 provider 直接禁掉。
		if activeNow <= pool.MinActiveProviders && currentWeight > 0 {
			target := degradedTargetWeight(currentWeight, minWeight)
			if WeightsEqual(currentWeight, target) {
				return base
			}
			base.Action = "set_weight"
			base.TargetWeight = target
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

	// 关键错误已经出现，但连续坏窗口还不够。
	// 这时先降到探测权重，不直接禁用。
	if metric.CriticalErrors >= rules.CriticalErrorThreshold &&
		metric.ErrorRate >= rules.DisableErrorRate &&
		metric.BadWindows > 0 {
		target := degradedTargetWeight(currentWeight, minWeight)
		if WeightsEqual(currentWeight, target) {
			return base
		}
		base.Action = "set_weight"
		base.TargetWeight = target
		base.Severity = "warning"
		base.Reason = fmt.Sprintf("critical credential/quota errors observed: %d; not disabling until %d consecutive bad windows", metric.CriticalErrors, rules.RequiredBadWindows)
		return base
	}

	// 普通错误率达到禁用阈值，并且已经连续坏了足够窗口。
	if metric.Errors >= rules.MinErrors && metric.ErrorRate >= rules.DisableErrorRate && metric.ConsecutiveBadWindows >= rules.RequiredBadWindows {
		if activeNow <= pool.MinActiveProviders && currentWeight > 0 {
			target := degradedTargetWeight(currentWeight, minWeight)
			if WeightsEqual(currentWeight, target) {
				return base
			}
			base.Action = "set_weight"
			base.TargetWeight = target
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

	// 普通错误率已经很高，但连续窗口证据还不够，先降到探测权重。
	if metric.Errors >= rules.MinErrors && metric.ErrorRate >= rules.DisableErrorRate && metric.BadWindows > 0 {
		target := degradedTargetWeight(currentWeight, minWeight)
		if WeightsEqual(currentWeight, target) {
			return base
		}
		base.Action = "set_weight"
		base.TargetWeight = target
		base.Severity = "warning"
		base.Reason = fmt.Sprintf("error rate %.2f%% reached disable threshold, but only %d consecutive bad windows", metric.ErrorRate*100, metric.ConsecutiveBadWindows)
		return base
	}

	// timeout / stream idle 太多，也先降到探测权重。
	if rules.MaxTimeoutOrIdle > 0 && metric.TimeoutOrStreamIdle > rules.MaxTimeoutOrIdle && metric.Errors >= rules.MinErrors && metric.BadWindows > 0 {
		target := degradedTargetWeight(currentWeight, minWeight)
		if WeightsEqual(currentWeight, target) {
			return base
		}
		base.Action = "set_weight"
		base.TargetWeight = target
		base.Severity = "warning"
		base.Reason = fmt.Sprintf("timeout/stream idle count %d exceeded %d", metric.TimeoutOrStreamIdle, rules.MaxTimeoutOrIdle)
		return base
	}

	// 错误率超过普通阈值，但没到禁用阈值，就按错误率比例降权。
	if metric.Errors >= rules.MinErrors && metric.ErrorRate > rules.MaxErrorRate && metric.BadWindows > 0 {
		target := degradedTargetWeight(currentWeight, ClampWeight(targetWeight*(1-metric.ErrorRate), minWeight, targetWeight))
		if WeightsEqual(currentWeight, target) {
			return base
		}
		base.Action = "set_weight"
		base.TargetWeight = target
		base.Severity = "warning"
		base.Reason = fmt.Sprintf("error rate %.2f%% exceeded %.2f%%", metric.ErrorRate*100, rules.MaxErrorRate*100)
		return base
	}

	// 延迟过高也可以降权，但需要成功样本够多，并且连续慢窗口够多。
	if metric.P95LatencyMS != nil &&
		rules.MaxP95LatencyMS > 0 &&
		metric.Success >= rules.MinLatencySamples &&
		*metric.P95LatencyMS > rules.MaxP95LatencyMS &&
		metric.ConsecutiveSlowWindows >= rules.RequiredBadWindows {
		target := degradedTargetWeight(currentWeight, ClampWeight(targetWeight*0.5, minWeight, targetWeight))
		if WeightsEqual(currentWeight, target) {
			return base
		}
		base.Action = "set_weight"
		base.TargetWeight = target
		base.Severity = "warning"
		base.Reason = fmt.Sprintf("p95 latency %.0fms exceeded %.0fms across %d consecutive slow windows", *metric.P95LatencyMS, rules.MaxP95LatencyMS, metric.ConsecutiveSlowWindows)
		return base
	}

	// 真实业务错误已经排除后，再用主动测速做首字优先和成本调权。
	if decision, ok := s.probeDecision(base, rules, metric, currentWeight, targetWeight, minWeight, probeTarget, hasProbeTarget); ok {
		return decision
	}

	// 如果本轮已经有“应该降权或至少不恢复”的证据，后面的恢复逻辑必须停住。
	//
	// 这里修复的是一类矛盾：
	// 前面异常分支发现 provider 坏了，但目标权重刚好等于当前权重，于是没有动作；
	// 如果不拦截，下面的“成功率达标恢复”会立刻把它提高，通知就会变成
	// “因为错误率高所以提高权重”。
	if s.hasDegradationEvidence(rules, metric) {
		return base
	}

	// 当前权重为 0，但最近成功率已经达标，就用最小权重重新放一点流量。
	if currentWeight == 0 && metric.SuccessRate >= rules.MinSuccessRateForRecovery {
		base.Action = "set_weight"
		base.TargetWeight = minWeight
		base.Severity = "info"
		base.Reason = fmt.Sprintf("provider looks recovered with %.2f%% success; re-enter at minimum weight", metric.SuccessRate*100)
		return base
	}

	// 当前权重不是目标权重，并且 provider 健康，就恢复到 cost_weight。
	if currentWeight > 0 && math.Abs(currentWeight-targetWeight) >= rules.MinWeightChange && metric.SuccessRate >= rules.MinSuccessRateForRecovery {
		target := steppedTargetWeight(currentWeight, targetWeight, rules.MaxWeightStep)
		if WeightsEqual(currentWeight, target) {
			return base
		}
		base.Action = "set_weight"
		base.TargetWeight = target
		base.Severity = "info"
		base.Reason = "provider is healthy; restore configured base weight"
		return base
	}

	return base
}

// probeDecision 根据主动测速证据生成温和调权决策。
//
// 注意它只处理主动测速，不处理 Bifrost 日志错误。
// Bifrost 日志错误率、关键错误和连续坏窗口仍然是更强的安全兜底。
func (s DecisionService) probeDecision(base Decision, rules PoolRules, metric ProviderMetric, currentWeight, targetWeight, minWeight, probeTarget float64, hasProbeTarget bool) (Decision, bool) {
	if !s.cfg.Probe.Enabled {
		return Decision{}, false
	}
	// 主动测速失败率高，先降到最小探测权重。
	if metric.ProbeTotal >= rules.MinProbeSamples &&
		metric.ProbeErrors > 0 &&
		metric.ProbeErrorRate >= rules.MaxErrorRate {
		if currentWeight == 0 {
			return Decision{}, false
		}
		target := degradedTargetWeight(currentWeight, minWeight)
		if WeightsEqual(currentWeight, target) {
			return Decision{}, false
		}
		base.Action = "set_weight"
		base.TargetWeight = target
		base.Severity = "warning"
		base.Reason = fmt.Sprintf("active probe error rate %.2f%% exceeded %.2f%%", metric.ProbeErrorRate*100, rules.MaxErrorRate*100)
		return base, true
	}
	// 首字优先：如果主动测速有足够样本，就按“首字速度 + 成本”计算目标权重。
	// 这不是用 Bifrost 日志 latency 冒充首字，而是用流式探测真正收到第一段响应的时间。
	if metric.P95TTFTMS == nil || metric.ProbeSuccess < rules.MinProbeSamples {
		return Decision{}, false
	}
	if rules.MaxP95TTFTMS > 0 && *metric.P95TTFTMS > rules.MaxP95TTFTMS {
		if currentWeight == 0 {
			return Decision{}, false
		}
		target := degradedTargetWeight(currentWeight, ClampWeight(targetWeight*0.5, minWeight, targetWeight))
		if WeightsEqual(currentWeight, target) {
			return Decision{}, false
		}
		base.Action = "set_weight"
		base.TargetWeight = target
		base.Severity = "warning"
		base.Reason = fmt.Sprintf("probe p95 ttft %.0fms exceeded %.0fms", *metric.P95TTFTMS, rules.MaxP95TTFTMS)
		return base, true
	}
	// 权重为 0 的 provider 即使主动测速恢复，也先用最小权重重新进池。
	// 不用 1-2 次主动测速直接拉到完整目标权重。
	if currentWeight == 0 && targetWeight > 0 {
		base.Action = "set_weight"
		base.TargetWeight = minWeight
		base.Severity = "info"
		base.Reason = "active probe looks recovered; re-enter at minimum weight"
		return base, true
	}
	if hasProbeTarget && math.Abs(currentWeight-probeTarget) >= rules.MinWeightChange {
		target := steppedTargetWeight(currentWeight, probeTarget, rules.MaxWeightStep)
		if WeightsEqual(currentWeight, target) {
			return Decision{}, false
		}
		base.Action = "set_weight"
		base.TargetWeight = target
		base.Severity = "info"
		base.Reason = "active probe ttft priority adjusted weight"
		return base, true
	}
	return Decision{}, false
}

// degradedTargetWeight 给所有“异常导致降权”的分支兜底。
//
// 这些分支的语义是“变坏了，所以降低或保持权重”。
// 如果公式算出的目标比当前权重更高，就保持当前权重，避免出现
// “因为错误率高，所以把权重提高”的矛盾动作。
func degradedTargetWeight(currentWeight, targetWeight float64) float64 {
	if currentWeight <= 0 {
		return 0
	}
	if targetWeight > currentWeight {
		return currentWeight
	}
	return targetWeight
}

// steppedTargetWeight 给“健康调权”限速。
//
// 主动测速是少量样本，适合提示方向，不适合每轮把权重大幅跳变。
// 这个函数只限制健康恢复/首字优先调权；异常降权仍然可以快速执行。
func steppedTargetWeight(currentWeight, targetWeight, maxStep float64) float64 {
	if maxStep <= 0 {
		return targetWeight
	}
	if targetWeight > currentWeight+maxStep {
		return ClampWeight(currentWeight+maxStep, 0, 1)
	}
	if targetWeight < currentWeight-maxStep {
		return ClampWeight(currentWeight-maxStep, 0, 1)
	}
	return targetWeight
}

// hasDegradationEvidence 判断“本轮有没有明确的异常证据”。
//
// 它不直接生成动作，只回答一个问题：
// 后面的恢复逻辑现在能不能把权重拉高？
//
// 返回 true 时表示不能恢复。原因可能来自两类：
//   - Bifrost 真实业务日志：错误率、关键错误、timeout/idle、慢窗口。
//   - 调度器主动测速：测速失败率高，或者首字 P95 超过阈值。
func (s DecisionService) hasDegradationEvidence(rules PoolRules, metric ProviderMetric) bool {
	if metric.CriticalErrors >= rules.CriticalErrorThreshold &&
		metric.ErrorRate >= rules.DisableErrorRate &&
		metric.BadWindows > 0 {
		return true
	}
	if metric.Errors >= rules.MinErrors &&
		metric.ErrorRate >= rules.DisableErrorRate &&
		metric.BadWindows > 0 {
		return true
	}
	if rules.MaxTimeoutOrIdle > 0 &&
		metric.TimeoutOrStreamIdle > rules.MaxTimeoutOrIdle &&
		metric.Errors >= rules.MinErrors &&
		metric.BadWindows > 0 {
		return true
	}
	if metric.Errors >= rules.MinErrors &&
		metric.ErrorRate > rules.MaxErrorRate &&
		metric.BadWindows > 0 {
		return true
	}
	if metric.P95LatencyMS != nil &&
		rules.MaxP95LatencyMS > 0 &&
		metric.Success >= rules.MinLatencySamples &&
		*metric.P95LatencyMS > rules.MaxP95LatencyMS &&
		metric.SlowWindows > 0 {
		return true
	}
	if !s.cfg.Probe.Enabled {
		return false
	}
	if metric.ProbeTotal >= rules.MinProbeSamples &&
		metric.ProbeErrors > 0 &&
		metric.ProbeErrorRate >= rules.MaxErrorRate {
		return true
	}
	if metric.P95TTFTMS != nil &&
		rules.MaxP95TTFTMS > 0 &&
		metric.ProbeSuccess >= rules.MinProbeSamples &&
		*metric.P95TTFTMS > rules.MaxP95TTFTMS {
		return true
	}
	return false
}

// probeAwareTargetWeights 按“首字优先，其次成本”计算每个 provider 的健康目标权重。
//
// 公式分两步：
//  1. 把 provider 的 TTFT 转成速度分。最快 provider 得 1，越慢越低。
//  2. 用 TTFTPriority 混合速度分和 cost_weight。
func (s DecisionService) probeAwareTargetWeights(metrics map[string]ProviderMetric) map[string]float64 {
	out := map[string]float64{}
	if !s.cfg.Probe.Enabled {
		return out
	}
	for _, pool := range s.cfg.Pools {
		rules := pool.EffectiveRules()
		fastestTTFT := 0.0
		maxCostWeight := 0.0
		eligible := map[string]ProviderMetric{}

		for _, provider := range pool.Providers {
			metric, ok := metrics[key(pool.ID, provider.Name)]
			if !ok || metric.P95TTFTMS == nil || metric.ProbeSuccess < rules.MinProbeSamples {
				continue
			}
			ttft := *metric.P95TTFTMS
			if ttft <= 0 {
				continue
			}
			eligible[provider.Name] = metric
			if fastestTTFT == 0 || ttft < fastestTTFT {
				fastestTTFT = ttft
			}
			costWeight := provider.CostWeight
			if costWeight <= 0 {
				costWeight = rules.DefaultCostWeight
			}
			if costWeight > maxCostWeight {
				maxCostWeight = costWeight
			}
		}
		if len(eligible) < 2 || fastestTTFT <= 0 || maxCostWeight <= 0 {
			continue
		}

		priority := ClampWeight(rules.TTFTPriority, 0, 1)
		for _, provider := range pool.Providers {
			metric, ok := eligible[provider.Name]
			if !ok {
				continue
			}
			costWeight := provider.CostWeight
			if costWeight <= 0 {
				costWeight = rules.DefaultCostWeight
			}
			minWeight := provider.MinWeight
			if minWeight == 0 {
				minWeight = rules.MinWeight
			}
			speedScore := fastestTTFT / *metric.P95TTFTMS
			costScore := costWeight / maxCostWeight
			score := priority*speedScore + (1-priority)*costScore
			out[key(pool.ID, provider.Name)] = ClampWeight(costWeight*score, minWeight, costWeight)
		}
	}
	return out
}

// pool 根据 poolID 查找 pool 配置。
func (s DecisionService) pool(poolID string) (PoolConfig, bool) {
	for _, pool := range s.cfg.Pools {
		if pool.ID == poolID {
			return pool, true
		}
	}
	return PoolConfig{}, false
}

// badWindow 判断一个小窗口是否算“坏窗口”。
//
// 只有样本数足够时才判断，避免 1 个失败请求就把窗口打坏。
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

// slowWindow 判断一个小窗口是否算“慢窗口”。
func slowWindow(window WindowMetric, rules PoolRules) bool {
	// 没配置 MaxP95LatencyMS，或者窗口没有 P95，就不判断慢。
	if rules.MaxP95LatencyMS <= 0 || window.P95LatencyMS == nil {
		return false
	}
	return window.Success >= rules.MinLatencySamples && *window.P95LatencyMS > rules.MaxP95LatencyMS
}

// DecisionNoop 判断一个决策是否等于“什么都不用做”。
//
// 返回 true 的决策不会出现在最终报告里。
func DecisionNoop(decision Decision) bool {
	if decision.Action == "keep" {
		return true
	}
	switch decision.Action {
	case "set_weight", "set_weight_zero":
		return WeightsEqual(decision.CurrentWeight, decision.TargetWeight)
	default:
		return false
	}
}

// PoolSnapshots 把完整 pool 配置转换成报告里需要展示的简短信息。
func PoolSnapshots(pools []PoolConfig) []PoolSnapshot {
	out := make([]PoolSnapshot, 0, len(pools))
	for _, pool := range pools {
		out = append(out, PoolSnapshot{ID: pool.ID, VirtualKey: pool.VirtualKey, Kind: pool.Kind})
	}
	return out
}

// activeProviderCount 统计某个 pool 当前有多少 provider 权重大于 0。
func activeProviderCount(poolID string, states map[string]PoolProviderState) int {
	count := 0
	for _, state := range states {
		if state.PoolID == poolID && state.CurrentInBifrost && state.CurrentWeight > 0 {
			count++
		}
	}
	return count
}

// projectedActiveProviderCount 根据一个决策推算活跃 provider 数量会怎么变化。
//
// 这个函数不写 Bifrost，只是在内存里做预测。
func projectedActiveProviderCount(current int, state PoolProviderState, decision Decision) int {
	// 只有这些动作会影响活跃 provider 数。
	switch decision.Action {
	case "set_weight", "set_weight_zero", "disable_provider":
	default:
		return current
	}
	if !state.CurrentInBifrost {
		return current
	}
	// 原来大于 0，目标小于等于 0，活跃数减少。
	if state.CurrentWeight > 0 && decision.TargetWeight <= 0 {
		return current - 1
	}
	// 原来小于等于 0，目标大于 0，活跃数增加。
	if state.CurrentWeight <= 0 && decision.TargetWeight > 0 {
		return current + 1
	}
	return current
}

// StateFor 从状态列表里找指定 pool/provider 的状态。
func StateFor(states []PoolProviderState, poolID, provider string) (PoolProviderState, bool) {
	for _, state := range states {
		if state.PoolID == poolID && state.Provider == provider {
			return state, true
		}
	}
	return PoolProviderState{}, false
}

// key 把 poolID 和 provider 拼成 map 的 key。
//
// 中间用 "\x00" 是为了避免普通字符冲突。
// 例如 pool="ab", provider="c" 和 pool="a", provider="bc" 不会混在一起。
func key(poolID, provider string) string {
	return poolID + "\x00" + provider
}

// DecisionInputsFromMetric 把 ProviderMetric 转成 DecisionInputs。
//
// ProviderMetric 是完整指标；DecisionInputs 是写入某个决策里的证据快照。
func DecisionInputsFromMetric(metric ProviderMetric) DecisionInputs {
	return DecisionInputs{
		Total:                  metric.Total,
		Success:                metric.Success,
		Errors:                 metric.Errors,
		ErrorRate:              metric.ErrorRate,
		SuccessRate:            metric.SuccessRate,
		P95LatencyMS:           metric.P95LatencyMS,
		ProbeTotal:             metric.ProbeTotal,
		ProbeSuccess:           metric.ProbeSuccess,
		ProbeErrors:            metric.ProbeErrors,
		ProbeErrorRate:         metric.ProbeErrorRate,
		P95TTFTMS:              metric.P95TTFTMS,
		P95ProbeLatencyMS:      metric.P95ProbeLatencyMS,
		TimeoutOrStreamIdle:    metric.TimeoutOrStreamIdle,
		CriticalErrors:         metric.CriticalErrors,
		IgnoredErrors:          metric.IgnoredErrors,
		IgnoredErrorFamilies:   metric.IgnoredErrorFamilies,
		ErrorFamilies:          metric.ErrorFamilies,
		ProbeErrorFamilies:     metric.ProbeErrorFamilies,
		BadWindows:             metric.BadWindows,
		ConsecutiveBadWindows:  metric.ConsecutiveBadWindows,
		SlowWindows:            metric.SlowWindows,
		ConsecutiveSlowWindows: metric.ConsecutiveSlowWindows,
		WindowCount:            len(metric.Windows),
	}
}

// ClampWeight 把权重限制在 min 和 max 之间。
//
// 例如算出来是 0.001，但 min 是 0.05，就返回 0.05。
func ClampWeight(value, min, max float64) float64 {
	if max <= 0 {
		max = 1
	}
	if value < min {
		return min
	}
	if value > max {
		return max
	}
	// 保留 4 位小数，避免浮点数出现 0.30000000004 这种显示。
	return math.Round(value*10000) / 10000
}

// WeightsEqual 判断两个权重是否可以视为相等。
//
// 浮点数不能总是直接用 == 比较，所以这里允许 0.001 的误差。
func WeightsEqual(a, b float64) bool {
	return math.Abs(a-b) <= 0.001
}

// severityRank 把严重级别转成数字，方便排序。
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
