// package main 表示这个文件和 main.go 属于同一个可执行程序。
//
// 这个文件专门放 Telegram 交互控制逻辑。
// 拆出来是为了让 main.go 保持清楚：main.go 管命令入口，这里管 bot 指令。
package main

// import 表示本文件要使用哪些包。
//
// context：控制后台轮询什么时候停止。
// fmt：格式化回复文本。
// html：转义 Telegram HTML 富文本里的运行时文本。
// log/slog：写结构化日志。
// strings：处理 /status、/mute 1h 这类文本命令。
// sync：用 Mutex 保护 daemonState，避免多个 goroutine 同时读写出错。
// time：处理运行时间、静音时长、轮询超时。
// domain：保存最近一次调度计划。
// notify：复用 Telegram Bot API 客户端和按钮结构。
import (
	"context"
	"fmt"
	"html"
	"log/slog"
	"strings"
	"sync"
	"time"

	domain "github.com/Akuma-real/bifrost-scheduler/internal/domain/scheduler"
	"github.com/Akuma-real/bifrost-scheduler/internal/notify"
)

// daemonState 保存 daemon 运行时状态。
//
// struct 是“结构体”，也就是把一组相关字段放在一起。
// 这里放最近一次计划、最近错误、启动时间、静音时间等。
type daemonState struct {
	// mu 是互斥锁。
	// daemon 主循环会写状态，Telegram goroutine 会读状态。
	// 两边同时访问同一份数据时，要先 Lock，避免读到一半被改掉。
	mu sync.Mutex

	startedAt time.Time
	interval  time.Duration
	apply     bool

	lastRunAt time.Time
	lastPlan  *domain.Plan
	lastError string

	mutedUntil time.Time
}

// daemonSnapshot 是 daemonState 的只读快照。
//
// 复制一份快照出来以后，就可以释放锁，再慢慢格式化文本。
// 这样不会让 Telegram 回复过程长时间占着锁。
type daemonSnapshot struct {
	startedAt  time.Time
	interval   time.Duration
	apply      bool
	lastRunAt  time.Time
	lastPlan   *domain.Plan
	lastError  string
	mutedUntil time.Time
}

// newDaemonState 创建 daemonState。
func newDaemonState(interval time.Duration, apply bool) *daemonState {
	return &daemonState{
		startedAt: time.Now(),
		interval:  interval,
		apply:     apply,
	}
}

// recordPlan 记录最近一次成功生成的调度计划。
func (s *daemonState) recordPlan(plan domain.Plan) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// p := plan 会复制一份 Plan 结构体。
	// 然后把 &p 存起来，避免外部再改原变量时影响状态。
	p := plan
	s.lastPlan = &p
	s.lastRunAt = time.Now()
	s.lastError = ""
}

// recordError 记录最近一次运行错误。
func (s *daemonState) recordError(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.lastRunAt = time.Now()
	if err == nil {
		s.lastError = ""
		return
	}
	s.lastError = err.Error()
}

// snapshot 读取当前状态快照。
func (s *daemonState) snapshot() daemonSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()

	var copiedPlan *domain.Plan
	if s.lastPlan != nil {
		p := *s.lastPlan
		copiedPlan = &p
	}

	return daemonSnapshot{
		startedAt:  s.startedAt,
		interval:   s.interval,
		apply:      s.apply,
		lastRunAt:  s.lastRunAt,
		lastPlan:   copiedPlan,
		lastError:  s.lastError,
		mutedUntil: s.mutedUntil,
	}
}

// muteFor 设置通知静音。
func (s *daemonState) muteFor(duration time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mutedUntil = time.Now().Add(duration)
}

// unmute 取消通知静音。
func (s *daemonState) unmute() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mutedUntil = time.Time{}
}

// notificationsMuted 判断当前是否处于静音期。
func (s *daemonState) notificationsMuted() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return time.Now().Before(s.mutedUntil)
}

// mutedUntilText 给日志使用，返回静音截止时间。
func (s *daemonState) mutedUntilText() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.mutedUntil.IsZero() {
		return ""
	}
	return s.mutedUntil.Format(time.RFC3339)
}

// startTelegramControl 启动 Telegram 指令监听。
//
// 返回值是一个停止函数。调用它会取消后台 goroutine。
// 如果没有启用交互或 Telegram 没配置完整，就返回一个空函数。
func startTelegramControl(parent context.Context, logger *slog.Logger, opts options, state *daemonState) func() {
	if !opts.TelegramInteractive {
		return func() {}
	}

	notifier, err := newNotifier(opts)
	if err != nil {
		logger.Error("telegram control setup failed", "error", err)
		return func() {}
	}
	if notifier == nil {
		logger.Error("telegram control disabled because telegram token/chat_id is not configured")
		return func() {}
	}

	allowedChatID, err := telegramChatIDInt(opts)
	if err != nil {
		logger.Error("telegram control setup failed", "error", err)
		return func() {}
	}

	ctx, cancel := context.WithCancel(parent)
	control := telegramControl{
		notifier:      notifier,
		logger:        logger,
		opts:          opts,
		state:         state,
		allowedChatID: allowedChatID,
	}

	// 注册 Telegram 命令菜单。失败只记日志，不影响轮询。
	if err := notifier.SetCommands(ctx, telegramCommands()); err != nil {
		logger.Error("telegram command menu setup failed", "error", err)
	}

	// go 表示启动一个 goroutine，也就是后台任务。
	// daemon 主循环继续跑；这个后台任务负责等待 Telegram 用户命令。
	go control.run(ctx)
	logger.Info("telegram control started", "chat_id", allowedChatID)

	return cancel
}

// telegramControl 是 Telegram 交互控制器。
//
// 它把 notifier、logger、配置和运行状态放在一起，方便多个方法共用。
type telegramControl struct {
	notifier      *notify.TelegramNotifier
	logger        *slog.Logger
	opts          options
	state         *daemonState
	allowedChatID int64
}

// run 持续调用 getUpdates 读取 Telegram 消息。
func (c telegramControl) run(ctx context.Context) {
	offset := 0
	for {
		select {
		case <-ctx.Done():
			c.logger.Info("telegram control stopped")
			return
		default:
		}

		updates, err := c.notifier.GetUpdates(ctx, offset, 25*time.Second)
		if err != nil {
			if ctx.Err() != nil {
				continue
			}
			c.logger.Error("telegram getUpdates failed", "error", err)
			time.Sleep(5 * time.Second)
			continue
		}

		for _, update := range updates {
			if update.UpdateID >= offset {
				offset = update.UpdateID + 1
			}
			c.handleUpdate(ctx, update)
		}
	}
}

// handleUpdate 分发普通消息和按钮点击。
func (c telegramControl) handleUpdate(ctx context.Context, update notify.TelegramUpdate) {
	if update.Message != nil {
		if update.Message.Chat.ID != c.allowedChatID {
			c.logger.Warn("telegram message rejected from unauthorized chat", "chat_id", update.Message.Chat.ID)
			return
		}
		c.handleCommand(ctx, strings.TrimSpace(update.Message.Text))
		return
	}

	if update.CallbackQuery != nil {
		chatID := int64(0)
		if update.CallbackQuery.Message != nil {
			chatID = update.CallbackQuery.Message.Chat.ID
		}
		if chatID != c.allowedChatID {
			c.logger.Warn("telegram callback rejected from unauthorized chat", "chat_id", chatID)
			_ = c.notifier.AnswerCallback(ctx, update.CallbackQuery.ID, "无权限")
			return
		}
		_ = c.notifier.AnswerCallback(ctx, update.CallbackQuery.ID, "处理中")
		c.handleCommand(ctx, update.CallbackQuery.Data)
	}
}

// handleCommand 处理一条用户命令。
func (c telegramControl) handleCommand(ctx context.Context, raw string) {
	command, arg := splitTelegramCommand(raw)
	if command == "" {
		return
	}

	// Telegram 客户端里的“正在输入...”只会持续几秒。
	// 对 /run 这种要等待 Bifrost API 的命令，后台循环会每隔几秒补一次 typing。
	stopTyping := c.startTyping(ctx)
	defer stopTyping()

	switch command {
	case "/start", "/help", "help":
		c.reply(ctx, helpText(), mainKeyboard())
	case "/status", "status":
		c.reply(ctx, statusText(c.state.snapshot()), mainKeyboard())
	case "/last", "last":
		c.reply(ctx, lastPlanText(c.state.snapshot()), mainKeyboard())
	case "/run", "run":
		c.runDryPlan(ctx)
	case "/mute", "mute":
		c.mute(ctx, arg)
	case "/unmute", "unmute":
		c.state.unmute()
		c.reply(ctx, "已恢复 Telegram 变更通知。", mainKeyboard())
	default:
		c.reply(ctx, "未知命令。\n\n"+helpText(), mainKeyboard())
	}
}

// runDryPlan 手动执行一次 dry-run。
//
// 这里故意传 apply=false。
// 即使生产 daemon 是 --apply，Telegram /run 也只预览，不写线上。
func (c telegramControl) runDryPlan(ctx context.Context) {
	plan, err := buildPlan(ctx, c.opts, false)
	if err != nil {
		c.state.recordError(err)
		c.reply(ctx, "<b>手动 dry-run 失败</b>\n\n<code>"+html.EscapeString(err.Error())+"</code>", mainKeyboard())
		return
	}
	c.state.recordPlan(plan)
	c.reply(ctx, manualRunText(plan), mainKeyboard())
}

// mute 处理 /mute 命令。
func (c telegramControl) mute(ctx context.Context, arg string) {
	duration := time.Hour
	if strings.TrimSpace(arg) != "" {
		parsed, err := time.ParseDuration(strings.TrimSpace(arg))
		if err != nil || parsed <= 0 {
			c.reply(ctx, "静音时长格式不对。例子：<code>/mute 30m</code> 或 <code>/mute 2h</code>", mainKeyboard())
			return
		}
		duration = parsed
	}
	c.state.muteFor(duration)
	c.reply(ctx, fmt.Sprintf("已静音 Telegram 变更通知 <code>%s</code>。调度器仍会继续运行。", html.EscapeString(duration.String())), mainKeyboard())
}

// reply 发送 Telegram HTML 富文本回复。
func (c telegramControl) reply(ctx context.Context, text string, keyboard [][]notify.TelegramInlineButton) {
	if err := c.notifier.SendHTMLWithKeyboard(ctx, text, keyboard); err != nil {
		c.logger.Error("telegram reply failed", "error", err)
	}
}

// startTyping 启动 Telegram “正在输入...”提示。
//
// 返回的 stop 函数用于停止后台循环。
func (c telegramControl) startTyping(ctx context.Context) func() {
	typingCtx, cancel := context.WithCancel(ctx)
	send := func() {
		if err := c.notifier.SendChatAction(typingCtx, notify.TelegramActionTyping); err != nil && typingCtx.Err() == nil {
			c.logger.Error("telegram typing action failed", "error", err)
		}
	}

	// 先立即发一次，避免用户点了按钮后看起来没有任何反应。
	send()

	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(4 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-typingCtx.Done():
				return
			case <-ticker.C:
				send()
			}
		}
	}()

	return func() {
		cancel()
		<-done
	}
}

// splitTelegramCommand 把原始文本拆成命令和参数。
func splitTelegramCommand(raw string) (string, string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", ""
	}
	fields := strings.Fields(raw)
	if len(fields) == 0 {
		return "", ""
	}
	command := strings.ToLower(fields[0])
	if at := strings.Index(command, "@"); at >= 0 {
		command = command[:at]
	}
	arg := ""
	if len(fields) > 1 {
		arg = strings.TrimSpace(strings.TrimPrefix(raw, fields[0]))
	}
	return command, arg
}

// telegramCommands 返回 Telegram 命令菜单。
func telegramCommands() []notify.TelegramBotCommand {
	return []notify.TelegramBotCommand{
		{Command: "status", Description: "查看调度器状态"},
		{Command: "last", Description: "查看最近一次调度计划摘要"},
		{Command: "run", Description: "立即执行一次 dry-run 预览"},
		{Command: "mute", Description: "静音变更通知，例如 /mute 1h"},
		{Command: "unmute", Description: "恢复变更通知"},
		{Command: "help", Description: "查看可用命令"},
	}
}

// mainKeyboard 返回 Telegram 消息下方的快捷按钮。
func mainKeyboard() [][]notify.TelegramInlineButton {
	return [][]notify.TelegramInlineButton{
		{
			{Text: "状态", CallbackData: "status"},
			{Text: "最近计划", CallbackData: "last"},
		},
		{
			{Text: "立即 dry-run", CallbackData: "run"},
			{Text: "静音 1h", CallbackData: "mute 1h"},
			{Text: "恢复通知", CallbackData: "unmute"},
		},
	}
}

// helpText 返回帮助文本。
func helpText() string {
	return strings.TrimSpace(`
<b>Bifrost 调度器 Telegram 控制台</b>

可用命令：
<code>/status</code> - 查看 daemon 是否运行、多久跑一次、最近是否报错
<code>/last</code> - 查看最近一次调度摘要
<code>/run</code> - 立即执行一次 dry-run 预览，不写线上
<code>/mute 1h</code> - 静音变更通知，调度器仍继续运行
<code>/unmute</code> - 恢复变更通知
<code>/help</code> - 查看帮助

安全边界：
Telegram 交互不提供直接写线上命令。自动写入仍只由 daemon 的 <code>config mode</code> 和 <code>--apply</code> 控制。
`)
}

// statusText 把状态快照格式化成 Telegram 文本。
func statusText(snapshot daemonSnapshot) string {
	mode := "dry-run"
	if snapshot.apply {
		mode = "daemon 带 --apply"
	}
	muted := "否"
	if time.Now().Before(snapshot.mutedUntil) {
		muted = "是，到 " + snapshot.mutedUntil.Format(time.RFC3339)
	}
	lastRun := "还没有成功运行"
	if !snapshot.lastRunAt.IsZero() {
		lastRun = snapshot.lastRunAt.Format(time.RFC3339)
	}
	lastError := "无"
	if snapshot.lastError != "" {
		lastError = snapshot.lastError
	}
	decisions := 0
	applyEnabled := false
	if snapshot.lastPlan != nil {
		decisions = len(snapshot.lastPlan.Decisions)
		applyEnabled = snapshot.lastPlan.ApplyEnabled
	}

	return fmt.Sprintf(
		"<b>Bifrost 调度器状态</b>\n\n启动时间：<code>%s</code>\n运行模式：<code>%s</code>\n实际写入开关：<b>%t</b>\n运行间隔：<code>%s</code>\n最近运行：<code>%s</code>\n最近决策数：<b>%d</b>\n通知静音：%s\n最近错误：<code>%s</code>",
		html.EscapeString(snapshot.startedAt.Format(time.RFC3339)),
		html.EscapeString(mode),
		applyEnabled,
		html.EscapeString(snapshot.interval.String()),
		html.EscapeString(lastRun),
		decisions,
		html.EscapeString(muted),
		html.EscapeString(lastError),
	)
}

// lastPlanText 把最近计划压缩成 Telegram 文本。
func lastPlanText(snapshot daemonSnapshot) string {
	if snapshot.lastPlan == nil {
		return "还没有最近计划。可以点“立即 dry-run”或发送 <code>/run</code> 先跑一次预览。"
	}
	return planSummaryText("最近一次调度计划", *snapshot.lastPlan)
}

// manualRunText 把手动 dry-run 结果压缩成 Telegram 文本。
func manualRunText(plan domain.Plan) string {
	return planSummaryText("手动 dry-run 完成", plan)
}

// planSummaryText 生成短计划摘要。
func planSummaryText(title string, plan domain.Plan) string {
	var b strings.Builder
	status := "不会写线上"
	if plan.ApplyEnabled {
		status = "会写线上"
	}
	fmt.Fprintf(&b, "<b>%s</b>\n\n时间：<code>%s</code>\n模式：<code>%s</code>\n执行：<b>%s</b>\n决策数：<b>%d</b>\n",
		html.EscapeString(title),
		html.EscapeString(plan.GeneratedAt.Format(time.RFC3339)),
		html.EscapeString(plan.Mode),
		html.EscapeString(status),
		len(plan.Decisions),
	)
	if len(plan.Decisions) == 0 {
		fmt.Fprintf(&b, "\n没有建议动作。")
		return b.String()
	}
	for i, decision := range plan.Decisions {
		if i >= 8 {
			fmt.Fprintf(&b, "\n...还有 %d 个动作，完整内容见日志。", len(plan.Decisions)-i)
			break
		}
		fmt.Fprintf(&b, "\n%d. <code>%s</code> / <code>%s</code>\n",
			i+1,
			html.EscapeString(decision.PoolID),
			html.EscapeString(decision.Provider),
		)
		fmt.Fprintf(&b, "   <code>%s</code>：<code>%.4f</code> -&gt; <code>%.4f</code>\n",
			html.EscapeString(decision.Action),
			decision.CurrentWeight,
			decision.TargetWeight,
		)
		if decision.Reason != "" {
			fmt.Fprintf(&b, "   %s\n", html.EscapeString(decision.Reason))
		}
	}
	return b.String()
}
