package scheduler

import (
	"strings"
	"testing"
	"time"
)

func TestPlanMarkdownShowsHumanActionPreview(t *testing.T) {
	plan := Plan{
		GeneratedAt:  time.Date(2026, 6, 19, 13, 0, 0, 0, time.UTC),
		WindowStart:  time.Date(2026, 6, 19, 12, 45, 0, 0, time.UTC),
		Window:       "15m0s",
		Mode:         "read_only",
		ApplyEnabled: false,
		Pools: []PoolSnapshot{{
			ID:         "gpt_stable",
			VirtualKey: "vk_stable_text",
			Kind:       "text",
		}},
		CurrentStates: []PoolProviderState{{
			PoolID:           "gpt_stable",
			VirtualKey:       "vk_stable_text",
			Provider:         "provider_a",
			CurrentWeight:    1,
			CurrentInBifrost: true,
		}},
		Metrics: []ProviderMetric{{
			PoolID:      "gpt_stable",
			VirtualKey:  "vk_stable_text",
			Provider:    "provider_a",
			Total:       12,
			Success:     0,
			Errors:      12,
			ErrorRate:   1,
			SuccessRate: 0,
		}},
		Decisions: []Decision{{
			PoolID:        "gpt_stable",
			VirtualKey:    "vk_stable_text",
			Provider:      "provider_a",
			Action:        "set_weight_zero",
			CurrentWeight: 1,
			TargetWeight:  0,
			Reason:        "provider is not allowed in this pool",
			Severity:      "critical",
			DryRun:        true,
			Inputs: DecisionInputs{
				Total:     12,
				Success:   0,
				Errors:    12,
				ErrorRate: 1,
			},
		}},
	}

	out := plan.Markdown()
	for _, want := range []string{
		"# Bifrost 调度预览",
		"是否会写线上：`不会`",
		"把 `provider_a` 的权重清零",
		"执行结果：未执行，只预览",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("markdown output missing %q:\n%s", want, out)
		}
	}
}
