package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

type responseTool struct {
	Type        string         `json:"type"`
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
	Strict      bool           `json:"strict"` // сохраняем необязательные поля существующих инструментов
}

type responseRequest struct {
	Model             string            `json:"model"`
	Input             []json.RawMessage `json:"input"`
	Tools             []responseTool    `json:"tools,omitempty"`
	MaxOutputTokens   int               `json:"max_output_tokens"`
	Store             bool              `json:"store"`
	Include           []string          `json:"include"`
	Reasoning         map[string]string `json:"reasoning"`
	ParallelToolCalls bool              `json:"parallel_tool_calls"`
}

func (a *Assistant) usesResponses(model string) bool {
	switch a.apiMode {
	case "responses":
		return true
	case "chat_completions":
		return false
	}
	name := strings.ToLower(model)
	name = strings.TrimPrefix(name, "openai/")
	return strings.HasPrefix(name, "gpt-")
}

func responseInput(item any) json.RawMessage {
	b, _ := json.Marshal(item)
	return b
}

// Передаём output Items целиком: рассуждения и call_id нужны следующему
// запросу даже при store:false. История не зависит от хранения у провайдера.
func responseHistory(messages []chatMessage) []json.RawMessage {
	var input []json.RawMessage
	for _, m := range messages {
		if len(m.responseItems) > 0 {
			input = append(input, m.responseItems...)
			continue
		}
		if m.Role == "tool" {
			input = append(input, responseInput(map[string]any{
				"type": "function_call_output", "call_id": m.ToolCallID, "output": contentString(m.Content),
			}))
			continue
		}
		role := m.Role
		if role == "system" {
			role = "developer"
		}
		text := contentString(m.Content)
		if blocks, ok := m.Content.([]contentBlock); ok {
			var parts []string
			for _, b := range blocks {
				parts = append(parts, b.Text)
			}
			text = strings.Join(parts, "\n\n") // cache_control Anthropic сюда не отправляется
		}
		if text != "" {
			input = append(input, responseInput(map[string]any{"role": role, "content": text}))
		}
		for _, call := range m.ToolCalls {
			input = append(input, responseInput(map[string]any{
				"type": "function_call", "call_id": call.ID, "name": call.Function.Name, "arguments": call.Function.Arguments,
			}))
		}
	}
	return input
}

func (a *Assistant) respond(ctx context.Context, model string, input []json.RawMessage, tools []toolDef, maxTokens int, effort string) (chatMessage, string, error) {
	req := responseRequest{Model: model, Input: input, MaxOutputTokens: maxTokens,
		Store: false, Include: []string{"reasoning.encrypted_content"}, Reasoning: map[string]string{"effort": effort}}
	for _, t := range tools {
		schema := t.Function.Parameters
		if schema == nil {
			schema = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		req.Tools = append(req.Tools, responseTool{Type: "function", Name: t.Function.Name,
			Description: t.Function.Description, Parameters: schema, Strict: false})
	}
	payload, err := json.Marshal(req)
	if err != nil {
		return chatMessage{}, "", err
	}
	body, status, terr := a.postJSON(ctx, "/responses", payload)
	msg, finish, err := interpretResponse(body, status, terr)
	return msg, finish, a.safeError(err)
}

func interpretResponse(body []byte, status int, transportErr error) (chatMessage, string, error) {
	if transportErr != nil {
		return chatMessage{}, "", classifyProviderError(0, "", transportErr)
	}
	var resp struct {
		Status string            `json:"status"`
		Output []json.RawMessage `json:"output"`
		Error  *struct {
			Message string `json:"message"`
			Code    string `json:"code"`
		} `json:"error"`
		IncompleteDetails *struct {
			Reason string `json:"reason"`
		} `json:"incomplete_details"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return chatMessage{}, "", classifyProviderError(status, snippet(body), nil)
	}
	if resp.Error != nil {
		return chatMessage{}, "", classifyProviderError(status, resp.Error.Code+": "+resp.Error.Message, nil)
	}
	if status != http.StatusOK {
		return chatMessage{}, "", classifyProviderError(status, snippet(body), nil)
	}
	malformed := func(detail string) (chatMessage, string, error) {
		return chatMessage{}, "", &ProviderError{"malformed", "ИИ не завершил ответ. Повтори запрос — незавершённые действия не выполнялись.", detail}
	}
	// ODIRouter допускает отсутствие status; явно незавершённый ответ нельзя
	// выполнять частично, особенно если в нём есть запись платежа.
	if resp.Status != "" && resp.Status != "completed" {
		return malformed("response status: " + resp.Status)
	}
	msg := chatMessage{Role: "assistant", responseItems: resp.Output}
	var text []string
	hasMessage := false
	for _, raw := range resp.Output {
		var item struct {
			Type      string `json:"type"`
			CallID    string `json:"call_id"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
			Content   []struct {
				Type    string `json:"type"`
				Text    string `json:"text"`
				Refusal string `json:"refusal"`
			} `json:"content"`
		}
		if err := json.Unmarshal(raw, &item); err != nil {
			return malformed("invalid output item")
		}
		switch item.Type {
		case "message":
			hasMessage = true
			for _, p := range item.Content {
				if p.Type == "output_text" && p.Text != "" {
					text = append(text, p.Text)
				}
				if p.Type == "refusal" && p.Refusal != "" {
					text = append(text, p.Refusal)
				}
			}
		case "function_call":
			var args map[string]json.RawMessage
			if item.CallID == "" || item.Name == "" || json.Unmarshal([]byte(item.Arguments), &args) != nil || args == nil {
				return malformed("invalid function call")
			}
			msg.ToolCalls = append(msg.ToolCalls, toolCall{ID: item.CallID, Type: "function", Function: toolCallFunc{Name: item.Name, Arguments: item.Arguments}})
		}
	}
	msg.Content = strings.Join(text, "\n")
	if len(msg.ToolCalls) > 0 {
		return msg, "tool_calls", nil
	}
	if strings.TrimSpace(contentString(msg.Content)) == "" && !hasMessage {
		return malformed("empty response output")
	}
	return msg, "stop", nil
}

type executedTool struct{ name, arguments, output string }

// Один call_id выполняется один раз: повтор результата модели не должен
// повторно записать платёж или отправить документ.
func runToolOnce(ctx context.Context, tools []Tool, call toolCall, executed map[string]executedTool) string {
	if old, ok := executed[call.ID]; ok {
		if old.name != call.Function.Name || old.arguments != call.Function.Arguments {
			return "ИНСТРУМЕНТ НЕ ВЫПОЛНЕН — повтор call_id с другими параметрами. Действие не сделано."
		}
		return old.output
	}
	out := runTool(ctx, tools, call)
	executed[call.ID] = executedTool{call.Function.Name, call.Function.Arguments, out}
	return out
}

// CheckConnection проверяет JSON, изображение и настоящий цикл вызова
// инструмента. Не подключается к WhatsApp и не меняет финансовый учёт.
func (a *Assistant) CheckConnection(ctx context.Context) error {
	out, err := a.Complete(ctx, `Верни только JSON {"ok":true}.`, "Проверка подключения")
	if err != nil {
		return fmt.Errorf("текст/JSON: %w", err)
	}
	var probe struct {
		OK bool `json:"ok"`
	}
	if json.Unmarshal([]byte(strings.TrimSpace(out)), &probe) != nil || !probe.OK {
		return errors.New("модель не вернула ожидаемый JSON")
	}
	if err := a.PingVision(ctx); err != nil {
		return fmt.Errorf("изображение: %w", err)
	}
	called := false
	_, err = a.Reply(ctx, "Вызови connection_check ровно один раз. После результата ответь: ок.", "", []Tool{{
		Name: "connection_check", Description: "Безопасная проверка вызова инструмента",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
		Handle:      func(context.Context, json.RawMessage) (string, error) { called = true; return "connection_ok", nil },
	}}, nil, "Проверь вызов инструмента")
	if err != nil {
		return fmt.Errorf("инструмент: %w", err)
	}
	if !called {
		return errors.New("модель ответила, но инструмент не вызвала")
	}
	return nil
}
