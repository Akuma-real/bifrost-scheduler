// package bifrost 表示这个文件属于“Bifrost API 适配层”。
//
// 适配层负责把本项目的 Store 接口转换成真实 HTTP API 调用。
// 业务规则不写在这里；这里只关心“怎么登录、怎么读 VK、怎么 PUT 更新权重”。
package bifrost

// import 表示这个文件要使用哪些包。
//
// bytes：把 JSON 字节变成 HTTP 请求体。
// context：控制请求取消和超时。
// encoding/json：编码请求 JSON、解析响应 JSON。
// fmt：生成错误信息。
// io：读取响应体。
// math：计算 P95 分位数。
// net/http：发 HTTP 请求。
// net/url：处理 URL 和查询参数。
// sort：排序 provider 名称、错误类型。
// strconv：数字和字符串互转。
// strings：字符串判断和替换。
// time：时间范围、请求超时、日志时间戳。
// domain：调度器领域层的数据结构。
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

	domain "github.com/Akuma-real/bifrost-scheduler/internal/domain/scheduler"
)

// ClientOptions 是创建 BifrostClient 时需要的参数。
//
// 这个结构体来自 CLI/config/env 的组合。
type ClientOptions struct {
	// BaseURL 是 Bifrost 的根地址，例如 https://bifrost.ggapi.cc。
	BaseURL string
	// Username 是 Bifrost 管理后台登录用户名。
	Username string
	// Password 是 Bifrost 管理后台登录密码。
	Password string
	// Paths 是各个 API 的路径，通常由配置默认值提供。
	Paths domain.APIPaths
	// Timeout 是单个 HTTP 请求最长等待时间。
	Timeout time.Duration
}

// BifrostClient 是真实连接 Bifrost 的客户端。
//
// 它实现了 internal/app/scheduler.Store 接口，所以 Planner 可以直接使用它。
type BifrostClient struct {
	// baseURL 是解析后的 Bifrost 地址。
	baseURL *url.URL
	// username / password 登录时使用。
	username string
	password string
	// token 保存登录后返回的 Bearer token。
	token string
	// cookies 保存登录后返回的 session cookie。
	// 有些 Bifrost 版本不返回 token，只返回 cookie，所以两种都支持。
	cookies []*http.Cookie
	// paths 保存 Bifrost API 路径。
	paths domain.APIPaths
	// http 是标准库 HTTP 客户端。
	http *http.Client
}

// NewBifrostClient 创建 Bifrost API 客户端，并校验必填参数。
func NewBifrostClient(opts ClientOptions) (*BifrostClient, error) {
	// BaseURL 必须有，否则不知道请求发到哪里。
	if opts.BaseURL == "" {
		return nil, fmt.Errorf("api base URL is required")
	}
	// url.Parse 把字符串解析成 URL 结构体。
	base, err := url.Parse(opts.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse api base URL: %w", err)
	}
	// Scheme 是 http/https，Host 是域名。
	if base.Scheme == "" || base.Host == "" {
		return nil, fmt.Errorf("api base URL must include scheme and host")
	}
	if opts.Username == "" {
		return nil, fmt.Errorf("api username is required")
	}
	if opts.Password == "" {
		return nil, fmt.Errorf("api password is required")
	}
	// 下面这些 path 正常由 NormalizeConfig 写死。
	// 这里再兜底一次，避免测试或外部直接调用 NewBifrostClient 时漏填。
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

	// &BifrostClient{...} 返回客户端指针。
	return &BifrostClient{
		baseURL:  base,
		username: opts.Username,
		password: opts.Password,
		paths:    opts.Paths,
		http:     &http.Client{Timeout: opts.Timeout},
	}, nil
}

// Close 预留清理入口。
//
// 当前 http.Client 没有需要关闭的长期连接对象，所以这里是空函数。
// 保留它能让上层统一 defer client.Close()。
func (c *BifrostClient) Close() {}

// Login 登录 Bifrost，并保存 token 或 session cookie。
func (c *BifrostClient) Login(ctx context.Context) error {
	// payload 是请求体，会被编码成 JSON。
	payload := loginRequest{
		Username: c.username,
		Password: c.password,
	}
	// response 用来接收 JSON 响应里的 token。
	var response loginResponse
	resp, err := c.doJSON(ctx, http.MethodPost, c.paths.Login, nil, payload, &response)
	if err != nil {
		return fmt.Errorf("login to bifrost: %w", err)
	}
	if response.Token != "" {
		c.token = response.Token
		return nil
	}
	// 某些 Bifrost 登录接口不返回 token，而是通过 Set-Cookie 建立会话。
	c.cookies = resp.Cookies()
	if len(c.cookies) == 0 {
		return fmt.Errorf("login to bifrost: response did not include token or session cookie")
	}
	return nil
}

// LoadProviderStates 读取每个 pool 当前在 Bifrost 里的 provider 状态。
//
// 返回值包括权重、是否 allow_all_keys、绑定 key id 等。
func (c *BifrostClient) LoadProviderStates(ctx context.Context, pools []domain.PoolConfig) ([]domain.PoolProviderState, error) {
	// 先把每个 pool 对应的 Virtual Key 加载出来。
	virtualKeys, err := c.loadVirtualKeysForPools(ctx, pools)
	if err != nil {
		return nil, err
	}

	var out []domain.PoolProviderState
	for _, pool := range pools {
		// 通过 pool.VirtualKey 找到真实 VK。
		vk, ok := virtualKeys[pool.VirtualKey]
		// seen 记录 Bifrost 里已经出现过哪些 provider。
		seen := map[string]bool{}
		if ok {
			for _, providerConfig := range vk.ProviderConfigs {
				// weight 是指针，nil 表示接口没返回权重，这里当作 0。
				weight := 0.0
				if providerConfig.Weight != nil {
					weight = *providerConfig.Weight
				}
				// 把 Bifrost API 返回结构转换成领域层状态结构。
				state := domain.PoolProviderState{
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
		// 配置里有、但 Bifrost 里没有的 provider，也要返回一个状态。
		// CurrentInBifrost 默认为 false，领域层会生成 review_missing_provider。
		for _, provider := range pool.Providers {
			if seen[provider.Name] {
				continue
			}
			out = append(out, domain.PoolProviderState{
				PoolID:     pool.ID,
				VirtualKey: pool.VirtualKey,
				Provider:   provider.Name,
			})
		}
	}
	return out, nil
}

// LoadMetrics 读取 Bifrost 调用日志，并聚合成 provider 指标。
func (c *BifrostClient) LoadMetrics(ctx context.Context, pools []domain.PoolConfig, windowStart, windowEnd time.Time, windowDuration time.Duration) ([]domain.ProviderMetric, error) {
	virtualKeys, err := c.loadVirtualKeysForPools(ctx, pools)
	if err != nil {
		return nil, err
	}
	// windowEnd 为空时，用当前时间兜底。
	if windowEnd.IsZero() {
		windowEnd = time.Now()
	}

	var out []domain.ProviderMetric
	for _, pool := range pools {
		vk, ok := virtualKeys[pool.VirtualKey]
		// 找不到 VK 时，仍然给每个配置 provider 返回零指标，报告不会缺行。
		if !ok || vk.ID == "" {
			out = append(out, zeroMetrics(pool)...)
			continue
		}
		// 读取这个 VK 在时间窗口内的日志。
		logs, err := c.loadLogs(ctx, vk.ID, windowStart, windowEnd)
		if err != nil {
			return nil, fmt.Errorf("load logs for pool %s: %w", pool.ID, err)
		}
		// 把日志聚合成指标。
		out = append(out, aggregateMetrics(pool, logs, windowStart, windowEnd, windowDuration)...)
	}
	return out, nil
}

// SetProviderWeight 更新某个 provider config 的权重。
//
// Bifrost 的 VK 更新接口通常要求提交整个 Virtual Key 配置，
// 所以这里会先 GET 当前 VK，再只改目标 provider 的 weight，最后 PUT 回去。
func (c *BifrostClient) SetProviderWeight(ctx context.Context, state domain.PoolProviderState, weight float64) error {
	if state.VirtualKeyID == "" {
		return fmt.Errorf("virtual key id is missing for provider %s", state.Provider)
	}
	if state.ProviderConfigID == 0 {
		return fmt.Errorf("provider config id is missing for provider %s", state.Provider)
	}

	// 先读取最新 VK，避免用旧数据覆盖线上其他字段。
	vk, err := c.getVirtualKey(ctx, state.VirtualKeyID)
	if err != nil {
		return err
	}

	found := false
	// payload 是 PUT 回 Bifrost 的请求体。
	// 先复制 VK 原有字段，尽量只改变 weight。
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
		// toUpdateRequest 会保留 allow_all_keys、key_ids、selected_key_ids 等字段。
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

	// PUT /api/governance/virtual-keys/{id}
	var response virtualKeyResponse
	path := joinPath(c.paths.VirtualKeys, url.PathEscape(state.VirtualKeyID))
	if _, err := c.doJSON(ctx, http.MethodPut, path, nil, payload, &response); err != nil {
		return fmt.Errorf("update virtual key provider weight: %w", err)
	}
	return nil
}

// SetProviderKeysEnabled 批量启用或禁用某个 provider 绑定的 key。
func (c *BifrostClient) SetProviderKeysEnabled(ctx context.Context, state domain.PoolProviderState, enabled bool) error {
	if len(state.ProviderKeyIDs) == 0 {
		return fmt.Errorf("provider %s has no bound key ids in virtual key %s", state.Provider, state.VirtualKey)
	}
	// failures 收集失败的 key，这样一次操作可以尽量处理所有 key。
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

// loadVirtualKeysForPools 按 pool 配置加载所有需要的 Virtual Key。
//
// 返回 map：key 是 VK 名称，value 是 VK 详情。
func (c *BifrostClient) loadVirtualKeysForPools(ctx context.Context, pools []domain.PoolConfig) (map[string]virtualKey, error) {
	out := make(map[string]virtualKey, len(pools))
	for _, pool := range pools {
		// 多个 pool 理论上可能指向同一个 VK，这里避免重复请求。
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

// findVirtualKeyByName 通过名称搜索 Bifrost Virtual Key。
func (c *BifrostClient) findVirtualKeyByName(ctx context.Context, name string) (virtualKey, error) {
	// url.Values 用来构造 query string。
	query := url.Values{}
	query.Set("search", name)
	query.Set("limit", "100")
	query.Set("offset", "0")

	var response listVirtualKeysResponse
	if _, err := c.doJSON(ctx, http.MethodGet, c.paths.VirtualKeys, query, nil, &response); err != nil {
		return virtualKey{}, err
	}

	// 搜索接口可能返回多个近似结果，所以这里必须精确匹配 name。
	for _, vk := range response.VirtualKeys {
		if vk.Name == name {
			return vk, nil
		}
	}
	return virtualKey{}, nil
}

// getVirtualKey 根据 VK id 读取完整 Virtual Key。
func (c *BifrostClient) getVirtualKey(ctx context.Context, id string) (virtualKey, error) {
	var response getVirtualKeyResponse
	path := joinPath(c.paths.VirtualKeys, url.PathEscape(id))
	if _, err := c.doJSON(ctx, http.MethodGet, path, nil, nil, &response); err != nil {
		return virtualKey{}, fmt.Errorf("get virtual key %s: %w", id, err)
	}
	return response.VirtualKey, nil
}

// loadLogs 分页读取某个 Virtual Key 在时间窗口内的调用日志。
func (c *BifrostClient) loadLogs(ctx context.Context, virtualKeyID string, windowStart, windowEnd time.Time) ([]logEntry, error) {
	const limit = 100
	var out []logEntry
	// for offset := 0; ; offset += limit 是无限循环分页。
	// 中间通过 break 在读完时退出。
	for offset := 0; ; offset += limit {
		query := url.Values{}
		query.Set("virtual_key_ids", virtualKeyID)
		// API 使用 UTC 时间，避免本地时区影响查询。
		query.Set("start_time", windowStart.UTC().Format(time.RFC3339Nano))
		if !windowEnd.IsZero() && windowEnd.After(windowStart) {
			query.Set("end_time", windowEnd.UTC().Format(time.RFC3339Nano))
		}
		query.Set("limit", strconv.Itoa(limit))
		query.Set("offset", strconv.Itoa(offset))
		query.Set("sort_by", "timestamp")
		query.Set("order", "asc")

		var response searchLogsResponse
		if _, err := c.doJSON(ctx, http.MethodGet, c.paths.Logs, query, nil, &response); err != nil {
			return nil, err
		}
		reachedEnd := false
		for _, log := range response.Logs {
			// 双保险：如果 API 返回了 end_time 之后的日志，这里过滤掉。
			if !windowEnd.IsZero() && windowEnd.After(windowStart) && !log.Timestamp.IsZero() && !log.Timestamp.Before(windowEnd) {
				reachedEnd = true
				continue
			}
			out = append(out, log)
		}
		// 以下三个条件任意满足，都说明分页可以停止。
		if reachedEnd {
			break
		}
		if len(response.Logs) < limit {
			break
		}
		if response.Pagination.TotalCount > 0 && int64(offset+limit) >= response.Pagination.TotalCount {
			break
		}
	}
	return out, nil
}

// setProviderKeyEnabled 启用或禁用单个 provider key。
func (c *BifrostClient) setProviderKeyEnabled(ctx context.Context, provider, keyID string, enabled bool) error {
	path := c.paths.ProviderKey
	// ProviderKey 路径里有占位符，这里替换成真实 provider 和 key id。
	path = strings.ReplaceAll(path, "{provider}", url.PathEscape(provider))
	path = strings.ReplaceAll(path, "{key_id}", url.PathEscape(keyID))
	payload := map[string]bool{"enabled": enabled}
	var response json.RawMessage
	if _, err := c.doJSON(ctx, http.MethodPut, path, nil, payload, &response); err != nil {
		return err
	}
	return nil
}

// doJSON 是所有 HTTP JSON 请求的公共 helper。
//
// 它负责：
//   - 拼 URL。
//   - 把 body 编码成 JSON。
//   - 加 Authorization 或 Cookie。
//   - 发送请求。
//   - 检查 HTTP 状态码。
//   - 把响应 JSON 解析到 out。
func (c *BifrostClient) doJSON(ctx context.Context, method, path string, query url.Values, body any, out any) (*http.Response, error) {
	// any 是 Go 1.18 后的写法，等价于 interface{}。
	// 它表示“任意类型”。
	requestURL := c.resolve(path, query)

	var reader io.Reader
	if body != nil {
		// json.Marshal 把 Go 对象转成 JSON 字节。
		data, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshal request: %w", err)
		}
		// bytes.NewReader 把 []byte 包装成 io.Reader，给 HTTP 请求体使用。
		reader = bytes.NewReader(data)
	}

	// NewRequestWithContext 创建带 ctx 的 HTTP 请求。
	// ctx 取消时，请求也会被取消。
	req, err := http.NewRequestWithContext(ctx, method, requestURL, reader)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	// 优先使用 Bearer token。
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	// 如果登录拿到的是 cookie，就把 cookie 加到请求里。
	for _, cookie := range c.cookies {
		req.AddCookie(cookie)
	}

	// 真正发 HTTP 请求。
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, requestURL, err)
	}
	// 函数返回前关闭响应体，避免连接泄漏。
	defer resp.Body.Close()

	// 限制最大响应体大小，避免异常响应占满内存。
	data, err := readLimited(resp.Body, maxAPIResponseBytes)
	if err != nil {
		return resp, fmt.Errorf("read response: %w", err)
	}
	// 2xx 以外都当作 API 错误。
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp, apiError(resp.StatusCode, data)
	}
	// out 为 nil 或响应为空时，不解析 JSON。
	if out == nil || len(data) == 0 {
		return resp, nil
	}
	// json.Unmarshal 把响应 JSON 填进 out。
	if err := json.Unmarshal(data, out); err != nil {
		return resp, fmt.Errorf("decode %s %s response: %w", method, requestURL, err)
	}
	return resp, nil
}

// maxAPIResponseBytes 是允许读取的最大响应体大小。
//
// 64 << 20 等于 64 MiB。
const maxAPIResponseBytes = 64 << 20

// readLimited 读取响应体，但最多允许 limit 字节。
func readLimited(r io.Reader, limit int64) ([]byte, error) {
	// LimitReader 最多读 limit+1 字节。
	// 多读 1 字节是为了判断是否超过限制。
	limited := io.LimitReader(r, limit+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("response body exceeded %d bytes", limit)
	}
	return data, nil
}

// resolve 把 baseURL、path、query 拼成完整请求 URL。
func (c *BifrostClient) resolve(path string, query url.Values) string {
	// u := *c.baseURL 是复制一份 URL 值，避免修改 c.baseURL 本身。
	u := *c.baseURL
	basePath := strings.TrimRight(u.Path, "/")
	child := "/" + strings.TrimLeft(path, "/")
	u.Path = basePath + child
	u.RawQuery = query.Encode()
	return u.String()
}

// apiError 尽量从 Bifrost 错误响应里提取可读错误信息。
func apiError(status int, data []byte) error {
	message := strings.TrimSpace(string(data))
	// parsed 是临时匿名结构体，只在这个函数里使用。
	var parsed struct {
		Error struct {
			Message string `json:"message"`
			Code    string `json:"code"`
			Type    string `json:"type"`
		} `json:"error"`
		Type string `json:"type"`
	}
	// 如果响应是 JSON，就优先使用 error.message / error.code / type。
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

// joinPath 拼接两个 URL path，避免出现双斜杠或少斜杠。
func joinPath(base, child string) string {
	return strings.TrimRight(base, "/") + "/" + strings.TrimLeft(child, "/")
}

// listVirtualKeysResponse 对应 Bifrost 列表 VK 接口的响应。
type listVirtualKeysResponse struct {
	// VirtualKeys 是搜索接口返回的 VK 列表。
	VirtualKeys []virtualKey `json:"virtual_keys"`
}

// loginRequest 是登录接口请求体。
type loginRequest struct {
	// Username 对应 JSON 里的 username。
	Username string `json:"username"`
	// Password 对应 JSON 里的 password。
	Password string `json:"password"`
}

// loginResponse 是登录接口响应体里调度器关心的部分。
type loginResponse struct {
	// Token 是登录成功后的 bearer token；有些部署可能不返回它。
	Token string `json:"token"`
}

// getVirtualKeyResponse 对应读取单个 VK 的响应。
type getVirtualKeyResponse struct {
	// VirtualKey 是响应里的 VK 详情。
	VirtualKey virtualKey `json:"virtual_key"`
}

// virtualKeyResponse 对应更新 VK 后的响应。
type virtualKeyResponse struct {
	// VirtualKey 是更新后的 VK 详情。
	VirtualKey virtualKey `json:"virtual_key"`
}

// virtualKey 是 Bifrost Virtual Key 的 API 结构。
//
// 这里只定义调度器需要读写的字段。
type virtualKey struct {
	// ID 是 Bifrost 内部 VK id，更新接口需要它。
	ID string `json:"id"`
	// Name 是 VK 名称。
	Name string `json:"name"`
	// Description 是 VK 描述。
	Description string `json:"description,omitempty"`
	// IsActive 表示 VK 是否启用。
	IsActive bool `json:"is_active"`
	// ProviderConfigs 是这个 VK 绑定的 provider 配置列表。
	ProviderConfigs []virtualProviderConfig `json:"provider_configs"`
	// TeamID / CustomerID 是 Bifrost 可能返回的归属字段，PUT 时要尽量保留。
	TeamID     string `json:"team_id,omitempty"`
	CustomerID string `json:"customer_id,omitempty"`
	// CalendarAligned 是 Bifrost VK 的计费/周期设置字段，PUT 时保留。
	CalendarAligned bool `json:"calendar_aligned"`
}

// virtualProviderConfig 是 Bifrost VK 里每个 provider config 的 API 结构。
type virtualProviderConfig struct {
	// ID 是 provider config id。
	ID int `json:"id"`
	// VirtualKeyID 是它所属的 VK id。
	VirtualKeyID string `json:"virtual_key_id"`
	// Provider 是 provider 名称。
	Provider string `json:"provider"`
	// Weight 是权重指针。用指针是为了区分“接口没返回”和“返回 0”。
	Weight *float64 `json:"weight"`
	// AllowedModels / BlacklistedModels 是模型白名单/黑名单。
	AllowedModels     []string `json:"allowed_models"`
	BlacklistedModels []string `json:"blacklisted_models"`
	// AllowAllKeys 表示这个 provider 是否使用全部 key。
	AllowAllKeys bool `json:"allow_all_keys"`
	// Keys 是 VK 当前选择或返回的 key 列表。
	Keys []providerKey `json:"keys"`
}

// providerKey 是 Bifrost provider key 在 VK 里的绑定信息。
type providerKey struct {
	// ID 是数字 id。
	ID int `json:"id"`
	// KeyID 是字符串 key id，更新 key 状态时优先用它。
	KeyID string `json:"key_id"`
	// Name 是 key 名称。
	Name string `json:"name"`
	// Enabled 是是否启用。指针 nil 表示接口没返回这个字段。
	Enabled *bool `json:"enabled"`
	// Weight / Models 是 Bifrost 可能返回的附加字段。
	Weight float64  `json:"weight,omitempty"`
	Models []string `json:"models,omitempty"`
}

// keyIDs 返回 provider config 绑定的 key id 列表。
//
// Bifrost 响应里可能同时有数字 id 和字符串 key_id。
// 调度器优先使用 key_id；没有 key_id 时再用数字 id 转字符串兜底。
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

// enabledKeyCount 统计当前启用的 key 数量。
//
// AllowAllKeys 且没有具体 keys 时，返回 0。
// 这里的 0 不是“没有 key 可用”，而是“Bifrost 没返回具体 key 列表”。
func (p virtualProviderConfig) enabledKeyCount() int {
	if p.AllowAllKeys && len(p.Keys) == 0 {
		return 0
	}
	count := 0
	for _, key := range p.Keys {
		// Enabled 为 nil 时，按启用处理。
		if key.Enabled == nil || *key.Enabled {
			count++
		}
	}
	return count
}

// toUpdateRequest 把读取到的 provider config 转成 PUT 更新接口需要的结构。
//
// 重要：这里必须保留 AllowAllKeys、KeyIDs、SelectedKeyIDs。
// 否则只改权重时可能误伤线上 key 策略。
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

// updateVirtualKeyRequest 是更新 VK 时提交给 Bifrost 的请求体。
type updateVirtualKeyRequest struct {
	// Name / Description 保留 VK 原有基础信息。
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	// ProviderConfigs 是完整 provider config 列表，不只是被修改的那个。
	ProviderConfigs []updateProviderConfigRequest `json:"provider_configs"`
	// TeamID / CustomerID / IsActive / CalendarAligned 都要保留，避免 PUT 时被清空。
	TeamID          string `json:"team_id,omitempty"`
	CustomerID      string `json:"customer_id,omitempty"`
	IsActive        bool   `json:"is_active"`
	CalendarAligned bool   `json:"calendar_aligned"`
}

// updateProviderConfigRequest 是更新 VK 时每个 provider config 的请求结构。
type updateProviderConfigRequest struct {
	// ID 是已有 provider config 的 id。
	ID int `json:"id,omitempty"`
	// Provider 是 provider 名称。
	Provider string `json:"provider,omitempty"`
	// Weight 是要写回的权重。
	Weight *float64 `json:"weight"`
	// AllowedModels / BlacklistedModels 保留原模型规则。
	AllowedModels     []string `json:"allowed_models,omitempty"`
	BlacklistedModels []string `json:"blacklisted_models,omitempty"`
	// AllowAllKeys 保留 key 策略。
	AllowAllKeys bool `json:"allow_all_keys"`
	// KeyIDs / SelectedKeyIDs 保留当前选择的 key。
	KeyIDs         []string `json:"key_ids,omitempty"`
	SelectedKeyIDs []string `json:"selected_key_ids,omitempty"`
}

// searchLogsResponse 对应 Bifrost 日志搜索接口响应。
type searchLogsResponse struct {
	// Logs 是当前页日志。
	Logs       []logEntry `json:"logs"`
	Pagination struct {
		// TotalCount 是总日志数量，用来判断分页是否结束。
		TotalCount int64 `json:"total_count"`
	} `json:"pagination"`
}

// logEntry 是单条调用日志里调度器关心的字段。
type logEntry struct {
	// Provider 是这条请求实际命中的 provider。
	Provider string `json:"provider"`
	// Status 是 success 或 error。
	Status string `json:"status"`
	// Timestamp 是日志时间。
	Timestamp time.Time `json:"timestamp"`
	// Latency 是延迟毫秒。
	Latency float64 `json:"latency"`
	// ErrorDetails 是原始错误 JSON。
	ErrorDetails json.RawMessage `json:"error_details"`
	// VirtualKeyName 是日志里记录的 VK 名称。
	VirtualKeyName string `json:"virtual_key_name"`
}

// aggregateMetrics 把原始日志聚合成每个 provider 的指标。
//
// 输入是 Bifrost 日志，输出是领域层 ProviderMetric。
// 这里会同时生成总窗口指标和小窗口指标。
func aggregateMetrics(pool domain.PoolConfig, logs []logEntry, windowStart, windowEnd time.Time, windowDuration time.Duration) []domain.ProviderMetric {
	// 防御式默认值：如果调用方没传窗口长度，就按 15 分钟。
	if windowDuration <= 0 {
		windowDuration = 15 * time.Minute
	}
	// windowEnd 不合法时，至少保证有一个窗口。
	if windowEnd.IsZero() || !windowEnd.After(windowStart) {
		windowEnd = windowStart.Add(windowDuration)
	}

	// byProvider 保存整个大窗口里的累计指标。
	byProvider := map[string]*metricAccumulator{}
	// byProviderWindow 保存每个 provider 的每个小窗口累计指标。
	byProviderWindow := map[string][]*metricAccumulator{}
	for _, log := range logs {
		// 没有 provider 名称的日志无法归类，跳过。
		if log.Provider == "" {
			continue
		}
		// 过滤窗口外日志。
		if !log.Timestamp.IsZero() && (log.Timestamp.Before(windowStart) || !log.Timestamp.Before(windowEnd)) {
			continue
		}
		// 取出 provider 的累计器；没有就新建。
		acc := byProvider[log.Provider]
		if acc == nil {
			acc = &metricAccumulator{}
			byProvider[log.Provider] = acc
		}
		acc.add(log)

		// 算出这条日志属于第几个小窗口。
		idx := windowIndex(log.Timestamp, windowStart, windowDuration)
		if idx < 0 {
			continue
		}
		windows := byProviderWindow[log.Provider]
		// 确保 windows 切片长度足够放 idx。
		for len(windows) <= idx {
			windows = append(windows, &metricAccumulator{})
		}
		windows[idx].add(log)
		byProviderWindow[log.Provider] = windows
	}

	// names 收集所有需要输出的 provider 名称：
	// 配置里有的要输出，日志里出现但配置没写的也要输出。
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
	// 排序让输出稳定，方便测试和人看 diff。
	sort.Strings(providerNames)

	out := make([]domain.ProviderMetric, 0, len(providerNames))
	for _, provider := range providerNames {
		acc := byProvider[provider]
		// 没有日志的 provider 也输出一行零指标。
		if acc == nil {
			out = append(out, domain.ProviderMetric{
				PoolID:     pool.ID,
				VirtualKey: pool.VirtualKey,
				Provider:   provider,
				Windows:    emptyWindows(windowStart, windowEnd, windowDuration),
			})
			continue
		}
		// 有日志就把累计器转换成 ProviderMetric。
		metric := acc.metric(pool, provider)
		metric.Windows = windowMetrics(byProviderWindow[provider], windowStart, windowEnd, windowDuration)
		out = append(out, metric)
	}
	return out
}

// windowIndex 计算某个时间戳属于第几个小窗口。
func windowIndex(ts, windowStart time.Time, windowDuration time.Duration) int {
	if ts.IsZero() || windowDuration <= 0 || ts.Before(windowStart) {
		return -1
	}
	// ts.Sub(windowStart) 得到时间差，再除以窗口长度得到下标。
	return int(ts.Sub(windowStart) / windowDuration)
}

// emptyWindows 根据开始/结束时间生成空的小窗口列表。
func emptyWindows(windowStart, windowEnd time.Time, windowDuration time.Duration) []domain.WindowMetric {
	if windowDuration <= 0 || !windowEnd.After(windowStart) {
		return nil
	}
	// math.Ceil 向上取整，确保最后一个不足 windowDuration 的窗口也被保留。
	count := int(math.Ceil(float64(windowEnd.Sub(windowStart)) / float64(windowDuration)))
	out := make([]domain.WindowMetric, 0, count)
	for i := 0; i < count; i++ {
		start := windowStart.Add(time.Duration(i) * windowDuration)
		end := start.Add(windowDuration)
		if end.After(windowEnd) {
			end = windowEnd
		}
		out = append(out, domain.WindowMetric{Start: start, End: end})
	}
	return out
}

// windowMetrics 把每个小窗口的累计器转换成 WindowMetric。
func windowMetrics(accumulators []*metricAccumulator, windowStart, windowEnd time.Time, windowDuration time.Duration) []domain.WindowMetric {
	windows := emptyWindows(windowStart, windowEnd, windowDuration)
	for i, acc := range accumulators {
		if i >= len(windows) || acc == nil {
			continue
		}
		windows[i] = acc.windowMetric(windows[i].Start, windows[i].End)
	}
	return windows
}

// zeroMetrics 给没有日志或没有 VK 的 provider 生成零指标。
func zeroMetrics(pool domain.PoolConfig) []domain.ProviderMetric {
	out := make([]domain.ProviderMetric, 0, len(pool.Providers))
	for _, provider := range pool.Providers {
		out = append(out, domain.ProviderMetric{
			PoolID:     pool.ID,
			VirtualKey: pool.VirtualKey,
			Provider:   provider.Name,
		})
	}
	return out
}

// metricAccumulator 是内部累计器。
//
// 它一条条接收 logEntry，最后再生成 ProviderMetric 或 WindowMetric。
type metricAccumulator struct {
	total               int
	success             int
	errors              int
	latencies           []float64
	timeoutOrStreamIdle int
	criticalErrors      int
	ignoredErrors       int
	lastSeenAt          *time.Time
	errorFamilies       map[string]bool
	ignoredFamilies     map[string]bool
}

// add 把一条日志计入累计器。
func (a *metricAccumulator) add(log logEntry) {
	// EqualFold 是不区分大小写比较。
	if strings.EqualFold(log.Status, "success") {
		a.total++
		a.success++
		// 只有成功请求的 latency 才参与 P95。
		if log.Latency > 0 {
			a.latencies = append(a.latencies, log.Latency)
		}
	} else if strings.EqualFold(log.Status, "error") {
		// 先把错误文本归类。
		category := categorizeError(log.ErrorDetails)
		// 用户请求错误不计入 provider 错误率。
		if ignoredErrorFamily(category) {
			a.ignoredErrors++
			if a.ignoredFamilies == nil {
				a.ignoredFamilies = map[string]bool{}
			}
			a.ignoredFamilies[category] = true
			if !log.Timestamp.IsZero() && (a.lastSeenAt == nil || log.Timestamp.After(*a.lastSeenAt)) {
				seen := log.Timestamp
				a.lastSeenAt = &seen
			}
			return
		}
		// 非忽略错误才计入 total/errors。
		a.total++
		a.errors++
		if a.errorFamilies == nil {
			a.errorFamilies = map[string]bool{}
		}
		a.errorFamilies[category] = true
		if category == "timeout" || category == "stream_idle_timeout" {
			a.timeoutOrStreamIdle++
		}
		// 凭证、额度、无 token 这类属于关键错误。
		if category == "credentials_exhausted" || category == "quota_or_no_token" {
			a.criticalErrors++
		}
	}
	// 记录最后一次看到这个 provider 的时间。
	if !log.Timestamp.IsZero() && (a.lastSeenAt == nil || log.Timestamp.After(*a.lastSeenAt)) {
		seen := log.Timestamp
		a.lastSeenAt = &seen
	}
}

// metric 把累计器转换成 ProviderMetric。
func (a *metricAccumulator) metric(pool domain.PoolConfig, provider string) domain.ProviderMetric {
	errorRate := 0.0
	successRate := 0.0
	// 防止 total 为 0 时除以 0。
	if a.total > 0 {
		errorRate = float64(a.errors) / float64(a.total)
		successRate = float64(a.success) / float64(a.total)
	}
	return domain.ProviderMetric{
		PoolID:               pool.ID,
		VirtualKey:           pool.VirtualKey,
		Provider:             provider,
		Total:                a.total,
		Success:              a.success,
		Errors:               a.errors,
		ErrorRate:            errorRate,
		SuccessRate:          successRate,
		P95LatencyMS:         percentile(a.latencies, 0.95),
		TimeoutOrStreamIdle:  a.timeoutOrStreamIdle,
		CriticalErrors:       a.criticalErrors,
		IgnoredErrors:        a.ignoredErrors,
		LastSeenAt:           a.lastSeenAt,
		ErrorFamilies:        sortedFamilies(a.errorFamilies),
		IgnoredErrorFamilies: sortedFamilies(a.ignoredFamilies),
	}
}

// windowMetric 把累计器转换成某个小窗口的 WindowMetric。
func (a *metricAccumulator) windowMetric(start, end time.Time) domain.WindowMetric {
	errorRate := 0.0
	successRate := 0.0
	if a.total > 0 {
		errorRate = float64(a.errors) / float64(a.total)
		successRate = float64(a.success) / float64(a.total)
	}
	return domain.WindowMetric{
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
		IgnoredErrors:       a.ignoredErrors,
	}
}

// categorizeError 把 Bifrost error_details 归类成调度器认识的错误家族。
//
// 这里故意用字符串包含判断，因为不同上游返回的错误 JSON 结构不完全一致。
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
	case strings.Contains(text, "instructions are required"),
		strings.Contains(text, "unsupported_value"),
		strings.Contains(text, "invalid_request_error"),
		strings.Contains(text, "permission_error"),
		strings.Contains(text, "request cancelled: client disconnected"):
		return "client_request_error"
	case strings.Contains(text, "no available channel for model"),
		strings.Contains(text, "model_not_found"):
		return "model_unavailable"
	case strings.Contains(text, "client disconnected"):
		return "client_disconnected"
	default:
		return "other"
	}
}

// ignoredErrorFamily 判断某类错误是否应该从 provider 错误率里忽略。
//
// 例如用户请求参数错误，不代表 provider 坏了。
func ignoredErrorFamily(family string) bool {
	switch family {
	case "client_request_error", "client_disconnected", "image_group_disabled":
		return true
	default:
		return false
	}
}

// percentile 计算分位数。
//
// p=0.95 表示 P95。
// 返回 *float64 是为了能用 nil 表示“没有样本，无法计算”。
func percentile(values []float64, p float64) *float64 {
	if len(values) == 0 {
		return nil
	}
	// append([]float64(nil), values...) 会复制一份切片。
	// 这样排序不会改动原始 values。
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
	// 返回局部变量地址在 Go 里是安全的，编译器会把它放到合适的位置。
	return &value
}

// sortedFamilies 把 map 里的错误家族名转成排序后的切片。
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

// cloneStrings 复制字符串切片。
//
// 这样构造 PUT 请求时，不会意外共享原切片底层数组。
func cloneStrings(values []string) []string {
	if values == nil {
		return nil
	}
	return append([]string(nil), values...)
}
