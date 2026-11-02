package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"personal-assistant/internal/memory"
	"personal-assistant/internal/notify"
	"personal-assistant/internal/scheduler"
	"personal-assistant/internal/tools"
)

// Job kinds besides tools.JobKindReminder.
const (
	jobMonitor     = "monitor"
	jobDailyPlan   = "daily_plan"
	jobDigest      = "github_digest"
	jobBackup      = "backup"
	jobMaintenance = "maintenance"
)

type jobs struct {
	a           *App
	notifier    notify.Notifier
	monitorHTTP *http.Client // user-configured monitors may target the LAN
}

func (j *jobs) register(s *scheduler.Scheduler) {
	s.Register(tools.JobKindReminder, j.reminder)
	s.Register(jobMonitor, j.monitor)
	s.Register(jobDailyPlan, j.dailyPlan)
	s.Register(jobDigest, j.digest)
	s.Register(jobBackup, j.backup)
	s.Register(jobMaintenance, j.maintenance)
}

// ensure creates/updates config-defined recurring jobs and disables ones
// removed from config. Reminders are user-created and untouched.
func (j *jobs) ensure(ctx context.Context) error {
	cfg, st, now, loc := j.a.cfg, j.a.store, time.Now(), j.a.loc
	upsert := func(kind, name, schedule string, payload any) error {
		first, err := scheduler.FirstRun(schedule, now, loc)
		if err != nil {
			return err
		}
		raw, _ := json.Marshal(payload)
		return st.UpsertJob(ctx, memory.JobSpec{Kind: kind, Name: name, Payload: string(raw), Schedule: schedule, NextRunAt: first})
	}

	var names []string
	for _, m := range cfg.Developer.Monitors {
		if err := upsert(jobMonitor, m.Name, "every "+m.Interval.Duration.String(), m); err != nil {
			return err
		}
		names = append(names, m.Name)
	}
	if err := st.DisableJobsExcept(ctx, jobMonitor, names); err != nil {
		return err
	}

	keep := func(kind string, enabled bool, schedule string) error {
		if !enabled {
			return st.DisableJobsExcept(ctx, kind, nil)
		}
		return upsert(kind, kind, schedule, map[string]string{})
	}
	if err := keep(jobDailyPlan, cfg.Scheduler.DailyPlanTime != "", "daily "+cfg.Scheduler.DailyPlanTime); err != nil {
		return err
	}
	if err := keep(jobDigest, cfg.Developer.DigestTime != "" && len(cfg.Developer.GitHubRepos) > 0, "daily "+cfg.Developer.DigestTime); err != nil {
		return err
	}
	if err := keep(jobBackup, cfg.Backup.Interval.Duration > 0, "every "+cfg.Backup.Interval.Duration.String()); err != nil {
		return err
	}
	return keep(jobMaintenance, true, "every 1h0m0s")
}

func (j *jobs) reminder(ctx context.Context, job memory.Job) error {
	var p tools.ReminderPayload
	if err := json.Unmarshal([]byte(job.Payload), &p); err != nil {
		return fmt.Errorf("bad reminder payload: %w", err)
	}
	text := "⏰ Reminder: " + p.Text
	if late := time.Since(job.NextRunAt); late > 2*time.Minute {
		// Be honest when Android delayed us (Doze, process killed, reboot).
		text += fmt.Sprintf("\n(delivered %s late: the phone was asleep or the assistant was not running)", late.Round(time.Minute))
	}
	return j.notifier.Notify(ctx, notify.Message{Text: text})
}

func (j *jobs) monitor(ctx context.Context, job memory.Job) error {
	var m struct {
		Name         string `json:"name"`
		URL          string `json:"url"`
		ExpectStatus int    `json:"expect_status"`
		ExpectJSON   string `json:"expect_json_key"`
		Timeout      string `json:"timeout"`
	}
	if err := json.Unmarshal([]byte(job.Payload), &m); err != nil {
		return fmt.Errorf("bad monitor payload: %w", err)
	}
	timeout, _ := time.ParseDuration(m.Timeout)
	prev, _ := j.a.store.LastMonitorResult(ctx, m.Name)
	r := tools.CheckHTTP(ctx, j.monitorHTTP, tools.CheckSpec{Name: m.Name, URL: m.URL, ExpectStatus: m.ExpectStatus, ExpectJSON: m.ExpectJSON, Timeout: timeout})
	if err := j.a.store.AddMonitorResult(ctx, memory.MonitorResult{Monitor: m.Name, OK: r.OK, StatusCode: r.StatusCode, LatencyMS: r.LatencyMS, Error: r.Error}); err != nil {
		return err
	}
	// Alert only on state changes, not every failed check.
	switch {
	case !r.OK && (prev == nil || prev.OK):
		j.notify(ctx, fmt.Sprintf("🔴 %s is DOWN (%s): %s", m.Name, m.URL, r.Error))
	case r.OK && prev != nil && !prev.OK:
		j.notify(ctx, fmt.Sprintf("🟢 %s recovered (HTTP %d, %d ms)", m.Name, r.StatusCode, r.LatencyMS))
	}
	if !r.OK {
		return fmt.Errorf("check failed: %s", r.Error)
	}
	return nil
}

func (j *jobs) notify(ctx context.Context, text string) {
	if err := j.notifier.Notify(ctx, notify.Message{Text: text}); err != nil {
		j.a.log.Warn("notify", "err", err)
	}
}

func (j *jobs) dailyPlan(ctx context.Context, _ memory.Job) error {
	return j.notifier.Notify(ctx, notify.Message{Text: j.a.orch.DailyPlan(ctx, "job")})
}

func (j *jobs) digest(ctx context.Context, _ memory.Job) error {
	return j.notifier.Notify(ctx, notify.Message{Text: j.a.orch.DevDigest(ctx, "job")})
}

// backup writes a consistent SQLite snapshot and keeps the newest N.
func (j *jobs) backup(ctx context.Context, _ memory.Job) error {
	dir := j.a.cfg.Paths.BackupDir
	dest := filepath.Join(dir, "assistant-"+time.Now().Format("20060102-150405")+".db")
	if err := j.a.store.Backup(ctx, dest); err != nil {
		return err
	}
	j.a.log.Info("database backup written", "path", dest)
	return pruneBackups(dir, j.a.cfg.Backup.Keep)
}

func pruneBackups(dir string, keep int) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var names []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "assistant-") && strings.HasSuffix(e.Name(), ".db") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names) // timestamped names sort chronologically
	for len(names) > keep {
		if err := os.Remove(filepath.Join(dir, names[0])); err != nil {
			return err
		}
		names = names[1:]
	}
	return nil
}

func (j *jobs) maintenance(ctx context.Context, _ memory.Job) error {
	if n, err := j.a.store.ExpireApprovals(ctx); err == nil && n > 0 {
		j.a.log.Info("expired approvals", "count", n)
	}
	_, err := j.a.store.PruneMonitorResults(ctx, time.Now().AddDate(0, 0, -30))
	return err
}
