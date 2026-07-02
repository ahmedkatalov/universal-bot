// Package ai отвечает на личные сообщения владельцу через OpenRouter
// (OpenAI-совместимый API, https://openrouter.ai/api/v1/chat/completions) —
// когда пишут не в рабочую группу, а напрямую номеру бота. Поддерживает
// tool use (function calling): модель сама решает, когда нужно свериться
// с данными бота (например, сформировать отчёт за произвольный период)
// и вызывает соответствующий инструмент.
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
)

const (
	defaultBaseURL = "https://openrouter.ai/api/v1"
	defaultModel   = "~anthropic/claude-sonnet-latest" // алиас OpenRouter на последнюю версию Sonnet

	// maxToolIterations — предохранитель от зацикливания, если модель
	// почему-то продолжает звать инструменты бесконечно.
	maxToolIterations = 5
)

// Turn — одна реплика в истории личного диалога.
type Turn struct {
	FromUser bool
	Text     string
}

// Tool — инструмент, который может вызвать модель. Handle выполняется
// синхронно на стороне бота (доступ к БД, отправка сообщений в WhatsApp и т.п.)
// и должен вернуть текстовый результат, который увидит модель.
type Tool struct {
	Name        string
	Description string
	InputSchema map[string]any
	Handle      func(ctx context.Context, input json.RawMessage) (string, error)
}

type Assistant struct {
	apiKey  string
	model   string
	baseURL string
	http    *http.Client
}

// New создаёт клиента OpenRouter. apiKey — значение OPENROUTER_API_KEY.
// model — id модели в каталоге OpenRouter (например, "anthropic/claude-sonnet-4.6");
// пусто -> берётся defaultModel. baseURL — обычно оставляют пустым
// (используется https://openrouter.ai/api/v1), задаётся отдельно только
// если стоит прокси/самостоятельный gateway с тем же протоколом.
func New(apiKey, model, baseURL string) *Assistant {
	if model == "" {
		model = defaultModel
	}
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	return &Assistant{
		apiKey:  apiKey,
		model:   model,
		baseURL: strings.TrimRight(baseURL, "/"),
		http:    &http.Client{Timeout: 60 * time.Second},
	}
}

type chatMessage struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []toolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

type toolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function toolCallFunc `json:"function"`
}

type toolCallFunc struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type toolDef struct {
	Type     string      `json:"type"`
	Function toolFuncDef `json:"function"`
}

type toolFuncDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

type chatRequest struct {
	Model     string        `json:"model"`
	Messages  []chatMessage `json:"messages"`
	Tools     []toolDef     `json:"tools,omitempty"`
	MaxTokens int           `json:"max_tokens,omitempty"`
}

type chatResponse struct {
	Choices []struct {
		Message      chatMessage `json:"message"`
		FinishReason string      `json:"finish_reason"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// Reply отвечает на сообщение владельца с учётом системного контекста,
// истории диалога и доступных инструментов. Если модель решает вызвать
// инструмент, Reply выполняет его через tool.Handle и отдаёт результат
// обратно модели, пока не получит финальный текстовый ответ.
func (a *Assistant) Reply(ctx context.Context, systemPrompt string, tools []Tool, history []Turn, userText string) (string, error) {
	messages := make([]chatMessage, 0, len(history)+2)
	messages = append(messages, chatMessage{Role: "system", Content: systemPrompt})
	for _, t := range history {
		role := "assistant"
		if t.FromUser {
			role = "user"
		}
		messages = append(messages, chatMessage{Role: role, Content: t.Text})
	}
	messages = append(messages, chatMessage{Role: "user", Content: userText})

	toolDefs := make([]toolDef, 0, len(tools))
	for _, t := range tools {
		toolDefs = append(toolDefs, toolDef{
			Type: "function",
			Function: toolFuncDef{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.InputSchema,
			},
		})
	}

	for i := 0; i < maxToolIterations; i++ {
		respMsg, finishReason, err := a.chat(ctx, messages, toolDefs)
		if err != nil {
			return "", err
		}
		messages = append(messages, respMsg)

		if finishReason != "tool_calls" || len(respMsg.ToolCalls) == 0 {
			return respMsg.Content, nil
		}

		for _, call := range respMsg.ToolCalls {
			result := runTool(ctx, tools, call)
			messages = append(messages, chatMessage{
				Role:       "tool",
				ToolCallID: call.ID,
				Content:    result,
			})
		}
	}

	return "", fmt.Errorf("openrouter: превышен лимит вызовов инструментов")
}

// Complete — одиночный запрос без инструментов и истории: системный промпт
// плюс один пользовательский текст -> текст ответа. Используется внутренними
// модулями бота (доразбор нераспознанных сообщений и чеков), а не для диалога.
func (a *Assistant) Complete(ctx context.Context, systemPrompt, userText string) (string, error) {
	msg, _, err := a.chat(ctx, []chatMessage{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: userText},
	}, nil)
	if err != nil {
		return "", err
	}
	return msg.Content, nil
}

func (a *Assistant) chat(ctx context.Context, messages []chatMessage, tools []toolDef) (chatMessage, string, error) {
	payload, err := json.Marshal(chatRequest{
		Model:     a.model,
		Messages:  messages,
		Tools:     tools,
		MaxTokens: 1024,
	})
	if err != nil {
		return chatMessage{}, "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.baseURL+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return chatMessage{}, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+a.apiKey)

	resp, err := a.http.Do(req)
	if err != nil {
		return chatMessage{}, "", fmt.Errorf("openrouter: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return chatMessage{}, "", fmt.Errorf("openrouter: чтение ответа: %w", err)
	}

	var parsed chatResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return chatMessage{}, "", fmt.Errorf("openrouter: не удалось разобрать ответ (%d): %s", resp.StatusCode, string(body))
	}
	if parsed.Error != nil {
		return chatMessage{}, "", fmt.Errorf("openrouter: %s", parsed.Error.Message)
	}
	if resp.StatusCode != http.StatusOK {
		return chatMessage{}, "", fmt.Errorf("openrouter вернул %d: %s", resp.StatusCode, string(body))
	}
	if len(parsed.Choices) == 0 {
		return chatMessage{}, "", fmt.Errorf("openrouter: пустой ответ")
	}

	choice := parsed.Choices[0]
	return choice.Message, choice.FinishReason, nil
}

func runTool(ctx context.Context, tools []Tool, call toolCall) string {
	for _, t := range tools {
		if t.Name != call.Function.Name {
			continue
		}
		out, err := t.Handle(ctx, json.RawMessage(call.Function.Arguments))
		if err != nil {
			return "Ошибка: " + err.Error()
		}
		return out
	}
	return fmt.Sprintf("неизвестный инструмент %q", call.Function.Name)
}
