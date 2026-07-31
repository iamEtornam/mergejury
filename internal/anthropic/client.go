// Package anthropic is a minimal Messages API client used by the modelapi
// adapter, the challenger, and the judge. Base URL and HTTP client are
// injectable so tests and the e2e fixture can stub it.
package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

type Client struct {
	BaseURL    string
	APIKey     string
	HTTPClient *http.Client
}

func NewFromEnv() *Client {
	base := os.Getenv("ANTHROPIC_BASE_URL")
	if base == "" {
		base = "https://api.anthropic.com"
	}
	return &Client{
		BaseURL:    base,
		APIKey:     os.Getenv("ANTHROPIC_API_KEY"),
		HTTPClient: &http.Client{Timeout: 10 * time.Minute},
	}
}

type Usage struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
}

type Response struct {
	Text  string
	Usage Usage
	Cost  float64
}

type apiResponse struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	Usage Usage `json:"usage"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// AuthError marks failures the caller must not retry.
type AuthError struct{ Msg string }

func (e AuthError) Error() string { return e.Msg }

// Complete sends one system+user exchange and returns the text response.
func (c *Client) Complete(ctx context.Context, model, system, user string, maxTokens int) (Response, error) {
	if c.APIKey == "" {
		return Response{}, AuthError{"ANTHROPIC_API_KEY is not set"}
	}
	if maxTokens <= 0 {
		maxTokens = 8192
	}
	body, _ := json.Marshal(map[string]any{
		"model":      model,
		"max_tokens": maxTokens,
		"system":     system,
		"messages": []map[string]any{
			{"role": "user", "content": user},
		},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(c.BaseURL, "/")+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return Response{}, err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("x-api-key", c.APIKey)
	req.Header.Set("anthropic-version", "2023-06-01")
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return Response{}, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return Response{}, err
	}
	var parsed apiResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return Response{}, fmt.Errorf("model API returned non-JSON (HTTP %d): %.200s", resp.StatusCode, raw)
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		msg := "model API auth failed"
		if parsed.Error != nil {
			msg = parsed.Error.Message
		}
		return Response{}, AuthError{msg}
	}
	if resp.StatusCode != http.StatusOK {
		msg := fmt.Sprintf("HTTP %d", resp.StatusCode)
		if parsed.Error != nil {
			msg = parsed.Error.Message
		}
		return Response{}, fmt.Errorf("model API error: %s", msg)
	}
	var text strings.Builder
	for _, block := range parsed.Content {
		if block.Type == "text" {
			text.WriteString(block.Text)
		}
	}
	return Response{
		Text:  text.String(),
		Usage: parsed.Usage,
		Cost:  EstimateCost(model, parsed.Usage),
	}, nil
}

// EstimateCost approximates USD cost from token usage.
// ponytail: static per-MTok price table, good enough for the scoreboard;
// swap for a priced API response if accuracy ever matters.
func EstimateCost(model string, u Usage) float64 {
	in, out := 3.0, 15.0 // sonnet-class default
	switch {
	case strings.Contains(model, "opus"), strings.Contains(model, "fable"):
		in, out = 15.0, 75.0
	case strings.Contains(model, "haiku"):
		in, out = 1.0, 5.0
	}
	return float64(u.InputTokens)/1e6*in + float64(u.OutputTokens)/1e6*out
}
