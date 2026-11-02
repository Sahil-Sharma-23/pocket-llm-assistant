package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"
)

// LlamaClient talks to a local llama.cpp server (OpenAI-compatible API).
type LlamaClient struct {
	baseURL     string
	http        *http.Client
	maxTokens   int
	temperature float64
}

// LlamaOptions configures NewLlamaClient.
type LlamaOptions struct {
	BaseURL     string // must be loopback
	MaxTokens   int
	Temperature float64
}

// NewLlamaClient builds a client. The transport refuses to dial anything but
// loopback addresses, so even a misconfiguration or DNS trick cannot send a
// prompt off the device.
func NewLlamaClient(o LlamaOptions) (*LlamaClient, error) {
	u, err := url.Parse(o.BaseURL)
	if err != nil || u.Scheme != "http" {
		return nil, fmt.Errorf("llm: invalid base url %q", o.BaseURL)
	}
	dialer := &net.Dialer{Timeout: 5 * time.Second, Control: loopbackOnly}
	tr := &http.Transport{
		DialContext:         dialer.DialContext,
		Proxy:               nil, // never route inference through a proxy
		MaxIdleConns:        2,
		IdleConnTimeout:     60 * time.Second,
		DisableCompression:  true,
		TLSHandshakeTimeout: 5 * time.Second,
	}
	return &LlamaClient{
		baseURL:     strings.TrimRight(o.BaseURL, "/"),
		http:        &http.Client{Transport: tr}, // timeouts come from ctx
		maxTokens:   o.MaxTokens,
		temperature: o.Temperature,
	}, nil
}

// loopbackOnly is a net.Dialer Control hook run after DNS resolution.
func loopbackOnly(network, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("llm: refusing non-loopback inference address %s", address)
	}
	return nil
}

type chatRequest struct {
	Messages           []Message       `json:"messages"`
	MaxTokens          int             `json:"max_tokens"`
	Temperature        float64         `json:"temperature"`
	Stream             bool            `json:"stream"`
	CachePrompt        bool            `json:"cache_prompt"`
	ChatTemplateKwargs map[string]any  `json:"chat_template_kwargs,omitempty"`
	ResponseFormat     *responseFormat `json:"response_format,omitempty"`
}

type responseFormat struct {
	Type       string `json:"type"`
	JSONSchema struct {
		Name   string          `json:"name"`
		Schema json.RawMessage `json:"schema"`
	} `json:"json_schema"`
}

type chatResponse struct {
	Choices []struct {
		FinishReason string `json:"finish_reason"`
		Message      struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
	Timings struct {
		CacheN          int     `json:"cache_n"`
		PromptN         int     `json:"prompt_n"`
		PromptPerSecond float64 `json:"prompt_per_second"`
		PredictedPerSec float64 `json:"predicted_per_second"`
	} `json:"timings"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// Chat implements Provider.
func (c *LlamaClient) Chat(ctx context.Context, req Request) (Response, error) {
	if len(req.Messages) == 0 {
		return Response{}, errors.New("llm: no messages")
	}
	body := chatRequest{
		Messages:    req.Messages,
		MaxTokens:   c.maxTokens,
		Temperature: c.temperature,
		CachePrompt: true,
		// Gemma 4 otherwise emits its chain of thought into the answer
		// (verified on-device); thinking also costs ~4 tok/s of latency.
		ChatTemplateKwargs: map[string]any{"enable_thinking": false},
	}
	if req.MaxTokens > 0 {
		body.MaxTokens = req.MaxTokens
	}
	if req.Temperature != nil {
		body.Temperature = *req.Temperature
	}
	if len(req.JSONSchema) > 0 {
		rf := &responseFormat{Type: "json_schema"}
		rf.JSONSchema.Name = "output"
		rf.JSONSchema.Schema = req.JSONSchema
		body.ResponseFormat = rf
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return Response{}, err
	}

	start := time.Now()
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return Response{}, err
	}
	hreq.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(hreq)
	if err != nil {
		if ctx.Err() != nil {
			return Response{}, ctx.Err()
		}
		return Response{}, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return Response{}, fmt.Errorf("%w: read body: %v", ErrUnavailable, err)
	}
	switch {
	case resp.StatusCode == http.StatusServiceUnavailable:
		return Response{}, fmt.Errorf("%w: server loading (503)", ErrUnavailable)
	case resp.StatusCode != http.StatusOK:
		return Response{}, fmt.Errorf("llm: http %d: %.200s", resp.StatusCode, raw)
	}

	var cr chatResponse
	if err := json.Unmarshal(raw, &cr); err != nil {
		return Response{}, fmt.Errorf("%w: decode: %v", ErrMalformed, err)
	}
	if cr.Error != nil {
		return Response{}, fmt.Errorf("llm: server error: %s", cr.Error.Message)
	}
	if len(cr.Choices) == 0 {
		return Response{}, fmt.Errorf("%w: no choices", ErrMalformed)
	}
	ch := cr.Choices[0]
	return Response{
		Content:      strings.TrimSpace(ch.Message.Content),
		FinishReason: ch.FinishReason,
		Duration:     time.Since(start),
		Usage: Usage{
			PromptTokens:     cr.Usage.PromptTokens,
			CachedTokens:     cr.Timings.CacheN,
			CompletionTokens: cr.Usage.CompletionTokens,
			PromptPerSec:     cr.Timings.PromptPerSecond,
			GenPerSec:        cr.Timings.PredictedPerSec,
		},
	}, nil
}

// Health implements Provider via GET /health.
func (c *LlamaClient) Health(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/health", nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: health status %d", ErrUnavailable, resp.StatusCode)
	}
	return nil
}
