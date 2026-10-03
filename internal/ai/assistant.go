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
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	defaultBaseURL = "https://openrouter.ai/api/v1"
	defaultModel   = "~anthropic/claude-sonnet-latest" // алиас OpenRouter на последнюю версию Sonnet

	// maxToolIterations — предохранитель от зацикливания, если модель
	// почему-то продолжает звать инструменты бесконечно. Щедрый запас, чтобы
	// длинная «человеческая» команда в несколько шагов (пересчитай -> отчёт по
	// каждой группе -> сверка -> PDF -> разослать) доходила до конца, а не
	// упиралась в лимит. При исчерпании бот честно скажет, что осталось.
	maxToolIterations = 24

	// maxHTTPAttempts — сколько раз повторяем запрос к OpenRouter при
	// временных сбоях (обрыв сети, таймаут, 429/5xx). Без этого один сетевой
	// «икание» рушил весь ответ бота; с ретраями — переживает.
	maxHTTPAttempts = 3

	defaultChatMaxTokens   = 3072 // диалог/инструменты: нужен запас на аргументы инструментов
	defaultVisionMaxTokens = 1024 // чтение чека: ответ короткий, 3000 токенов не нужны
)

// ProviderError — ошибка ИИ-провайдера с ЧИСТЫМ сообщением для пользователя и
// техническими деталями для логов. Детали (сырой текст OpenRouter, ключи, коды)
// в WhatsApp НЕ показываем — только короткое человеческое User-сообщение.
type ProviderError struct {
	Category string // timeout | rate_limit | server | credits | context | model | malformed | other
	User     string // короткое человеческое сообщение (можно слать в чат)
	Detail   string // технические детали (только в лог)
}

func (e *ProviderError) Error() string { return "ai(" + e.Category + "): " + e.Detail }

// UserMessage возвращает ЧИСТОЕ сообщение для чата: для ProviderError — его
// User; иначе общий вежливый текст. Технические детали в чат не попадают.
func UserMessage(err error) string {
	if err == nil {
		return ""
	}
	var pe *ProviderError
	if errors.As(err, &pe) && pe.User != "" {
		return pe.User
	}
	return "Не получилось ответить — что-то с ИИ-сервисом. Попробуй ещё раз через минуту."
}

// classifyProviderError разбирает ответ провайдера в категорию и человеческое
// сообщение. providerMsg — текст ошибки от OpenRouter (в лог), transportErr —
// сетевой сбой, если был. status — HTTP-код (0, если сети не было).
func classifyProviderError(status int, providerMsg string, transportErr error) *ProviderError {
	m := strings.ToLower(providerMsg)
	detail := providerMsg
	if status > 0 {
		detail = fmt.Sprintf("%d: %s", status, providerMsg)
	}
	has := func(subs ...string) bool {
		for _, s := range subs {
			if strings.Contains(m, s) {
				return true
			}
		}
		return false
	}
	switch {
	case transportErr != nil:
		return &ProviderError{"timeout", "Сеть подвисла, не достучался до ИИ. Попробуй ещё раз через минуту.", transportErr.Error()}
	case status == http.StatusTooManyRequests || has("rate limit", "rate-limit", "too many requests"):
		return &ProviderError{"rate_limit", "Слишком много запросов к ИИ подряд — чуть перегружено. Повтори через минуту.", detail}
	case status == http.StatusPaymentRequired || has("credit", "insufficient", "quota", "billing", "payment"):
		return &ProviderError{"credits", "Не могу ответить: на балансе ИИ-сервиса закончились средства — нужно пополнить OpenRouter.", detail}
	case has("maximum context", "context length", "context_length", "too long") || (status == http.StatusBadRequest && has("token")):
		return &ProviderError{"context", "Слишком длинный запрос для ИИ. Сформулируй короче или разбей на части.", detail}
	case status == http.StatusNotFound || has("no endpoints", "no allowed providers", "model not found", "unavailable", "not a valid model"):
		return &ProviderError{"model", "Модель ИИ сейчас недоступна. Попробуй позже.", detail}
	case status >= 500:
		return &ProviderError{"server", "ИИ-сервис сейчас недоступен (это на их стороне). Попробуй чуть позже.", detail}
	case has("parse", "malformed", "unexpected"):
		return &ProviderError{"malformed", "ИИ вернул непонятный ответ. Повтори запрос.", detail}
	default:
		return &ProviderError{"other", "Не получилось ответить — что-то с ИИ-сервисом. Попробуй ещё раз.", detail}
	}
}

// shouldFallback — стоит ли пробовать следующую модель из цепочки при этой
// ошибке. Смена модели помогает при недоступности/перегрузке/сбоях провайдера,
// но НЕ при нехватке баланса (общий кошелёк) и слишком длинном контексте.
func shouldFallback(category string) bool {
	switch category {
	case "server", "model", "timeout", "rate_limit":
		return true
	default:
		return false
	}
}

// splitModels разбирает список моделей из env (через запятую).
func splitModels(raw string) []string {
	var out []string
	for _, p := range strings.Split(raw, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func envInt(key string, def int) int {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

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

	// Резервные модели: если основная недоступна/перегружена/отвечает 5xx, бот
	// пробует их по очереди (OPENROUTER_FALLBACK_MODELS / _VISION_FALLBACK_MODELS).
	fallbackModels       []string
	visionFallbackModels []string
	// Лимит токенов ответа по типу запроса (OPENROUTER_MAX_TOKENS / _VISION_MAX_TOKENS).
	chatMaxTokens   int
	visionMaxTokens int
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
		apiKey:               apiKey,
		model:                model,
		visionModel:          visionModel,
		baseURL:              strings.TrimRight(baseURL, "/"),
		http:                 &http.Client{Timeout: 90 * time.Second},
		fallbackModels:       splitModels(os.Getenv("OPENROUTER_FALLBACK_MODELS")),
		visionFallbackModels: splitModels(os.Getenv("OPENROUTER_VISION_FALLBACK_MODELS")),
		chatMaxTokens:        envInt("OPENROUTER_MAX_TOKENS", defaultChatMaxTokens),
		visionMaxTokens:      envInt("OPENROUTER_VISION_MAX_TOKENS", defaultVisionMaxTokens),
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
		if strings.TrimSpace(t.Text) == "" {
			continue // пустой ход (модель промолчала) — провайдер отвергает пустые сообщения
		}
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
		respMsg, _, err := a.chat(ctx, messages, toolDefs)
		if err != nil {
			return "", err
		}
		messages = append(messages, respMsg)

		// Выполняем инструменты, если модель их вызвала, НЕЗАВИСИМО от
		// finish_reason: часть моделей через OpenRouter возвращает tool_calls
		// вместе с finish_reason "stop"/"length". Раньше такой ответ отбрасывался
		// и текст «Записал платёж» уходил пользователю, а в БД ничего не писалось.
		if len(respMsg.ToolCalls) == 0 {
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

	// Лимит вызовов исчерпан. Инструменты УЖЕ отработали (платежи записаны,
	// отчёты отправлены) — нельзя вернуть ошибку и оставить владельца без
	// ответа: делаем финальный запрос БЕЗ инструментов, чтобы модель подвела
	// итог по тому, что успела сделать.
	messages = append(messages, chatMessage{
		Role:    "user",
		Content: "Лимит действий за один запрос исчерпан. Кратко подведи итог: что уже сделано и что осталось — без новых действий.",
	})
	// ВАЖНО: передаём toolDefs (а не nil). История содержит tool_calls и tool-
	// сообщения; запрос без определения инструментов часть моделей отвергает как
	// невалидный, и ошибка утекла бы в чат владельцу. С определениями запрос
	// корректен, а «без новых действий» почти всегда даёт текстовый итог.
	respMsg, _, err := a.chat(ctx, messages, toolDefs)
	if err != nil {
		return "", fmt.Errorf("openrouter: превышен лимит вызовов инструментов: %w", err)
	}
	out := strings.TrimSpace(contentString(respMsg.Content))
	// Если модель на этом (итоговом) шаге СНОВА просит инструменты — значит работа
	// НЕ закончена, а новые вызовы мы уже не выполняем. НЕЛЬЗЯ выдавать это за успех:
	// честно говорим, что осталось недоделанное, и предлагаем продолжить.
	if len(respMsg.ToolCalls) > 0 {
		pending := toolCallNames(respMsg.ToolCalls)
		if out == "" {
			return "Не успел доделать всё за один заход — осталось: " + pending +
				". Напиши «продолжай», и я доведу до конца.", nil
		}
		return out + "\n\n⚠️ Успел не всё: осталось " + pending + ". Напиши «продолжай» — доделаю.", nil
	}
	if out != "" {
		return out, nil
	}
	// Модель не вернула ни текста, ни новых действий — не знаем точно, всё ли
	// сделано. НЕ заявляем успех: нейтральная честная формулировка.
	return "Сделал по тому, что успел за один заход. Если чего-то не хватило — напиши, продолжу.", nil
}

// toolCallNames — список имён инструментов (для честного «осталось: …»), без
// дублей и в порядке первого появления.
func toolCallNames(calls []toolCall) string {
	seen := map[string]bool{}
	var names []string
	for _, c := range calls {
		n := c.Function.Name
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		names = append(names, n)
	}
	if len(names) == 0 {
		return "незавершённые действия"
	}
	return strings.Join(names, ", ")
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
	dataURL := "data:" + mimeType + ";base64," + base64.StdEncoding.EncodeToString(image)
	models := append([]string{a.visionModel}, a.visionFallbackModels...)
	var lastErr error
	for _, model := range models {
		payload, err := json.Marshal(visionRequest{
			Model: model,
			Messages: []visionMessage{
				{Role: "system", Content: systemPrompt},
				{Role: "user", Content: []visionContentPart{
					{Type: "text", Text: userText},
					{Type: "image_url", ImageURL: &visionImgURL{URL: dataURL}},
				}},
			},
			MaxTokens: a.visionMaxTokens,
		})
		if err != nil {
			return "", err
		}
		body, status, terr := a.postCompletions(ctx, payload)
		msg, cerr := interpretText(body, status, terr)
		if cerr == nil {
			return msg, nil
		}
		lastErr = cerr
		var pe *ProviderError
		if errors.As(cerr, &pe) && shouldFallback(pe.Category) {
			continue // пробуем следующую vision-модель
		}
		return "", cerr
	}
	return "", lastErr
}

// Ping проверяет, что ОСНОВНАЯ модель реально отвечает (правильный id, живой
// ключ, есть баланс). Нужен для стартовой самодиагностики: если модель молчит,
// весь «ум» бота и чтение чеков ломаются — важно увидеть это в логах сразу.
func (a *Assistant) Ping(ctx context.Context) error {
	_, err := a.Complete(ctx, "Ответь одним словом.", "Скажи: ок")
	return err
}

// PingVision проверяет, что модель ЗРЕНИЯ принимает картинки и отвечает (тем же
// id, что и мозг, или отдельным OPENROUTER_VISION_MODEL). Если ломается — чеки
// будут читаться плохо (падение на слабый OCR).
func (a *Assistant) PingVision(ctx context.Context) error {
	// Валидный PNG 64×64 (шахматка): содержимое неважно, проверяем сам вызов.
	// Раньше был 1×1 — некоторые шлюзы (напр. odirouter) отклоняют вырожденную
	// картинку как «битую» (400 Upstream rejected), из-за чего проверка зрения
	// ложно падала, хотя реальные чеки читаются нормально.
	img, _ := base64.StdEncoding.DecodeString(
		"iVBORw0KGgoAAAANSUhEUgAAAEAAAABACAIAAAAlC+aJAAAAh0lEQVR4nOzXwQnEIAAF0c2yxWj/xViO24A5TwLvHY2XISD83xjjc7LWOp7POR91/3s8fREBNQE1AbVr73388LT3/u7+6/+AgJqAmoDaZQ/EBNQE1ATU7IGagJqAmoCaPVATUBNQE1CzB2oCagJqAmr2QE1ATUBNQM0eqAmoCagJqP0DAAD//9CjYlDZqnHYAAAAAElFTkSuQmCC")
	_, err := a.CompleteWithImage(ctx, "Ответь одним словом.", "Что-нибудь видно? Ответь: ок", img, "image/png")
	return err
}

func (a *Assistant) chat(ctx context.Context, messages []chatMessage, tools []toolDef) (chatMessage, string, error) {
	models := append([]string{a.model}, a.fallbackModels...)
	var lastErr error
	for _, model := range models {
		payload, err := json.Marshal(chatRequest{
			Model:     model,
			Messages:  messages,
			Tools:     tools,
			MaxTokens: a.chatMaxTokens,
		})
		if err != nil {
			return chatMessage{}, "", err
		}
		body, status, terr := a.postCompletions(ctx, payload)
		msg, finish, cerr := interpretChat(body, status, terr)
		if cerr == nil {
			return msg, finish, nil
		}
		lastErr = cerr
		var pe *ProviderError
		if errors.As(cerr, &pe) && shouldFallback(pe.Category) {
			continue // основная модель недоступна/перегружена — пробуем резервную
		}
		return chatMessage{}, "", cerr
	}
	return chatMessage{}, "", lastErr
}

// interpretChat превращает сырой ответ HTTP в сообщение модели или в
// классифицированную ProviderError (чистое сообщение + детали в лог).
func interpretChat(body []byte, status int, transportErr error) (chatMessage, string, error) {
	if transportErr != nil {
		return chatMessage{}, "", classifyProviderError(0, transportErr.Error(), transportErr)
	}
	var parsed chatResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		// Тело не-JSON (часто у 5xx/429/HTML-страниц) — классифицируем по статусу.
		return chatMessage{}, "", classifyProviderError(status, snippet(body), nil)
	}
	if parsed.Error != nil {
		return chatMessage{}, "", classifyProviderError(status, parsed.Error.Message, nil)
	}
	if status != http.StatusOK {
		return chatMessage{}, "", classifyProviderError(status, snippet(body), nil)
	}
	if len(parsed.Choices) == 0 {
		return chatMessage{}, "", &ProviderError{Category: "malformed", User: "ИИ вернул пустой ответ. Повтори запрос.", Detail: "empty choices"}
	}
	choice := parsed.Choices[0]
	return choice.Message, choice.FinishReason, nil
}

// interpretText — как interpretChat, но для запросов без инструментов (Complete,
// зрение): возвращает текст ответа.
func interpretText(body []byte, status int, transportErr error) (string, error) {
	msg, _, err := interpretChat(body, status, transportErr)
	if err != nil {
		return "", err
	}
	return contentString(msg.Content), nil
}

// postCompletions отправляет запрос на /chat/completions и повторяет попытку при
// временных сбоях (обрыв сети, таймаут, 429, 5xx). Возвращает СЫРОЕ тело и статус
// последней попытки — классификацию/разбор делает вызывающий (interpretChat).
// transportErr != nil только если ответа так и не получили (сеть/таймаут).
// backoff растёт (≈0.6s, 1.2s), но упирается в ctx.
func (a *Assistant) postCompletions(ctx context.Context, payload []byte) (body []byte, status int, transportErr error) {
	var lastErr error
	var lastStatus int
	var lastBody []byte
	for attempt := 0; attempt < maxHTTPAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, 0, ctx.Err()
			case <-time.After(time.Duration(attempt) * 600 * time.Millisecond):
			}
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.baseURL+"/chat/completions", bytes.NewReader(payload))
		if err != nil {
			return nil, 0, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+a.apiKey)

		resp, err := a.http.Do(req)
		if err != nil {
			lastErr = err
			if ctx.Err() != nil {
				return nil, 0, ctx.Err()
			}
			continue // сетевой сбой — если контекст жив, пробуем снова
		}

		b, rerr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if rerr != nil {
			lastErr = rerr
			continue
		}
		lastStatus, lastBody = resp.StatusCode, b

		// Временная перегрузка провайдера — стоит повторить.
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			lastErr = fmt.Errorf("status %d", resp.StatusCode)
			continue
		}
		return b, resp.StatusCode, nil
	}
	// Ретраи исчерпаны: если хоть раз получили ответ (429/5xx) — отдаём его на
	// классификацию по статусу; иначе это чистый сетевой сбой.
	if lastStatus > 0 {
		return lastBody, lastStatus, nil
	}
	return nil, 0, lastErr
}

// snippet укорачивает текст для логов (не тащим гигантские тела в детали ошибки).
func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}

func runTool(ctx context.Context, tools []Tool, call toolCall) string {
	for _, t := range tools {
		if t.Name != call.Function.Name {
			continue
		}
		out, err := t.Handle(ctx, json.RawMessage(call.Function.Arguments))
		if err != nil {
			// Явный сигнал модели: действие НЕ выполнено. Нельзя выдавать за успех —
			// нужно честно сказать владельцу, что не вышло и почему.
			return "ИНСТРУМЕНТ НЕ ВЫПОЛНЕН — ошибка: " + err.Error() +
				". Действие НЕ сделано: честно скажи это владельцу и не пиши, что выполнил."
		}
		return out
	}
	return fmt.Sprintf("неизвестный инструмент %q", call.Function.Name)
}
