// package main 表示测试同一个可执行程序包里的内部函数。
package main

// import 表示测试用到的包。
//
// errors：构造测试错误。
// strings：检查输出文本是否包含关键内容。
// testing：Go 标准测试包。
// time：构造间隔、静音时间和固定计划时间。
// domain：构造测试用调度计划。
import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	domain "github.com/Akuma-real/bifrost-scheduler/internal/domain/scheduler"
)

// TestSplitTelegramCommand 验证 Telegram 命令解析。
func TestSplitTelegramCommand(t *testing.T) {
	command, arg := splitTelegramCommand(" /mute@my_bot 1h ")
	if command != "/mute" {
		t.Fatalf("command = %q, want /mute", command)
	}
	if arg != "1h" {
		t.Fatalf("arg = %q, want 1h", arg)
	}
}

// TestDaemonStateRecordsPlanAndMute 验证 daemon 状态记录、静音和恢复。
func TestDaemonStateRecordsPlanAndMute(t *testing.T) {
	state := newDaemonState(5*time.Minute, true)
	plan := domain.Plan{
		GeneratedAt:  time.Date(2026, 6, 20, 9, 0, 0, 0, time.UTC),
		Mode:         "guarded_write",
		ApplyEnabled: true,
		Decisions: []domain.Decision{{
			PoolID:        "gpt_low",
			VirtualKey:    "gpt_low",
			Provider:      "provider_a",
			Action:        "set_weight",
			CurrentWeight: 0.9,
			TargetWeight:  0.05,
			Reason:        "error rate exceeded threshold",
		}},
	}

	state.recordError(errors.New("temporary error"))
	if got := state.snapshot().lastError; got != "temporary error" {
		t.Fatalf("lastError = %q, want temporary error", got)
	}

	state.recordPlan(plan)
	snapshot := state.snapshot()
	if snapshot.lastError != "" {
		t.Fatalf("lastError = %q, want empty after successful plan", snapshot.lastError)
	}
	if snapshot.lastPlan == nil || len(snapshot.lastPlan.Decisions) != 1 {
		t.Fatalf("lastPlan = %+v, want one decision", snapshot.lastPlan)
	}

	state.muteFor(time.Hour)
	if !state.notificationsMuted() {
		t.Fatalf("notificationsMuted = false, want true")
	}
	state.unmute()
	if state.notificationsMuted() {
		t.Fatalf("notificationsMuted = true, want false after unmute")
	}
}

// TestStatusAndLastPlanText 验证 Telegram 状态文本和最近计划文本可读。
func TestStatusAndLastPlanText(t *testing.T) {
	state := newDaemonState(5*time.Minute, true)
	state.recordPlan(domain.Plan{
		GeneratedAt:  time.Date(2026, 6, 20, 9, 0, 0, 0, time.UTC),
		Mode:         "read_only",
		ApplyEnabled: false,
		Decisions: []domain.Decision{{
			PoolID:        "gpt_stable",
			Provider:      "provider_b",
			Action:        "set_weight_zero",
			CurrentWeight: 0.5,
			TargetWeight:  0,
			Reason:        "provider is not allowed in this pool",
		}},
	})

	status := statusText(state.snapshot())
	if !strings.Contains(status, "<b>Bifrost 调度器状态</b>") {
		t.Fatalf("status text = %q, want title", status)
	}
	if !strings.Contains(status, "运行间隔：<code>5m0s</code>") {
		t.Fatalf("status text = %q, want interval", status)
	}

	last := lastPlanText(state.snapshot())
	if !strings.Contains(last, "<b>最近一次调度计划</b>") {
		t.Fatalf("last text = %q, want title", last)
	}
	if !strings.Contains(last, "<code>provider_b</code>") {
		t.Fatalf("last text = %q, want provider name", last)
	}
}

// TestNotificationPlanKeepsOnlyProblemDecisions 验证自动 TG 只推送 warning/critical 问题。
func TestNotificationPlanKeepsOnlyProblemDecisions(t *testing.T) {
	plan := domain.Plan{Decisions: []domain.Decision{
		{Provider: "healthy_restore", Severity: "info"},
		{Provider: "bad_channel", Severity: "warning"},
		{Provider: "broken_channel", Severity: "critical"},
	}}

	filtered := notificationPlan(plan)
	if len(filtered.Decisions) != 2 {
		t.Fatalf("filtered decisions = %+v, want warning and critical only", filtered.Decisions)
	}
	if filtered.Decisions[0].Provider != "bad_channel" || filtered.Decisions[1].Provider != "broken_channel" {
		t.Fatalf("filtered decisions = %+v, want problem decisions only", filtered.Decisions)
	}
	if len(plan.Decisions) != 3 {
		t.Fatalf("original plan decisions changed: %+v", plan.Decisions)
	}
}

// TestParsePriceUpdate 验证 Telegram 价格命令解析。
func TestParsePriceUpdate(t *testing.T) {
	update, err := parsePriceUpdate("gpt_low provider_a 0.055")
	if err != nil {
		t.Fatalf("parsePriceUpdate returned error: %v", err)
	}
	if update.PoolID != "gpt_low" || update.Provider != "provider_a" || update.Price != 0.055 {
		t.Fatalf("update = %+v, want parsed pool/provider/price", update)
	}
	if _, err := parsePriceUpdate("gpt_low provider_a 0"); err == nil {
		t.Fatalf("parsePriceUpdate returned nil error, want positive price validation")
	}
}

// TestConfigWithPriceBootstrapsMissingPricesFromCostWeight 验证老配置只有 cost_weight 时，
// Telegram 输入一个 RMB/刀 价格就能按旧权重比例补齐同池价格，并自动换算 cost_weight。
func TestConfigWithPriceBootstrapsMissingPricesFromCostWeight(t *testing.T) {
	path := writeTestConfig(t, domain.Config{
		Pools: []domain.PoolConfig{{
			ID:         "gpt_low",
			VirtualKey: "vk_low_text",
			Providers: []domain.ProviderConfig{
				{Name: "cheap", CostWeight: 1},
				{Name: "expensive", CostWeight: 0.5},
				{Name: "quarantine", Role: "quarantine", CostWeight: 1},
			},
		}},
	})

	cfg, preview, err := configWithPrice(path, "gpt_low", "expensive", 0.1)
	if err != nil {
		t.Fatalf("configWithPrice returned error: %v", err)
	}

	providers := cfg.Pools[0].Providers
	if providers[0].PriceRMBPerDao != 0.05 {
		t.Fatalf("cheap price = %.4f, want 0.05", providers[0].PriceRMBPerDao)
	}
	if providers[1].PriceRMBPerDao != 0.1 {
		t.Fatalf("expensive price = %.4f, want 0.1", providers[1].PriceRMBPerDao)
	}
	if providers[0].CostWeight != 1 || providers[1].CostWeight != 0.5 {
		t.Fatalf("providers = %+v, want derived cost weights 1 and 0.5", providers)
	}
	if providers[2].PriceRMBPerDao != 0 {
		t.Fatalf("quarantine price = %.4f, want unchanged", providers[2].PriceRMBPerDao)
	}
	if preview.OldPrice != 0 || preview.NewPrice != 0.1 || len(preview.Providers) != 3 {
		t.Fatalf("preview = %+v, want old/new prices and all providers", preview)
	}
}

// TestConfigWithPriceUsesPriceAsSource 验证价格填全后，cost_weight 由 RMB/刀 自动派生。
func TestConfigWithPriceUsesPriceAsSource(t *testing.T) {
	path := writeTestConfig(t, domain.Config{
		Pools: []domain.PoolConfig{{
			ID:         "gpt_low",
			VirtualKey: "vk_low_text",
			Providers: []domain.ProviderConfig{
				{Name: "cheap", PriceRMBPerDao: 0.05},
				{Name: "expensive", PriceRMBPerDao: 0.1},
			},
		}},
	})

	cfg, preview, err := configWithPrice(path, "gpt_low", "expensive", 0.2)
	if err != nil {
		t.Fatalf("configWithPrice returned error: %v", err)
	}

	providers := cfg.Pools[0].Providers
	if providers[0].CostWeight != 1 || providers[1].CostWeight != 0.25 {
		t.Fatalf("providers = %+v, want cost_weight from prices 0.05/0.2", providers)
	}
	text := pricePreviewText(preview)
	if !strings.Contains(text, "RMB/刀") || !strings.Contains(text, "cost_weight") {
		t.Fatalf("preview text = %q, want price and cost_weight", text)
	}
}

// TestConfigWithPriceAcceptsVirtualKeyName 验证 /price 第一个参数也可以写 Bifrost virtual_key。
func TestConfigWithPriceAcceptsVirtualKeyName(t *testing.T) {
	path := writeTestConfig(t, domain.Config{
		Pools: []domain.PoolConfig{{
			ID:         "gpt_low",
			VirtualKey: "vk_low_text",
			Providers: []domain.ProviderConfig{
				{Name: "provider_a", PriceRMBPerDao: 0.05},
				{Name: "provider_b", PriceRMBPerDao: 0.1},
			},
		}},
	})

	cfg, preview, err := configWithPrice(path, "vk_low_text", "provider_b", 0.2)
	if err != nil {
		t.Fatalf("configWithPrice returned error: %v", err)
	}
	if preview.PoolID != "gpt_low" {
		t.Fatalf("preview pool = %q, want canonical pool id", preview.PoolID)
	}
	if cfg.Pools[0].Providers[1].PriceRMBPerDao != 0.2 {
		t.Fatalf("provider_b price = %.4f, want 0.2", cfg.Pools[0].Providers[1].PriceRMBPerDao)
	}
}

func writeTestConfig(t *testing.T, cfg domain.Config) string {
	t.Helper()
	path := t.TempDir() + "/config.json"
	if err := writeRawConfig(path, cfg); err != nil {
		t.Fatalf("writeRawConfig returned error: %v", err)
	}
	// 确认测试文件确实落盘，避免后续只测到内存里的 cfg。
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("stat config: %v", err)
	}
	return path
}
