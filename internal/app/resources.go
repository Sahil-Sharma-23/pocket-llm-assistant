package app

import (
	"context"
	"errors"
	"os"
	"sync"
	"time"

	"personal-assistant/internal/config"
	"personal-assistant/internal/sysinfo"
)

// snapshot is one resource sample.
type snapshot struct {
	At    time.Time
	Mem   sysinfo.Mem
	Self  sysinfo.Proc
	Llama *sysinfo.Proc
}

// resourceMonitor samples memory periodically, logs a compact line every
// 10 minutes and warns when free memory falls below the threshold.
type resourceMonitor struct {
	a   *App
	cfg config.ResourcesConfig

	mu   sync.Mutex
	last snapshot
}

func newResourceMonitor(a *App, cfg config.ResourcesConfig) *resourceMonitor {
	m := &resourceMonitor{a: a, cfg: cfg}
	m.sample()
	return m
}

func (m *resourceMonitor) latest() snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.last
}

func (m *resourceMonitor) sample() snapshot {
	s := snapshot{At: time.Now()}
	var err error
	if s.Mem, err = sysinfo.ReadMem(); err != nil && !errors.Is(err, sysinfo.ErrUnsupported) {
		m.a.log.Debug("read meminfo", "err", err)
	}
	s.Self, _ = sysinfo.ReadProc(os.Getpid())
	if m.a.supervisor != nil {
		if pid := m.a.supervisor.Status().PID; pid > 0 {
			if p, err := sysinfo.ReadProc(pid); err == nil {
				s.Llama = &p
			}
		}
	}
	m.mu.Lock()
	m.last = s
	m.mu.Unlock()
	return s
}

func (m *resourceMonitor) run(ctx context.Context) {
	t := time.NewTicker(m.cfg.SampleInterval.Duration)
	defer t.Stop()
	lastLog := time.Time{}
	warned := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		s := m.sample()
		if s.Mem.TotalMB == 0 {
			continue // unsupported platform
		}
		low := s.Mem.AvailableMB < int64(m.cfg.WarnMemAvailableMB)
		if low && !warned {
			m.a.log.Warn("low free memory", "available_mb", s.Mem.AvailableMB, "threshold_mb", m.cfg.WarnMemAvailableMB)
		}
		warned = low
		if time.Since(lastLog) >= 10*time.Minute {
			lastLog = time.Now()
			attrs := []any{"mem_available_mb", s.Mem.AvailableMB, "swap_free_mb", s.Mem.SwapFreeMB, "self_rss_mb", s.Self.RSSMB}
			if s.Llama != nil {
				attrs = append(attrs, "llama_rss_mb", s.Llama.RSSMB, "llama_anon_mb", s.Llama.AnonMB)
			}
			m.a.log.Info("resources", attrs...)
		}
	}
}
