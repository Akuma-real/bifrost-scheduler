// package scheduler 表示这个文件属于“领域层调度器”包。
//
// 领域层负责描述调度器自己的业务概念：
// 配置长什么样、默认值是什么、provider 是否允许进入池子。
package scheduler

// import 表示这个文件要使用哪些外部代码。
//
// fmt：生成错误信息。
// strings：做字符串比较，比如不区分大小写判断 quarantine。
// time：把 "15m"、"30m" 这类字符串解析成时间长度。
import (
	"fmt"
	"strings"
	"time"
)

// APIConfig 表示配置文件里的 api 部分。
//
// BaseURL 是 Bifrost 地址，例如 https://bifrost.ggapi.cc。
// Paths 是 Bifrost API 路径；正常使用不需要在 JSON 里写，程序会写死默认值。
type APIConfig struct {
	// `json:"base_url"` 叫 struct tag。
	// 它告诉 encoding/json：JSON 里的 base_url 字段对应 Go 里的 BaseURL 字段。
	BaseURL string `json:"base_url"`

	// json:"-" 表示这个字段不从 config.json 读取。
	// 这些路径是针对 Bifrost 写死的默认 API 路径，避免用户配置文件太复杂。
	Paths APIPaths `json:"-"`
}

// APIPaths 保存调度器要调用的 Bifrost API 路径。
//
// 这些路径单独放成结构体，是为了以后 Bifrost API 路径变化时，只改一个地方。
type APIPaths struct {
	VirtualKeys string `json:"virtual_keys"`
	Logs        string `json:"logs"`
	Login       string `json:"login"`
	ProviderKey string `json:"provider_key"`
}

// Config 是 config.json 直接解析出来的结构。
//
// 注意：它还不是最终运行配置。
// JSON 里没有写的字段会是 Go 的零值，例如 ""、0、nil。
// NormalizeConfig 会把这些零值补成默认值。
type Config struct {
	// Mode 控制是否允许写线上：
	//   - read_only：只预览。
	//   - guarded_write：允许在 --apply 时执行受保护的写入。
	Mode            string       `json:"mode"`
	API             APIConfig    `json:"api"`
	Window          string       `json:"window"`
	QualityWindows  int          `json:"quality_windows"`
	MinimumAttempts int          `json:"minimum_attempts"`
	Cooldown        string       `json:"cooldown"`
	Probe           ProbeConfig  `json:"probe"`
	Pools           []PoolConfig `json:"pools"`
}

// ProbeConfig 表示主动测速配置。
//
// Bifrost 当前日志没有真实首字字段，所以要想按首字调度，
// 就需要调度器自己发很小的流式请求，记录“发出请求 -> 收到第一个 token”的时间。
type ProbeConfig struct {
	// Enabled 为 true 时才会发主动测速请求。
	Enabled bool `json:"enabled"`
	// Model 是默认测速模型，例如 gpt-5.5。
	// 真正请求时会变成 provider/model，例如 zz1cc_openai_lv4/gpt-5.5。
	Model string `json:"model"`
	// Prompt 是测速用提示词，应该非常短，减少费用和 token 噪音。
	Prompt string `json:"prompt"`
	// Samples 是每个 provider 每轮测速次数。
	Samples int `json:"samples"`
	// Timeout 是单次测速最长等待时间，例如 20s。
	Timeout string `json:"timeout"`
	// TimeoutDuration 是 Timeout 解析后的 time.Duration。
	// json:"-" 表示它不从 JSON 读取，也不输出到 JSON。
	TimeoutDuration time.Duration `json:"-"`
}

// PoolConfig 表示一个受管池子。
//
// 这里的 pool 对应 Bifrost 里的一个 Virtual Key。
// 例如 gpt_low、gpt_stable 都可以是一个 pool。
type PoolConfig struct {
	// ID 是调度器内部名字，用来在报告里区分池子。
	ID string `json:"id"`
	// VirtualKey 是 Bifrost 里的 VK 名称。
	VirtualKey string `json:"virtual_key"`
	// Kind 目前主要用于报告展示，例如 text、image。
	Kind string `json:"kind"`
	// MinActiveProviders 是安全底线：至少保留几个有权重的 provider。
	// 它防止调度器把整个池子全部清零。
	MinActiveProviders int `json:"min_active_providers"`
	// Rules 是这个池子的判断阈值。nil 表示使用默认规则。
	Rules *PoolRules `json:"rules"`
	// Providers 是这个池子里允许调度器管理的 provider 列表。
	Providers []ProviderConfig `json:"providers"`
}

// PoolRules 表示一个池子的调度规则阈值。
//
// 初学 Go 时可以把它看成“规则参数表”。
// 大部分字段可以不写，NormalizeConfig 会用 DefaultPoolRules 补默认值。
type PoolRules struct {
	// MaxErrorRate 是开始降权的错误率阈值，例如 0.5 表示 50%。
	MaxErrorRate float64 `json:"max_error_rate"`
	// DisableErrorRate 是更严重的错误率阈值，达到后可能清零或禁用。
	DisableErrorRate float64 `json:"disable_error_rate"`
	// MaxTimeoutOrIdle 是 timeout / stream idle 的数量阈值。
	MaxTimeoutOrIdle int `json:"max_timeout_or_idle"`
	// MaxP95LatencyMS 是 P95 延迟阈值；0 表示不因为延迟降权。
	MaxP95LatencyMS float64 `json:"max_p95_latency_ms"`
	// MaxP95TTFTMS 是主动测速得到的首字 P95 阈值；0 表示不按绝对首字阈值降权。
	MaxP95TTFTMS float64 `json:"max_p95_ttft_ms"`
	// MinSuccessRateForRecovery 是恢复权重时要求的最低成功率。
	MinSuccessRateForRecovery float64 `json:"min_success_rate_for_recovery"`
	// MinErrors 是触发错误率判断前至少要有多少失败样本。
	MinErrors int `json:"min_errors"`
	// CriticalErrorThreshold 是关键错误数量阈值，例如额度、凭证类错误。
	CriticalErrorThreshold int `json:"critical_error_threshold"`
	// MinLatencySamples 是判断 P95 延迟前至少要有多少成功样本。
	MinLatencySamples int `json:"min_latency_samples"`
	// MinProbeSamples 是主动测速至少成功多少次，才允许用首字结果调权。
	MinProbeSamples int `json:"min_probe_samples"`
	// RequiredBadWindows 是防误判用的连续坏窗口数量。
	RequiredBadWindows int `json:"required_bad_windows"`
	// TTFTPriority 是首字速度在“首字 + 成本”综合目标权重里的占比。
	// 0.75 表示首字速度占 75%，成本权重占 25%。
	TTFTPriority float64 `json:"ttft_priority"`
	// MinWeightChange 是最小权重变动幅度，小于它就不写回，避免权重频繁抖动。
	MinWeightChange float64 `json:"min_weight_change"`
	// MaxWeightStep 是单轮健康调权最大步长。
	// 主动测速只有少量样本时，不能每 5 分钟把健康 provider 大幅拉上拉下。
	MaxWeightStep float64 `json:"max_weight_step"`
	// DefaultCostWeight 是 provider 没写 cost_weight 时的默认目标权重。
	DefaultCostWeight float64 `json:"default_cost_weight"`
	// MinWeight 是探测权重默认值。
	// 所以 provider 里通常不用单独写 min_weight。
	MinWeight float64 `json:"min_weight"`
}

// ProviderConfig 表示一个 provider 在某个池子里的配置。
//
// 这里不存 API key，不碰数据库，只存调度器需要知道的策略字段。
type ProviderConfig struct {
	// Name 必须和 Bifrost 里的 provider 名称一致。
	Name string `json:"name"`
	// Role 是角色。空字符串会默认成 fallback。
	// quarantine 表示隔离：调度器会认为它不允许进入这个池。
	Role string `json:"role"`
	// Allowed 是一个可选开关。
	// *bool 表示“指向 bool 的指针”，它可以区分三种情况：
	//   - nil：没写，默认允许。
	//   - true：明确允许。
	//   - false：明确不允许。
	Allowed *bool `json:"allowed,omitempty"`
	// CostWeight 是正常健康时的目标权重。
	// 通常由 price_rmb_per_dao 自动换算；价格没填全时才手写兜底。
	CostWeight float64 `json:"cost_weight"`
	// PriceRMBPerDao 是这个 provider 的价格，单位是 RMB/刀。
	// 如果同一个 pool 的可用 provider 都填写了价格，cost_weight 会自动按最低价换算。
	PriceRMBPerDao float64 `json:"price_rmb_per_dao,omitempty"`
	// MinWeight 是单个 provider 的探测权重覆盖值。
	// 不写就是 0，Normalize/DecideProvider 会改用 pool rules 里的默认 MinWeight。
	MinWeight float64 `json:"min_weight"`
	// ProbeModel 是单个 provider 的测速模型覆盖值。
	// 不写就用全局 probe.model。
	ProbeModel string `json:"probe_model,omitempty"`
}

// RuntimeConfig 是程序真正运行时使用的配置。
//
// 它内嵌 Config，所以可以直接访问 cfg.Pools、cfg.Mode 等字段。
// 额外的 WindowDuration/CooldownDuration 是已经解析好的 time.Duration。
type RuntimeConfig struct {
	Config
	WindowDuration   time.Duration
	CooldownDuration time.Duration
}

// NormalizeConfig 给配置补默认值，并做基础校验。
//
// 为什么需要它：
// config.json 应该尽量简单，用户只写自己关心的字段。
// 程序启动后统一在这里把默认值补齐，后面的业务代码就不用到处判断空值。
func NormalizeConfig(cfg Config) (RuntimeConfig, error) {
	// mode 不写时默认只读，避免程序默认写线上。
	if cfg.Mode == "" {
		cfg.Mode = "read_only"
	}
	// 下面这些 API path 是针对 Bifrost 现有 API 写死的默认值。
	// 用户配置文件不需要写它们。
	if cfg.API.Paths.VirtualKeys == "" {
		cfg.API.Paths.VirtualKeys = "/api/governance/virtual-keys"
	}
	if cfg.API.Paths.Logs == "" {
		cfg.API.Paths.Logs = "/api/logs"
	}
	if cfg.API.Paths.Login == "" {
		cfg.API.Paths.Login = "/api/session/login"
	}
	if cfg.API.Paths.ProviderKey == "" {
		cfg.API.Paths.ProviderKey = "/api/providers/{provider}/keys/{key_id}"
	}
	// window 是单个统计小窗口长度。
	if cfg.Window == "" {
		cfg.Window = "15m"
	}
	// cooldown 预留给冷却控制；当前配置仍然解析它，方便以后扩展。
	if cfg.Cooldown == "" {
		cfg.Cooldown = "30m"
	}
	// 主动测速默认关闭。只有用户明确写 probe.enabled=true 才会发送额外请求。
	if cfg.Probe.Enabled {
		if cfg.Probe.Model == "" {
			cfg.Probe.Model = "gpt-5.5"
		}
		if cfg.Probe.Prompt == "" {
			cfg.Probe.Prompt = "ping"
		}
		if cfg.Probe.Samples <= 0 {
			cfg.Probe.Samples = 3
		}
		// 主动测速会真的调用上游，为了避免误配置导致费用失控，单轮样本数做硬上限。
		if cfg.Probe.Samples > 5 {
			return RuntimeConfig{}, fmt.Errorf("probe.samples cannot be greater than 5")
		}
		if cfg.Probe.Timeout == "" {
			cfg.Probe.Timeout = "20s"
		}
		timeout, err := time.ParseDuration(cfg.Probe.Timeout)
		if err != nil {
			return RuntimeConfig{}, fmt.Errorf("parse probe.timeout: %w", err)
		}
		if timeout <= 0 {
			return RuntimeConfig{}, fmt.Errorf("probe.timeout must be positive")
		}
		cfg.Probe.TimeoutDuration = timeout
	}
	// QualityWindows 表示看几个连续小窗口。
	// 默认 3 个 15 分钟，就是总共看 45 分钟。
	if cfg.QualityWindows <= 0 {
		cfg.QualityWindows = 3
	}
	// MinimumAttempts 防止样本太少时误判。
	if cfg.MinimumAttempts <= 0 {
		cfg.MinimumAttempts = 10
	}
	// 没有 pool 就没东西可调度，直接返回错误。
	if len(cfg.Pools) == 0 {
		return RuntimeConfig{}, fmt.Errorf("at least one pool is required")
	}

	// time.ParseDuration 能解析 "15m"、"30s"、"1h" 这样的字符串。
	window, err := time.ParseDuration(cfg.Window)
	if err != nil {
		return RuntimeConfig{}, fmt.Errorf("parse window: %w", err)
	}
	cooldown, err := time.ParseDuration(cfg.Cooldown)
	if err != nil {
		return RuntimeConfig{}, fmt.Errorf("parse cooldown: %w", err)
	}
	if window <= 0 {
		return RuntimeConfig{}, fmt.Errorf("window must be positive")
	}
	if cooldown < 0 {
		return RuntimeConfig{}, fmt.Errorf("cooldown cannot be negative")
	}

	// map[string]bool 用来记录已经见过的 pool id，防止重复。
	seenPools := map[string]bool{}

	// for i := range cfg.Pools 用下标遍历。
	// 这样后面可以拿 &cfg.Pools[i]，直接修改切片里的原始元素。
	for i := range cfg.Pools {
		pool := &cfg.Pools[i]
		if pool.ID == "" {
			return RuntimeConfig{}, fmt.Errorf("pool id is required")
		}
		if seenPools[pool.ID] {
			return RuntimeConfig{}, fmt.Errorf("duplicate pool id %q", pool.ID)
		}
		seenPools[pool.ID] = true
		if pool.VirtualKey == "" {
			return RuntimeConfig{}, fmt.Errorf("pool %s virtual_key is required", pool.ID)
		}
		if pool.Kind == "" {
			pool.Kind = "text"
		}
		if pool.MinActiveProviders <= 0 {
			pool.MinActiveProviders = defaultMinActiveProviders()
		}
		// rules 是指针，所以 nil 表示用户完全没写 rules。
		if pool.Rules == nil {
			rules := DefaultPoolRules()

			// &rules 表示取 rules 变量的地址。
			// 赋给 pool.Rules 后，pool 就有了一份默认规则。
			pool.Rules = &rules
		} else {
			normalizePoolRules(pool.Rules)
		}
		if len(pool.Providers) == 0 {
			return RuntimeConfig{}, fmt.Errorf("pool %s must define providers", pool.ID)
		}
		// 每个 pool 内 provider 名称不能重复。
		seenProviders := map[string]bool{}
		for j := range pool.Providers {
			provider := &pool.Providers[j]
			if provider.Name == "" {
				return RuntimeConfig{}, fmt.Errorf("pool %s provider name is required", pool.ID)
			}
			if seenProviders[provider.Name] {
				return RuntimeConfig{}, fmt.Errorf("pool %s has duplicate provider %q", pool.ID, provider.Name)
			}
			seenProviders[provider.Name] = true
			if provider.Role == "" {
				provider.Role = "fallback"
			}
			// cost_weight 允许为 0。
			// 0 的意思是后续用 DefaultCostWeight，不是错误。
			if provider.CostWeight < 0 {
				return RuntimeConfig{}, fmt.Errorf("pool %s provider %s cost_weight cannot be negative", pool.ID, provider.Name)
			}
			if provider.PriceRMBPerDao < 0 {
				return RuntimeConfig{}, fmt.Errorf("pool %s provider %s price_rmb_per_dao cannot be negative", pool.ID, provider.Name)
			}
			// min_weight 允许为 0。
			// 0 的意思是使用 pool rules 里的默认 MinWeight。
			if provider.MinWeight < 0 {
				return RuntimeConfig{}, fmt.Errorf("pool %s provider %s min_weight cannot be negative", pool.ID, provider.Name)
			}
		}
		deriveCostWeightsFromPrices(pool)
	}

	// 返回 RuntimeConfig，把补完默认值的 cfg 和解析后的时间一起交给上层。
	return RuntimeConfig{
		Config:           cfg,
		WindowDuration:   window,
		CooldownDuration: cooldown,
	}, nil
}

// deriveCostWeightsFromPrices 用 RMB/刀 自动换算 cost_weight。
//
// 只有同一个 pool 的所有可用 provider 都写了价格时才换算。
// 这样不会因为只补了一个渠道价格，就把这个渠道误算成最低价权重 1。
func deriveCostWeightsFromPrices(pool *PoolConfig) {
	minPrice := 0.0
	eligible := 0
	priced := 0
	for _, provider := range pool.Providers {
		if !provider.AllowedInPool() {
			continue
		}
		eligible++
		if provider.PriceRMBPerDao <= 0 {
			continue
		}
		priced++
		if minPrice == 0 || provider.PriceRMBPerDao < minPrice {
			minPrice = provider.PriceRMBPerDao
		}
	}
	if eligible == 0 || priced != eligible || minPrice <= 0 {
		return
	}
	for i := range pool.Providers {
		provider := &pool.Providers[i]
		if !provider.AllowedInPool() || provider.PriceRMBPerDao <= 0 {
			continue
		}
		provider.CostWeight = ClampWeight(minPrice/provider.PriceRMBPerDao, 0, 1)
	}
}

// defaultMinActiveProviders 返回默认最小活跃 provider 数。
//
// 单独做成函数，是为了让默认值有一个清晰名字。
func defaultMinActiveProviders() int {
	return 1
}

// DefaultPoolRules 返回一整套默认规则。
//
// 这就是为什么最小配置可以不写 rules：
// 程序会自动使用这些默认值。
func DefaultPoolRules() PoolRules {
	return PoolRules{
		MaxErrorRate:              0.5,
		DisableErrorRate:          0.8,
		MaxTimeoutOrIdle:          10,
		MaxP95LatencyMS:           0,
		MaxP95TTFTMS:              0,
		MinSuccessRateForRecovery: 0.95,
		MinErrors:                 3,
		CriticalErrorThreshold:    2,
		MinLatencySamples:         5,
		MinProbeSamples:           3,
		RequiredBadWindows:        2,
		TTFTPriority:              0.75,
		MinWeightChange:           0.02,
		MaxWeightStep:             0.2,
		DefaultCostWeight:         1,
		MinWeight:                 0.05,
	}
}

// normalizePoolRules 给用户只写了一部分的 rules 补默认值。
//
// 例如用户只写了 max_p95_latency_ms，其他字段仍然自动使用 DefaultPoolRules。
func normalizePoolRules(rules *PoolRules) {
	// defaults 是完整默认规则。
	defaults := DefaultPoolRules()
	// 下面每个 if 都是“没写或写了无效非正值，就用默认值”。
	if rules.MaxErrorRate <= 0 {
		rules.MaxErrorRate = defaults.MaxErrorRate
	}
	if rules.DisableErrorRate <= 0 {
		rules.DisableErrorRate = defaults.DisableErrorRate
	}
	if rules.MaxTimeoutOrIdle <= 0 {
		rules.MaxTimeoutOrIdle = defaults.MaxTimeoutOrIdle
	}
	if rules.MinSuccessRateForRecovery <= 0 {
		rules.MinSuccessRateForRecovery = defaults.MinSuccessRateForRecovery
	}
	if rules.MinErrors <= 0 {
		rules.MinErrors = defaults.MinErrors
	}
	if rules.CriticalErrorThreshold <= 0 {
		rules.CriticalErrorThreshold = defaults.CriticalErrorThreshold
	}
	if rules.MinLatencySamples <= 0 {
		rules.MinLatencySamples = defaults.MinLatencySamples
	}
	if rules.MinProbeSamples <= 0 {
		rules.MinProbeSamples = defaults.MinProbeSamples
	}
	if rules.RequiredBadWindows <= 0 {
		rules.RequiredBadWindows = defaults.RequiredBadWindows
	}
	if rules.TTFTPriority <= 0 {
		rules.TTFTPriority = defaults.TTFTPriority
	}
	if rules.TTFTPriority > 1 {
		rules.TTFTPriority = 1
	}
	if rules.MinWeightChange <= 0 {
		rules.MinWeightChange = defaults.MinWeightChange
	}
	if rules.MaxWeightStep <= 0 {
		rules.MaxWeightStep = defaults.MaxWeightStep
	}
	if rules.DefaultCostWeight <= 0 {
		rules.DefaultCostWeight = defaults.DefaultCostWeight
	}
	if rules.MinWeight <= 0 {
		rules.MinWeight = defaults.MinWeight
	}
}

// EffectiveRules 返回一个 pool 最终应该使用的规则。
//
// 如果 pool.Rules 是 nil，就返回默认规则。
// 如果不是 nil，就返回用户配置和默认值合并后的规则。
func (p PoolConfig) EffectiveRules() PoolRules {
	if p.Rules == nil {
		return DefaultPoolRules()
	}
	return *p.Rules
}

// Provider 在运行时配置里查找指定 pool + provider 的配置。
//
// 返回两个值是 Go 里常见的查找写法：
//   - 第一个值是找到的 ProviderConfig。
//   - 第二个 bool 表示是否真的找到了。
func (c RuntimeConfig) Provider(poolID, providerName string) (ProviderConfig, bool) {
	for _, pool := range c.Pools {
		if pool.ID != poolID {
			// continue 表示跳过本轮循环，继续看下一个 pool。
			continue
		}
		for _, provider := range pool.Providers {
			if provider.Name == providerName {
				return provider, true
			}
		}
	}
	return ProviderConfig{}, false
}

// AllowedInPool 判断 provider 是否允许留在这个池子里。
//
// 返回 true 表示允许，false 表示调度器会尝试把它清零或保持隔离。
func (p ProviderConfig) AllowedInPool() bool {
	// EqualFold 是不区分大小写比较。
	// quarantine、Quarantine、QUARANTINE 都会被认为是隔离。
	if strings.EqualFold(p.Role, "quarantine") {
		return false
	}
	// Allowed 为 nil 表示用户没写 allowed，默认允许。
	if p.Allowed == nil {
		return true
	}
	// p.Allowed 是 *bool，所以要用 *p.Allowed 取出真正的 bool 值。
	return *p.Allowed
}
