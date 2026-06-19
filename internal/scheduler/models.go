package scheduler

import "time"

type PoolProviderState struct {
	PoolID           string     `json:"pool_id"`
	VirtualKey       string     `json:"virtual_key"`
	VirtualKeyID     string     `json:"virtual_key_id"`
	ProviderConfigID int        `json:"provider_config_id"`
	Provider         string     `json:"provider"`
	CurrentWeight    float64    `json:"current_weight"`
	AllowedModels    []string   `json:"allowed_models,omitempty"`
	AllowAllKeys     bool       `json:"allow_all_keys"`
	ProviderKeyIDs   []string   `json:"provider_key_ids,omitempty"`
	EnabledKeyCount  int        `json:"enabled_key_count"`
	LastObservedAt   *time.Time `json:"last_observed_at,omitempty"`
	CurrentInBifrost bool       `json:"current_in_bifrost"`
}

type ProviderMetric struct {
	PoolID                 string         `json:"pool_id"`
	VirtualKey             string         `json:"virtual_key"`
	Provider               string         `json:"provider"`
	Total                  int            `json:"total"`
	Success                int            `json:"success"`
	Errors                 int            `json:"errors"`
	ErrorRate              float64        `json:"error_rate"`
	SuccessRate            float64        `json:"success_rate"`
	P95LatencyMS           *float64       `json:"p95_latency_ms,omitempty"`
	TimeoutOrStreamIdle    int            `json:"timeout_or_stream_idle"`
	CriticalErrors         int            `json:"critical_errors"`
	LastSeenAt             *time.Time     `json:"last_seen_at,omitempty"`
	ErrorFamilies          []string       `json:"error_families,omitempty"`
	Windows                []WindowMetric `json:"-"`
	BadWindows             int            `json:"bad_windows,omitempty"`
	ConsecutiveBadWindows  int            `json:"consecutive_bad_windows,omitempty"`
	SlowWindows            int            `json:"slow_windows,omitempty"`
	ConsecutiveSlowWindows int            `json:"consecutive_slow_windows,omitempty"`
}

type Decision struct {
	PoolID        string         `json:"pool_id"`
	VirtualKey    string         `json:"virtual_key"`
	Provider      string         `json:"provider"`
	Action        string         `json:"action"`
	CurrentWeight float64        `json:"current_weight"`
	TargetWeight  float64        `json:"target_weight"`
	Reason        string         `json:"reason"`
	Severity      string         `json:"severity"`
	DryRun        bool           `json:"dry_run"`
	Inputs        DecisionInputs `json:"inputs"`
	Apply         *ApplyResult   `json:"apply,omitempty"`
}

type DecisionInputs struct {
	Total                  int      `json:"total"`
	Success                int      `json:"success"`
	Errors                 int      `json:"errors"`
	ErrorRate              float64  `json:"error_rate"`
	SuccessRate            float64  `json:"success_rate"`
	P95LatencyMS           *float64 `json:"p95_latency_ms,omitempty"`
	TimeoutOrStreamIdle    int      `json:"timeout_or_stream_idle"`
	CriticalErrors         int      `json:"critical_errors"`
	ErrorFamilies          []string `json:"error_families,omitempty"`
	BadWindows             int      `json:"bad_windows,omitempty"`
	ConsecutiveBadWindows  int      `json:"consecutive_bad_windows,omitempty"`
	SlowWindows            int      `json:"slow_windows,omitempty"`
	ConsecutiveSlowWindows int      `json:"consecutive_slow_windows,omitempty"`
	WindowCount            int      `json:"window_count,omitempty"`
}

type WindowMetric struct {
	Start               time.Time `json:"start"`
	End                 time.Time `json:"end"`
	Total               int       `json:"total"`
	Success             int       `json:"success"`
	Errors              int       `json:"errors"`
	ErrorRate           float64   `json:"error_rate"`
	SuccessRate         float64   `json:"success_rate"`
	P95LatencyMS        *float64  `json:"p95_latency_ms,omitempty"`
	TimeoutOrStreamIdle int       `json:"timeout_or_stream_idle"`
	CriticalErrors      int       `json:"critical_errors"`
	Bad                 bool      `json:"bad"`
	Slow                bool      `json:"slow"`
}

type ApplyResult struct {
	Applied bool   `json:"applied"`
	Message string `json:"message"`
}

type Plan struct {
	GeneratedAt   time.Time           `json:"generated_at"`
	WindowStart   time.Time           `json:"window_start"`
	Window        string              `json:"window"`
	Mode          string              `json:"mode"`
	ApplyEnabled  bool                `json:"apply_enabled"`
	Pools         []PoolSnapshot      `json:"pools"`
	Metrics       []ProviderMetric    `json:"metrics"`
	CurrentStates []PoolProviderState `json:"current_states"`
	Decisions     []Decision          `json:"decisions"`
}

type PoolSnapshot struct {
	ID         string `json:"id"`
	VirtualKey string `json:"virtual_key"`
	Kind       string `json:"kind"`
}
