// package main 表示这是一个“可执行程序”的入口包。
//
// Go 规定：只有 package main 里写了 main() 函数，才能编译成可以直接运行的命令。
package main

// import 表示“这个文件要用哪些别的代码包”。
//
// 上面这些是 Go 标准库，例如 context、flag、os、time。
// 下面这些带 github.com/Akuma-real/... 的，是本项目自己的包。
import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	appscheduler "github.com/Akuma-real/bifrost-scheduler/internal/app/scheduler"
	"github.com/Akuma-real/bifrost-scheduler/internal/bifrost"
	domain "github.com/Akuma-real/bifrost-scheduler/internal/domain/scheduler"
	"github.com/Akuma-real/bifrost-scheduler/internal/notify"
	"github.com/Akuma-real/bifrost-scheduler/internal/report"
)

func main() {
	// main 是 Go 程序真正的入口。
	// os.Exit 会把 run() 返回的整数交给操作系统作为退出码。
	os.Exit(run())
}

// func 表示“定义一个函数”，也就是一个可以被调用的命名步骤。
//
// run 是最顶层的命令分发函数。
//
// 如果你刚开始学 Go，可以把这个函数读成：
// “看命令行第一个词是什么，然后决定接下来调用哪个小函数”。
//
// 返回值是进程退出码：
//   - 0 表示成功
//   - 1 通常表示运行失败
//   - 2 通常表示命令用法错误
//   - 10 表示生成的计划里有 critical 级别决策
func run() int {
	// os.Args 是 Go 标准库提供的命令行参数列表。
	// os.Args[0] 通常是程序名，例如 bifrost-scheduler。
	// os.Args[1] 才是用户输入的第一个真正命令，例如 plan、daemon、version。
	// 所以如果长度小于 2，说明用户没有输入子命令。
	if len(os.Args) < 2 {
		usage()
		return 2
	}

	// context.Background() 创建一个最基础的上下文。
	// signal.NotifyContext 会基于它再创建一个可取消的 ctx。
	// 当进程收到 Ctrl+C、SIGINT 或 SIGTERM 时，ctx 会被取消。
	// 后面的网络请求和 daemon 循环可以通过 ctx 知道“该停止了”。
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)

	// defer 表示：等 run() 函数快结束时，再执行 stop()。
	// 这里用来释放 signal.NotifyContext 注册的信号监听。
	defer stop()

	// slog.NewJSONHandler 创建一个 JSON 格式日志处理器。
	// os.Stderr 表示日志写到标准错误输出，避免和正常报告输出混在一起。
	// LevelInfo 表示只输出 info 及以上级别日志。
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	// switch 根据 os.Args[1] 的值选择不同分支。
	// 也就是根据用户输入的子命令决定接下来执行什么。
	switch os.Args[1] {
	case "plan":
		// flag.NewFlagSet 创建一个只属于 plan 子命令的参数集合。
		// flag.ExitOnError 表示参数解析失败时直接退出。
		fs := flag.NewFlagSet("plan", flag.ExitOnError)

		// commonFlags 注册 plan 和 daemon 共用的参数，例如 --config、--format、--api-url。
		// 返回的 opts 是 *options，前面的 * 表示“指向 options 的指针”。
		opts := commonFlags(fs)

		// fs.Bool 注册一个布尔参数 --apply。
		// 第一个 false 是默认值：默认不写线上。
		// 返回的 apply 是 *bool，所以后面使用时要写 *apply 取出真正的 bool 值。
		apply := fs.Bool("apply", false, "apply guarded changes; requires config mode guarded_write")

		// os.Args[2:] 表示从第 3 个命令行参数开始取。
		// 例子：bifrost-scheduler plan --config config.json
		// os.Args[1] 是 plan，os.Args[2:] 就是 --config config.json。
		// _ 表示忽略返回值。这里 Parse 出错会由 flag.ExitOnError 处理。
		_ = fs.Parse(os.Args[2:])

		// *opts 把指针里的 options 取出来。
		// *apply 把指针里的 bool 取出来。
		// runPlan 会真正执行一次调度，并返回退出码。
		return runPlan(ctx, logger, *opts, *apply)
	case "daemon":
		// daemon 和 plan 一样，也先创建属于 daemon 的参数集合。
		fs := flag.NewFlagSet("daemon", flag.ExitOnError)

		// 注册共用参数。
		opts := commonFlags(fs)

		// daemon 也支持 --apply。默认 false，避免默认写线上。
		apply := fs.Bool("apply", false, "apply guarded changes on each interval; requires config mode guarded_write")

		// 注册 --interval 参数。
		// envDuration 会先看环境变量 BIFROST_SCHEDULER_INTERVAL。
		// 如果没配置，就默认 5 分钟跑一次。
		interval := fs.Duration("interval", envDuration("BIFROST_SCHEDULER_INTERVAL", 5*time.Minute), "run interval")

		// 解析 daemon 后面的参数。
		_ = fs.Parse(os.Args[2:])

		// runDaemon 会进入循环，每隔 interval 执行一次调度。
		return runDaemon(ctx, logger, *opts, *interval, *apply)
	case "version":
		// version 不需要读取配置，也不需要连接 Bifrost，只打印版本信息。
		fmt.Println("bifrost-scheduler dev")
		return 0
	default:
		// 用户输入了未知子命令，例如 bifrost-scheduler abc。
		// 打印用法，然后返回 2 表示命令用法错误。
		usage()
		return 2
	}
}

// type 表示“定义一种新类型”。
//
// struct 表示“一组字段放在一起”。options 这个结构体保存命令行参数和环境变量解析后的结果。
// 这样后面的函数只需要传一个 opts，而不是分别传 ConfigPath、APIURL、APIUsername 等很多参数。
type options struct {
	// ConfigPath 是调度器配置文件路径。
	ConfigPath string
	// APIURL 是 Bifrost API 地址。命令行/env 可以覆盖 config.json 里的 api.base_url。
	APIURL string
	// APIUsername 是 Bifrost 登录用户名。
	APIUsername string
	// APIPassword 是 Bifrost 登录密码。
	APIPassword string
	// APITimeout 是每个 Bifrost HTTP 请求的超时时间。
	APITimeout time.Duration
	// Format 是输出格式：markdown 或 json。
	Format string
	// LogFile 是完整报告和状态日志写入的文件路径。
	LogFile string
	// LogMaxSize 是单个日志文件最大大小，例如 10MB。
	LogMaxSize string
	// LogMaxBackups 是最多保留几个历史日志文件。
	LogMaxBackups int
	// LogStdout 表示配置了日志文件时，完整报告是否仍然输出到 stdout。
	LogStdout bool
	// TelegramBotToken 是 Telegram BotFather 提供的 bot token。
	TelegramBotToken string
	// TelegramChatID 是 Telegram 目标 chat id。
	TelegramChatID string
	// TelegramThreadID 是 Telegram 群组话题 ID，可选。
	TelegramThreadID string
	// TelegramInteractive 表示是否启用 Telegram 交互指令。
	TelegramInteractive bool
}

// commonFlags 定义 plan 和 daemon 两个命令共用的参数。
//
// flag 是命令行选项，例如：
//
//	--config config.json
//	--format markdown
//
// 大多数 flag 也支持环境变量作为默认值，因为 Docker 部署时用 .env 文件更方便。
func commonFlags(fs *flag.FlagSet) *options {
	// &options{} 创建 options 结构体并返回它的地址。
	// 后面的 StringVar/BoolVar 会把解析结果直接写入这个结构体。
	opts := &options{}

	// StringVar 注册字符串参数。
	// 第一个参数是要写入的变量地址，例如 &opts.ConfigPath。
	// 第二个参数是命令行参数名，例如 --config。
	// 第三个参数是默认值。
	// 第四个参数是 help 文案。
	fs.StringVar(&opts.ConfigPath, "config", envDefault("BIFROST_SCHEDULER_CONFIG", "config.example.json"), "scheduler config JSON path")
	fs.StringVar(&opts.APIURL, "api-url", os.Getenv("BIFROST_API_URL"), "Bifrost API base URL")
	fs.StringVar(&opts.APIUsername, "api-username", os.Getenv("BIFROST_API_USERNAME"), "Bifrost dashboard/admin username")
	fs.StringVar(&opts.APIPassword, "api-password", os.Getenv("BIFROST_API_PASSWORD"), "Bifrost dashboard/admin password")

	// DurationVar 注册 time.Duration 参数，例如 30s、5m。
	fs.DurationVar(&opts.APITimeout, "api-timeout", envDuration("BIFROST_API_TIMEOUT", 30*time.Second), "Bifrost API request timeout")
	fs.StringVar(&opts.Format, "format", envDefault("BIFROST_SCHEDULER_FORMAT", "markdown"), "output format: markdown or json")
	fs.StringVar(&opts.LogFile, "log-file", os.Getenv("BIFROST_SCHEDULER_LOG_FILE"), "rotating log file path; empty writes to stdout/stderr only")
	fs.StringVar(&opts.LogMaxSize, "log-max-size", envDefault("BIFROST_SCHEDULER_LOG_MAX_SIZE", "10MB"), "maximum size of one log file before rotation")

	// IntVar 注册整数参数。
	fs.IntVar(&opts.LogMaxBackups, "log-max-backups", envInt("BIFROST_SCHEDULER_LOG_MAX_BACKUPS", 5), "number of rotated log files to keep")

	// BoolVar 注册布尔参数。
	fs.BoolVar(&opts.LogStdout, "log-stdout", envBool("BIFROST_SCHEDULER_LOG_STDOUT", true), "also write full plan output to stdout when log-file is set; status logs still go to stderr")

	// Telegram 通知默认关闭；只有 token 和 chat_id 都配置后才启用。
	fs.StringVar(&opts.TelegramBotToken, "telegram-bot-token", os.Getenv("BIFROST_SCHEDULER_TG_BOT_TOKEN"), "Telegram bot token for change notifications")
	fs.StringVar(&opts.TelegramChatID, "telegram-chat-id", os.Getenv("BIFROST_SCHEDULER_TG_CHAT_ID"), "Telegram chat id for change notifications")
	fs.StringVar(&opts.TelegramThreadID, "telegram-thread-id", os.Getenv("BIFROST_SCHEDULER_TG_THREAD_ID"), "optional Telegram forum topic thread id")
	fs.BoolVar(&opts.TelegramInteractive, "telegram-interactive", envBool("BIFROST_SCHEDULER_TG_INTERACTIVE", false), "enable Telegram bot commands in daemon mode")
	return opts
}

// runPlan 执行一次调度。
//
// 这个函数的输入参数：
//   - ctx：停止信号。进程收到 Ctrl+C 或 stop 时，可以通知正在运行的逻辑退出。
//   - logger：写简短的 JSON 状态日志。
//   - opts：命令行参数和环境变量解析后的配置。
//   - apply：这次运行是否允许执行受保护的写入。
//
// 这个函数的返回值：
//   - 一个整数退出码，交给操作系统判断命令是否成功。
func runPlan(ctx context.Context, logger *slog.Logger, opts options, apply bool) int {
	// setupLogging 可能会返回新的 logger 和 output。
	// 因为用户可能配置了 log-file，此时日志要同时写 stderr 和文件。
	logger, output, closeLogs, err := setupLogging(opts)
	if err != nil {
		logger.Error("setup logging failed", "error", err)
		return 1
	}
	// closeLogs 是 setupLogging 返回的清理函数。
	defer closeLogs()

	// buildPlan 真正生成一次调度计划。
	plan, err := buildPlan(ctx, opts, apply)
	if err != nil {
		logger.Error("plan failed", "error", err)
		return 1
	}
	// 把计划写成用户指定格式。
	if err := report.WritePlan(output, plan, opts.Format); err != nil {
		logger.Error("write plan failed", "error", err)
		return 1
	}
	// 写一行简短日志，方便 docker logs 查看。
	logPlanSummary(logger, plan)
	// plan 命令是一次性运行；只有 warning/critical 问题决策才发送 Telegram 通知。
	notifyPlan(ctx, logger, opts, plan)
	// 如果有 critical 决策，命令返回 10，方便外部自动化识别严重问题。
	if hasCritical(plan) {
		return 10
	}
	return 0
}

// runDaemon 会一直重复执行调度，直到进程被停止。
//
// time.Ticker 是 Go 标准库里的定时器，意思是“每隔一段时间触发一次”。
// select 这一段会等待两种情况：
//   - ctx.Done()：进程应该停止了。
//   - ticker.C：到了下一轮调度时间。
func runDaemon(ctx context.Context, logger *slog.Logger, opts options, interval time.Duration, apply bool) int {
	logger, output, closeLogs, err := setupLogging(opts)
	if err != nil {
		logger.Error("setup logging failed", "error", err)
		return 1
	}
	defer closeLogs()

	// interval 必须大于 0，否则 ticker 没法工作。
	if interval <= 0 {
		logger.Error("interval must be positive", "interval", interval.String())
		return 2
	}
	// NewTicker 创建定时器。ticker.C 是一个 channel。
	// 每到 interval，ticker.C 就会收到一个时间值。
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// lastNotificationFingerprint 记录上一次已通知的决策指纹。
	// 如果下一轮仍然是完全相同的变更，就不重复通知。
	lastNotificationFingerprint := ""

	// daemonState 保存 daemon 最近一次运行结果。
	// Telegram 交互指令会读取它来回答 /status 和 /last。
	state := newDaemonState(interval, apply)

	// 如果开启 Telegram 交互，就启动一个 goroutine 并行监听用户命令。
	// goroutine 可以理解成 Go 里的轻量后台任务。
	// 它和下面的调度循环同时运行，通过 state 共享最近状态。
	stopTelegramControl := startTelegramControl(ctx, logger, opts, state)
	defer stopTelegramControl()

	// for { ... } 是无限循环。
	// daemon 模式就是靠这个循环一直运行。
	for {
		// 每轮先立即跑一次计划，而不是等第一个 interval。
		plan, err := buildPlan(ctx, opts, apply)
		if err != nil {
			state.recordError(err)
			logger.Error("plan failed", "error", err)
		} else if err := report.WritePlan(output, plan, opts.Format); err != nil {
			state.recordError(err)
			logger.Error("write plan failed", "error", err)
		} else {
			state.recordPlan(plan)
			logPlanSummary(logger, plan)
			if state.notificationsMuted() {
				logger.Info("telegram notification muted", "until", state.mutedUntilText())
			} else {
				lastNotificationFingerprint = notifyPlanOnce(ctx, logger, opts, plan, lastNotificationFingerprint)
			}
		}

		// select 会等待多个 channel。
		// 哪个先准备好，就执行哪个 case。
		select {
		case <-ctx.Done():
			// ctx.Done() 收到信号，说明进程要停止。
			logger.Info("daemon stopped")
			return 0
		case <-ticker.C:
			// 到了下一轮时间，什么都不做，循环会回到顶部再跑一次。
		}
	}
}

// setupLogging 决定日志和完整调度报告写到哪里。
//
// 这个函数返回三个东西：
//   - logger：写短 JSON 状态日志。
//   - output：完整调度报告写入的位置。
//   - close 函数：清理函数，用来关闭日志文件。
//
// “返回一个清理函数”是 Go 里常见写法：谁打开文件，谁就提供关闭它的方法。
func setupLogging(opts options) (*slog.Logger, io.Writer, func(), error) {
	// 没有配置日志文件时：
	//   - 状态日志写到 stderr。
	//   - 完整报告写到 stdout。
	//   - close 函数什么都不做。
	if opts.LogFile == "" {
		return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})), os.Stdout, func() {}, nil
	}

	// parseByteSize 把 "10MB" 这种字符串转成字节数。
	maxBytes, err := parseByteSize(opts.LogMaxSize)
	if err != nil {
		return nil, nil, nil, err
	}
	// newRotatingFile 创建一个带轮转能力的日志文件。
	logFile, err := newRotatingFile(opts.LogFile, maxBytes, opts.LogMaxBackups)
	if err != nil {
		return nil, nil, nil, err
	}
	// closeFn 是返回给调用者的清理函数。
	// 调用者 defer closeLogs() 就能在退出时关闭日志文件。
	closeFn := func() {
		_ = logFile.Close()
	}

	// output 是完整 Markdown/JSON 报告写到哪里。
	var output io.Writer = logFile

	// loggerOutput 是简短 JSON 状态日志写到哪里。
	// MultiWriter 表示同一份内容同时写到多个地方。
	var loggerOutput io.Writer = io.MultiWriter(os.Stderr, logFile)
	if opts.LogStdout {
		// 如果允许 stdout，就完整报告同时写 stdout 和日志文件。
		output = io.MultiWriter(os.Stdout, logFile)
	}

	return slog.New(slog.NewJSONHandler(loggerOutput, &slog.HandlerOptions{Level: slog.LevelInfo})), output, closeFn, nil
}

// newNotifier 根据 options 创建 Telegram 通知器。
//
// 返回 nil 表示通知没有启用。
func newNotifier(opts options) (*notify.TelegramNotifier, error) {
	return notify.NewTelegramNotifier(notify.TelegramConfig{
		BotToken: opts.TelegramBotToken,
		ChatID:   opts.TelegramChatID,
		ThreadID: opts.TelegramThreadID,
	})
}

// telegramChatIDInt 把配置里的 Telegram chat id 转成 int64。
//
// Telegram 私聊 chat id 通常是正数，群组/频道通常是负数。
func telegramChatIDInt(opts options) (int64, error) {
	value := opts.TelegramChatID
	if value == "" {
		return 0, fmt.Errorf("telegram chat id is empty")
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("telegram chat id must be an integer: %w", err)
	}
	return parsed, nil
}

// notifyPlan 发送一次计划通知。
//
// 通知失败只写日志，不影响调度器本身。
func notifyPlan(ctx context.Context, logger *slog.Logger, opts options, plan domain.Plan) bool {
	plan = notificationPlan(plan)
	notifier, err := newNotifier(opts)
	if err != nil {
		logger.Error("telegram notification setup failed", "error", err)
		return false
	}
	if notifier == nil || len(plan.Decisions) == 0 {
		return false
	}

	if opts.TelegramInteractive {
		// 如果开启了 Telegram 交互，就顺手刷新常驻键盘。
		// 用户不需要记 slash 命令，直接点输入框上方的按钮即可。
		err = notifier.NotifyPlanWithReplyKeyboard(ctx, plan, mainKeyboard())
	} else {
		err = notifier.NotifyPlan(ctx, plan)
	}
	if err != nil {
		logger.Error("telegram notification failed", "error", err)
		return false
	}
	logger.Info("telegram notification sent", "decisions", len(plan.Decisions))
	return true
}

// notifyPlanOnce 在 daemon 模式下只通知新的变更指纹。
//
// 返回值是新的 lastNotificationFingerprint。
func notifyPlanOnce(ctx context.Context, logger *slog.Logger, opts options, plan domain.Plan, lastFingerprint string) string {
	plan = notificationPlan(plan)
	if len(plan.Decisions) == 0 {
		return ""
	}
	fingerprint := notify.Fingerprint(plan)
	if fingerprint == lastFingerprint {
		return lastFingerprint
	}
	if notifyPlan(ctx, logger, opts, plan) {
		return fingerprint
	}
	return lastFingerprint
}

// notificationPlan 只保留需要主动推送到 Telegram 的问题决策。
//
// info 级别通常是健康恢复或正常调权，完整报告里仍然会记录，
// 但不应该像渠道故障一样刷 Telegram。
func notificationPlan(plan domain.Plan) domain.Plan {
	filtered := plan
	filtered.Decisions = make([]domain.Decision, 0, len(plan.Decisions))
	for _, decision := range plan.Decisions {
		if decision.Severity == "warning" || decision.Severity == "critical" {
			filtered.Decisions = append(filtered.Decisions, decision)
		}
	}
	return filtered
}

// logPlanSummary 在每次调度后写一行简短摘要。
//
// 完整计划可能很长，不适合全部塞进 docker logs。
// 所以这里只统计决策数量、严重级别、执行结果，再写一行 summary。
func logPlanSummary(logger *slog.Logger, plan domain.Plan) {
	// 下面这些计数器最后会写成一行 JSON 日志。
	critical := 0
	warning := 0
	applied := 0
	skipped := 0
	failed := 0

	// range 遍历所有决策。
	for _, decision := range plan.Decisions {
		// 根据严重级别计数。
		switch decision.Severity {
		case "critical":
			critical++
		case "warning":
			warning++
		}
		// Apply 为 nil 表示这次只是 dry-run，或者没有进入执行阶段。
		if decision.Apply == nil {
			continue
		}
		// Applied/Skipped/failed 三类执行结果分开统计。
		if decision.Apply.Applied {
			applied++
		} else if decision.Apply.Skipped {
			skipped++
		} else {
			failed++
		}
	}
	// logger.Info 写一行结构化日志。
	// 后面的 "mode", plan.Mode 这种是一组 key/value。
	logger.Info(
		"plan completed",
		"mode", plan.Mode,
		"apply_enabled", plan.ApplyEnabled,
		"decisions", len(plan.Decisions),
		"critical", critical,
		"warning", warning,
		"applied", applied,
		"skipped", skipped,
		"failed", failed,
	)
}

// buildPlan 把项目各层串起来。
//
// 这是一次调度的主流程：
//  1. 从 JSON 文件读取配置。
//  2. 创建 Bifrost API 客户端。
//  3. 登录 Bifrost。
//  4. 创建应用层 Planner。
//  5. 生成调度计划。
//
// 注意：这里不写健康判断规则。健康规则放在 internal/domain/scheduler 里。
// 这样 CLI 入口只负责“把东西接起来”，不会变成一坨业务判断。
func buildPlan(ctx context.Context, opts options, apply bool) (domain.Plan, error) {
	// 读取并标准化 config.json。
	cfg, err := appscheduler.LoadConfig(opts.ConfigPath)
	if err != nil {
		return domain.Plan{}, err
	}
	// 命令行或环境变量里的 API URL 优先。
	// 如果没传，就用 config.json 里的 api.base_url。
	apiURL := opts.APIURL
	if apiURL == "" {
		apiURL = cfg.API.BaseURL
	}
	// 创建 Bifrost API 客户端。
	client, err := bifrost.NewBifrostClient(bifrost.ClientOptions{
		BaseURL:  apiURL,
		Username: opts.APIUsername,
		Password: opts.APIPassword,
		Paths:    cfg.API.Paths,
		Timeout:  opts.APITimeout,
	})
	if err != nil {
		return domain.Plan{}, err
	}
	// defer client.Close() 表示 buildPlan 结束时关闭客户端资源。
	// 现在 Close 是空实现，但保留这个习惯方便以后扩展。
	defer client.Close()
	// 登录 Bifrost。客户端会保存 token 或 session cookie。
	if err := client.Login(ctx); err != nil {
		return domain.Plan{}, err
	}

	// Planner 是应用层用例对象，负责“加载状态 -> 加载指标 -> 生成决策 -> 可选执行”。
	planner := appscheduler.NewPlanner(cfg, client, time.Now())
	return planner.BuildPlan(ctx, apply)
}

// hasCritical 根据计划里有没有 critical 决策，决定 CLI 是否返回非 0 退出码。
//
// 调度器会先输出计划；如果发现 critical，再用非 0 退出码告诉脚本或 GitHub Actions：
// “这次检查发现严重问题”。
func hasCritical(plan domain.Plan) bool {
	for _, decision := range plan.Decisions {
		if decision.Severity == "critical" {
			return true
		}
	}
	return false
}

// usage 打印命令用法。
//
// fmt.Fprint 的第一个参数是写到哪里。
// 这里写到 os.Stderr，表示这是给用户看的命令错误提示。
func usage() {
	fmt.Fprint(os.Stderr, `usage: bifrost-scheduler <command> [flags]

commands:
  plan     Build a dry-run scheduler plan. Add --apply only with guarded_write config.
  daemon   Run the scheduler plan loop.
  version  Print version.
`)
}

// envDefault 从环境变量读取字符串配置。
//
// 如果环境变量不存在或为空，就使用 fallback 默认值。
func envDefault(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

// envDuration 从环境变量读取 time.Duration。
//
// 例如 BIFROST_SCHEDULER_INTERVAL=5m 会解析成 5 分钟。
// 如果没写或写错，就使用 fallback，避免程序因为可选环境变量写错直接崩掉。
func envDuration(key string, fallback time.Duration) time.Duration {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return fallback
	}
	return parsed
}

// envInt 从环境变量读取正整数。
func envInt(key string, fallback int) int {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	parsed, err := parsePositiveInt(value)
	if err != nil {
		return fallback
	}
	return parsed
}

// envBool 从环境变量读取布尔值。
//
// 支持 1/0、true/false、yes/no、on/off。
func envBool(key string, fallback bool) bool {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	switch value {
	case "1", "true", "TRUE", "yes", "YES", "on", "ON":
		return true
	case "0", "false", "FALSE", "no", "NO", "off", "OFF":
		return false
	default:
		return fallback
	}
}
