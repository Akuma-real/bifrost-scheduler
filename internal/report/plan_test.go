// package report 表示这个测试文件属于报告输出层。
package report

// import 是测试需要用到的包。
//
// strings：检查 Markdown 字符串是否包含某段文本。
// testing：Go 标准测试包。
// time：构造固定时间。
// domain：构造 Plan 测试数据。
import (
	"strings"
	"testing"
	"time"

	domain "github.com/Akuma-real/bifrost-scheduler/internal/domain/scheduler"
)

// TestPlanMarkdownShowsHumanActionPreview 验证 Markdown 报告会输出人能看懂的动作说明。
func TestPlanMarkdownShowsHumanActionPreview(t *testing.T) {
	// 手工构造一个最小 Plan。
	// 这样测试只关注 Markdown 渲染，不依赖真实 Bifrost 或 Planner。
	plan := domain.Plan{
		GeneratedAt:  time.Date(2026, 6, 19, 13, 0, 0, 0, time.UTC),
		WindowStart:  time.Date(2026, 6, 19, 12, 45, 0, 0, time.UTC),
		Window:       "15m0s",
		Mode:         "read_only",
		ApplyEnabled: false,
		Pools: []domain.PoolSnapshot{{
			ID:         "gpt_stable",
			VirtualKey: "vk_stable_text",
			Kind:       "text",
		}},
		CurrentStates: []domain.PoolProviderState{{
			PoolID:           "gpt_stable",
			VirtualKey:       "vk_stable_text",
			Provider:         "provider_a",
			CurrentWeight:    1,
			CurrentInBifrost: true,
		}},
		Metrics: []domain.ProviderMetric{{
			PoolID:      "gpt_stable",
			VirtualKey:  "vk_stable_text",
			Provider:    "provider_a",
			Total:       12,
			Success:     0,
			Errors:      12,
			ErrorRate:   1,
			SuccessRate: 0,
		}},
		Decisions: []domain.Decision{{
			PoolID:        "gpt_stable",
			VirtualKey:    "vk_stable_text",
			Provider:      "provider_a",
			Action:        "set_weight_zero",
			CurrentWeight: 1,
			TargetWeight:  0,
			Reason:        "provider is not allowed in this pool",
			Severity:      "critical",
			DryRun:        true,
			Inputs: domain.DecisionInputs{
				Total:     12,
				Success:   0,
				Errors:    12,
				ErrorRate: 1,
			},
		}},
	}

	// 生成 Markdown。
	out := Markdown(plan)
	// 逐个检查关键文字是否存在。
	for _, want := range []string{
		"# Bifrost 调度预览",
		"是否会写线上：`不会`",
		"把 `provider_a` 的权重清零",
		"执行结果：未执行，只预览",
	} {
		if !strings.Contains(out, want) {
			// 如果缺少关键文字，就把完整输出打印出来，方便定位。
			t.Fatalf("markdown output missing %q:\n%s", want, out)
		}
	}
}

// TestHumanReasonKeepsActiveProbeErrorSpecific 验证主动测速失败率不会被普通错误率文案覆盖。
func TestHumanReasonKeepsActiveProbeErrorSpecific(t *testing.T) {
	reason := HumanReason(domain.Decision{
		Action: "set_weight",
		Reason: "active probe error rate 100.00% exceeded 50.00%",
	})
	if !strings.Contains(reason, "主动测速失败率") {
		t.Fatalf("reason = %q, want active-probe-specific text", reason)
	}
}
