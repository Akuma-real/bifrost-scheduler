package scheduler

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

func WritePlan(w io.Writer, plan Plan, format string) error {
	switch strings.ToLower(format) {
	case "json":
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(plan)
	case "markdown", "md", "":
		_, err := fmt.Fprint(w, plan.Markdown())
		return err
	default:
		return fmt.Errorf("unsupported output format %q", format)
	}
}

func (p Plan) Markdown() string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Bifrost 调度预览\n\n")
	fmt.Fprintf(&b, "## 执行状态\n\n")
	fmt.Fprintf(&b, "- 生成时间：`%s`\n", p.GeneratedAt.Format(time.RFC3339))
	fmt.Fprintf(&b, "- 统计窗口：`%s` 到 `生成时间`，总长度 `%s`\n", p.WindowStart.Format(time.RFC3339), p.Window)
	fmt.Fprintf(&b, "- 配置模式：`%s`\n", p.Mode)
	if p.ApplyEnabled {
		fmt.Fprintf(&b, "- 是否会写线上：`会`，已启用 apply\n\n")
	} else {
		fmt.Fprintf(&b, "- 是否会写线上：`不会`，当前只是 dry-run 预览\n\n")
	}

	fmt.Fprintf(&b, "## 即将做什么\n\n")
	if len(p.Decisions) == 0 {
		fmt.Fprintf(&b, "没有建议动作。当前配置和最近窗口内的 Bifrost 状态不需要调度器调整。\n\n")
	} else {
		for i, d := range p.Decisions {
			fmt.Fprintf(&b, "%d. %s\n", i+1, d.HumanSummary())
			fmt.Fprintf(&b, "   - 范围：pool `%s` / VK `%s` / provider `%s`\n", d.PoolID, d.VirtualKey, d.Provider)
			fmt.Fprintf(&b, "   - 权重：当前 `%.4f` -> 目标 `%.4f`\n", d.CurrentWeight, d.TargetWeight)
			fmt.Fprintf(&b, "   - 最近窗口：请求 `%d`，成功 `%d`，失败 `%d`，错误率 `%s`，P95 `%s ms`\n",
				d.Inputs.Total, d.Inputs.Success, d.Inputs.Errors, percent(d.Inputs.ErrorRate), formatFloat(d.Inputs.P95LatencyMS))
			if d.Inputs.WindowCount > 0 {
				fmt.Fprintf(&b, "   - 防误判证据：连续坏窗口 `%d` / 总坏窗口 `%d`；连续慢窗口 `%d` / 总慢窗口 `%d`；统计子窗口 `%d`\n",
					d.Inputs.ConsecutiveBadWindows, d.Inputs.BadWindows, d.Inputs.ConsecutiveSlowWindows, d.Inputs.SlowWindows, d.Inputs.WindowCount)
			}
			if len(d.Inputs.ErrorFamilies) > 0 {
				fmt.Fprintf(&b, "   - 错误类型：`%s`\n", strings.Join(d.Inputs.ErrorFamilies, "`, `"))
			}
			fmt.Fprintf(&b, "   - 原因：%s\n", humanReason(d))
			if d.Apply != nil {
				fmt.Fprintf(&b, "   - 执行结果：%s\n", humanApply(*d.Apply))
			} else if d.DryRun {
				fmt.Fprintf(&b, "   - 执行结果：未执行，只预览\n")
			} else {
				fmt.Fprintf(&b, "   - 执行结果：等待执行\n")
			}
			fmt.Fprintf(&b, "\n")
		}
	}

	fmt.Fprintf(&b, "## 受管池\n\n")
	fmt.Fprintf(&b, "| Pool | Virtual Key | 类型 |\n")
	fmt.Fprintf(&b, "| --- | --- | --- |\n")
	for _, pool := range p.Pools {
		fmt.Fprintf(&b, "| `%s` | `%s` | `%s` |\n", pool.ID, pool.VirtualKey, pool.Kind)
	}

	fmt.Fprintf(&b, "\n## 当前权重\n\n")
	fmt.Fprintf(&b, "| Pool | Provider | 当前权重 | 在 Bifrost 中 | Key 策略 |\n")
	fmt.Fprintf(&b, "| --- | --- | ---: | --- | --- |\n")
	for _, state := range p.CurrentStates {
		keyPolicy := "指定 key"
		if state.AllowAllKeys {
			keyPolicy = "全部 key"
		}
		fmt.Fprintf(&b, "| `%s` | `%s` | %.4f | `%t` | %s |\n",
			state.PoolID, state.Provider, state.CurrentWeight, state.CurrentInBifrost, keyPolicy)
	}

	fmt.Fprintf(&b, "\n## 最近指标\n\n")
	fmt.Fprintf(&b, "| Pool | Provider | 请求 | 成功 | 失败 | 错误率 | P95 ms | Timeout/Idle | 关键错误 | 连续坏窗口 | 连续慢窗口 |\n")
	fmt.Fprintf(&b, "| --- | --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |\n")
	for _, metric := range p.Metrics {
		fmt.Fprintf(&b, "| `%s` | `%s` | %d | %d | %d | %s | %s | %d | %d | %d | %d |\n",
			metric.PoolID, metric.Provider, metric.Total, metric.Success, metric.Errors, percent(metric.ErrorRate),
			formatFloat(metric.P95LatencyMS), metric.TimeoutOrStreamIdle, metric.CriticalErrors, metric.ConsecutiveBadWindows, metric.ConsecutiveSlowWindows)
	}
	return b.String()
}

func (d Decision) HumanSummary() string {
	switch d.Action {
	case "set_weight_zero":
		return fmt.Sprintf("把 `%s` 的权重清零", d.Provider)
	case "set_weight":
		if d.TargetWeight > d.CurrentWeight {
			return fmt.Sprintf("把 `%s` 的权重提高到 %.4f", d.Provider, d.TargetWeight)
		}
		return fmt.Sprintf("把 `%s` 的权重降到 %.4f", d.Provider, d.TargetWeight)
	case "disable_provider":
		return fmt.Sprintf("禁用 `%s`：权重清零，并禁用绑定 key", d.Provider)
	case "disable_provider_keys":
		return fmt.Sprintf("继续禁用 `%s` 的绑定 key", d.Provider)
	case "review_missing_provider":
		return fmt.Sprintf("人工检查 `%s`：配置里有，但 Bifrost VK 中缺失", d.Provider)
	default:
		return fmt.Sprintf("对 `%s` 执行动作 `%s`", d.Provider, d.Action)
	}
}

func humanReason(d Decision) string {
	switch d.Action {
	case "set_weight_zero":
		if strings.Contains(d.Reason, "not allowed") {
			return "配置中该 provider 不允许进入这个池，或被标记为 quarantine。"
		}
		if strings.Contains(d.Reason, "missing from scheduler config") {
			return "Bifrost 里存在该 provider，但调度器配置里没有它。"
		}
	case "set_weight":
		if strings.Contains(d.Reason, "healthy") {
			return "最近窗口成功率达标，恢复到配置里的目标权重。"
		}
		if strings.Contains(d.Reason, "too little recent traffic") {
			return "最近流量太少，给最小探测权重。"
		}
		if strings.Contains(d.Reason, "not disabling until") || strings.Contains(d.Reason, "consecutive bad windows") {
			return "已经看到明显异常，但还没达到连续坏窗口要求；先降到最小探测权重，避免单窗口误判。"
		}
		if strings.Contains(d.Reason, "error rate") {
			return "错误率超过阈值，降低权重。"
		}
		if strings.Contains(d.Reason, "p95 latency") {
			return "P95 延迟超过阈值，降低权重。"
		}
	case "disable_provider":
		return "发现凭证、额度或无可用 token 等关键错误。"
	case "disable_provider_keys":
		return "该 provider 权重已经为 0，但仍有关键错误证据；继续尝试禁用绑定 key。"
	case "review_missing_provider":
		return "需要人工在 Bifrost 中补齐 provider config。"
	}
	if d.Reason == "" {
		return "无额外原因。"
	}
	return d.Reason
}

func humanApply(result ApplyResult) string {
	if result.Applied {
		return "已执行：" + result.Message
	}
	return "未执行：" + result.Message
}

func formatFloat(value *float64) string {
	if value == nil {
		return "-"
	}
	return fmt.Sprintf("%.0f", *value)
}

func percent(value float64) string {
	return fmt.Sprintf("%.2f%%", value*100)
}
