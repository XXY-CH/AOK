package runtime

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
	"os"
	"strings"
	"time"
)

type AnthropicConfig struct {
	APIKey           string
	Endpoint         string
	Model            string
	MaxTokens        int
	MaxResponseBytes int64
	Timeout          time.Duration
}

// AnthropicProvider speaks the versioned Messages API. Credentials are read
// from the environment when omitted and are never included in errors.
type AnthropicProvider struct {
	config AnthropicConfig
	client *http.Client
}

func NewAnthropicProvider(config AnthropicConfig) (*AnthropicProvider, error) {
	if config.APIKey == "" {
		config.APIKey = os.Getenv("ANTHROPIC_API_KEY")
	}
	if config.APIKey == "" {
		return nil, errors.New("anthropic API key is not configured")
	}
	if config.Endpoint == "" {
		config.Endpoint = "https://api.anthropic.com/v1/messages"
	}
	u, err := url.Parse(config.Endpoint)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" ||
		(u.Scheme != "https" && !isLoopbackHost(u.Hostname())) {
		return nil, errors.New("anthropic endpoint must be HTTPS without credentials, query or fragment")
	}
	if config.Model == "" {
		config.Model = "claude-3-5-sonnet-latest"
	}
	if config.MaxTokens == 0 {
		config.MaxTokens = 1024
	}
	if config.MaxResponseBytes == 0 {
		config.MaxResponseBytes = 2 << 20
	}
	if config.Timeout == 0 {
		config.Timeout = 2 * time.Minute
	}
	if config.MaxTokens < 1 || config.MaxTokens > 32768 || config.MaxResponseBytes < 1 || config.MaxResponseBytes > 16<<20 || config.Timeout < 0 {
		return nil, errors.New("invalid anthropic generation limits")
	}
	return &AnthropicProvider{config: config, client: &http.Client{
		Timeout: config.Timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("anthropic redirects are disabled")
		},
	}}, nil
}

func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (*AnthropicProvider) Name() string { return "anthropic" }

func (p *AnthropicProvider) Complete(ctx context.Context, prompt string) (string, Usage, error) {
	if len(prompt) > maxTextBytes {
		return "", Usage{}, errors.New("anthropic prompt exceeds engine limit")
	}
	body, err := json.Marshal(struct {
		Model     string `json:"model"`
		MaxTokens int    `json:"max_tokens"`
		Messages  []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}{Model: p.config.Model, MaxTokens: p.config.MaxTokens, Messages: []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}{{Role: "user", Content: prompt}}})
	if err != nil {
		return "", Usage{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.config.Endpoint, bytes.NewReader(body))
	if err != nil {
		return "", Usage{}, errors.New("create anthropic request")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", p.config.APIKey)
	req.Header.Set("anthropic-version", "2023-06-01")
	resp, err := p.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return "", Usage{}, ctx.Err()
		}
		return "", Usage{}, errors.New("anthropic backend request failed")
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, p.config.MaxResponseBytes+1))
	if err != nil {
		if ctx.Err() != nil {
			return "", Usage{}, ctx.Err()
		}
		return "", Usage{}, errors.New("read anthropic response")
	}
	if int64(len(data)) > p.config.MaxResponseBytes {
		return "", Usage{}, errors.New("anthropic response exceeds size limit")
	}
	if resp.StatusCode != http.StatusOK {
		return "", Usage{}, fmt.Errorf("anthropic backend HTTP status %d", resp.StatusCode)
	}
	var result struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Usage struct {
			InputTokens              *uint64 `json:"input_tokens"`
			OutputTokens             *uint64 `json:"output_tokens"`
			CacheCreationInputTokens uint64  `json:"cache_creation_input_tokens"`
			CacheReadInputTokens     uint64  `json:"cache_read_input_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(data, &result) != nil || result.Usage.InputTokens == nil || result.Usage.OutputTokens == nil {
		return "", Usage{}, errors.New("invalid anthropic completion or usage")
	}
	if *result.Usage.OutputTokens > uint64(p.config.MaxTokens) {
		return "", Usage{}, errors.New("invalid anthropic output token accounting")
	}
	var text strings.Builder
	for _, block := range result.Content {
		if block.Type != "text" {
			continue
		}
		text.WriteString(block.Text)
	}
	if text.Len() == 0 {
		return "", Usage{}, errors.New("anthropic response has no text content")
	}
	return text.String(), Usage{InputTokens: *result.Usage.InputTokens, OutputTokens: *result.Usage.OutputTokens, CachedTokens: result.Usage.CacheCreationInputTokens + result.Usage.CacheReadInputTokens}, nil
}
