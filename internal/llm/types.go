// Package llm abstracts local inference behind a small Provider interface so
// the engine (llama.cpp today, possibly LiteRT-LM later) can change without
// touching the agent system. Only loopback endpoints are ever contacted.
package llm

import (
	"encoding/json"
	"errors"
	"time"
)

// Message roles.
const (
	RoleSystem    = "system"
	RoleUser      = "user"
	RoleAssistant = "assistant"
)

// Message is one chat turn.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Request is a chat completion request.
type Request struct {
	Messages []Message
	// MaxTokens caps generated tokens; 0 = provider default.
	MaxTokens int
	// Temperature; nil = provider default.
	Temperature *float64
	// JSONSchema, when set, constrains the output to JSON matching the schema
	// (llama.cpp converts it to a grammar). This is how the agents get
	// reliable structured decisions ("tool calls") from a small model.
	JSONSchema json.RawMessage
}

// Response is a completed generation.
type Response struct {
	Content      string
	FinishReason string // stop | length
	Usage        Usage
	Duration     time.Duration
}

// Usage reports token counts and throughput.
type Usage struct {
	PromptTokens     int     `json:"prompt_tokens"`
	CachedTokens     int     `json:"cached_tokens"`
	CompletionTokens int     `json:"completion_tokens"`
	PromptPerSec     float64 `json:"prompt_per_sec"`
	GenPerSec        float64 `json:"gen_per_sec"`
}

// Temp is a helper for Request.Temperature.
func Temp(t float64) *float64 { return &t }

// Errors returned by providers. Callers use errors.Is.
var (
	// ErrBusy: the single inference slot and its queue are full.
	ErrBusy = errors.New("llm: busy (inference queue full)")
	// ErrUnavailable: the inference server is not reachable or still loading.
	ErrUnavailable = errors.New("llm: inference server unavailable")
	// ErrInsufficientMemory: the device lacks free RAM for inference.
	ErrInsufficientMemory = errors.New("llm: insufficient free memory")
	// ErrMalformed: the model returned something unusable (empty,
	// truncated, or not valid JSON when a schema was requested).
	ErrMalformed = errors.New("llm: malformed model response")
)
