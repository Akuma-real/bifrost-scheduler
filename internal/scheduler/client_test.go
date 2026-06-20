package scheduler

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestLoginUsesSessionCookieWhenTokenIsMissing(t *testing.T) {
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
			resp := jsonResponse(t, map[string]any{"message": "Login successful"})
			resp.Header.Add("Set-Cookie", "bifrost_session=session-cookie; Path=/; HttpOnly")
			return resp, nil
		case r.Method == http.MethodGet && r.URL.Path == "/api/governance/virtual-keys":
			if got := r.Header.Get("Cookie"); !strings.Contains(got, "bifrost_session=session-cookie") {
				t.Fatalf("Cookie header = %q", got)
			}
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
	if _, err := client.LoadProviderStates(context.Background(), []PoolConfig{testPool()}); err != nil {
		t.Fatalf("LoadProviderStates returned error: %v", err)
	}
}

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
			var payload loginRequest
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatalf("decode login payload: %v", err)
			}
			if payload.Username != "admin" || payload.Password != "password" {
				t.Fatalf("login payload = %+v", payload)
			}
			return jsonResponse(t, map[string]any{"token": "session-token"}), nil
		case r.Method == http.MethodGet && r.URL.Path == "/api/governance/virtual-keys":
			if got := r.Header.Get("Authorization"); got != "Bearer session-token" {
				t.Fatalf("Authorization header = %q", got)
			}
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
			if r.URL.Query().Get("virtual_key_ids") != "vk-1" {
				t.Fatalf("virtual_key_ids query = %q", r.URL.Query().Get("virtual_key_ids"))
			}
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
				},
				"pagination": map[string]any{"total_count": 3},
			}), nil
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.String())
		}
		return nil, nil
	})}
	if err := client.Login(context.Background()); err != nil {
		t.Fatalf("Login returned error: %v", err)
	}
	pools := []PoolConfig{testPool()}

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
	metricA := metricFor(metrics, "provider_a")
	if metricA.Total != 2 || metricA.Success != 1 || metricA.Errors != 1 || metricA.TimeoutOrStreamIdle != 1 {
		t.Fatalf("provider_a metric = %+v", metricA)
	}
	metricB := metricFor(metrics, "provider_b")
	if metricB.Total != 1 || metricB.CriticalErrors != 1 || !contains(metricB.ErrorFamilies, "quota_or_no_token") {
		t.Fatalf("provider_b metric = %+v", metricB)
	}
	if len(metricA.Windows) != 2 {
		t.Fatalf("provider_a windows = %+v, want 2 windows", metricA.Windows)
	}
}

func TestSetProviderWeightPreservesOtherProviderConfigs(t *testing.T) {
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
			if got := r.Header.Get("Authorization"); got != "Bearer session-token" {
				t.Fatalf("Authorization header = %q", got)
			}
			return jsonResponse(t, map[string]any{"virtual_key": testVirtualKey()}), nil
		case r.Method == http.MethodPut && r.URL.Path == "/api/governance/virtual-keys/vk-1":
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

	err = client.SetProviderWeight(context.Background(), PoolProviderState{
		VirtualKeyID:     "vk-1",
		ProviderConfigID: 11,
		Provider:         "provider_a",
	}, 0.25)
	if err != nil {
		t.Fatalf("SetProviderWeight returned error: %v", err)
	}

	if putPayload.Name != "vk_low_text" || len(putPayload.ProviderConfigs) != 2 {
		t.Fatalf("PUT payload = %+v", putPayload)
	}
	first := putPayload.ProviderConfigs[0]
	if first.ID != 11 || first.Provider != "provider_a" || first.Weight == nil || *first.Weight != 0.25 {
		t.Fatalf("first provider config = %+v", first)
	}
	if len(first.KeyIDs) != 1 || first.KeyIDs[0] != "key-a" {
		t.Fatalf("first key ids = %+v", first.KeyIDs)
	}
	if len(first.SelectedKeyIDs) != 1 || first.SelectedKeyIDs[0] != "key-a" {
		t.Fatalf("first selected key ids = %+v", first.SelectedKeyIDs)
	}
	if first.AllowAllKeys {
		t.Fatalf("first allow_all_keys = true, want false")
	}
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

func testPool() PoolConfig {
	return PoolConfig{
		ID:         "gpt_low",
		VirtualKey: "vk_low_text",
		Providers: []ProviderConfig{
			{Name: "provider_a", CostWeight: 0.8},
			{Name: "provider_b", CostWeight: 0.2},
		},
	}
}

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

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func jsonResponse(t *testing.T, value any) *http.Response {
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

func metricFor(metrics []ProviderMetric, provider string) ProviderMetric {
	for _, metric := range metrics {
		if metric.Provider == provider {
			return metric
		}
	}
	return ProviderMetric{}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
