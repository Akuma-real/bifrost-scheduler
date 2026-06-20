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
	Pools           []PoolConfig `json:"pools"`
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
	// MinSuccessRateForRecovery 是恢复权重时要求的最低成功率。
	MinSuccessRateForRecovery float64 `json:"min_success_rate_for_recovery"`
	// MinErrors 是触发错误率判断前至少要有多少失败样本。
	MinErrors int `json:"min_errors"`
	// CriticalErrorThreshold 是关键错误数量阈值，例如额度、凭证类错误。
	CriticalErrorThreshold int `json:"critical_error_threshold"`
	// MinLatencySamples 是判断 P95 延迟前至少要有多少成功样本。
	MinLatencySamples int `json:"min_latency_samples"`
	// RequiredBadWindows 是防误判用的连续坏窗口数量。
	RequiredBadWindows int `json:"required_bad_windows"`
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
	// 你把成本换算成相对权重后，主要就是填这个字段。
	CostWeight float64 `json:"cost_weight"`
	// MinWeight 是单个 provider 的探测权重覆盖值。
	// 不写就是 0，Normalize/DecideProvider 会改用 pool rules 里的默认 MinWeight。
	MinWeight float64 `json:"min_weight"`
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
			// min_weight 允许为 0。
			// 0 的意思是使用 pool rules 里的默认 MinWeight。
			if provider.MinWeight < 0 {
				return RuntimeConfig{}, fmt.Errorf("pool %s provider %s min_weight cannot be negative", pool.ID, provider.Name)
			}
		}
	}

	// 返回 RuntimeConfig，把补完默认值的 cfg 和解析后的时间一起交给上层。
	return RuntimeConfig{
		Config:           cfg,
		WindowDuration:   window,
		CooldownDuration: cooldown,
	}, nil
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
		MinSuccessRateForRecovery: 0.95,
		MinErrors:                 3,
		CriticalErrorThreshold:    2,
		MinLatencySamples:         5,
		RequiredBadWindows:        2,
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
	if rules.RequiredBadWindows <= 0 {
		rules.RequiredBadWindows = defaults.RequiredBadWindows
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
