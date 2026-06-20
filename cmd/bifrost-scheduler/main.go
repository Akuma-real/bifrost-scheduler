package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Akuma-real/bifrost-scheduler/internal/scheduler"
)

func main() {
	os.Exit(run())
}

func run() int {
	if len(os.Args) < 2 {
		usage()
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	switch os.Args[1] {
	case "plan":
		fs := flag.NewFlagSet("plan", flag.ExitOnError)
		opts := commonFlags(fs)
		apply := fs.Bool("apply", false, "apply guarded changes; requires config mode guarded_write")
		_ = fs.Parse(os.Args[2:])
		return runPlan(ctx, logger, *opts, *apply)
	case "daemon":
		fs := flag.NewFlagSet("daemon", flag.ExitOnError)
		opts := commonFlags(fs)
		apply := fs.Bool("apply", false, "apply guarded changes on each interval; requires config mode guarded_write")
		interval := fs.Duration("interval", envDuration("BIFROST_SCHEDULER_INTERVAL", 5*time.Minute), "run interval")
		_ = fs.Parse(os.Args[2:])
		return runDaemon(ctx, logger, *opts, *interval, *apply)
	case "version":
		fmt.Println("bifrost-scheduler dev")
		return 0
	default:
		usage()
		return 2
	}
}

type options struct {
	ConfigPath    string
	APIURL        string
	APIUsername   string
	APIPassword   string
	APITimeout    time.Duration
	Format        string
	LogFile       string
	LogMaxSize    string
	LogMaxBackups int
	LogStdout     bool
}

func commonFlags(fs *flag.FlagSet) *options {
	opts := &options{}
	fs.StringVar(&opts.ConfigPath, "config", envDefault("BIFROST_SCHEDULER_CONFIG", "config.example.json"), "scheduler config JSON path")
	fs.StringVar(&opts.APIURL, "api-url", os.Getenv("BIFROST_API_URL"), "Bifrost API base URL")
	fs.StringVar(&opts.APIUsername, "api-username", os.Getenv("BIFROST_API_USERNAME"), "Bifrost dashboard/admin username")
	fs.StringVar(&opts.APIPassword, "api-password", os.Getenv("BIFROST_API_PASSWORD"), "Bifrost dashboard/admin password")
	fs.DurationVar(&opts.APITimeout, "api-timeout", envDuration("BIFROST_API_TIMEOUT", 30*time.Second), "Bifrost API request timeout")
	fs.StringVar(&opts.Format, "format", envDefault("BIFROST_SCHEDULER_FORMAT", "markdown"), "output format: markdown or json")
	fs.StringVar(&opts.LogFile, "log-file", os.Getenv("BIFROST_SCHEDULER_LOG_FILE"), "rotating log file path; empty writes to stdout/stderr only")
	fs.StringVar(&opts.LogMaxSize, "log-max-size", envDefault("BIFROST_SCHEDULER_LOG_MAX_SIZE", "10MB"), "maximum size of one log file before rotation")
	fs.IntVar(&opts.LogMaxBackups, "log-max-backups", envInt("BIFROST_SCHEDULER_LOG_MAX_BACKUPS", 5), "number of rotated log files to keep")
	fs.BoolVar(&opts.LogStdout, "log-stdout", envBool("BIFROST_SCHEDULER_LOG_STDOUT", true), "also write full plan output to stdout when log-file is set; status logs still go to stderr")
	return opts
}

func runPlan(ctx context.Context, logger *slog.Logger, opts options, apply bool) int {
	logger, output, closeLogs, err := setupLogging(opts)
	if err != nil {
		logger.Error("setup logging failed", "error", err)
		return 1
	}
	defer closeLogs()

	plan, err := buildPlan(ctx, opts, apply)
	if err != nil {
		logger.Error("plan failed", "error", err)
		return 1
	}
	if err := scheduler.WritePlan(output, plan, opts.Format); err != nil {
		logger.Error("write plan failed", "error", err)
		return 1
	}
	logPlanSummary(logger, plan)
	if hasCritical(plan) {
		return 10
	}
	return 0
}

func runDaemon(ctx context.Context, logger *slog.Logger, opts options, interval time.Duration, apply bool) int {
	logger, output, closeLogs, err := setupLogging(opts)
	if err != nil {
		logger.Error("setup logging failed", "error", err)
		return 1
	}
	defer closeLogs()

	if interval <= 0 {
		logger.Error("interval must be positive", "interval", interval.String())
		return 2
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		plan, err := buildPlan(ctx, opts, apply)
		if err != nil {
			logger.Error("plan failed", "error", err)
		} else if err := scheduler.WritePlan(output, plan, opts.Format); err != nil {
			logger.Error("write plan failed", "error", err)
		} else {
			logPlanSummary(logger, plan)
		}

		select {
		case <-ctx.Done():
			logger.Info("daemon stopped")
			return 0
		case <-ticker.C:
		}
	}
}

func setupLogging(opts options) (*slog.Logger, io.Writer, func(), error) {
	if opts.LogFile == "" {
		return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})), os.Stdout, func() {}, nil
	}

	maxBytes, err := parseByteSize(opts.LogMaxSize)
	if err != nil {
		return nil, nil, nil, err
	}
	logFile, err := newRotatingFile(opts.LogFile, maxBytes, opts.LogMaxBackups)
	if err != nil {
		return nil, nil, nil, err
	}
	closeFn := func() {
		_ = logFile.Close()
	}

	var output io.Writer = logFile
	var loggerOutput io.Writer = io.MultiWriter(os.Stderr, logFile)
	if opts.LogStdout {
		output = io.MultiWriter(os.Stdout, logFile)
	}

	return slog.New(slog.NewJSONHandler(loggerOutput, &slog.HandlerOptions{Level: slog.LevelInfo})), output, closeFn, nil
}

func logPlanSummary(logger *slog.Logger, plan scheduler.Plan) {
	critical := 0
	warning := 0
	applied := 0
	skipped := 0
	failed := 0
	for _, decision := range plan.Decisions {
		switch decision.Severity {
		case "critical":
			critical++
		case "warning":
			warning++
		}
		if decision.Apply == nil {
			continue
		}
		if decision.Apply.Applied {
			applied++
		} else if decision.Apply.Skipped {
			skipped++
		} else {
			failed++
		}
	}
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

func buildPlan(ctx context.Context, opts options, apply bool) (scheduler.Plan, error) {
	cfg, err := scheduler.LoadConfig(opts.ConfigPath)
	if err != nil {
		return scheduler.Plan{}, err
	}
	apiURL := opts.APIURL
	if apiURL == "" {
		apiURL = cfg.API.BaseURL
	}
	client, err := scheduler.NewBifrostClient(scheduler.ClientOptions{
		BaseURL:  apiURL,
		Username: opts.APIUsername,
		Password: opts.APIPassword,
		Paths:    cfg.API.Paths,
		Timeout:  opts.APITimeout,
	})
	if err != nil {
		return scheduler.Plan{}, err
	}
	defer client.Close()
	if err := client.Login(ctx); err != nil {
		return scheduler.Plan{}, err
	}

	planner := scheduler.NewPlanner(cfg, client, time.Now())
	return planner.BuildPlan(ctx, apply)
}

func hasCritical(plan scheduler.Plan) bool {
	for _, decision := range plan.Decisions {
		if decision.Severity == "critical" {
			return true
		}
	}
	return false
}

func usage() {
	fmt.Fprint(os.Stderr, `usage: bifrost-scheduler <command> [flags]

commands:
  plan     Build a dry-run scheduler plan. Add --apply only with guarded_write config.
  daemon   Run the scheduler plan loop.
  version  Print version.
`)
}

func envDefault(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

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
