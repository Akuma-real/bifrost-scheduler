// package notify 表示这个文件属于“通知输出层”。
//
// 它只负责把调度计划发到外部通知渠道。
// 它不读取 Bifrost，不判断 provider 好坏，也不修改线上权重。
package notify

// import 表示这个文件需要使用哪些包。
//
// bytes：把 JSON 字节变成 HTTP 请求体。
// context：让 Telegram 请求可以被取消。
// crypto/sha256、encoding/hex：给计划生成稳定指纹，用于 daemon 去重。
// encoding/json：编码 Telegram 请求和解析响应。
// fmt：格式化错误和消息。
// io：读取 Telegram 错误响应体。
// net/http：发送 Telegram Bot API 请求。
// sort：让指纹生成顺序稳定。
// strconv：把线程 ID 字符串转成整数。
// strings：拼接消息和处理字符串。
// time：设置默认 HTTP 超时。
// domain/report：读取调度计划并复用人类可读动作文案。
import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	domain "github.com/Akuma-real/bifrost-scheduler/internal/domain/scheduler"
	"github.com/Akuma-real/bifrost-scheduler/internal/report"
)

// TelegramConfig 是 Telegram 通知配置。
//
// 这些值通常来自环境变量，而不是 config.json。
// 这样 bot token 不会被写进公开配置模板。
type TelegramConfig struct {
	// BotToken 是 BotFather 给你的 Telegram bot token。
	BotToken string
	// ChatID 是要发送到的 chat id，可以是个人、群组、频道。
	ChatID string
	// ThreadID 是 Telegram forum topic 的 message_thread_id，可选。
	ThreadID string
	// HTTPClient 是可选的 HTTP 客户端。
	// 生产环境可以不传，测试里会传假的客户端。
	HTTPClient *http.Client
}

// TelegramNotifier 是 Telegram Bot API 通知器。
type TelegramNotifier struct {
	botToken string
	chatID   string
	threadID int
	http     *http.Client
}

// TelegramInlineButton 表示 Telegram 消息下面的一个按钮。
//
// 用户点按钮后，Telegram 会把 CallbackData 原样发回 bot。
// 调度器用它区分用户点的是“状态”“最近计划”还是“手动 dry-run”。
type TelegramInlineButton struct {
	Text         string `json:"text"`
	CallbackData string `json:"callback_data"`
}

// TelegramBotCommand 表示 Telegram 左下角命令菜单里的一项。
//
// SetCommands 会把这些命令注册到 Telegram，这样用户不用记命令名。
type TelegramBotCommand struct {
	Command     string `json:"command"`
	Description string `json:"description"`
}

// TelegramUpdate 是 getUpdates 返回的一条用户交互事件。
//
// 现在只关心两种事件：
//   - Message：用户直接发 /status 这样的文本命令。
//   - CallbackQuery：用户点击消息里的按钮。
type TelegramUpdate struct {
	UpdateID      int                    `json:"update_id"`
	Message       *TelegramMessage       `json:"message,omitempty"`
	CallbackQuery *TelegramCallbackQuery `json:"callback_query,omitempty"`
}

// TelegramMessage 表示 Telegram 普通消息。
type TelegramMessage struct {
	MessageID int           `json:"message_id"`
	From      *TelegramUser `json:"from,omitempty"`
	Chat      TelegramChat  `json:"chat"`
	Text      string        `json:"text,omitempty"`
}

// TelegramCallbackQuery 表示 Telegram 按钮点击事件。
type TelegramCallbackQuery struct {
	ID      string           `json:"id"`
	From    TelegramUser     `json:"from"`
	Message *TelegramMessage `json:"message,omitempty"`
	Data    string           `json:"data,omitempty"`
}

// TelegramUser 表示发消息或点按钮的 Telegram 用户。
type TelegramUser struct {
	ID        int64  `json:"id"`
	Username  string `json:"username,omitempty"`
	FirstName string `json:"first_name,omitempty"`
	LastName  string `json:"last_name,omitempty"`
}

// TelegramChat 表示 Telegram 会话。
//
// 私聊时 Chat.ID 通常等于用户 ID；群组和频道会是负数。
type TelegramChat struct {
	ID    int64  `json:"id"`
	Type  string `json:"type,omitempty"`
	Title string `json:"title,omitempty"`
}

// NewTelegramNotifier 根据配置创建 TelegramNotifier。
//
// 返回值有两种正常情况：
//   - nil, nil：没有配置 Telegram，表示通知关闭。
//   - notifier, nil：配置完整，可以发送通知。
func NewTelegramNotifier(cfg TelegramConfig) (*TelegramNotifier, error) {
	cfg.BotToken = strings.TrimSpace(cfg.BotToken)
	cfg.ChatID = strings.TrimSpace(cfg.ChatID)
	cfg.ThreadID = strings.TrimSpace(cfg.ThreadID)

	// token 和 chat_id 都没填，表示用户没有启用 Telegram 通知。
	if cfg.BotToken == "" && cfg.ChatID == "" {
		return nil, nil
	}
	// 只填一个通常是配置错误，直接报错比静默不发更容易排查。
	if cfg.BotToken == "" || cfg.ChatID == "" {
		return nil, fmt.Errorf("telegram notification requires both bot token and chat id")
	}

	threadID := 0
	if cfg.ThreadID != "" {
		parsed, err := strconv.Atoi(cfg.ThreadID)
		if err != nil || parsed <= 0 {
			return nil, fmt.Errorf("telegram thread id must be a positive integer")
		}
		threadID = parsed
	}

	client := cfg.HTTPClient
	if client == nil {
		// getUpdates 会使用长轮询，所以默认超时要比普通 sendMessage 长一点。
		client = &http.Client{Timeout: 35 * time.Second}
	}

	return &TelegramNotifier{
		botToken: cfg.BotToken,
		chatID:   cfg.ChatID,
		threadID: threadID,
		http:     client,
	}, nil
}

// NotifyPlan 把有变更的调度计划发送到 Telegram。
//
// 如果 plan.Decisions 为空，函数直接返回 nil，不发送消息。
func (n *TelegramNotifier) NotifyPlan(ctx context.Context, plan domain.Plan) error {
	if n == nil || len(plan.Decisions) == 0 {
		return nil
	}
	text := FormatPlanMessage(plan)
	return n.SendText(ctx, text)
}

// SendText 发送一条普通 Telegram 文本消息。
func (n *TelegramNotifier) SendText(ctx context.Context, text string) error {
	return n.SendTextWithKeyboard(ctx, text, nil)
}

// SendTextWithKeyboard 发送文本消息，并可选带一组内联按钮。
//
// keyboard 是二维切片：外层是一行一行按钮，内层是同一行里的多个按钮。
func (n *TelegramNotifier) SendTextWithKeyboard(ctx context.Context, text string, keyboard [][]TelegramInlineButton) error {
	payload := telegramSendMessageRequest{
		ChatID:                n.chatID,
		Text:                  text,
		DisableWebPagePreview: true,
	}
	if n.threadID > 0 {
		payload.MessageThreadID = n.threadID
	}
	if len(keyboard) > 0 {
		payload.ReplyMarkup = &telegramInlineKeyboardMarkup{InlineKeyboard: keyboard}
	}

	return n.postTelegram(ctx, "sendMessage", payload, nil)
}

// GetUpdates 用 Telegram 长轮询读取用户命令和按钮点击。
//
// offset 表示“从哪个 update_id 之后开始读”，用于避免重复处理旧消息。
// timeout 是 Telegram 服务端等待新消息的时间，不是整个程序的运行周期。
func (n *TelegramNotifier) GetUpdates(ctx context.Context, offset int, timeout time.Duration) ([]TelegramUpdate, error) {
	seconds := int(timeout.Seconds())
	if seconds < 1 {
		seconds = 1
	}
	payload := telegramGetUpdatesRequest{
		Offset:         offset,
		Timeout:        seconds,
		AllowedUpdates: []string{"message", "callback_query"},
	}

	var parsed telegramUpdatesResponse
	if err := n.postTelegram(ctx, "getUpdates", payload, &parsed); err != nil {
		return nil, err
	}
	if !parsed.OK {
		return nil, fmt.Errorf("telegram getUpdates returned not ok: %s", parsed.Description)
	}
	return parsed.Result, nil
}

// AnswerCallback 告诉 Telegram“按钮点击已处理”。
//
// 如果不调用它，用户点按钮后 Telegram 客户端可能一直显示加载状态。
func (n *TelegramNotifier) AnswerCallback(ctx context.Context, callbackID, text string) error {
	payload := telegramAnswerCallbackRequest{
		CallbackQueryID: callbackID,
		Text:            text,
	}
	return n.postTelegram(ctx, "answerCallbackQuery", payload, nil)
}

// SetCommands 注册 Telegram 命令菜单。
//
// 这不是调度器必须功能；失败时上层只记录日志，不影响调度。
func (n *TelegramNotifier) SetCommands(ctx context.Context, commands []TelegramBotCommand) error {
	if len(commands) == 0 {
		return nil
	}
	payload := telegramSetCommandsRequest{Commands: commands}
	return n.postTelegram(ctx, "setMyCommands", payload, nil)
}

// postTelegram 统一调用 Telegram Bot API。
//
// result 可以是 nil；如果不是 nil，就把 Telegram 响应 JSON 解析进去。
func (n *TelegramNotifier) postTelegram(ctx context.Context, method string, payload any, result any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode telegram payload: %w", err)
	}

	url := "https://api.telegram.org/bot" + n.botToken + "/" + method
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("build telegram request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := n.http.Do(req)
	if err != nil {
		// 注意：不要把原始 err 直接返回，因为里面可能带 bot token URL。
		return fmt.Errorf("send telegram request failed: %s", redactToken(err.Error(), n.botToken))
	}
	defer resp.Body.Close()

	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if readErr != nil {
		return fmt.Errorf("read telegram response: %w", readErr)
	}

	var parsed telegramResponse
	if result != nil {
		if err := json.Unmarshal(body, result); err != nil {
			return fmt.Errorf("decode telegram response: %w", err)
		}
		if base, ok := result.(interface {
			baseResponse() telegramResponse
		}); ok {
			parsed = base.baseResponse()
		}
	} else {
		_ = json.Unmarshal(body, &parsed)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || !parsed.OK {
		message := parsed.Description
		if message == "" {
			message = strings.TrimSpace(string(body))
		}
		if message == "" {
			message = resp.Status
		}
		return fmt.Errorf("telegram API returned %s: %s", resp.Status, message)
	}
	return nil
}

// FormatPlanMessage 把调度计划压缩成适合 Telegram 的短消息。
//
// Telegram 单条消息有长度限制，所以这里只放关键动作摘要。
func FormatPlanMessage(plan domain.Plan) string {
	var b strings.Builder
	status := "不会写线上"
	if plan.ApplyEnabled {
		status = "会写线上"
	}

	fmt.Fprintf(&b, "Bifrost 调度器发现 %d 个变更\n", len(plan.Decisions))
	fmt.Fprintf(&b, "模式：%s；执行：%s\n", plan.Mode, status)
	fmt.Fprintf(&b, "时间：%s\n", plan.GeneratedAt.Format(time.RFC3339))

	for i, decision := range plan.Decisions {
		fmt.Fprintf(&b, "\n%d. [%s] %s\n", i+1, decision.Severity, report.HumanSummary(decision))
		fmt.Fprintf(&b, "   %s / %s\n", decision.PoolID, decision.Provider)
		fmt.Fprintf(&b, "   权重：%.4f -> %.4f\n", decision.CurrentWeight, decision.TargetWeight)
		if decision.Apply != nil {
			fmt.Fprintf(&b, "   执行：%s\n", applyText(*decision.Apply))
		} else if decision.DryRun {
			fmt.Fprintf(&b, "   执行：未执行，只预览\n")
		}
		if decision.Reason != "" {
			fmt.Fprintf(&b, "   原因：%s\n", decision.Reason)
		}
	}

	return truncateTelegramText(b.String())
}

// Fingerprint 给本轮决策生成稳定指纹。
//
// daemon 可以用它判断“这轮变更是否和上一轮完全一样”，避免重复刷屏。
func Fingerprint(plan domain.Plan) string {
	parts := make([]string, 0, len(plan.Decisions))
	for _, decision := range plan.Decisions {
		parts = append(parts, fmt.Sprintf(
			"%s\x00%s\x00%s\x00%s\x00%.4f\x00%.4f\x00%s\x00%s",
			decision.PoolID,
			decision.VirtualKey,
			decision.Provider,
			decision.Action,
			decision.CurrentWeight,
			decision.TargetWeight,
			decision.Severity,
			decision.Reason,
		))
	}
	sort.Strings(parts)
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00\n")))
	return hex.EncodeToString(sum[:])
}

// applyText 把执行结果压缩成 Telegram 里的一行。
func applyText(result domain.ApplyResult) string {
	if result.Applied {
		return "已执行：" + result.Message
	}
	if result.Skipped {
		return "已跳过：" + result.Message
	}
	return "失败：" + result.Message
}

// truncateTelegramText 避免消息超过 Telegram 单条消息限制。
func truncateTelegramText(text string) string {
	const maxRunes = 3900
	runes := []rune(text)
	if len(runes) <= maxRunes {
		return text
	}
	return string(runes[:maxRunes]) + "\n\n...已截断，完整报告见调度器日志。"
}

// redactToken 从错误文本里隐藏 bot token。
func redactToken(text, token string) string {
	if token == "" {
		return text
	}
	return strings.ReplaceAll(text, token, "***")
}

// telegramSendMessageRequest 是 Telegram sendMessage 的 JSON 请求体。
type telegramSendMessageRequest struct {
	ChatID                string                        `json:"chat_id"`
	MessageThreadID       int                           `json:"message_thread_id,omitempty"`
	Text                  string                        `json:"text"`
	DisableWebPagePreview bool                          `json:"disable_web_page_preview"`
	ReplyMarkup           *telegramInlineKeyboardMarkup `json:"reply_markup,omitempty"`
}

// telegramResponse 是 Telegram Bot API 的通用响应结构。
type telegramResponse struct {
	OK          bool   `json:"ok"`
	Description string `json:"description"`
}

// telegramInlineKeyboardMarkup 是 Telegram 内联按钮布局。
type telegramInlineKeyboardMarkup struct {
	InlineKeyboard [][]TelegramInlineButton `json:"inline_keyboard"`
}

// telegramGetUpdatesRequest 是 getUpdates 的请求体。
type telegramGetUpdatesRequest struct {
	Offset         int      `json:"offset,omitempty"`
	Timeout        int      `json:"timeout,omitempty"`
	AllowedUpdates []string `json:"allowed_updates,omitempty"`
}

// telegramUpdatesResponse 是 getUpdates 的响应体。
type telegramUpdatesResponse struct {
	OK          bool             `json:"ok"`
	Description string           `json:"description"`
	Result      []TelegramUpdate `json:"result"`
}

// baseResponse 让 postTelegram 能从具体响应里取出通用 OK/Description。
func (r telegramUpdatesResponse) baseResponse() telegramResponse {
	return telegramResponse{OK: r.OK, Description: r.Description}
}

// telegramAnswerCallbackRequest 是 answerCallbackQuery 的请求体。
type telegramAnswerCallbackRequest struct {
	CallbackQueryID string `json:"callback_query_id"`
	Text            string `json:"text,omitempty"`
}

// telegramSetCommandsRequest 是 setMyCommands 的请求体。
type telegramSetCommandsRequest struct {
	Commands []TelegramBotCommand `json:"commands"`
}
