// package scheduler 表示这个测试文件属于应用层 scheduler 包。
//
// 和生产代码同包，测试可以调用 applyDecision 这种小写开头的内部函数。
package scheduler

// import 是测试要用到的包。
//
// context：创建测试用上下文。
// fmt：制造一个假的错误。
// testing：Go 标准测试包。
// time：构造空时间。
// domain：领域层配置和数据结构。
import (
	"context"
	"fmt"
	"testing"
	"time"

	domain "github.com/Akuma-real/bifrost-scheduler/internal/domain/scheduler"
)

// TestDisableProviderDoesNotChangeWeightWhenKeyDisableFails 验证一个安全顺序：
//
// disable_provider 需要先禁用 key，再清零权重。
// 如果禁用 key 失败，就不能继续改权重，避免线上出现半执行状态。
func TestDisableProviderDoesNotChangeWeightWhenKeyDisableFails(t *testing.T) {
	// 先构造一个最小 guarded_write 配置。
	cfg, err := domain.NormalizeConfig(domain.Config{
		Mode:            "guarded_write",
		MinimumAttempts: 10,
		Pools: []domain.PoolConfig{
			{
				ID:         "pool_a",
				VirtualKey: "vk_a",
				Providers: []domain.ProviderConfig{
					{Name: "provider_a", CostWeight: 0.7},
					{Name: "provider_b", CostWeight: 0.7},
				},
				Rules: &domain.PoolRules{CriticalErrorThreshold: 2, RequiredBadWindows: 2},
			},
		},
	})
	if err != nil {
		t.Fatalf("NormalizeConfig returned error: %v", err)
	}

	// fakeStore 是测试里的假 Store。
	// 它不访问真实 Bifrost，只返回我们准备好的 states/metrics。
	store := &fakeStore{
		states: []domain.PoolProviderState{
			{PoolID: "pool_a", VirtualKey: "vk_a", Provider: "provider_a", CurrentWeight: 0.7, CurrentInBifrost: true},
			{PoolID: "pool_a", VirtualKey: "vk_a", Provider: "provider_b", CurrentWeight: 0.7, CurrentInBifrost: true},
		},
		metrics: []domain.ProviderMetric{
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
		// keyErr 表示禁用 key 时模拟失败。
		keyErr: fmt.Errorf("no bound keys"),
	}

	// BuildPlan(..., true) 表示测试 apply=true 的执行路径。
	plan, err := NewPlanner(cfg, store, zeroTime()).BuildPlan(context.Background(), true)
	if err != nil {
		t.Fatalf("BuildPlan returned error: %v", err)
	}
	if len(plan.Decisions) != 1 {
		t.Fatalf("decisions = %+v, want one decision", plan.Decisions)
	}
	// 期望生成 disable_provider，但是 Apply 失败。
	if plan.Decisions[0].Action != "disable_provider" || plan.Decisions[0].Apply == nil || plan.Decisions[0].Apply.Applied {
		t.Fatalf("decision = %+v, want failed disable_provider", plan.Decisions[0])
	}
	// key 禁用调用了 1 次。
	if store.keyDisableCalls != 1 {
		t.Fatalf("keyDisableCalls = %d, want 1", store.keyDisableCalls)
	}
	// 权重更新不能被调用，因为前一步失败了。
	if store.weightCalls != 0 {
		t.Fatalf("weightCalls = %d, want 0", store.weightCalls)
	}
}

// TestApplyDecisionSkipsNoopWeightUpdate 验证“目标权重等于当前权重”时不会发写入请求。
func TestApplyDecisionSkipsNoopWeightUpdate(t *testing.T) {
	cfg, err := domain.NormalizeConfig(domain.Config{
		Mode: "guarded_write",
		Pools: []domain.PoolConfig{{
			ID:         "pool_a",
			VirtualKey: "vk_a",
			Providers:  []domain.ProviderConfig{{Name: "provider_a", CostWeight: 0.7}},
		}},
	})
	if err != nil {
		t.Fatalf("NormalizeConfig returned error: %v", err)
	}

	store := &fakeStore{}
	// 这里直接测试 applyDecision，而不是完整 BuildPlan。
	result := NewPlanner(cfg, store, zeroTime()).applyDecision(
		context.Background(),
		domain.Decision{PoolID: "pool_a", Provider: "provider_a", Action: "set_weight", TargetWeight: 0.05},
		[]domain.PoolProviderState{{PoolID: "pool_a", Provider: "provider_a", CurrentWeight: 0.05, CurrentInBifrost: true}},
	)

	if !result.Skipped || result.Applied {
		t.Fatalf("result = %+v, want skipped no-op", result)
	}
	// 没有真正写入，所以 weightCalls 必须是 0。
	if store.weightCalls != 0 {
		t.Fatalf("weightCalls = %d, want 0", store.weightCalls)
	}
}

// zeroTime 返回 Go 的空时间。
//
// NewPlanner 收到空时间后会自己使用 time.Now()。
func zeroTime() time.Time {
	return time.Time{}
}

// fakeStore 是测试用的假外部存储/API。
//
// 它实现了 Store 接口，但不会发网络请求。
// 这种写法可以让应用层测试稳定、快速、不依赖真实 Bifrost。
type fakeStore struct {
	states          []domain.PoolProviderState
	metrics         []domain.ProviderMetric
	weightCalls     int
	keyDisableCalls int
	weightErr       error
	keyErr          error
}

// LoadProviderStates 返回测试预置的状态。
//
// 参数名前面的 _ 表示这个参数当前不用。
func (s fakeStore) LoadProviderStates(_ context.Context, _ []domain.PoolConfig) ([]domain.PoolProviderState, error) {
	return s.states, nil
}

// LoadMetrics 返回测试预置的指标。
func (s fakeStore) LoadMetrics(_ context.Context, _ []domain.PoolConfig, _, _ time.Time, _ time.Duration) ([]domain.ProviderMetric, error) {
	return s.metrics, nil
}

// SetProviderWeight 模拟写权重，并记录调用次数。
func (s *fakeStore) SetProviderWeight(_ context.Context, _ domain.PoolProviderState, _ float64) error {
	s.weightCalls++
	return s.weightErr
}

// SetProviderKeysEnabled 模拟启用/禁用 key，并记录调用次数。
func (s *fakeStore) SetProviderKeysEnabled(_ context.Context, _ domain.PoolProviderState, _ bool) error {
	s.keyDisableCalls++
	return s.keyErr
}
