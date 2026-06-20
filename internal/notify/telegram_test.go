// package notify 表示这个测试文件属于通知输出层。
package notify

// import 是测试需要用到的包。
//
// context：创建测试用上下文。
// encoding/json：解析 Telegram 请求体。
// io：构造假的 HTTP 响应体。
// net/http：模拟 Telegram HTTP 请求。
// strings：检查消息内容。
// testing：Go 标准测试包。
// time：构造固定时间。
// domain：构造测试用调度计划。
import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	domain "github.com/Akuma-real/bifrost-scheduler/internal/domain/scheduler"
)

// TestNewTelegramNotifierDisabledWhenEmpty 验证没有配置 Telegram 时通知器关闭。
func TestNewTelegramNotifierDisabledWhenEmpty(t *testing.T) {
	notifier, err := NewTelegramNotifier(TelegramConfig{})
	if err != nil {
		t.Fatalf("NewTelegramNotifier returned error: %v", err)
	}
	if notifier != nil {
		t.Fatalf("notifier = %+v, want nil when telegram config is empty", notifier)
	}
}

// TestNewTelegramNotifierRequiresTokenAndChatID 验证 token/chat_id 必须同时存在。
func TestNewTelegramNotifierRequiresTokenAndChatID(t *testing.T) {
	if _, err := NewTelegramNotifier(TelegramConfig{BotToken: "token"}); err == nil {
		t.Fatalf("NewTelegramNotifier returned nil error, want missing chat id error")
	}
	if _, err := NewTelegramNotifier(TelegramConfig{ChatID: "123"}); err == nil {
		t.Fatalf("NewTelegramNotifier returned nil error, want missing bot token error")
	}
}

// TestTelegramNotifierSendsPlanDecision 验证有决策时会调用 Telegram sendMessage。
func TestTelegramNotifierSendsPlanDecision(t *testing.T) {
	var captured telegramSendMessageRequest
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s, want POST", r.Method)
		}
		if !strings.Contains(r.URL.Path, "/bot123456:ABC/sendMessage") {
			t.Fatalf("path = %s, want bot token sendMessage path", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Fatalf("decode telegram payload: %v", err)
		}
		return jsonResponse(map[string]any{"ok": true}), nil
	})}

	notifier, err := NewTelegramNotifier(TelegramConfig{
		BotToken:   "123456:ABC",
		ChatID:     "-100123",
		ThreadID:   "456",
		HTTPClient: client,
	})
	if err != nil {
		t.Fatalf("NewTelegramNotifier returned error: %v", err)
	}

	if err := notifier.NotifyPlan(context.Background(), samplePlan()); err != nil {
		t.Fatalf("NotifyPlan returned error: %v", err)
	}

	if captured.ChatID != "-100123" {
		t.Fatalf("chat_id = %q, want -100123", captured.ChatID)
	}
	if captured.MessageThreadID != 456 {
		t.Fatalf("message_thread_id = %d, want 456", captured.MessageThreadID)
	}
	if !strings.Contains(captured.Text, "Bifrost 调度器发现 1 个变更") {
		t.Fatalf("telegram text = %q, want change summary", captured.Text)
	}
	if !strings.Contains(captured.Text, "把 `provider_a` 的权重降到 0.0500") {
		t.Fatalf("telegram text = %q, want human decision summary", captured.Text)
	}
}

// TestTelegramNotifierSkipsEmptyPlan 验证没有决策时不发送通知。
func TestTelegramNotifierSkipsEmptyPlan(t *testing.T) {
	calls := 0
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		return jsonResponse(map[string]any{"ok": true}), nil
	})}

	notifier, err := NewTelegramNotifier(TelegramConfig{
		BotToken:   "123456:ABC",
		ChatID:     "-100123",
		HTTPClient: client,
	})
	if err != nil {
		t.Fatalf("NewTelegramNotifier returned error: %v", err)
	}

	if err := notifier.NotifyPlan(context.Background(), domain.Plan{}); err != nil {
		t.Fatalf("NotifyPlan returned error: %v", err)
	}
	if calls != 0 {
		t.Fatalf("calls = %d, want 0 for empty plan", calls)
	}
}

// TestFingerprintIgnoresDecisionOrder 验证指纹不受决策顺序影响。
func TestFingerprintIgnoresDecisionOrder(t *testing.T) {
	first := samplePlan()
	first.Decisions = append(first.Decisions, domain.Decision{
		PoolID:        "pool_b",
		VirtualKey:    "vk_b",
		Provider:      "provider_b",
		Action:        "set_weight_zero",
		CurrentWeight: 0.2,
		TargetWeight:  0,
		Severity:      "critical",
		Reason:        "bad",
	})

	second := first
	second.Decisions = []domain.Decision{first.Decisions[1], first.Decisions[0]}

	if Fingerprint(first) != Fingerprint(second) {
		t.Fatalf("fingerprints differ for same decisions in different order")
	}
}

// samplePlan 构造一份有单个变更的测试计划。
func samplePlan() domain.Plan {
	return domain.Plan{
		GeneratedAt:  time.Date(2026, 6, 20, 12, 0, 0, 0, time.UTC),
		Mode:         "guarded_write",
		ApplyEnabled: true,
		Decisions: []domain.Decision{{
			PoolID:        "pool_a",
			VirtualKey:    "vk_a",
			Provider:      "provider_a",
			Action:        "set_weight",
			CurrentWeight: 0.9,
			TargetWeight:  0.05,
			Severity:      "warning",
			Reason:        "error rate exceeded threshold",
			Apply:         &domain.ApplyResult{Applied: true, Message: "provider weight updated"},
		}},
	}
}

// roundTripFunc 是函数类型，用来模拟 http.RoundTripper。
type roundTripFunc func(*http.Request) (*http.Response, error)

// RoundTrip 让 roundTripFunc 满足 http.RoundTripper 接口。
func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

// jsonResponse 构造一个假的 JSON HTTP 响应。
func jsonResponse(value any) *http.Response {
	data, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(string(data))),
	}
}
