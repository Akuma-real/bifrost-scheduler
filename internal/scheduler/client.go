package scheduler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

type ClientOptions struct {
	BaseURL  string
	Username string
	Password string
	Paths    APIPaths
	Timeout  time.Duration
}

type BifrostClient struct {
	baseURL  *url.URL
	username string
	password string
	token    string
	cookies  []*http.Cookie
	paths    APIPaths
	http     *http.Client
}

func NewBifrostClient(opts ClientOptions) (*BifrostClient, error) {
	if opts.BaseURL == "" {
		return nil, fmt.Errorf("api base URL is required")
	}
	base, err := url.Parse(opts.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse api base URL: %w", err)
	}
	if base.Scheme == "" || base.Host == "" {
		return nil, fmt.Errorf("api base URL must include scheme and host")
	}
	if opts.Username == "" {
		return nil, fmt.Errorf("api username is required")
	}
	if opts.Password == "" {
		return nil, fmt.Errorf("api password is required")
	}
	if opts.Paths.VirtualKeys == "" {
		opts.Paths.VirtualKeys = "/api/governance/virtual-keys"
	}
	if opts.Paths.Logs == "" {
		opts.Paths.Logs = "/api/logs"
	}
	if opts.Paths.Login == "" {
		opts.Paths.Login = "/api/session/login"
	}
	if opts.Paths.ProviderKey == "" {
		opts.Paths.ProviderKey = "/api/providers/{provider}/keys/{key_id}"
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 30 * time.Second
	}

	return &BifrostClient{
		baseURL:  base,
		username: opts.Username,
		password: opts.Password,
		paths:    opts.Paths,
		http:     &http.Client{Timeout: opts.Timeout},
	}, nil
}

func (c *BifrostClient) Close() {}

func (c *BifrostClient) Login(ctx context.Context) error {
	payload := loginRequest{
		Username: c.username,
		Password: c.password,
	}
	var response loginResponse
	resp, err := c.doJSON(ctx, http.MethodPost, c.paths.Login, nil, payload, &response)
	if err != nil {
		return fmt.Errorf("login to bifrost: %w", err)
	}
	if response.Token != "" {
		c.token = response.Token
		return nil
	}
	c.cookies = resp.Cookies()
	if len(c.cookies) == 0 {
		return fmt.Errorf("login to bifrost: response did not include token or session cookie")
	}
	return nil
}

func (c *BifrostClient) LoadProviderStates(ctx context.Context, pools []PoolConfig) ([]PoolProviderState, error) {
	virtualKeys, err := c.loadVirtualKeysForPools(ctx, pools)
	if err != nil {
		return nil, err
	}

	var out []PoolProviderState
	for _, pool := range pools {
		vk, ok := virtualKeys[pool.VirtualKey]
		seen := map[string]bool{}
		if ok {
			for _, providerConfig := range vk.ProviderConfigs {
				weight := 0.0
				if providerConfig.Weight != nil {
					weight = *providerConfig.Weight
				}
				state := PoolProviderState{
					PoolID:           pool.ID,
					VirtualKey:       pool.VirtualKey,
					VirtualKeyID:     vk.ID,
					ProviderConfigID: providerConfig.ID,
					Provider:         providerConfig.Provider,
					CurrentWeight:    weight,
					AllowedModels:    providerConfig.AllowedModels,
					AllowAllKeys:     providerConfig.AllowAllKeys,
					ProviderKeyIDs:   providerConfig.keyIDs(),
					EnabledKeyCount:  providerConfig.enabledKeyCount(),
					CurrentInBifrost: true,
				}
				seen[state.Provider] = true
				out = append(out, state)
			}
		}
		for _, provider := range pool.Providers {
			if seen[provider.Name] {
				continue
			}
			out = append(out, PoolProviderState{
				PoolID:     pool.ID,
				VirtualKey: pool.VirtualKey,
				Provider:   provider.Name,
			})
		}
	}
	return out, nil
}

func (c *BifrostClient) LoadMetrics(ctx context.Context, pools []PoolConfig, windowStart, windowEnd time.Time, windowDuration time.Duration) ([]ProviderMetric, error) {
	virtualKeys, err := c.loadVirtualKeysForPools(ctx, pools)
	if err != nil {
		return nil, err
	}
	if windowEnd.IsZero() {
		windowEnd = time.Now()
	}

	var out []ProviderMetric
	for _, pool := range pools {
		vk, ok := virtualKeys[pool.VirtualKey]
		if !ok || vk.ID == "" {
			out = append(out, zeroMetrics(pool)...)
			continue
		}
		logs, err := c.loadLogs(ctx, vk.ID, windowStart)
		if err != nil {
			return nil, fmt.Errorf("load logs for pool %s: %w", pool.ID, err)
		}
		out = append(out, aggregateMetrics(pool, logs, windowStart, windowEnd, windowDuration)...)
	}
	return out, nil
}

func (c *BifrostClient) SetProviderWeight(ctx context.Context, state PoolProviderState, weight float64) error {
	if state.VirtualKeyID == "" {
		return fmt.Errorf("virtual key id is missing for provider %s", state.Provider)
	}
	if state.ProviderConfigID == 0 {
		return fmt.Errorf("provider config id is missing for provider %s", state.Provider)
	}

	vk, err := c.getVirtualKey(ctx, state.VirtualKeyID)
	if err != nil {
		return err
	}

	found := false
	payload := updateVirtualKeyRequest{
		Name:            vk.Name,
		Description:     vk.Description,
		ProviderConfigs: make([]updateProviderConfigRequest, 0, len(vk.ProviderConfigs)),
		TeamID:          vk.TeamID,
		CustomerID:      vk.CustomerID,
		IsActive:        vk.IsActive,
		CalendarAligned: vk.CalendarAligned,
	}
	for _, providerConfig := range vk.ProviderConfigs {
		request := providerConfig.toUpdateRequest()
		if providerConfig.ID == state.ProviderConfigID {
			found = true
			request.Weight = &weight
		}
		payload.ProviderConfigs = append(payload.ProviderConfigs, request)
	}
	if !found {
		return fmt.Errorf("provider config %d not found on virtual key %s", state.ProviderConfigID, state.VirtualKeyID)
	}

	var response virtualKeyResponse
	path := joinPath(c.paths.VirtualKeys, url.PathEscape(state.VirtualKeyID))
	if _, err := c.doJSON(ctx, http.MethodPut, path, nil, payload, &response); err != nil {
		return fmt.Errorf("update virtual key provider weight: %w", err)
	}
	return nil
}

func (c *BifrostClient) SetProviderKeysEnabled(ctx context.Context, state PoolProviderState, enabled bool) error {
	if len(state.ProviderKeyIDs) == 0 {
		return fmt.Errorf("provider %s has no bound key ids in virtual key %s", state.Provider, state.VirtualKey)
	}
	var failures []string
	for _, keyID := range state.ProviderKeyIDs {
		if err := c.setProviderKeyEnabled(ctx, state.Provider, keyID, enabled); err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", keyID, err))
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("%s", strings.Join(failures, "; "))
	}
	return nil
}

func (c *BifrostClient) loadVirtualKeysForPools(ctx context.Context, pools []PoolConfig) (map[string]virtualKey, error) {
	out := make(map[string]virtualKey, len(pools))
	for _, pool := range pools {
		if _, ok := out[pool.VirtualKey]; ok {
			continue
		}
		vk, err := c.findVirtualKeyByName(ctx, pool.VirtualKey)
		if err != nil {
			return nil, fmt.Errorf("load virtual key %s: %w", pool.VirtualKey, err)
		}
		if vk.ID != "" {
			out[pool.VirtualKey] = vk
		}
	}
	return out, nil
}

func (c *BifrostClient) findVirtualKeyByName(ctx context.Context, name string) (virtualKey, error) {
	query := url.Values{}
	query.Set("search", name)
	query.Set("limit", "100")
	query.Set("offset", "0")

	var response listVirtualKeysResponse
	if _, err := c.doJSON(ctx, http.MethodGet, c.paths.VirtualKeys, query, nil, &response); err != nil {
		return virtualKey{}, err
	}

	for _, vk := range response.VirtualKeys {
		if vk.Name == name {
			return vk, nil
		}
	}
	return virtualKey{}, nil
}

func (c *BifrostClient) getVirtualKey(ctx context.Context, id string) (virtualKey, error) {
	var response getVirtualKeyResponse
	path := joinPath(c.paths.VirtualKeys, url.PathEscape(id))
	if _, err := c.doJSON(ctx, http.MethodGet, path, nil, nil, &response); err != nil {
		return virtualKey{}, fmt.Errorf("get virtual key %s: %w", id, err)
	}
	return response.VirtualKey, nil
}

func (c *BifrostClient) loadLogs(ctx context.Context, virtualKeyID string, windowStart time.Time) ([]logEntry, error) {
	const limit = 1000
	var out []logEntry
	for offset := 0; ; offset += limit {
		query := url.Values{}
		query.Set("virtual_key_ids", virtualKeyID)
		query.Set("start_time", windowStart.UTC().Format(time.RFC3339Nano))
		query.Set("limit", strconv.Itoa(limit))
		query.Set("offset", strconv.Itoa(offset))
		query.Set("sort_by", "timestamp")
		query.Set("order", "asc")

		var response searchLogsResponse
		if _, err := c.doJSON(ctx, http.MethodGet, c.paths.Logs, query, nil, &response); err != nil {
			return nil, err
		}
		out = append(out, response.Logs...)
		if len(response.Logs) < limit {
			break
		}
		if response.Pagination.TotalCount > 0 && int64(offset+limit) >= response.Pagination.TotalCount {
			break
		}
	}
	return out, nil
}

func (c *BifrostClient) setProviderKeyEnabled(ctx context.Context, provider, keyID string, enabled bool) error {
	path := c.paths.ProviderKey
	path = strings.ReplaceAll(path, "{provider}", url.PathEscape(provider))
	path = strings.ReplaceAll(path, "{key_id}", url.PathEscape(keyID))
	payload := map[string]bool{"enabled": enabled}
	var response json.RawMessage
	if _, err := c.doJSON(ctx, http.MethodPut, path, nil, payload, &response); err != nil {
		return err
	}
	return nil
}

func (c *BifrostClient) doJSON(ctx context.Context, method, path string, query url.Values, body any, out any) (*http.Response, error) {
	requestURL := c.resolve(path, query)

	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshal request: %w", err)
		}
		reader = bytes.NewReader(data)
	}

	req, err := http.NewRequestWithContext(ctx, method, requestURL, reader)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	for _, cookie := range c.cookies {
		req.AddCookie(cookie)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, requestURL, err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return resp, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp, apiError(resp.StatusCode, data)
	}
	if out == nil || len(data) == 0 {
		return resp, nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return resp, fmt.Errorf("decode %s %s response: %w", method, requestURL, err)
	}
	return resp, nil
}

func (c *BifrostClient) resolve(path string, query url.Values) string {
	u := *c.baseURL
	basePath := strings.TrimRight(u.Path, "/")
	child := "/" + strings.TrimLeft(path, "/")
	u.Path = basePath + child
	u.RawQuery = query.Encode()
	return u.String()
}

func apiError(status int, data []byte) error {
	message := strings.TrimSpace(string(data))
	var parsed struct {
		Error struct {
			Message string `json:"message"`
			Code    string `json:"code"`
			Type    string `json:"type"`
		} `json:"error"`
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &parsed); err == nil {
		if parsed.Error.Message != "" {
			message = parsed.Error.Message
		} else if parsed.Error.Code != "" {
			message = parsed.Error.Code
		} else if parsed.Type != "" {
			message = parsed.Type
		}
	}
	if message == "" {
		message = http.StatusText(status)
	}
	return fmt.Errorf("bifrost API returned HTTP %d: %s", status, message)
}

func joinPath(base, child string) string {
	return strings.TrimRight(base, "/") + "/" + strings.TrimLeft(child, "/")
}

type listVirtualKeysResponse struct {
	VirtualKeys []virtualKey `json:"virtual_keys"`
}

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type loginResponse struct {
	Token string `json:"token"`
}

type getVirtualKeyResponse struct {
	VirtualKey virtualKey `json:"virtual_key"`
}

type virtualKeyResponse struct {
	VirtualKey virtualKey `json:"virtual_key"`
}

type virtualKey struct {
	ID              string                  `json:"id"`
	Name            string                  `json:"name"`
	Description     string                  `json:"description,omitempty"`
	IsActive        bool                    `json:"is_active"`
	ProviderConfigs []virtualProviderConfig `json:"provider_configs"`
	TeamID          string                  `json:"team_id,omitempty"`
	CustomerID      string                  `json:"customer_id,omitempty"`
	CalendarAligned bool                    `json:"calendar_aligned"`
}

type virtualProviderConfig struct {
	ID                int           `json:"id"`
	VirtualKeyID      string        `json:"virtual_key_id"`
	Provider          string        `json:"provider"`
	Weight            *float64      `json:"weight"`
	AllowedModels     []string      `json:"allowed_models"`
	BlacklistedModels []string      `json:"blacklisted_models"`
	AllowAllKeys      bool          `json:"allow_all_keys"`
	Keys              []providerKey `json:"keys"`
}

type providerKey struct {
	ID      int      `json:"id"`
	KeyID   string   `json:"key_id"`
	Name    string   `json:"name"`
	Enabled *bool    `json:"enabled"`
	Weight  float64  `json:"weight,omitempty"`
	Models  []string `json:"models,omitempty"`
}

func (p virtualProviderConfig) keyIDs() []string {
	out := make([]string, 0, len(p.Keys))
	for _, key := range p.Keys {
		if key.KeyID != "" {
			out = append(out, key.KeyID)
			continue
		}
		if key.ID != 0 {
			out = append(out, strconv.Itoa(key.ID))
		}
	}
	return out
}

func (p virtualProviderConfig) enabledKeyCount() int {
	if p.AllowAllKeys && len(p.Keys) == 0 {
		return 0
	}
	count := 0
	for _, key := range p.Keys {
		if key.Enabled == nil || *key.Enabled {
			count++
		}
	}
	return count
}

func (p virtualProviderConfig) toUpdateRequest() updateProviderConfigRequest {
	return updateProviderConfigRequest{
		ID:                p.ID,
		Provider:          p.Provider,
		Weight:            p.Weight,
		AllowedModels:     cloneStrings(p.AllowedModels),
		BlacklistedModels: cloneStrings(p.BlacklistedModels),
		AllowAllKeys:      p.AllowAllKeys,
		KeyIDs:            p.keyIDs(),
		SelectedKeyIDs:    p.keyIDs(),
	}
}

type updateVirtualKeyRequest struct {
	Name            string                        `json:"name,omitempty"`
	Description     string                        `json:"description,omitempty"`
	ProviderConfigs []updateProviderConfigRequest `json:"provider_configs"`
	TeamID          string                        `json:"team_id,omitempty"`
	CustomerID      string                        `json:"customer_id,omitempty"`
	IsActive        bool                          `json:"is_active"`
	CalendarAligned bool                          `json:"calendar_aligned"`
}

type updateProviderConfigRequest struct {
	ID                int      `json:"id,omitempty"`
	Provider          string   `json:"provider,omitempty"`
	Weight            *float64 `json:"weight"`
	AllowedModels     []string `json:"allowed_models,omitempty"`
	BlacklistedModels []string `json:"blacklisted_models,omitempty"`
	AllowAllKeys      bool     `json:"allow_all_keys"`
	KeyIDs            []string `json:"key_ids,omitempty"`
	SelectedKeyIDs    []string `json:"selected_key_ids,omitempty"`
}

type searchLogsResponse struct {
	Logs       []logEntry `json:"logs"`
	Pagination struct {
		TotalCount int64 `json:"total_count"`
	} `json:"pagination"`
}

type logEntry struct {
	Provider       string          `json:"provider"`
	Status         string          `json:"status"`
	Timestamp      time.Time       `json:"timestamp"`
	Latency        float64         `json:"latency"`
	ErrorDetails   json.RawMessage `json:"error_details"`
	VirtualKeyName string          `json:"virtual_key_name"`
}

func aggregateMetrics(pool PoolConfig, logs []logEntry, windowStart, windowEnd time.Time, windowDuration time.Duration) []ProviderMetric {
	if windowDuration <= 0 {
		windowDuration = 15 * time.Minute
	}
	if windowEnd.IsZero() || !windowEnd.After(windowStart) {
		windowEnd = windowStart.Add(windowDuration)
	}

	byProvider := map[string]*metricAccumulator{}
	byProviderWindow := map[string][]*metricAccumulator{}
	for _, log := range logs {
		if log.Provider == "" {
			continue
		}
		if !log.Timestamp.IsZero() && (log.Timestamp.Before(windowStart) || !log.Timestamp.Before(windowEnd)) {
			continue
		}
		acc := byProvider[log.Provider]
		if acc == nil {
			acc = &metricAccumulator{}
			byProvider[log.Provider] = acc
		}
		acc.add(log)

		idx := windowIndex(log.Timestamp, windowStart, windowDuration)
		if idx < 0 {
			continue
		}
		windows := byProviderWindow[log.Provider]
		for len(windows) <= idx {
			windows = append(windows, &metricAccumulator{})
		}
		windows[idx].add(log)
		byProviderWindow[log.Provider] = windows
	}

	names := map[string]bool{}
	for _, provider := range pool.Providers {
		names[provider.Name] = true
	}
	for name := range byProvider {
		names[name] = true
	}

	providerNames := make([]string, 0, len(names))
	for name := range names {
		providerNames = append(providerNames, name)
	}
	sort.Strings(providerNames)

	out := make([]ProviderMetric, 0, len(providerNames))
	for _, provider := range providerNames {
		acc := byProvider[provider]
		if acc == nil {
			out = append(out, ProviderMetric{
				PoolID:     pool.ID,
				VirtualKey: pool.VirtualKey,
				Provider:   provider,
				Windows:    emptyWindows(windowStart, windowEnd, windowDuration),
			})
			continue
		}
		metric := acc.metric(pool, provider)
		metric.Windows = windowMetrics(byProviderWindow[provider], windowStart, windowEnd, windowDuration)
		out = append(out, metric)
	}
	return out
}

func windowIndex(ts, windowStart time.Time, windowDuration time.Duration) int {
	if ts.IsZero() || windowDuration <= 0 || ts.Before(windowStart) {
		return -1
	}
	return int(ts.Sub(windowStart) / windowDuration)
}

func emptyWindows(windowStart, windowEnd time.Time, windowDuration time.Duration) []WindowMetric {
	if windowDuration <= 0 || !windowEnd.After(windowStart) {
		return nil
	}
	count := int(math.Ceil(float64(windowEnd.Sub(windowStart)) / float64(windowDuration)))
	out := make([]WindowMetric, 0, count)
	for i := 0; i < count; i++ {
		start := windowStart.Add(time.Duration(i) * windowDuration)
		end := start.Add(windowDuration)
		if end.After(windowEnd) {
			end = windowEnd
		}
		out = append(out, WindowMetric{Start: start, End: end})
	}
	return out
}

func windowMetrics(accumulators []*metricAccumulator, windowStart, windowEnd time.Time, windowDuration time.Duration) []WindowMetric {
	windows := emptyWindows(windowStart, windowEnd, windowDuration)
	for i, acc := range accumulators {
		if i >= len(windows) || acc == nil {
			continue
		}
		windows[i] = acc.windowMetric(windows[i].Start, windows[i].End)
	}
	return windows
}

func zeroMetrics(pool PoolConfig) []ProviderMetric {
	out := make([]ProviderMetric, 0, len(pool.Providers))
	for _, provider := range pool.Providers {
		out = append(out, ProviderMetric{
			PoolID:     pool.ID,
			VirtualKey: pool.VirtualKey,
			Provider:   provider.Name,
		})
	}
	return out
}

type metricAccumulator struct {
	total               int
	success             int
	errors              int
	latencies           []float64
	timeoutOrStreamIdle int
	criticalErrors      int
	lastSeenAt          *time.Time
	errorFamilies       map[string]bool
}

func (a *metricAccumulator) add(log logEntry) {
	a.total++
	if strings.EqualFold(log.Status, "success") {
		a.success++
		if log.Latency > 0 {
			a.latencies = append(a.latencies, log.Latency)
		}
	} else if strings.EqualFold(log.Status, "error") {
		a.errors++
		category := categorizeError(log.ErrorDetails)
		if a.errorFamilies == nil {
			a.errorFamilies = map[string]bool{}
		}
		a.errorFamilies[category] = true
		if category == "timeout" || category == "stream_idle_timeout" {
			a.timeoutOrStreamIdle++
		}
		if category == "credentials_exhausted" || category == "quota_or_no_token" {
			a.criticalErrors++
		}
	}
	if !log.Timestamp.IsZero() && (a.lastSeenAt == nil || log.Timestamp.After(*a.lastSeenAt)) {
		seen := log.Timestamp
		a.lastSeenAt = &seen
	}
}

func (a *metricAccumulator) metric(pool PoolConfig, provider string) ProviderMetric {
	errorRate := 0.0
	successRate := 0.0
	if a.total > 0 {
		errorRate = float64(a.errors) / float64(a.total)
		successRate = float64(a.success) / float64(a.total)
	}
	return ProviderMetric{
		PoolID:              pool.ID,
		VirtualKey:          pool.VirtualKey,
		Provider:            provider,
		Total:               a.total,
		Success:             a.success,
		Errors:              a.errors,
		ErrorRate:           errorRate,
		SuccessRate:         successRate,
		P95LatencyMS:        percentile(a.latencies, 0.95),
		TimeoutOrStreamIdle: a.timeoutOrStreamIdle,
		CriticalErrors:      a.criticalErrors,
		LastSeenAt:          a.lastSeenAt,
		ErrorFamilies:       sortedFamilies(a.errorFamilies),
	}
}

func (a *metricAccumulator) windowMetric(start, end time.Time) WindowMetric {
	errorRate := 0.0
	successRate := 0.0
	if a.total > 0 {
		errorRate = float64(a.errors) / float64(a.total)
		successRate = float64(a.success) / float64(a.total)
	}
	return WindowMetric{
		Start:               start,
		End:                 end,
		Total:               a.total,
		Success:             a.success,
		Errors:              a.errors,
		ErrorRate:           errorRate,
		SuccessRate:         successRate,
		P95LatencyMS:        percentile(a.latencies, 0.95),
		TimeoutOrStreamIdle: a.timeoutOrStreamIdle,
		CriticalErrors:      a.criticalErrors,
	}
}

func categorizeError(raw json.RawMessage) string {
	text := strings.ToLower(string(raw))
	if strings.TrimSpace(text) == "" || text == "null" {
		return "other"
	}
	switch {
	case strings.Contains(text, "upstream_credentials_exhausted"):
		return "credentials_exhausted"
	case strings.Contains(text, "insufficient_user_quota"),
		strings.Contains(text, "用户额度不足"),
		strings.Contains(text, "没有可用token"),
		strings.Contains(text, "预扣费额度失败"),
		strings.Contains(text, "insufficient account balance"):
		return "quota_or_no_token"
	case strings.Contains(text, "concurrency limit exceeded"),
		strings.Contains(text, "rate limit"),
		strings.Contains(text, "too many pending requests"):
		return "rate_or_concurrency_limit"
	case strings.Contains(text, "stream idle timeout"):
		return "stream_idle_timeout"
	case strings.Contains(text, "stream closed"):
		return "stream_closed"
	case strings.Contains(text, "request timed out"),
		strings.Contains(text, "504 gateway time-out"),
		strings.Contains(text, `"status_code":524`):
		return "timeout"
	case strings.Contains(text, "service temporarily unavailable"),
		strings.Contains(text, `"status_code":503`):
		return "service_unavailable"
	case strings.Contains(text, "image generation is not enabled for this group"):
		return "image_group_disabled"
	case strings.Contains(text, "no available channel for model"),
		strings.Contains(text, "model_not_found"):
		return "model_unavailable"
	case strings.Contains(text, "client disconnected"):
		return "client_disconnected"
	default:
		return "other"
	}
}

func percentile(values []float64, p float64) *float64 {
	if len(values) == 0 {
		return nil
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	idx := int(math.Ceil(p*float64(len(sorted)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	value := sorted[idx]
	return &value
}

func sortedFamilies(families map[string]bool) []string {
	if len(families) == 0 {
		return nil
	}
	out := make([]string, 0, len(families))
	for family := range families {
		out = append(out, family)
	}
	sort.Strings(out)
	return out
}

func cloneStrings(values []string) []string {
	if values == nil {
		return nil
	}
	return append([]string(nil), values...)
}
