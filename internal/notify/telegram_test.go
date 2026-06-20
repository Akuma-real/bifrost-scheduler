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
	if captured.ParseMode != TelegramParseHTML {
		t.Fatalf("parse_mode = %q, want HTML", captured.ParseMode)
	}
	if !strings.Contains(captured.Text, "<b>Bifrost 调度器发现 1 个变更</b>") {
		t.Fatalf("telegram text = %q, want change summary", captured.Text)
	}
	if !strings.Contains(captured.Text, "把 <code>provider_a</code> 的权重降到 <code>0.0500</code>") {
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

// TestTelegramNotifierSendsKeyboard 验证可以发送带按钮的 Telegram 消息。
func TestTelegramNotifierSendsKeyboard(t *testing.T) {
	var captured telegramSendMessageRequest
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if !strings.Contains(r.URL.Path, "/sendMessage") {
			t.Fatalf("path = %s, want sendMessage", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Fatalf("decode telegram payload: %v", err)
		}
		return jsonResponse(map[string]any{"ok": true}), nil
	})}

	notifier, err := NewTelegramNotifier(TelegramConfig{
		BotToken:   "123456:ABC",
		ChatID:     "1926854736",
		HTTPClient: client,
	})
	if err != nil {
		t.Fatalf("NewTelegramNotifier returned error: %v", err)
	}

	err = notifier.SendTextWithKeyboard(context.Background(), "hello", [][]TelegramInlineButton{
		{{Text: "状态", CallbackData: "status"}},
	})
	if err != nil {
		t.Fatalf("SendTextWithKeyboard returned error: %v", err)
	}
	if captured.ReplyMarkup == nil {
		t.Fatalf("reply_markup is nil, want inline keyboard")
	}
	if captured.ReplyMarkup.InlineKeyboard[0][0].CallbackData != "status" {
		t.Fatalf("callback_data = %q, want status", captured.ReplyMarkup.InlineKeyboard[0][0].CallbackData)
	}
}

// TestTelegramNotifierSendsHTMLWithKeyboard 验证 HTML 富文本消息会带 parse_mode。
func TestTelegramNotifierSendsHTMLWithKeyboard(t *testing.T) {
	var captured telegramSendMessageRequest
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Fatalf("decode telegram payload: %v", err)
		}
		return jsonResponse(map[string]any{"ok": true}), nil
	})}

	notifier, err := NewTelegramNotifier(TelegramConfig{
		BotToken:   "123456:ABC",
		ChatID:     "1926854736",
		HTTPClient: client,
	})
	if err != nil {
		t.Fatalf("NewTelegramNotifier returned error: %v", err)
	}

	err = notifier.SendHTMLWithKeyboard(context.Background(), "<b>hello</b>", [][]TelegramInlineButton{
		{{Text: "状态", CallbackData: "status"}},
	})
	if err != nil {
		t.Fatalf("SendHTMLWithKeyboard returned error: %v", err)
	}
	if captured.ParseMode != TelegramParseHTML {
		t.Fatalf("parse_mode = %q, want HTML", captured.ParseMode)
	}
	if captured.ReplyMarkup == nil {
		t.Fatalf("reply_markup is nil, want inline keyboard")
	}
}

// TestTelegramNotifierSendChatAction 验证 typing 状态会调用 sendChatAction。
func TestTelegramNotifierSendChatAction(t *testing.T) {
	var captured telegramSendChatActionRequest
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if !strings.Contains(r.URL.Path, "/sendChatAction") {
			t.Fatalf("path = %s, want sendChatAction", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Fatalf("decode sendChatAction payload: %v", err)
		}
		return jsonResponse(map[string]any{"ok": true}), nil
	})}

	notifier, err := NewTelegramNotifier(TelegramConfig{
		BotToken:   "123456:ABC",
		ChatID:     "1926854736",
		ThreadID:   "456",
		HTTPClient: client,
	})
	if err != nil {
		t.Fatalf("NewTelegramNotifier returned error: %v", err)
	}

	if err := notifier.SendChatAction(context.Background(), TelegramActionTyping); err != nil {
		t.Fatalf("SendChatAction returned error: %v", err)
	}
	if captured.ChatID != "1926854736" {
		t.Fatalf("chat_id = %q, want 1926854736", captured.ChatID)
	}
	if captured.MessageThreadID != 456 {
		t.Fatalf("thread id = %d, want 456", captured.MessageThreadID)
	}
	if captured.Action != TelegramActionTyping {
		t.Fatalf("action = %q, want typing", captured.Action)
	}
}

// TestFormatPlanHTMLMessageEscapesRuntimeText 验证 HTML 消息会转义运行时文本。
func TestFormatPlanHTMLMessageEscapesRuntimeText(t *testing.T) {
	plan := samplePlan()
	plan.Decisions[0].Provider = "provider_<a>&b"
	plan.Decisions[0].Reason = "bad <token> & retry"

	text := FormatPlanHTMLMessage(plan)
	if !strings.Contains(text, "provider_&lt;a&gt;&amp;b") {
		t.Fatalf("html text = %q, want escaped provider", text)
	}
	if !strings.Contains(text, "bad &lt;token&gt; &amp; retry") {
		t.Fatalf("html text = %q, want escaped reason", text)
	}
}

// TestTelegramNotifierGetUpdates 验证 getUpdates 可以解析普通消息和按钮点击。
func TestTelegramNotifierGetUpdates(t *testing.T) {
	var captured telegramGetUpdatesRequest
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if !strings.Contains(r.URL.Path, "/getUpdates") {
			t.Fatalf("path = %s, want getUpdates", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Fatalf("decode getUpdates payload: %v", err)
		}
		return jsonResponse(map[string]any{
			"ok": true,
			"result": []map[string]any{
				{
					"update_id": float64(11),
					"message": map[string]any{
						"message_id": float64(22),
						"chat":       map[string]any{"id": float64(1926854736), "type": "private"},
						"text":       "/status",
					},
				},
				{
					"update_id": float64(12),
					"callback_query": map[string]any{
						"id":   "callback-1",
						"from": map[string]any{"id": float64(1926854736)},
						"message": map[string]any{
							"message_id": float64(23),
							"chat":       map[string]any{"id": float64(1926854736), "type": "private"},
						},
						"data": "last",
					},
				},
			},
		}), nil
	})}

	notifier, err := NewTelegramNotifier(TelegramConfig{
		BotToken:   "123456:ABC",
		ChatID:     "1926854736",
		HTTPClient: client,
	})
	if err != nil {
		t.Fatalf("NewTelegramNotifier returned error: %v", err)
	}

	updates, err := notifier.GetUpdates(context.Background(), 10, 2*time.Second)
	if err != nil {
		t.Fatalf("GetUpdates returned error: %v", err)
	}
	if captured.Offset != 10 {
		t.Fatalf("offset = %d, want 10", captured.Offset)
	}
	if captured.Timeout != 2 {
		t.Fatalf("timeout = %d, want 2", captured.Timeout)
	}
	if len(updates) != 2 {
		t.Fatalf("updates len = %d, want 2", len(updates))
	}
	if updates[0].Message == nil || updates[0].Message.Text != "/status" {
		t.Fatalf("first update = %+v, want /status message", updates[0])
	}
	if updates[1].CallbackQuery == nil || updates[1].CallbackQuery.Data != "last" {
		t.Fatalf("second update = %+v, want last callback", updates[1])
	}
}

// TestTelegramNotifierSetCommands 验证命令菜单会调用 setMyCommands。
func TestTelegramNotifierSetCommands(t *testing.T) {
	var captured telegramSetCommandsRequest
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if !strings.Contains(r.URL.Path, "/setMyCommands") {
			t.Fatalf("path = %s, want setMyCommands", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Fatalf("decode setMyCommands payload: %v", err)
		}
		return jsonResponse(map[string]any{"ok": true}), nil
	})}

	notifier, err := NewTelegramNotifier(TelegramConfig{
		BotToken:   "123456:ABC",
		ChatID:     "1926854736",
		HTTPClient: client,
	})
	if err != nil {
		t.Fatalf("NewTelegramNotifier returned error: %v", err)
	}

	err = notifier.SetCommands(context.Background(), []TelegramBotCommand{
		{Command: "status", Description: "查看状态"},
	})
	if err != nil {
		t.Fatalf("SetCommands returned error: %v", err)
	}
	if len(captured.Commands) != 1 || captured.Commands[0].Command != "status" {
		t.Fatalf("commands = %+v, want status command", captured.Commands)
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
