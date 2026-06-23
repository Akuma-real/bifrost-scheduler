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
	"encoding/json"
	"fmt"
	"html"
	"log/slog"
	"os"
	"strconv"
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

	mutedUntil        time.Time
	pendingPrice      *priceUpdate
	pendingPriceDraft *priceDraft
}

// daemonSnapshot 是 daemonState 的只读快照。
//
// 复制一份快照出来以后，就可以释放锁，再慢慢格式化文本。
// 这样不会让 Telegram 回复过程长时间占着锁。
type daemonSnapshot struct {
	startedAt    time.Time
	interval     time.Duration
	apply        bool
	lastRunAt    time.Time
	lastPlan     *domain.Plan
	lastError    string
	mutedUntil   time.Time
	pendingPrice *priceUpdate
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
	var copiedPendingPrice *priceUpdate
	if s.pendingPrice != nil {
		p := *s.pendingPrice
		copiedPendingPrice = &p
	}

	return daemonSnapshot{
		startedAt:    s.startedAt,
		interval:     s.interval,
		apply:        s.apply,
		lastRunAt:    s.lastRunAt,
		lastPlan:     copiedPlan,
		lastError:    s.lastError,
		mutedUntil:   s.mutedUntil,
		pendingPrice: copiedPendingPrice,
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

// setPendingPrice 保存一条待确认的价格修改。
func (s *daemonState) setPendingPrice(update priceUpdate) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pendingPrice = &update
}

// clearPendingPrice 清掉待确认价格修改。
func (s *daemonState) clearPendingPrice() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pendingPrice = nil
	s.pendingPriceDraft = nil
}

// getPendingPrice 取出待确认价格修改。
func (s *daemonState) getPendingPrice() (priceUpdate, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pendingPrice == nil {
		return priceUpdate{}, false
	}
	return *s.pendingPrice, true
}

// setPriceDraft 保存一次按钮选择出来的 pool/provider，等待用户输入价格。
func (s *daemonState) setPriceDraft(draft priceDraft) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pendingPriceDraft = &draft
}

// clearPriceDraft 清掉正在输入价格的状态。
func (s *daemonState) clearPriceDraft() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pendingPriceDraft = nil
}

// getPriceDraft 取出正在输入价格的状态。
func (s *daemonState) getPriceDraft() (priceDraft, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pendingPriceDraft == nil {
		return priceDraft{}, false
	}
	return *s.pendingPriceDraft, true
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

// priceUpdate 是一次待确认的价格修改。
type priceUpdate struct {
	PoolID   string
	Provider string
	Price    float64
	Preview  priceUpdatePreview
}

// priceDraft 是按钮选完 pool/provider 后，等待输入价格的临时状态。
type priceDraft struct {
	PoolID   string
	Provider string
}

// priceUpdatePreview 是修改价格后的配置预览。
type priceUpdatePreview struct {
	PoolID    string
	Provider  string
	OldPrice  float64
	NewPrice  float64
	Providers []priceProviderPreview
}

// priceProviderPreview 展示同一个 pool 里每个 provider 的价格和换算权重。
type priceProviderPreview struct {
	Name       string
	Price      float64
	CostWeight float64
	Allowed    bool
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
		c.handleMessage(ctx, strings.TrimSpace(update.Message.Text))
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

// handleMessage 处理用户手动发来的文本。
func (c telegramControl) handleMessage(ctx context.Context, raw string) {
	command, _ := splitTelegramCommand(raw)
	if _, waitingForPrice := c.state.getPriceDraft(); waitingForPrice && !isTelegramTextCommand(command) {
		c.previewDraftPriceUpdate(ctx, raw)
		return
	}
	c.handleCommand(ctx, raw)
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
	case "/prices", "prices":
		c.listPrices(ctx)
	case "price_pool":
		c.choosePricePool(ctx, arg)
	case "price_provider":
		c.choosePriceProvider(ctx, arg)
	case "/price", "price":
		c.previewPriceUpdate(ctx, arg)
	case "/price_apply", "price_apply":
		c.applyPendingPrice(ctx)
	case "/price_cancel", "price_cancel":
		c.state.clearPendingPrice()
		c.reply(ctx, "已取消价格修改。", mainKeyboard())
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

// listPrices 展示当前配置中的 RMB/刀 和派生权重。
func (c telegramControl) listPrices(ctx context.Context) {
	cfg, err := loadRawConfig(c.opts.ConfigPath)
	if err != nil {
		c.reply(ctx, "<b>读取价格失败</b>\n\n<code>"+html.EscapeString(err.Error())+"</code>", mainKeyboard())
		return
	}
	runtimeCfg, err := domain.NormalizeConfig(cfg)
	if err != nil {
		c.reply(ctx, "<b>解析价格失败</b>\n\n<code>"+html.EscapeString(err.Error())+"</code>", mainKeyboard())
		return
	}

	var b strings.Builder
	fmt.Fprintf(&b, "<b>当前渠道价格</b>\n")
	for _, pool := range runtimeCfg.Pools {
		fmt.Fprintf(&b, "\n<b>%s</b>", html.EscapeString(pool.ID))
		if pool.VirtualKey != "" && pool.VirtualKey != pool.ID {
			fmt.Fprintf(&b, "（VK: <code>%s</code>）", html.EscapeString(pool.VirtualKey))
		}
		fmt.Fprintf(&b, "\n")
		for _, provider := range pool.Providers {
			if !provider.AllowedInPool() {
				continue
			}
			price := "-"
			if provider.PriceRMBPerDao > 0 {
				price = fmt.Sprintf("%.6g RMB/刀", provider.PriceRMBPerDao)
			}
			fmt.Fprintf(&b, "- <code>%s</code>：<code>%s</code>，cost_weight <code>%.4f</code>\n",
				html.EscapeString(provider.Name), html.EscapeString(price), effectiveCostWeight(provider, pool.EffectiveRules()))
		}
	}
	c.reply(ctx, b.String(), pricesKeyboard(runtimeCfg))
}

// previewPriceUpdate 预览一次价格修改，不立即写配置。
func (c telegramControl) previewPriceUpdate(ctx context.Context, arg string) {
	update, err := parsePriceUpdate(arg)
	if err != nil {
		c.reply(ctx, "格式不对。例子：<code>/price gpt_low congmingai_openai_lv1 0.055</code>", mainKeyboard())
		return
	}
	_, preview, err := configWithPrice(c.opts.ConfigPath, update.PoolID, update.Provider, update.Price)
	if err != nil {
		c.reply(ctx, "<b>价格预览失败</b>\n\n<code>"+html.EscapeString(err.Error())+"</code>", mainKeyboard())
		return
	}
	update.Preview = preview
	c.state.setPendingPrice(update)
	c.reply(ctx, pricePreviewText(preview), priceConfirmKeyboard())
}

// choosePricePool 处理“调整价格”入口和 pool 选择。
func (c telegramControl) choosePricePool(ctx context.Context, arg string) {
	runtimeCfg, err := loadRuntimeConfig(c.opts.ConfigPath)
	if err != nil {
		c.reply(ctx, "<b>读取配置失败</b>\n\n<code>"+html.EscapeString(err.Error())+"</code>", mainKeyboard())
		return
	}
	index, err := parseCallbackIndex(arg)
	if err != nil {
		c.reply(ctx, "<b>选择无效</b>\n\n请重新点“价格”。", pricesKeyboard(runtimeCfg))
		return
	}
	if index < 0 {
		c.reply(ctx, "<b>选择要调整的 key</b>", pricePoolsKeyboard(runtimeCfg))
		return
	}
	if index >= len(runtimeCfg.Pools) {
		c.reply(ctx, "<b>选择无效</b>\n\n请重新选择 key。", pricePoolsKeyboard(runtimeCfg))
		return
	}
	pool := runtimeCfg.Pools[index]
	c.reply(ctx, fmt.Sprintf("<b>选择渠道</b>\n\nKey：<code>%s</code>", html.EscapeString(pool.ID)), priceProvidersKeyboard(pool, index))
}

// choosePriceProvider 保存按钮选中的 provider，然后等待用户输入 RMB/刀。
func (c telegramControl) choosePriceProvider(ctx context.Context, arg string) {
	runtimeCfg, err := loadRuntimeConfig(c.opts.ConfigPath)
	if err != nil {
		c.reply(ctx, "<b>读取配置失败</b>\n\n<code>"+html.EscapeString(err.Error())+"</code>", mainKeyboard())
		return
	}
	poolIndex, providerIndex, err := parseProviderCallback(arg)
	if err != nil || poolIndex < 0 || providerIndex < 0 || poolIndex >= len(runtimeCfg.Pools) {
		c.reply(ctx, "<b>选择无效</b>\n\n请重新点“调整价格”。", pricePoolsKeyboard(runtimeCfg))
		return
	}
	pool := runtimeCfg.Pools[poolIndex]
	providers := allowedProviders(pool)
	if providerIndex >= len(providers) {
		c.reply(ctx, "<b>选择无效</b>\n\n请重新选择渠道。", priceProvidersKeyboard(pool, poolIndex))
		return
	}
	provider := providers[providerIndex]
	c.state.setPriceDraft(priceDraft{PoolID: pool.ID, Provider: provider.Name})
	current := "未填写"
	if provider.PriceRMBPerDao > 0 {
		current = fmt.Sprintf("%.6g RMB/刀", provider.PriceRMBPerDao)
	}
	c.reply(ctx, fmt.Sprintf("<b>输入新价格</b>\n\nKey：<code>%s</code>\n渠道：<code>%s</code>\n当前价格：<code>%s</code>\n\n直接发送类似 <code>0.055 RMB/刀</code> 或 <code>0.055</code>。",
		html.EscapeString(pool.ID),
		html.EscapeString(provider.Name),
		html.EscapeString(current),
	), priceInputKeyboard())
}

// previewDraftPriceUpdate 把按钮选择后的普通文本当成 RMB/刀 价格。
func (c telegramControl) previewDraftPriceUpdate(ctx context.Context, raw string) {
	draft, ok := c.state.getPriceDraft()
	if !ok {
		c.reply(ctx, "没有正在调整的价格。请先点“价格”再点“调整价格”。", mainKeyboard())
		return
	}
	price, err := parsePriceText(raw)
	if err != nil {
		c.reply(ctx, "价格格式不对。请发送类似 <code>0.055 RMB/刀</code> 或 <code>0.055</code>。", priceInputKeyboard())
		return
	}
	update := priceUpdate{PoolID: draft.PoolID, Provider: draft.Provider, Price: price}
	_, preview, err := configWithPrice(c.opts.ConfigPath, update.PoolID, update.Provider, update.Price)
	if err != nil {
		c.reply(ctx, "<b>价格预览失败</b>\n\n<code>"+html.EscapeString(err.Error())+"</code>", mainKeyboard())
		return
	}
	update.Preview = preview
	c.state.clearPriceDraft()
	c.state.setPendingPrice(update)
	c.reply(ctx, pricePreviewText(preview), priceConfirmKeyboard())
}

// applyPendingPrice 写入最近一次预览过的价格修改。
func (c telegramControl) applyPendingPrice(ctx context.Context) {
	update, ok := c.state.getPendingPrice()
	if !ok {
		c.reply(ctx, "没有待确认的价格修改。先发送 <code>/price pool provider 0.055</code>。", mainKeyboard())
		return
	}
	cfg, preview, err := configWithPrice(c.opts.ConfigPath, update.PoolID, update.Provider, update.Price)
	if err != nil {
		c.reply(ctx, "<b>价格写入失败</b>\n\n<code>"+html.EscapeString(err.Error())+"</code>", mainKeyboard())
		return
	}
	if err := writeRawConfig(c.opts.ConfigPath, cfg); err != nil {
		c.reply(ctx, "<b>价格写入失败</b>\n\n<code>"+html.EscapeString(err.Error())+"</code>", mainKeyboard())
		return
	}
	c.state.clearPendingPrice()
	c.reply(ctx, "<b>价格已写入配置</b>\n\n"+pricePreviewText(preview), mainKeyboard())
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

// isTelegramTextCommand 判断普通文本是不是 Telegram 命令或按钮 callback。
func isTelegramTextCommand(command string) bool {
	return command != "" && (strings.HasPrefix(command, "/") || strings.Contains(command, "_") || command == "help" || command == "status" || command == "last" || command == "run" || command == "prices" || command == "price")
}

// telegramCommands 返回 Telegram 命令菜单。
func telegramCommands() []notify.TelegramBotCommand {
	return []notify.TelegramBotCommand{
		{Command: "status", Description: "查看调度器状态"},
		{Command: "last", Description: "查看最近一次调度计划摘要"},
		{Command: "run", Description: "立即执行一次 dry-run 预览"},
		{Command: "prices", Description: "查看当前渠道价格"},
		{Command: "price", Description: "预览价格修改：/price pool provider 0.055"},
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
			{Text: "价格", CallbackData: "prices"},
		},
		{
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
<code>/prices</code> - 查看当前配置里的 RMB/刀价格
<code>/price pool provider 0.055</code> - 预览修改某个渠道价格
<code>/mute 1h</code> - 静音变更通知，调度器仍继续运行
<code>/unmute</code> - 恢复变更通知
<code>/help</code> - 查看帮助

安全边界：
Telegram 价格命令只写调度器配置。真正写 Bifrost 仍只由 daemon 的 <code>config mode</code> 和 <code>--apply</code> 控制。
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

// parsePriceUpdate 解析 /price pool provider 0.055。
func parsePriceUpdate(arg string) (priceUpdate, error) {
	fields := strings.Fields(arg)
	if len(fields) < 3 {
		return priceUpdate{}, fmt.Errorf("price command requires pool provider price")
	}
	price, err := parsePriceText(strings.Join(fields[2:], " "))
	if err != nil {
		return priceUpdate{}, err
	}
	return priceUpdate{PoolID: fields[0], Provider: fields[1], Price: price}, nil
}

// parsePriceText 解析用户输入的价格文本，例如 0.055 或 0.055 RMB/刀。
func parsePriceText(text string) (float64, error) {
	fields := strings.Fields(strings.TrimSpace(text))
	if len(fields) == 0 {
		return 0, fmt.Errorf("price is required")
	}
	price, err := strconv.ParseFloat(fields[0], 64)
	if err != nil || price <= 0 {
		return 0, fmt.Errorf("price must be positive")
	}
	return price, nil
}

// parseCallbackIndex 解析按钮里的单个下标。
func parseCallbackIndex(arg string) (int, error) {
	arg = strings.TrimSpace(arg)
	if arg == "" {
		return -1, nil
	}
	return strconv.Atoi(arg)
}

// parseProviderCallback 解析 provider 选择按钮里的 pool/provider 下标。
func parseProviderCallback(arg string) (int, int, error) {
	parts := strings.Split(strings.TrimSpace(arg), " ")
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("provider callback requires two indexes")
	}
	poolIndex, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, err
	}
	providerIndex, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, 0, err
	}
	return poolIndex, providerIndex, nil
}

// loadRawConfig 读取未标准化的 JSON 配置。
func loadRawConfig(path string) (domain.Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return domain.Config{}, err
	}
	var cfg domain.Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return domain.Config{}, err
	}
	return cfg, nil
}

// loadRuntimeConfig 读取并标准化配置。
func loadRuntimeConfig(path string) (domain.RuntimeConfig, error) {
	cfg, err := loadRawConfig(path)
	if err != nil {
		return domain.RuntimeConfig{}, err
	}
	return domain.NormalizeConfig(cfg)
}

// writeRawConfig 写回 JSON 配置。
func writeRawConfig(path string, cfg domain.Config) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0600)
}

// configWithPrice 返回修改价格后的配置和预览。
func configWithPrice(path, poolID, providerName string, price float64) (domain.Config, priceUpdatePreview, error) {
	cfg, err := loadRawConfig(path)
	if err != nil {
		return domain.Config{}, priceUpdatePreview{}, err
	}
	poolRef := poolID
	oldRuntimeCfg, err := domain.NormalizeConfig(cfg)
	if err != nil {
		return domain.Config{}, priceUpdatePreview{}, err
	}
	oldRuntimePool, oldRuntimeProvider, ok := runtimePoolProvider(oldRuntimeCfg, poolRef, providerName)
	if !ok {
		if _, poolOK := runtimePool(oldRuntimeCfg, poolRef); !poolOK {
			return domain.Config{}, priceUpdatePreview{}, fmt.Errorf("pool %q not found", poolRef)
		}
		return domain.Config{}, priceUpdatePreview{}, fmt.Errorf("provider %q not found in pool %q", providerName, poolRef)
	}
	poolID = oldRuntimePool.ID
	if !oldRuntimeProvider.AllowedInPool() {
		return domain.Config{}, priceUpdatePreview{}, fmt.Errorf("provider %q is not allowed in pool %q", providerName, poolID)
	}

	foundPool := false
	foundProvider := false
	oldPrice := 0.0
	for i := range cfg.Pools {
		pool := &cfg.Pools[i]
		if pool.ID != poolID {
			continue
		}
		foundPool = true
		for j := range pool.Providers {
			provider := &pool.Providers[j]
			if provider.Name != providerName {
				continue
			}
			foundProvider = true
			oldPrice = provider.PriceRMBPerDao
			provider.PriceRMBPerDao = price
		}
		if err := fillMissingPoolPrices(pool, oldRuntimePool, oldRuntimeProvider, price); err != nil {
			return domain.Config{}, priceUpdatePreview{}, err
		}
	}
	if !foundPool {
		return domain.Config{}, priceUpdatePreview{}, fmt.Errorf("pool %q not found", poolID)
	}
	if !foundProvider {
		return domain.Config{}, priceUpdatePreview{}, fmt.Errorf("provider %q not found in pool %q", providerName, poolID)
	}
	runtimeCfg, err := domain.NormalizeConfig(cfg)
	if err != nil {
		return domain.Config{}, priceUpdatePreview{}, err
	}
	preview := priceUpdatePreview{
		PoolID:   poolID,
		Provider: providerName,
		OldPrice: oldPrice,
		NewPrice: price,
	}
	newRuntimePool, ok := runtimePool(runtimeCfg, poolID)
	if !ok {
		return domain.Config{}, priceUpdatePreview{}, fmt.Errorf("pool %q not found after normalization", poolID)
	}
	for _, pool := range runtimeCfg.Pools {
		if pool.ID != poolID {
			continue
		}
		for _, provider := range pool.Providers {
			preview.Providers = append(preview.Providers, priceProviderPreview{
				Name:       provider.Name,
				Price:      provider.PriceRMBPerDao,
				CostWeight: provider.CostWeight,
				Allowed:    provider.AllowedInPool(),
			})
		}
	}
	for i := range cfg.Pools {
		pool := &cfg.Pools[i]
		if pool.ID != poolID {
			continue
		}
		for j := range pool.Providers {
			runtimeProvider, ok := providerInPool(newRuntimePool, pool.Providers[j].Name)
			if !ok {
				continue
			}
			pool.Providers[j].CostWeight = runtimeProvider.CostWeight
		}
	}
	return cfg, preview, nil
}

// fillMissingPoolPrices 用旧 cost_weight 比例补齐同池缺失价格。
//
// 老配置可能只有 cost_weight，没有 price_rmb_per_dao。用户在 Telegram 里给一个渠道输入 RMB/刀 后，
// 这里会按旧权重比例反推同池其他渠道价格，再让 NormalizeConfig 统一重新换算 cost_weight。
func fillMissingPoolPrices(pool *domain.PoolConfig, runtimePool domain.PoolConfig, targetProvider domain.ProviderConfig, targetPrice float64) error {
	targetWeight := effectiveCostWeight(targetProvider, runtimePool.EffectiveRules())
	if targetWeight <= 0 {
		return fmt.Errorf("provider %q has no usable cost_weight for price conversion", targetProvider.Name)
	}
	for i := range pool.Providers {
		rawProvider := &pool.Providers[i]
		runtimeProvider, ok := providerInPool(runtimePool, rawProvider.Name)
		if !ok || !runtimeProvider.AllowedInPool() || rawProvider.PriceRMBPerDao > 0 {
			continue
		}
		weight := effectiveCostWeight(runtimeProvider, runtimePool.EffectiveRules())
		if weight <= 0 {
			return fmt.Errorf("provider %q has no usable cost_weight for price conversion", runtimeProvider.Name)
		}
		rawProvider.PriceRMBPerDao = targetPrice * targetWeight / weight
	}
	return nil
}

// effectiveCostWeight 返回真正参与换算的健康目标权重。
func effectiveCostWeight(provider domain.ProviderConfig, rules domain.PoolRules) float64 {
	if provider.CostWeight > 0 {
		return provider.CostWeight
	}
	return rules.DefaultCostWeight
}

// runtimePoolProvider 在已标准化配置里找 pool 和 provider。
func runtimePoolProvider(cfg domain.RuntimeConfig, poolID, providerName string) (domain.PoolConfig, domain.ProviderConfig, bool) {
	pool, ok := runtimePool(cfg, poolID)
	if !ok {
		return domain.PoolConfig{}, domain.ProviderConfig{}, false
	}
	provider, ok := providerInPool(pool, providerName)
	if !ok {
		return domain.PoolConfig{}, domain.ProviderConfig{}, false
	}
	return pool, provider, true
}

// runtimePool 在已标准化配置里找 pool。
func runtimePool(cfg domain.RuntimeConfig, poolID string) (domain.PoolConfig, bool) {
	for _, pool := range cfg.Pools {
		if pool.ID == poolID || pool.VirtualKey == poolID {
			return pool, true
		}
	}
	return domain.PoolConfig{}, false
}

// providerInPool 在 pool 里按名称找 provider。
func providerInPool(pool domain.PoolConfig, providerName string) (domain.ProviderConfig, bool) {
	for _, provider := range pool.Providers {
		if provider.Name == providerName {
			return provider, true
		}
	}
	return domain.ProviderConfig{}, false
}

// pricePreviewText 格式化价格修改预览。
func pricePreviewText(preview priceUpdatePreview) string {
	var b strings.Builder
	fmt.Fprintf(&b, "<b>价格修改预览</b>\n\nPool：<code>%s</code>\nProvider：<code>%s</code>\n价格：<code>%.6g</code> -&gt; <code>%.6g RMB/刀</code>\n\n同池权重：\n",
		html.EscapeString(preview.PoolID),
		html.EscapeString(preview.Provider),
		preview.OldPrice,
		preview.NewPrice,
	)
	for _, provider := range preview.Providers {
		if !provider.Allowed {
			continue
		}
		fmt.Fprintf(&b, "- <code>%s</code>：价格 <code>%.6g</code>，cost_weight <code>%.4f</code>\n",
			html.EscapeString(provider.Name),
			provider.Price,
			provider.CostWeight,
		)
	}
	return b.String()
}

// priceConfirmKeyboard 返回价格写入确认按钮。
func priceConfirmKeyboard() [][]notify.TelegramInlineButton {
	return [][]notify.TelegramInlineButton{
		{
			{Text: "确认写入配置", CallbackData: "price_apply"},
			{Text: "取消", CallbackData: "price_cancel"},
		},
	}
}

// pricesKeyboard 返回价格列表下面的按钮。
func pricesKeyboard(cfg domain.RuntimeConfig) [][]notify.TelegramInlineButton {
	keyboard := [][]notify.TelegramInlineButton{
		{{Text: "调整价格", CallbackData: "price_pool"}},
	}
	return append(keyboard, mainKeyboard()...)
}

// pricePoolsKeyboard 返回 pool/key 选择按钮。
func pricePoolsKeyboard(cfg domain.RuntimeConfig) [][]notify.TelegramInlineButton {
	keyboard := make([][]notify.TelegramInlineButton, 0, len(cfg.Pools)+1)
	for i, pool := range cfg.Pools {
		text := pool.ID
		if pool.VirtualKey != "" && pool.VirtualKey != pool.ID {
			text += " / " + pool.VirtualKey
		}
		keyboard = append(keyboard, []notify.TelegramInlineButton{{Text: text, CallbackData: fmt.Sprintf("price_pool %d", i)}})
	}
	keyboard = append(keyboard, []notify.TelegramInlineButton{{Text: "返回价格", CallbackData: "prices"}})
	return keyboard
}

// priceProvidersKeyboard 返回 provider 选择按钮。
func priceProvidersKeyboard(pool domain.PoolConfig, poolIndex int) [][]notify.TelegramInlineButton {
	providers := allowedProviders(pool)
	keyboard := make([][]notify.TelegramInlineButton, 0, len(providers)+1)
	for i, provider := range providers {
		text := provider.Name
		if provider.PriceRMBPerDao > 0 {
			text = fmt.Sprintf("%s · %.6g RMB/刀", provider.Name, provider.PriceRMBPerDao)
		}
		keyboard = append(keyboard, []notify.TelegramInlineButton{{Text: text, CallbackData: fmt.Sprintf("price_provider %d %d", poolIndex, i)}})
	}
	keyboard = append(keyboard, []notify.TelegramInlineButton{{Text: "返回 key", CallbackData: "price_pool"}})
	return keyboard
}

// priceInputKeyboard 返回等待输入价格时的按钮。
func priceInputKeyboard() [][]notify.TelegramInlineButton {
	return [][]notify.TelegramInlineButton{
		{{Text: "取消", CallbackData: "price_cancel"}},
	}
}

// allowedProviders 返回当前允许进池的 provider。
func allowedProviders(pool domain.PoolConfig) []domain.ProviderConfig {
	providers := make([]domain.ProviderConfig, 0, len(pool.Providers))
	for _, provider := range pool.Providers {
		if provider.AllowedInPool() {
			providers = append(providers, provider)
		}
	}
	return providers
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
