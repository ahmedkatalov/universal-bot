// Package ai отвечает на личные сообщения владельцу через Claude API —
// когда пишут не в рабочую группу, а напрямую номеру бота. Поддерживает
// tool use: Claude сама решает, когда нужно свериться с данными бота
// (например, сформировать отчёт за произвольный период) и вызывает
// соответствующий инструмент.
package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

const model = anthropic.ModelClaudeOpus4_8

// maxToolIterations — предохранитель от зацикливания, если модель почему-то
// продолжает звать инструменты бесконечно.
const maxToolIterations = 5

// Turn — одна реплика в истории личного диалога.
type Turn struct {
	FromUser bool
	Text     string
}

// Tool — инструмент, который может вызвать Claude. Handle выполняется
// синхронно на стороне бота (доступ к БД, отправка сообщений в WhatsApp и т.п.)
// и должен вернуть текстовый результат, который увидит модель.
type Tool struct {
	Name        string
	Description string
	InputSchema map[string]any
	Handle      func(ctx context.Context, input json.RawMessage) (string, error)
}

type Assistant struct {
	client anthropic.Client
}

// New создаёт клиента Claude. apiKey — значение ANTHROPIC_API_KEY.
func New(apiKey string) *Assistant {
	return &Assistant{client: anthropic.NewClient(option.WithAPIKey(apiKey))}
}

// Reply отвечает на сообщение владельца с учётом системного контекста,
// истории диалога и доступных инструментов. Если модель решает вызвать
// инструмент, Reply выполняет его через tool.Handle и отдаёт результат
// обратно модели, пока не получит финальный текстовый ответ.
func (a *Assistant) Reply(ctx context.Context, systemPrompt string, tools []Tool, history []Turn, userText string) (string, error) {
	messages := make([]anthropic.MessageParam, 0, len(history)+1)
	for _, t := range history {
		block := anthropic.NewTextBlock(t.Text)
		if t.FromUser {
			messages = append(messages, anthropic.NewUserMessage(block))
		} else {
			messages = append(messages, anthropic.NewAssistantMessage(block))
		}
	}
	messages = append(messages, anthropic.NewUserMessage(anthropic.NewTextBlock(userText)))

	toolParams := make([]anthropic.ToolUnionParam, 0, len(tools))
	for _, t := range tools {
		toolParams = append(toolParams, anthropic.ToolUnionParam{OfTool: &anthropic.ToolParam{
			Name:        t.Name,
			Description: anthropic.String(t.Description),
			InputSchema: anthropic.ToolInputSchemaParam{
				Properties: t.InputSchema["properties"],
				Required:   toStringSlice(t.InputSchema["required"]),
			},
		}})
	}

	for i := 0; i < maxToolIterations; i++ {
		resp, err := a.client.Messages.New(ctx, anthropic.MessageNewParams{
			Model:     model,
			MaxTokens: 1024,
			System:    []anthropic.TextBlockParam{{Text: systemPrompt}},
			Messages:  messages,
			Tools:     toolParams,
		})
		if err != nil {
			return "", fmt.Errorf("claude: %w", err)
		}

		messages = append(messages, resp.ToParam())

		if resp.StopReason != anthropic.StopReasonToolUse {
			return extractText(resp), nil
		}

		var toolResults []anthropic.ContentBlockParamUnion
		for _, block := range resp.Content {
			toolUse, ok := block.AsAny().(anthropic.ToolUseBlock)
			if !ok {
				continue
			}
			result, isErr := runTool(ctx, tools, toolUse)
			toolResults = append(toolResults, anthropic.NewToolResultBlock(toolUse.ID, result, isErr))
		}
		messages = append(messages, anthropic.NewUserMessage(toolResults...))
	}

	return "", fmt.Errorf("claude: превышен лимit вызовов инструментов")
}

func runTool(ctx context.Context, tools []Tool, use anthropic.ToolUseBlock) (result string, isErr bool) {
	for _, t := range tools {
		if t.Name != use.Name {
			continue
		}
		out, err := t.Handle(ctx, use.Input)
		if err != nil {
			return err.Error(), true
		}
		return out, false
	}
	return fmt.Sprintf("неизвестный инструмент %q", use.Name), true
}

func extractText(resp *anthropic.Message) string {
	var sb strings.Builder
	for _, block := range resp.Content {
		if tb, ok := block.AsAny().(anthropic.TextBlock); ok {
			sb.WriteString(tb.Text)
		}
	}
	return sb.String()
}

func toStringSlice(v any) []string {
	list, ok := v.([]string)
	if ok {
		return list
	}
	anyList, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(anyList))
	for _, item := range anyList {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}
