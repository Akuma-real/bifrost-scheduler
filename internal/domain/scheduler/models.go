// package scheduler 表示这个文件属于“领域层调度器”这个包。
//
// 这个文件主要定义数据结构。数据结构可以先理解成“程序内部使用的表格格式”。
package scheduler

// import 表示这个文件需要使用别的包。
// time 是 Go 标准库，用来表示时间。
import "time"

// PoolProviderState 表示某个 provider 在某个 VK 里的“当前 Bifrost 状态”。
//
// 你可以把它读成“Bifrost 现在是什么样”：
//   - 当前权重是多少。
//   - provider config 是否存在。
//   - VK 使用全部 key 还是指定 key。
//   - 绑定了哪些 provider key。
type PoolProviderState struct {
	// PoolID 是调度器配置里的池子 ID，例如 gpt_low。
	PoolID string `json:"pool_id"`
	// VirtualKey 是 Bifrost 里的 VK 名称。
	VirtualKey string `json:"virtual_key"`
	// VirtualKeyID 是 Bifrost 返回的 VK 内部 ID，写回 API 时需要它。
	VirtualKeyID string `json:"virtual_key_id"`
	// ProviderConfigID 是 Bifrost 里这个 provider config 的内部 ID。
	ProviderConfigID int `json:"provider_config_id"`
	// Provider 是 provider 名称，例如某个 openai_lv1。
	Provider string `json:"provider"`
	// CurrentWeight 是 Bifrost 当前权重。
	CurrentWeight float64 `json:"current_weight"`
	// AllowedModels 是这个 provider config 允许的模型列表。
	AllowedModels []string `json:"allowed_models,omitempty"`
	// AllowAllKeys 表示 VK 是否使用这个 provider 下全部 key。
	AllowAllKeys bool `json:"allow_all_keys"`
	// ProviderKeyIDs 是当前 VK 绑定的 provider key id 列表。
	ProviderKeyIDs []string `json:"provider_key_ids,omitempty"`
	// EnabledKeyCount 是已启用 key 数量，用于报告展示。
	EnabledKeyCount int `json:"enabled_key_count"`
	// LastObservedAt 预留字段，用来记录最后一次观察到状态的时间。
	LastObservedAt *time.Time `json:"last_observed_at,omitempty"`
	// CurrentInBifrost 表示这个 provider config 当前是否真的存在于 Bifrost。
	CurrentInBifrost bool `json:"current_in_bifrost"`
}

// ProviderMetric 表示某个 provider 最近一段时间的调用证据。
//
// 它由 Bifrost 调用日志统计出来。
// 调度规则会根据这些数字判断 provider 是健康、异常、太慢，还是样本太少不能判断。
type ProviderMetric struct {
	// PoolID / VirtualKey / Provider 用来定位这条指标属于哪个池子和 provider。
	PoolID     string `json:"pool_id"`
	VirtualKey string `json:"virtual_key"`
	Provider   string `json:"provider"`
	// Total 是有效请求数。用户侧错误会被忽略，不计入 Total。
	Total int `json:"total"`
	// Success 是成功请求数。
	Success int `json:"success"`
	// Errors 是 provider 侧有效失败数。
	Errors int `json:"errors"`
	// ErrorRate 是 Errors / Total。
	ErrorRate float64 `json:"error_rate"`
	// SuccessRate 是 Success / Total。
	SuccessRate float64 `json:"success_rate"`
	// P95LatencyMS 是成功请求延迟的 P95；nil 表示没有足够样本。
	P95LatencyMS *float64 `json:"p95_latency_ms,omitempty"`
	// ProbeTotal 是主动测速次数。
	ProbeTotal int `json:"probe_total,omitempty"`
	// ProbeSuccess 是主动测速成功次数。
	ProbeSuccess int `json:"probe_success,omitempty"`
	// ProbeErrors 是主动测速失败次数。
	ProbeErrors int `json:"probe_errors,omitempty"`
	// ProbeErrorRate 是主动测速失败率。
	ProbeErrorRate float64 `json:"probe_error_rate,omitempty"`
	// P95TTFTMS 是主动流式测速得到的首字 P95。
	// 它不是 Bifrost 日志里的 latency，也不能用总耗时代替。
	P95TTFTMS *float64 `json:"p95_ttft_ms,omitempty"`
	// P95ProbeLatencyMS 是主动测速的完整响应 P95，用来辅助判断。
	P95ProbeLatencyMS *float64 `json:"p95_probe_latency_ms,omitempty"`
	// ProbeErrorFamilies 是主动测速失败类型。
	ProbeErrorFamilies []string `json:"probe_error_families,omitempty"`
	// TimeoutOrStreamIdle 是 timeout 或 stream idle 类型错误数量。
	TimeoutOrStreamIdle int `json:"timeout_or_stream_idle"`
	// CriticalErrors 是额度、凭证、无 token 等关键错误数量。
	CriticalErrors int `json:"critical_errors"`
	// IgnoredErrors 是被判定为用户侧问题、不会惩罚 provider 的错误数量。
	IgnoredErrors int `json:"ignored_errors,omitempty"`
	// IgnoredErrorFamilies 是被忽略错误的类型列表。
	IgnoredErrorFamilies []string `json:"ignored_error_families,omitempty"`
	// LastSeenAt 是最近一次看到这个 provider 日志的时间。
	LastSeenAt *time.Time `json:"last_seen_at,omitempty"`
	// ErrorFamilies 是有效 provider 错误类型列表。
	ErrorFamilies []string `json:"error_families,omitempty"`
	// Windows 是小窗口指标。json:"-" 表示不输出到 JSON，避免报告过大。
	Windows []WindowMetric `json:"-"`
	// BadWindows 是坏窗口总数。
	BadWindows int `json:"bad_windows,omitempty"`
	// ConsecutiveBadWindows 是最大连续坏窗口数。
	ConsecutiveBadWindows int `json:"consecutive_bad_windows,omitempty"`
	// SlowWindows 是慢窗口总数。
	SlowWindows int `json:"slow_windows,omitempty"`
	// ConsecutiveSlowWindows 是最大连续慢窗口数。
	ConsecutiveSlowWindows int `json:"consecutive_slow_windows,omitempty"`
}

// Decision 表示一个建议动作。
//
// 注意：Decision 只是数据，本身不会修改 Bifrost。
// 应用层会根据配置判断是否允许把这个 Decision 真正执行。
type Decision struct {
	// PoolID / VirtualKey / Provider 定位这个动作作用到哪里。
	PoolID     string `json:"pool_id"`
	VirtualKey string `json:"virtual_key"`
	Provider   string `json:"provider"`
	// Action 是机器动作名，例如 set_weight、set_weight_zero。
	Action string `json:"action"`
	// CurrentWeight 是当前权重。
	CurrentWeight float64 `json:"current_weight"`
	// TargetWeight 是建议改到的目标权重。
	TargetWeight float64 `json:"target_weight"`
	// Reason 是内部原因，报告层会把常见原因翻译成中文。
	Reason string `json:"reason"`
	// Severity 是严重级别：critical、warning、info。
	Severity string `json:"severity"`
	// DryRun 表示这条决策是否只是预览。
	DryRun bool `json:"dry_run"`
	// Inputs 是做出这个决策时用到的证据。
	Inputs DecisionInputs `json:"inputs"`
	// Apply 是执行结果；nil 表示还没有执行或不需要执行。
	Apply *ApplyResult `json:"apply,omitempty"`
}

// DecisionInputs 记录产生某个 Decision 时用到的证据。
//
// 有了它，报告不只是说“我要改权重”，还可以解释“为什么要改”。
type DecisionInputs struct {
	// 下面这些字段来自 ProviderMetric，是决策证据的快照。
	Total                int      `json:"total"`
	Success              int      `json:"success"`
	Errors               int      `json:"errors"`
	ErrorRate            float64  `json:"error_rate"`
	SuccessRate          float64  `json:"success_rate"`
	P95LatencyMS         *float64 `json:"p95_latency_ms,omitempty"`
	ProbeTotal           int      `json:"probe_total,omitempty"`
	ProbeSuccess         int      `json:"probe_success,omitempty"`
	ProbeErrors          int      `json:"probe_errors,omitempty"`
	ProbeErrorRate       float64  `json:"probe_error_rate,omitempty"`
	P95TTFTMS            *float64 `json:"p95_ttft_ms,omitempty"`
	P95ProbeLatencyMS    *float64 `json:"p95_probe_latency_ms,omitempty"`
	TimeoutOrStreamIdle  int      `json:"timeout_or_stream_idle"`
	CriticalErrors       int      `json:"critical_errors"`
	IgnoredErrors        int      `json:"ignored_errors,omitempty"`
	IgnoredErrorFamilies []string `json:"ignored_error_families,omitempty"`
	ErrorFamilies        []string `json:"error_families,omitempty"`
	ProbeErrorFamilies   []string `json:"probe_error_families,omitempty"`
	// 下面这些字段解释防误判窗口证据。
	BadWindows             int `json:"bad_windows,omitempty"`
	ConsecutiveBadWindows  int `json:"consecutive_bad_windows,omitempty"`
	SlowWindows            int `json:"slow_windows,omitempty"`
	ConsecutiveSlowWindows int `json:"consecutive_slow_windows,omitempty"`
	WindowCount            int `json:"window_count,omitempty"`
}

// ProbeMetric 表示一次主动测速聚合后的 provider 指标。
//
// 它和 Bifrost 日志指标分开，是为了不混淆：
// 日志里的 latency 是完整请求耗时；主动测速的 TTFT 才是首字时间。
type ProbeMetric struct {
	PoolID            string   `json:"pool_id"`
	VirtualKey        string   `json:"virtual_key"`
	Provider          string   `json:"provider"`
	Total             int      `json:"total"`
	Success           int      `json:"success"`
	Errors            int      `json:"errors"`
	ErrorRate         float64  `json:"error_rate"`
	P95TTFTMS         *float64 `json:"p95_ttft_ms,omitempty"`
	P95LatencyMS      *float64 `json:"p95_latency_ms,omitempty"`
	ErrorFamilies     []string `json:"error_families,omitempty"`
	SampleDescription string   `json:"sample_description,omitempty"`
}

// WindowMetric 表示总统计窗口里的一个小时间桶。
//
// 例子：45 分钟总窗口可以拆成 3 个 15 分钟小窗口。
// 连续坏窗口可以避免程序因为一次短暂异常就过度反应。
type WindowMetric struct {
	// Start 和 End 是这个小窗口的时间范围。
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
	// Total / Success / Errors 是这个小窗口内的有效请求统计。
	Total   int `json:"total"`
	Success int `json:"success"`
	Errors  int `json:"errors"`
	// ErrorRate / SuccessRate 是比例值，不是百分数字符串。
	ErrorRate   float64 `json:"error_rate"`
	SuccessRate float64 `json:"success_rate"`
	// P95LatencyMS 是这个小窗口内成功请求的 P95。
	P95LatencyMS *float64 `json:"p95_latency_ms,omitempty"`
	// TimeoutOrStreamIdle 是 timeout 或 stream idle 错误数量。
	TimeoutOrStreamIdle int `json:"timeout_or_stream_idle"`
	// CriticalErrors 是关键错误数量。
	CriticalErrors int `json:"critical_errors"`
	// IgnoredErrors 是这个窗口里被忽略的用户侧错误数量。
	IgnoredErrors int `json:"ignored_errors,omitempty"`
	// Bad / Slow 是 AnnotateWindows 之后打上的标记。
	Bad  bool `json:"bad"`
	Slow bool `json:"slow"`
}

// ApplyResult 记录执行某个 Decision 后发生了什么。
//
// 如果是 dry-run，只预览不执行，Decision.Apply 会保持 nil。
type ApplyResult struct {
	// Applied 为 true 表示已经执行成功。
	Applied bool `json:"applied"`
	// Skipped 为 true 表示没有执行，因为不需要或不可自动执行。
	Skipped bool `json:"skipped,omitempty"`
	// Message 是执行结果说明。
	Message string `json:"message"`
}

// Plan 表示一次调度运行的完整输出。
//
// JSON 输出就是把这个结构体转成 JSON。
// Markdown 输出则是把同一份数据渲染成人能读的中文报告。
type Plan struct {
	// GeneratedAt 是本次计划生成时间。
	GeneratedAt time.Time `json:"generated_at"`
	// WindowStart 是统计窗口开始时间。
	WindowStart time.Time `json:"window_start"`
	// Window 是统计窗口总长度的字符串表示。
	Window string `json:"window"`
	// Mode 是配置模式，例如 read_only 或 guarded_write。
	Mode string `json:"mode"`
	// ApplyEnabled 表示这次运行是否真的允许写线上。
	ApplyEnabled bool `json:"apply_enabled"`
	// Pools 是受管池列表。
	Pools []PoolSnapshot `json:"pools"`
	// Metrics 是最近调用指标。
	Metrics []ProviderMetric `json:"metrics"`
	// CurrentStates 是 Bifrost 当前状态。
	CurrentStates []PoolProviderState `json:"current_states"`
	// Decisions 是调度器建议动作。
	Decisions []Decision `json:"decisions"`
}

// PoolSnapshot 是报告里展示的 pool 简短信息。
type PoolSnapshot struct {
	// ID 是调度器内部 pool id。
	ID string `json:"id"`
	// VirtualKey 是 Bifrost VK 名称。
	VirtualKey string `json:"virtual_key"`
	// Kind 是池子类型，例如 text。
	Kind string `json:"kind"`
}
