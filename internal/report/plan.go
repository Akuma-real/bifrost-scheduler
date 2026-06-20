// package report 表示这个文件属于“报告输出层”。
//
// 它只负责把 domain.Plan 变成 JSON 或 Markdown 文本。
// 它不读取 Bifrost，不判断 provider 好坏，也不执行写入。
package report

// import 表示这个文件要使用哪些包。
//
// encoding/json：把 Go 结构体编码成 JSON。
// fmt：格式化字符串。
// io：接收任何可以写入的目标，例如 stdout、文件。
// strings：拼接 Markdown 和处理字符串。
// time：格式化时间。
// domain：调度器领域层的数据结构。
import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	domain "github.com/Akuma-real/bifrost-scheduler/internal/domain/scheduler"
)

// WritePlan 根据 format 把计划写到 w。
//
// w 是 io.Writer，意思是“任何能写入字节的东西”。
// 它可以是 os.Stdout、日志文件，也可以是测试里的 buffer。
func WritePlan(w io.Writer, plan domain.Plan, format string) error {
	// strings.ToLower 把格式转成小写，JSON/json/Json 都能识别。
	switch strings.ToLower(format) {
	case "json":
		// json.NewEncoder 创建一个 JSON 编码器。
		enc := json.NewEncoder(w)
		// SetIndent 让 JSON 输出更好读。
		enc.SetIndent("", "  ")
		return enc.Encode(plan)
	case "markdown", "md", "":
		// fmt.Fprint 把 Markdown 字符串写到 w。
		_, err := fmt.Fprint(w, Markdown(plan))
		return err
	default:
		return fmt.Errorf("unsupported output format %q", format)
	}
}

// Markdown 把 Plan 渲染成中文 Markdown 报告。
//
// strings.Builder 是高效拼接字符串的工具。
// 你可以把它理解成“不断往里面写文本，最后一次性变成 string”。
func Markdown(p domain.Plan) string {
	// var b strings.Builder 声明一个空的字符串构造器。
	var b strings.Builder

	// fmt.Fprintf(&b, ...) 表示把格式化后的文本写进 b。
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
	// 没有决策时，报告直接说明当前无需调整。
	if len(p.Decisions) == 0 {
		fmt.Fprintf(&b, "没有建议动作。当前配置和最近窗口内的 Bifrost 状态不需要调度器调整。\n\n")
	} else {
		// i 是下标，从 0 开始；d 是当前 decision。
		for i, d := range p.Decisions {
			fmt.Fprintf(&b, "%d. %s\n", i+1, HumanSummary(d))
			fmt.Fprintf(&b, "   - 范围：pool `%s` / VK `%s` / provider `%s`\n", d.PoolID, d.VirtualKey, d.Provider)
			fmt.Fprintf(&b, "   - 权重：当前 `%.4f` -> 目标 `%.4f`\n", d.CurrentWeight, d.TargetWeight)
			fmt.Fprintf(&b, "   - 最近窗口：请求 `%d`，成功 `%d`，失败 `%d`，错误率 `%s`，P95 `%s ms`\n",
				d.Inputs.Total, d.Inputs.Success, d.Inputs.Errors, percent(d.Inputs.ErrorRate), formatFloat(d.Inputs.P95LatencyMS))
			if d.Inputs.WindowCount > 0 {
				fmt.Fprintf(&b, "   - 防误判证据：连续坏窗口 `%d` / 总坏窗口 `%d`；连续慢窗口 `%d` / 总慢窗口 `%d`；统计子窗口 `%d`\n",
					d.Inputs.ConsecutiveBadWindows, d.Inputs.BadWindows, d.Inputs.ConsecutiveSlowWindows, d.Inputs.SlowWindows, d.Inputs.WindowCount)
			}
			// 有错误类型才展示，避免空列表占位置。
			if len(d.Inputs.ErrorFamilies) > 0 {
				fmt.Fprintf(&b, "   - 错误类型：`%s`\n", strings.Join(d.Inputs.ErrorFamilies, "`, `"))
			}
			// ignored errors 是用户侧错误，不应该拿来惩罚 provider，但报告里要透明展示。
			if d.Inputs.IgnoredErrors > 0 {
				fmt.Fprintf(&b, "   - 已忽略用户侧错误：`%d`", d.Inputs.IgnoredErrors)
				if len(d.Inputs.IgnoredErrorFamilies) > 0 {
					fmt.Fprintf(&b, "，类型：`%s`", strings.Join(d.Inputs.IgnoredErrorFamilies, "`, `"))
				}
				fmt.Fprintf(&b, "\n")
			}
			fmt.Fprintf(&b, "   - 原因：%s\n", humanReason(d))
			// Apply 不为 nil 表示已经尝试执行过，报告要写执行结果。
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
		// 默认显示“指定 key”；如果 AllowAllKeys 为 true，就显示“全部 key”。
		keyPolicy := "指定 key"
		if state.AllowAllKeys {
			keyPolicy = "全部 key"
		}
		fmt.Fprintf(&b, "| `%s` | `%s` | %.4f | `%t` | %s |\n",
			state.PoolID, state.Provider, state.CurrentWeight, state.CurrentInBifrost, keyPolicy)
	}

	fmt.Fprintf(&b, "\n## 最近指标\n\n")
	fmt.Fprintf(&b, "| Pool | Provider | 有效请求 | 成功 | 失败 | 忽略错误 | 错误率 | P95 ms | Timeout/Idle | 关键错误 | 连续坏窗口 | 连续慢窗口 |\n")
	fmt.Fprintf(&b, "| --- | --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |\n")
	for _, metric := range p.Metrics {
		fmt.Fprintf(&b, "| `%s` | `%s` | %d | %d | %d | %d | %s | %s | %d | %d | %d | %d |\n",
			metric.PoolID, metric.Provider, metric.Total, metric.Success, metric.Errors, metric.IgnoredErrors, percent(metric.ErrorRate),
			formatFloat(metric.P95LatencyMS), metric.TimeoutOrStreamIdle, metric.CriticalErrors, metric.ConsecutiveBadWindows, metric.ConsecutiveSlowWindows)
	}
	// Builder.String() 把全部拼好的文本变成 string 返回。
	return b.String()
}

// HumanSummary 把机器动作翻译成人能看懂的一句话。
//
// 例如 action=set_weight_zero 会变成“把 xxx 的权重清零”。
func HumanSummary(d domain.Decision) string {
	switch d.Action {
	case "set_weight_zero":
		return fmt.Sprintf("把 `%s` 的权重清零", d.Provider)
	case "set_weight":
		if domain.WeightsEqual(d.TargetWeight, d.CurrentWeight) {
			return fmt.Sprintf("保持 `%s` 的权重为 %.4f", d.Provider, d.TargetWeight)
		}
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

// humanReason 把内部英文原因转换成更适合报告阅读的中文原因。
//
// 内部原因保留英文是为了代码和测试更稳定。
// 报告输出给人看，所以这里做一层中文解释。
func humanReason(d domain.Decision) string {
	switch d.Action {
	case "set_weight_zero":
		// strings.Contains 判断原因文本里是否包含某段关键词。
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

// humanApply 把执行结果转换成中文。
func humanApply(result domain.ApplyResult) string {
	if result.Applied {
		return "已执行：" + result.Message
	}
	return "未执行：" + result.Message
}

// formatFloat 把 *float64 格式化成报告里的数字。
//
// *float64 可以为 nil，nil 表示没有这个值，例如没有成功样本就没有 P95。
func formatFloat(value *float64) string {
	if value == nil {
		return "-"
	}
	return fmt.Sprintf("%.0f", *value)
}

// percent 把 0.1234 这种比例显示成 12.34%。
func percent(value float64) string {
	return fmt.Sprintf("%.2f%%", value*100)
}
