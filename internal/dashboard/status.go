package dashboard

import (
	"time"

	"personal-assistant/internal/health"
	"personal-assistant/internal/llm"
)

// StatusView is the overview data assembled by the app.
type StatusView struct {
	Version string
	Uptime  string
	Model   string
	Health  health.Report

	LLMManaged  bool
	LLMState    string
	LLMPID      int
	LLMRestarts int
	LLMError    string
	LLM         llm.Stats

	MemTotalMB     int64
	MemAvailableMB int64
	SelfRSSMB      int64
	LlamaRSSMB     int64
	LlamaAnonMB    int64

	SchedulerRunning int
	SchedulerLastRun time.Time
	TelegramEnabled  bool
}
