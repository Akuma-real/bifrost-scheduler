// package scheduler 表示这个文件属于“应用层调度器”这个包。
//
// 这里的代码负责把流程串起来：读取状态、读取指标、调用领域规则、必要时执行写入。
package scheduler

// import 表示这个文件要用哪些包。
//
// context 和 time 是 Go 标准库。
// domain 是本项目的领域层包，里面放调度规则和核心数据结构。
import (
	"context"
	"time"

	domain "github.com/Akuma-real/bifrost-scheduler/internal/domain/scheduler"
)

// Planner 是应用层的“用例对象”。
//
// 初学时可以把它理解成“调度总管”：
//  1. 让 Store 读取当前 provider 状态。
//  2. 让 Store 读取最近日志指标。
//  3. 把状态和指标交给领域层 DecisionService 判断。
//  4. 如果允许写入，就通过 Store 执行受保护的变更。
//
// 它不知道 Bifrost HTTP 怎么调用，也不知道 Markdown 怎么渲染。
type Planner struct {
	cfg     domain.RuntimeConfig
	store   Store
	now     time.Time
	decider domain.DecisionService
}

// interface 表示“规定一组必须会做的动作”。
//
// Store 是调度器和外部世界之间的边界。
// Planner 不关心 Store 背后是真实 Bifrost API，还是测试里假的对象。
// 只要某个对象有下面这四个方法，它就可以当 Store 用。
type Store interface {
	LoadProviderStates(ctx context.Context, pools []domain.PoolConfig) ([]domain.PoolProviderState, error)
	LoadMetrics(ctx context.Context, pools []domain.PoolConfig, windowStart, windowEnd time.Time, windowDuration time.Duration) ([]domain.ProviderMetric, error)
	SetProviderWeight(ctx context.Context, state domain.PoolProviderState, weight float64) error
	SetProviderKeysEnabled(ctx context.Context, state domain.PoolProviderState, enabled bool) error
}

// NewPlanner 创建一个 Planner。
//
// Go 里经常用 NewX 这种函数名表示“创建某个对象”。
// 如果 now 是空时间，就使用当前时间。
func NewPlanner(cfg domain.RuntimeConfig, store Store, now time.Time) Planner {
	// now.IsZero() 判断是否传入了空时间。
	// 测试里常传固定时间，生产运行时如果没传，就用 time.Now()。
	if now.IsZero() {
		now = time.Now()
	}
	// 返回一个 Planner 结构体。
	return Planner{
		cfg:     cfg,
		store:   store,
		now:     now,
		decider: domain.NewDecisionService(cfg),
	}
}

// BuildPlan 表示一次完整调度。
//
// 这个函数返回两个值：
//   - domain.Plan：可以输出成 JSON/Markdown 的调度计划。
//   - error：如果调度失败，这里会告诉上层失败原因；成功时是 nil。
//
// apply 这个参数不是单独决定写入。
// 只有同时满足下面两个条件，才会真的写 Bifrost：
//   - apply 是 true。
//   - 配置文件 mode 是 guarded_write。
func (p Planner) BuildPlan(ctx context.Context, apply bool) (domain.Plan, error) {
	// windowStart 是统计窗口开始时间。
	// 例如单窗口 15m，QualityWindows=3，那么总窗口是 45m。
	windowStart := p.now.Add(-p.cfg.WindowDuration * time.Duration(p.cfg.QualityWindows))
	// windowLength 是报告里显示的总统计窗口长度。
	windowLength := p.cfg.WindowDuration * time.Duration(p.cfg.QualityWindows)

	// 读取 Bifrost 当前 VK/provider 权重和 key 策略。
	states, err := p.store.LoadProviderStates(ctx, p.cfg.Pools)
	if err != nil {
		return domain.Plan{}, err
	}
	// 读取最近窗口里的调用日志，并聚合成成功率、错误率、P95 等指标。
	metrics, err := p.store.LoadMetrics(ctx, p.cfg.Pools, windowStart, p.now, p.cfg.WindowDuration)
	if err != nil {
		return domain.Plan{}, err
	}
	// 领域层给每个小窗口打“坏窗口/慢窗口”标记。
	metrics = p.decider.AnnotateWindows(metrics)

	// 先组装 Plan 的基础字段。
	plan := domain.Plan{
		GeneratedAt:   p.now,
		WindowStart:   windowStart,
		Window:        windowLength.String(),
		Mode:          p.cfg.Mode,
		ApplyEnabled:  apply && p.cfg.Mode == "guarded_write",
		Pools:         domain.PoolSnapshots(p.cfg.Pools),
		Metrics:       metrics,
		CurrentStates: states,
	}
	// 再根据状态和指标生成决策。
	plan.Decisions = p.decider.Decide(states, metrics, plan.ApplyEnabled)

	// apply 为 true 表示用户命令行加了 --apply。
	if apply {
		// 但如果配置文件不是 guarded_write，仍然不写线上。
		// 这里给每个决策补一个 ApplyResult，告诉报告“因为模式不允许，所以没有执行”。
		if p.cfg.Mode != "guarded_write" {
			for i := range plan.Decisions {
				plan.Decisions[i].Apply = &domain.ApplyResult{
					Applied: false,
					Message: "apply requested but config mode is not guarded_write",
				}
			}
			return plan, nil
		}
		// 真正执行每个决策，并把执行结果写回 plan.Decisions[i].Apply。
		for i := range plan.Decisions {
			result := p.applyDecision(ctx, plan.Decisions[i], states)
			plan.Decisions[i].Apply = &result
		}
	}

	return plan, nil
}

// applyDecision 执行一个已经生成的决策。
//
// 领域层只负责判断“应该做什么”。
// 应用层负责真的调用 Store 去执行。
// 这样规则测试不用连接真实 Bifrost。
func (p Planner) applyDecision(ctx context.Context, decision domain.Decision, states []domain.PoolProviderState) domain.ApplyResult {
	// 先从当前状态列表里找到这个 decision 对应的 provider。
	state, ok := domain.StateFor(states, decision.PoolID, decision.Provider)
	if !ok || !state.CurrentInBifrost {
		return domain.ApplyResult{Applied: false, Message: "provider config not found in Bifrost"}
	}

	// switch 根据动作类型选择不同执行方式。
	switch decision.Action {
	case "set_weight", "set_weight_zero":
		// 如果当前权重已经等于目标权重，就不发 PUT 请求，直接标记 skipped。
		if domain.WeightsEqual(state.CurrentWeight, decision.TargetWeight) {
			return domain.ApplyResult{Skipped: true, Message: "provider weight already matches target"}
		}
		// 调用 Store 写 Bifrost 权重。
		if err := p.store.SetProviderWeight(ctx, state, decision.TargetWeight); err != nil {
			return domain.ApplyResult{Applied: false, Message: err.Error()}
		}
		return domain.ApplyResult{Applied: true, Message: "provider weight updated"}
	case "disable_provider_keys":
		// 只禁用绑定 key，不改权重。
		if err := p.store.SetProviderKeysEnabled(ctx, state, false); err != nil {
			return domain.ApplyResult{Applied: false, Message: "key disable failed: " + err.Error()}
		}
		return domain.ApplyResult{Applied: true, Message: "provider keys disabled"}
	case "disable_provider":
		// 更强的禁用动作：先禁用 key，再把权重改成 0。
		// 顺序很重要：如果禁用 key 失败，就不要继续改权重，避免状态半吊子。
		if err := p.store.SetProviderKeysEnabled(ctx, state, false); err != nil {
			return domain.ApplyResult{Applied: false, Message: "key disable failed before weight update: " + err.Error()}
		}
		if err := p.store.SetProviderWeight(ctx, state, 0); err != nil {
			return domain.ApplyResult{Applied: false, Message: "keys disabled, but weight update failed: " + err.Error()}
		}
		return domain.ApplyResult{Applied: true, Message: "provider weight set to zero and keys disabled"}
	case "review_missing_provider":
		// 缺失 provider config 需要人工在 Bifrost UI/API 创建，调度器不自动创建。
		return domain.ApplyResult{Skipped: true, Message: "manual Bifrost provider-config creation is required"}
	default:
		// 未知或不可执行动作统一跳过。
		return domain.ApplyResult{Skipped: true, Message: "action is not applyable"}
	}
}
