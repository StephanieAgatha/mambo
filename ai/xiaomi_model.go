package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"charm.land/fantasy"
	"charm.land/fantasy/object"

	"mambo/config"
)

// xiaomiModel implements fantasy.LanguageModel using raw HTTP to Xiaomi's API.
// Xiaomi MiMo doesn't support OpenAI SDK extras like parallel_tool_calls,
// reasoning_effort, or response_format — so we bypass the SDK entirely.
type xiaomiModel struct {
	cfg        *config.Config
	httpClient *http.Client
}

func newXiaomiModel(cfg *config.Config) *xiaomiModel {
	return &xiaomiModel{
		cfg: cfg,
		httpClient: &http.Client{
			Timeout: 120 * time.Second,
		},
	}
}

func (m *xiaomiModel) Provider() string { return m.cfg.AIProvider }
func (m *xiaomiModel) Model() string    { return m.cfg.AIModel }

func (m *xiaomiModel) Generate(ctx context.Context, call fantasy.Call) (*fantasy.Response, error) {
	return m.chat(ctx, call)
}

func (m *xiaomiModel) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	return nil, fmt.Errorf("xiaomi: streaming not supported")
}

func (m *xiaomiModel) GenerateObject(ctx context.Context, call fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return object.GenerateWithTool(ctx, m, call)
}

func (m *xiaomiModel) StreamObject(ctx context.Context, call fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return nil, fmt.Errorf("xiaomi: streaming not supported")
}

func (m *xiaomiModel) chat(ctx context.Context, call fantasy.Call) (*fantasy.Response, error) {
	messages := buildMessages(call)

	reqBody := map[string]any{
		"model":    m.cfg.AIModel,
		"messages": messages,
	}
	if call.MaxOutputTokens != nil {
		reqBody["max_tokens"] = *call.MaxOutputTokens
	}
	if call.Temperature != nil {
		reqBody["temperature"] = *call.Temperature
	}
	if call.TopP != nil {
		reqBody["top_p"] = *call.TopP
	}

	if len(call.Tools) > 0 {
		tools := make([]map[string]any, 0, len(call.Tools))
		for _, t := range call.Tools {
			if ft, ok := t.(fantasy.FunctionTool); ok {
				tools = append(tools, map[string]any{
					"type": "function",
					"function": map[string]any{
						"name":        ft.Name,
						"description": ft.Description,
						"parameters":  ft.InputSchema,
					},
				})
			}
		}
		reqBody["tools"] = tools
	}
	if call.ToolChoice != nil {
		tc := string(*call.ToolChoice)
		if tc == "none" || tc == "auto" || tc == "required" {
			reqBody["tool_choice"] = tc
		} else {
			reqBody["tool_choice"] = map[string]any{
				"type": "function",
				"function": map[string]string{
					"name": tc,
				},
			}
		}
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("xiaomi: marshal request: %w", err)
	}

	url := strings.TrimRight(m.cfg.AIBaseURL, "/") + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("xiaomi: build request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+m.cfg.AIAPIKey)

	resp, err := m.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("xiaomi: http request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("xiaomi: read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("xiaomi: status=%d body=%s", resp.StatusCode, string(respBody))
	}

	var parsed struct {
		Choices []struct {
			Message struct {
				Content   string `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Type     string `json:"type"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
	}

	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, fmt.Errorf("xiaomi: parse response: %w", err)
	}

	if len(parsed.Choices) == 0 {
		return nil, fmt.Errorf("xiaomi: no choices in response")
	}

	msg := parsed.Choices[0].Message

	var content fantasy.ResponseContent
	if msg.Content != "" {
		content = append(content, fantasy.TextContent{Text: msg.Content})
	}
	for _, tc := range msg.ToolCalls {
		content = append(content, fantasy.ToolCallContent{
			ToolCallID: tc.ID,
			ToolName:   tc.Function.Name,
			Input:      tc.Function.Arguments,
		})
	}

	return &fantasy.Response{
		Content:      content,
		Usage:        fantasy.Usage{InputTokens: int64(parsed.Usage.PromptTokens), OutputTokens: int64(parsed.Usage.CompletionTokens), TotalTokens: int64(parsed.Usage.TotalTokens)},
		FinishReason: fantasy.FinishReasonUnknown,
	}, nil
}

func buildMessages(call fantasy.Call) []map[string]string {
	var msgs []map[string]string
	for _, msg := range call.Prompt {
		role := string(msg.Role)
		var text string
		for _, c := range msg.Content {
			if tp, ok := fantasy.AsContentType[fantasy.TextPart](c); ok {
				text += tp.Text
			}
		}
		msgs = append(msgs, map[string]string{"role": role, "content": text})
	}
	return msgs
}

// compile-time interface check
var _ fantasy.LanguageModel = (*xiaomiModel)(nil)