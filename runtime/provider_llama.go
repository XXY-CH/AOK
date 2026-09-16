package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type LlamaConfig struct {
	Endpoint         string
	MaxTokens        int
	MaxResponseBytes int64
	Timeout          time.Duration
	CachePrompt      bool
}

// LlamaProvider uses llama.cpp's native completion API. Usage is reported by
// the backend; it is never estimated from text or replaced with echo output.
type LlamaProvider struct {
	config LlamaConfig
	client *http.Client
}

func NewLlamaProvider(config LlamaConfig) (*LlamaProvider, error) {
	u, err := url.Parse(config.Endpoint)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("llama endpoint must be an HTTP(S) base URL without credentials, query or fragment")
	}
	if config.MaxTokens == 0 {
		config.MaxTokens = 128
	}
	if config.MaxResponseBytes == 0 {
		config.MaxResponseBytes = 1 << 20
	}
	if config.Timeout == 0 {
		config.Timeout = 2 * time.Minute
	}
	if config.MaxTokens < 1 || config.MaxTokens > 32768 || config.MaxResponseBytes < 1 || config.MaxResponseBytes > 8<<20 || config.Timeout < 0 {
		return nil, errors.New("invalid llama generation limits")
	}
	config.Endpoint = strings.TrimRight(config.Endpoint, "/") + "/completion"
	return &LlamaProvider{config: config, client: &http.Client{
		Timeout:       config.Timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("llama redirects are disabled") },
	}}, nil
}

func (*LlamaProvider) Name() string { return "llama.cpp" }

func (p *LlamaProvider) Complete(ctx context.Context, prompt string) (string, Usage, error) {
	if len(prompt) > maxTextBytes {
		return "", Usage{}, errors.New("llama prompt exceeds engine limit")
	}
	body, err := json.Marshal(struct {
		Prompt      string  `json:"prompt"`
		NPredict    int     `json:"n_predict"`
		Temperature float64 `json:"temperature"`
		Stream      bool    `json:"stream"`
		CachePrompt bool    `json:"cache_prompt"`
	}{Prompt: prompt, NPredict: p.config.MaxTokens, CachePrompt: p.config.CachePrompt})
	if err != nil {
		return "", Usage{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.config.Endpoint, bytes.NewReader(body))
	if err != nil {
		return "", Usage{}, errors.New("create llama request")
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return "", Usage{}, ctx.Err()
		}
		// Avoid forwarding transport errors that can include endpoint credentials.
		return "", Usage{}, errors.New("llama backend request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", Usage{}, fmt.Errorf("llama backend HTTP status %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, p.config.MaxResponseBytes+1))
	if err != nil {
		if ctx.Err() != nil {
			return "", Usage{}, ctx.Err()
		}
		return "", Usage{}, errors.New("read llama response")
	}
	if int64(len(data)) > p.config.MaxResponseBytes {
		return "", Usage{}, errors.New("llama response exceeds size limit")
	}
	var result struct {
		Content         *string         `json:"content"`
		TokensEvaluated *uint64         `json:"tokens_evaluated"`
		TokensPredicted *uint64         `json:"tokens_predicted"`
		Truncated       bool            `json:"truncated"`
		Error           json.RawMessage `json:"error"`
		Timings         struct {
			CacheN  *uint64 `json:"cache_n"`
			PromptN *uint64 `json:"prompt_n"`
		} `json:"timings"`
	}
	if json.Unmarshal(data, &result) != nil || result.Content == nil || result.TokensEvaluated == nil || result.TokensPredicted == nil || len(result.Error) > 0 && string(result.Error) != "null" {
		return "", Usage{}, errors.New("invalid llama completion or usage")
	}
	if result.Truncated {
		return "", Usage{}, errors.New("llama backend truncated the input context")
	}
	usage := Usage{InputTokens: *result.TokensEvaluated, OutputTokens: *result.TokensPredicted}
	// tokens_cached can include generated tokens in current llama.cpp builds.
	// timings.cache_n measures the input prefix actually reused for this request.
	if result.Timings.CacheN != nil {
		usage.CachedTokens = *result.Timings.CacheN
	} else if p.config.CachePrompt {
		return "", Usage{}, errors.New("llama backend omitted input cache accounting")
	}
	if usage.OutputTokens > uint64(p.config.MaxTokens) || usage.CachedTokens > usage.InputTokens {
		return "", Usage{}, errors.New("invalid llama token accounting")
	}
	if result.Timings.PromptN != nil && *result.Timings.PromptN != usage.InputTokens-usage.CachedTokens {
		return "", Usage{}, errors.New("inconsistent llama input token accounting")
	}
	return *result.Content, usage, nil
}
