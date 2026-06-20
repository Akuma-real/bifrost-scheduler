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
