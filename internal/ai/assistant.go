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
	"encoding/base64"
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
	// почему-то продолжает звать инструменты бесконечно. Запас на длинные
	// цепочки вида "пересчитай -> отчёт по группе -> сверка -> оформить в PDF".
	maxToolIterations = 12

	// maxHTTPAttempts — сколько раз повторяем запрос к OpenRouter при
	// временных сбоях (обрыв сети, таймаут, 429/5xx). Без этого один сетевой
	// «икание» рушил весь ответ бота; с ретраями — переживает.
	maxHTTPAttempts = 3
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
	apiKey      string
	model       string // «мозг»: диалог, отчёты, сверка, вызовы инструментов
	visionModel string // чтение чеков (зрение); по умолчанию совпадает с model
	baseURL     string
	http        *http.Client
}

// New создаёт клиента OpenRouter. apiKey — значение OPENROUTER_API_KEY.
// model — id модели в каталоге OpenRouter (например, "anthropic/claude-sonnet-4.6");
// пусто -> берётся defaultModel. baseURL — обычно оставляют пустым
// (используется https://openrouter.ai/api/v1), задаётся отдельно только
// если стоит прокси/самостоятельный gateway с тем же протоколом.
// visionModel пусто -> зрение идёт той же моделью, что и «мозг» (лучшее
// распознавание). Можно задать отдельную дешёвую модель (напр. Haiku) через
// OPENROUTER_VISION_MODEL, если распознавание чеков хочется удешевить.
func New(apiKey, model, visionModel, baseURL string) *Assistant {
	if model == "" {
		model = defaultModel
	}
	if visionModel == "" {
		visionModel = model
	}
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	return &Assistant{
		apiKey:      apiKey,
		model:       model,
		visionModel: visionModel,
		baseURL:     strings.TrimRight(baseURL, "/"),
		http:        &http.Client{Timeout: 90 * time.Second},
	}
}

type chatMessage struct {
	Role       string     `json:"role"`
	Content    any        `json:"content"` // string или []contentBlock (для кэширования)
	ToolCalls  []toolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

// contentBlock — блок контента; на статичный блок вешаем cache_control,
// чтобы OpenRouter/Anthropic кэшировали большой неизменный промпт и брали
// с него ~10% цены при повторных запросах (модель та же — «ум» не теряется).
type contentBlock struct {
	Type         string        `json:"type"`
	Text         string        `json:"text"`
	CacheControl *cacheControl `json:"cache_control,omitempty"`
}

type cacheControl struct {
	Type string `json:"type"` // "ephemeral"
}

// contentString извлекает текст из поля content ответа (там всегда строка).
func contentString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// systemWithCache формирует системное сообщение: большой статичный блок с
// пометкой кэширования + небольшой динамический блок (дата, сводка) без неё.
func systemWithCache(staticPart, dynamicPart string) chatMessage {
	blocks := []contentBlock{
		{Type: "text", Text: staticPart, CacheControl: &cacheControl{Type: "ephemeral"}},
	}
	if dynamicPart != "" {
		blocks = append(blocks, contentBlock{Type: "text", Text: dynamicPart})
	}
	return chatMessage{Role: "system", Content: blocks}
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
func (a *Assistant) Reply(ctx context.Context, staticSystem, dynamicSystem string, tools []Tool, history []Turn, userText string) (string, error) {
	messages := make([]chatMessage, 0, len(history)+2)
	messages = append(messages, systemWithCache(staticSystem, dynamicSystem))
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
			return contentString(respMsg.Content), nil
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
	return contentString(msg.Content), nil
}

// Мультимодальный запрос: контент пользователя — массив блоков (текст + картинка).
type visionContentPart struct {
	Type     string        `json:"type"`
	Text     string        `json:"text,omitempty"`
	ImageURL *visionImgURL `json:"image_url,omitempty"`
}

type visionImgURL struct {
	URL string `json:"url"`
}

type visionMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"` // string для system, []visionContentPart для user
}

type visionRequest struct {
	Model     string          `json:"model"`
	Messages  []visionMessage `json:"messages"`
	MaxTokens int             `json:"max_tokens,omitempty"`
}

// CompleteWithImage — одиночный запрос с картинкой: модель СМОТРИТ на
// изображение (фото чека) и отвечает по нему. mimeType — "image/jpeg"
// или "image/png".
func (a *Assistant) CompleteWithImage(ctx context.Context, systemPrompt, userText string, image []byte, mimeType string) (string, error) {
	payload, err := json.Marshal(visionRequest{
		Model: a.visionModel,
		Messages: []visionMessage{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: []visionContentPart{
				{Type: "text", Text: userText},
				{Type: "image_url", ImageURL: &visionImgURL{
					URL: "data:" + mimeType + ";base64," + base64.StdEncoding.EncodeToString(image),
				}},
			}},
		},
		MaxTokens: 1024,
	})
	if err != nil {
		return "", err
	}

	parsed, status, err := a.postCompletions(ctx, payload)
	if err != nil {
		return "", err
	}
	if parsed.Error != nil {
		return "", fmt.Errorf("openrouter: %s", parsed.Error.Message)
	}
	if status != http.StatusOK || len(parsed.Choices) == 0 {
		return "", fmt.Errorf("openrouter вернул %d", status)
	}
	return contentString(parsed.Choices[0].Message.Content), nil
}

func (a *Assistant) chat(ctx context.Context, messages []chatMessage, tools []toolDef) (chatMessage, string, error) {
	payload, err := json.Marshal(chatRequest{
		Model:     a.model,
		Messages:  messages,
		Tools:     tools,
		MaxTokens: 3072,
	})
	if err != nil {
		return chatMessage{}, "", err
	}

	parsed, status, err := a.postCompletions(ctx, payload)
	if err != nil {
		return chatMessage{}, "", err
	}
	if parsed.Error != nil {
		return chatMessage{}, "", fmt.Errorf("openrouter: %s", parsed.Error.Message)
	}
	if status != http.StatusOK {
		return chatMessage{}, "", fmt.Errorf("openrouter вернул %d", status)
	}
	if len(parsed.Choices) == 0 {
		return chatMessage{}, "", fmt.Errorf("openrouter: пустой ответ")
	}

	choice := parsed.Choices[0]
	return choice.Message, choice.FinishReason, nil
}

// postCompletions отправляет уже сериализованный запрос на /chat/completions
// и повторяет попытку при временных сбоях: обрыв сети, таймаут, ответы 429 и
// 5xx. Возвращает разобранный ответ и HTTP-статус последней попытки. Ошибки
// уровня приложения (200 с error в теле, 4xx кроме 429) не ретраятся — их
// разбирают вызывающие. backoff растёт (≈0.6s, 1.2s), но упирается в ctx.
func (a *Assistant) postCompletions(ctx context.Context, payload []byte) (chatResponse, int, error) {
	var lastErr error
	for attempt := 0; attempt < maxHTTPAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return chatResponse{}, 0, ctx.Err()
			case <-time.After(time.Duration(attempt) * 600 * time.Millisecond):
			}
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.baseURL+"/chat/completions", bytes.NewReader(payload))
		if err != nil {
			return chatResponse{}, 0, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+a.apiKey)

		resp, err := a.http.Do(req)
		if err != nil {
			// Сетевой сбой/таймаут — если контекст ещё жив, пробуем снова.
			lastErr = fmt.Errorf("openrouter: %w", err)
			if ctx.Err() != nil {
				return chatResponse{}, 0, ctx.Err()
			}
			continue
		}

		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			lastErr = fmt.Errorf("openrouter: чтение ответа: %w", err)
			continue
		}

		// Временная перегрузка провайдера — стоит повторить.
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			lastErr = fmt.Errorf("openrouter вернул %d: %s", resp.StatusCode, string(body))
			continue
		}

		var parsed chatResponse
		if err := json.Unmarshal(body, &parsed); err != nil {
			return chatResponse{}, resp.StatusCode, fmt.Errorf("openrouter: не удалось разобрать ответ (%d): %s", resp.StatusCode, string(body))
		}
		return parsed, resp.StatusCode, nil
	}
	return chatResponse{}, 0, lastErr
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
