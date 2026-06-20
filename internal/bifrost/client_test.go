// package bifrost 表示这个测试文件属于 Bifrost API 适配层。
package bifrost

// import 是测试要用到的包。
//
// context：创建测试请求上下文。
// encoding/json：解析和构造 JSON。
// io：构造 HTTP 响应体。
// net/http、net/url：模拟 HTTP 请求和响应。
// strings：检查请求头、错误文本。
// testing：Go 标准测试包。
// time：构造日志时间窗口。
// domain：领域层数据结构。
import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	domain "github.com/Akuma-real/bifrost-scheduler/internal/domain/scheduler"
)

// TestLoginUsesSessionCookieWhenTokenIsMissing 验证登录接口没有 token 时，客户端会使用 session cookie。
func TestLoginUsesSessionCookieWhenTokenIsMissing(t *testing.T) {
	client, err := NewBifrostClient(ClientOptions{
		BaseURL:  "http://bifrost.local",
		Username: "admin",
		Password: "password",
	})
	if err != nil {
		t.Fatalf("NewBifrostClient returned error: %v", err)
	}
	// 用假的 Transport 替换真实网络。
	// 这样测试不会访问外网，也不会碰线上 Bifrost。
	client.http = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/session/login":
			// 模拟 Bifrost 登录成功，但只返回 Set-Cookie，不返回 token。
			resp := jsonResponse(t, map[string]any{"message": "Login successful"})
			resp.Header.Add("Set-Cookie", "bifrost_session=session-cookie; Path=/; HttpOnly")
			return resp, nil
		case r.Method == http.MethodGet && r.URL.Path == "/api/governance/virtual-keys":
			// 后续请求必须带 Cookie。
			if got := r.Header.Get("Cookie"); !strings.Contains(got, "bifrost_session=session-cookie") {
				t.Fatalf("Cookie header = %q", got)
			}
			// 因为没有 token，所以 Authorization 必须为空。
			if got := r.Header.Get("Authorization"); got != "" {
				t.Fatalf("Authorization header = %q, want empty", got)
			}
			return jsonResponse(t, map[string]any{"virtual_keys": []any{testVirtualKey()}}), nil
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.String())
		}
		return nil, nil
	})}

	if err := client.Login(context.Background()); err != nil {
		t.Fatalf("Login returned error: %v", err)
	}
	// 调用 LoadProviderStates 是为了验证登录后 cookie 会带到下一次请求里。
	if _, err := client.LoadProviderStates(context.Background(), []domain.PoolConfig{testPool()}); err != nil {
		t.Fatalf("LoadProviderStates returned error: %v", err)
	}
}

// TestClientLoadsProviderStatesAndMetrics 验证客户端能读取 provider 状态，并从日志聚合指标。
func TestClientLoadsProviderStatesAndMetrics(t *testing.T) {
	client, err := NewBifrostClient(ClientOptions{
		BaseURL:  "http://bifrost.local",
		Username: "admin",
		Password: "password",
	})
	if err != nil {
		t.Fatalf("NewBifrostClient returned error: %v", err)
	}
	client.http = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/session/login":
			// 检查登录请求体确实带了用户名密码。
			var payload loginRequest
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatalf("decode login payload: %v", err)
			}
			if payload.Username != "admin" || payload.Password != "password" {
				t.Fatalf("login payload = %+v", payload)
			}
			return jsonResponse(t, map[string]any{"token": "session-token"}), nil
		case r.Method == http.MethodGet && r.URL.Path == "/api/governance/virtual-keys":
			// token 登录后，后续请求应该带 Authorization。
			if got := r.Header.Get("Authorization"); got != "Bearer session-token" {
				t.Fatalf("Authorization header = %q", got)
			}
			// 搜索 VK 时应该按 virtual_key 名称查。
			if r.URL.Query().Get("search") != "vk_low_text" {
				t.Fatalf("search query = %q", r.URL.Query().Get("search"))
			}
			return jsonResponse(t, map[string]any{
				"virtual_keys": []any{testVirtualKey()},
			}), nil
		case r.Method == http.MethodGet && r.URL.Path == "/api/logs":
			if got := r.Header.Get("Authorization"); got != "Bearer session-token" {
				t.Fatalf("Authorization header = %q", got)
			}
			// 日志查询必须绑定 VK id。
			if r.URL.Query().Get("virtual_key_ids") != "vk-1" {
				t.Fatalf("virtual_key_ids query = %q", r.URL.Query().Get("virtual_key_ids"))
			}
			if got := r.URL.Query().Get("limit"); got != "100" {
				t.Fatalf("limit query = %q, want 100", got)
			}
			if got := r.URL.Query().Get("end_time"); got == "" {
				t.Fatalf("end_time query is empty")
			}
			// 构造 4 条日志：
			// provider_a：1 成功、1 provider 错误、1 用户侧错误。
			// provider_b：1 关键错误。
			return jsonResponse(t, map[string]any{
				"logs": []any{
					map[string]any{
						"provider":      "provider_a",
						"status":        "success",
						"timestamp":     "2026-06-19T01:00:00Z",
						"latency":       1200,
						"error_details": nil,
					},
					map[string]any{
						"provider":  "provider_a",
						"status":    "error",
						"timestamp": "2026-06-19T01:01:00Z",
						"latency":   0,
						"error_details": map[string]any{
							"error": map[string]any{"message": "stream idle timeout"},
						},
					},
					map[string]any{
						"provider":  "provider_b",
						"status":    "error",
						"timestamp": "2026-06-19T01:02:00Z",
						"latency":   0,
						"error_details": map[string]any{
							"error": map[string]any{"message": "insufficient_user_quota"},
						},
					},
					map[string]any{
						"provider":  "provider_a",
						"status":    "error",
						"timestamp": "2026-06-19T01:03:00Z",
						"latency":   0,
						"error_details": map[string]any{
							"error": map[string]any{"type": "invalid_request_error", "message": "Instructions are required"},
						},
					},
				},
				"pagination": map[string]any{"total_count": 4},
			}), nil
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.String())
		}
		return nil, nil
	})}
	if err := client.Login(context.Background()); err != nil {
		t.Fatalf("Login returned error: %v", err)
	}
	pools := []domain.PoolConfig{testPool()}

	// 先验证当前状态转换。
	states, err := client.LoadProviderStates(context.Background(), pools)
	if err != nil {
		t.Fatalf("LoadProviderStates returned error: %v", err)
	}
	if len(states) != 2 {
		t.Fatalf("len(states) = %d, want 2", len(states))
	}
	if states[0].Provider != "provider_a" || states[0].CurrentWeight != 0.8 || states[0].EnabledKeyCount != 1 {
		t.Fatalf("state[0] = %+v", states[0])
	}
	if states[1].Provider != "provider_b" || states[1].CurrentWeight != 0.2 || states[1].EnabledKeyCount != 0 {
		t.Fatalf("state[1] = %+v", states[1])
	}

	// 再验证日志聚合指标。
	metrics, err := client.LoadMetrics(
		context.Background(),
		pools,
		time.Date(2026, 6, 19, 0, 45, 0, 0, time.UTC),
		time.Date(2026, 6, 19, 1, 15, 0, 0, time.UTC),
		15*time.Minute,
	)
	if err != nil {
		t.Fatalf("LoadMetrics returned error: %v", err)
	}
	// provider_a 的用户侧 invalid_request_error 应被忽略。
	metricA := metricFor(metrics, "provider_a")
	if metricA.Total != 2 || metricA.Success != 1 || metricA.Errors != 1 || metricA.IgnoredErrors != 1 || metricA.TimeoutOrStreamIdle != 1 {
		t.Fatalf("provider_a metric = %+v", metricA)
	}
	if !contains(metricA.IgnoredErrorFamilies, "client_request_error") {
		t.Fatalf("provider_a ignored families = %+v, want client_request_error", metricA.IgnoredErrorFamilies)
	}
	metricB := metricFor(metrics, "provider_b")
	if metricB.Total != 1 || metricB.CriticalErrors != 1 || !contains(metricB.ErrorFamilies, "quota_or_no_token") {
		t.Fatalf("provider_b metric = %+v", metricB)
	}
	if len(metricA.Windows) != 2 {
		t.Fatalf("provider_a windows = %+v, want 2 windows", metricA.Windows)
	}
}

// TestReadLimitedRejectsOversizedResponse 验证响应体过大时会报错。
func TestReadLimitedRejectsOversizedResponse(t *testing.T) {
	_, err := readLimited(strings.NewReader("123456"), 5)
	if err == nil || !strings.Contains(err.Error(), "response body exceeded 5 bytes") {
		t.Fatalf("readLimited error = %v, want size error", err)
	}
}

// TestSetProviderWeightPreservesOtherProviderConfigs 验证只改权重时，不会丢掉其他 provider config 字段。
//
// 这个测试很重要，因为之前线上出现过 key 策略被误改成空的问题。
func TestSetProviderWeightPreservesOtherProviderConfigs(t *testing.T) {
	// putPayload 用来接住客户端 PUT 给 Bifrost 的请求体，后面检查它是否正确。
	var putPayload updateVirtualKeyRequest
	client, err := NewBifrostClient(ClientOptions{
		BaseURL:  "http://bifrost.local",
		Username: "admin",
		Password: "password",
	})
	if err != nil {
		t.Fatalf("NewBifrostClient returned error: %v", err)
	}
	client.http = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/session/login":
			return jsonResponse(t, map[string]any{"token": "session-token"}), nil
		case r.Method == http.MethodGet && r.URL.Path == "/api/governance/virtual-keys/vk-1":
			// SetProviderWeight 会先 GET 当前 VK。
			if got := r.Header.Get("Authorization"); got != "Bearer session-token" {
				t.Fatalf("Authorization header = %q", got)
			}
			return jsonResponse(t, map[string]any{"virtual_key": testVirtualKey()}), nil
		case r.Method == http.MethodPut && r.URL.Path == "/api/governance/virtual-keys/vk-1":
			// 然后 PUT 修改后的 VK。
			if got := r.Header.Get("Authorization"); got != "Bearer session-token" {
				t.Fatalf("Authorization header = %q", got)
			}
			if err := json.NewDecoder(r.Body).Decode(&putPayload); err != nil {
				t.Fatalf("decode PUT payload: %v", err)
			}
			return jsonResponse(t, map[string]any{"virtual_key": testVirtualKey()}), nil
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.String())
		}
		return nil, nil
	})}
	if err := client.Login(context.Background()); err != nil {
		t.Fatalf("Login returned error: %v", err)
	}

	err = client.SetProviderWeight(context.Background(), domain.PoolProviderState{
		VirtualKeyID:     "vk-1",
		ProviderConfigID: 11,
		Provider:         "provider_a",
	}, 0.25)
	if err != nil {
		t.Fatalf("SetProviderWeight returned error: %v", err)
	}

	// 确认请求体保留了 VK 名称和两个 provider configs。
	if putPayload.Name != "vk_low_text" || len(putPayload.ProviderConfigs) != 2 {
		t.Fatalf("PUT payload = %+v", putPayload)
	}
	// 第一个 provider 是目标 provider，权重应该被改成 0.25。
	first := putPayload.ProviderConfigs[0]
	if first.ID != 11 || first.Provider != "provider_a" || first.Weight == nil || *first.Weight != 0.25 {
		t.Fatalf("first provider config = %+v", first)
	}
	// 指定 key 策略必须被保留。
	if len(first.KeyIDs) != 1 || first.KeyIDs[0] != "key-a" {
		t.Fatalf("first key ids = %+v", first.KeyIDs)
	}
	if len(first.SelectedKeyIDs) != 1 || first.SelectedKeyIDs[0] != "key-a" {
		t.Fatalf("first selected key ids = %+v", first.SelectedKeyIDs)
	}
	if first.AllowAllKeys {
		t.Fatalf("first allow_all_keys = true, want false")
	}
	// 第二个 provider 不是目标 provider，权重和 allow_all_keys 都必须保持原样。
	second := putPayload.ProviderConfigs[1]
	if second.ID != 12 || second.Provider != "provider_b" || second.Weight == nil || *second.Weight != 0.2 {
		t.Fatalf("second provider config = %+v", second)
	}
	if !second.AllowAllKeys {
		t.Fatalf("second allow_all_keys = false, want true")
	}
	if len(second.SelectedKeyIDs) != 1 || second.SelectedKeyIDs[0] != "key-b" {
		t.Fatalf("second selected key ids = %+v", second.SelectedKeyIDs)
	}
}

// testPool 返回测试用的 pool 配置。
func testPool() domain.PoolConfig {
	return domain.PoolConfig{
		ID:         "gpt_low",
		VirtualKey: "vk_low_text",
		Providers: []domain.ProviderConfig{
			{Name: "provider_a", CostWeight: 0.8},
			{Name: "provider_b", CostWeight: 0.2},
		},
	}
}

// testVirtualKey 返回测试用的 Bifrost VK JSON。
//
// 返回 map[string]any 是为了模拟真实 API JSON，而不是直接构造 Go 结构体。
func testVirtualKey() map[string]any {
	return map[string]any{
		"id":        "vk-1",
		"name":      "vk_low_text",
		"is_active": true,
		"provider_configs": []any{
			map[string]any{
				"id":                 11,
				"virtual_key_id":     "vk-1",
				"provider":           "provider_a",
				"weight":             0.8,
				"allowed_models":     []string{"gpt-5.5"},
				"blacklisted_models": []string{},
				"allow_all_keys":     false,
				"keys": []any{
					map[string]any{"id": 101, "key_id": "key-a", "name": "key a", "enabled": true},
				},
			},
			map[string]any{
				"id":                 12,
				"virtual_key_id":     "vk-1",
				"provider":           "provider_b",
				"weight":             0.2,
				"allowed_models":     []string{"gpt-5.5"},
				"blacklisted_models": []string{},
				"allow_all_keys":     true,
				"keys": []any{
					map[string]any{"id": 102, "key_id": "key-b", "name": "key b", "enabled": false},
				},
			},
		},
	}
}

// roundTripFunc 是一个函数类型。
//
// net/http 要求 Transport 实现 RoundTrip 方法。
// 我们给函数类型加一个 RoundTrip 方法，就能用普通函数模拟 HTTP。
type roundTripFunc func(*http.Request) (*http.Response, error)

// RoundTrip 让 roundTripFunc 满足 http.RoundTripper 接口。
func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

// jsonResponse 把任意 Go 值包装成一个 JSON HTTP 响应。
func jsonResponse(t *testing.T, value any) *http.Response {
	// t.Helper() 告诉 Go：如果这里失败，报错位置尽量指向调用它的测试。
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal response: %v", err)
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(string(data))),
		Request:    &http.Request{URL: &url.URL{}},
	}
}

// metricFor 从指标列表里找某个 provider 的指标。
func metricFor(metrics []domain.ProviderMetric, provider string) domain.ProviderMetric {
	for _, metric := range metrics {
		if metric.Provider == provider {
			return metric
		}
	}
	return domain.ProviderMetric{}
}

// contains 判断字符串切片里是否包含某个值。
func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
