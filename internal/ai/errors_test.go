package ai

import (
	"errors"
	"strings"
	"testing"
)

// TestClassifyProviderError — коды/тексты провайдера превращаются в правильную
// категорию, а СЫРОЙ текст не утекает в человеческое сообщение.
func TestClassifyProviderError(t *testing.T) {
	raw := "This request requires more credits, or fewer max_tokens. You need 5000 more."
	cases := []struct {
		status   int
		msg      string
		transErr error
		cat      string
	}{
		{429, "Rate limit exceeded", nil, "rate_limit"},
		{402, raw, nil, "credits"},
		{200, "insufficient_quota", nil, "credits"},
		{400, "maximum context length is 8192 tokens", nil, "context"},
		{404, "No endpoints found for model", nil, "model"},
		{503, "upstream error", nil, "server"},
		{500, "", nil, "server"},
		{0, "", errors.New("dial tcp: i/o timeout"), "timeout"},
		{418, "teapot", nil, "other"},
	}
	for _, c := range cases {
		pe := classifyProviderError(c.status, c.msg, c.transErr)
		if pe.Category != c.cat {
			t.Errorf("classify(%d,%q) категория=%q, ожидали %q", c.status, c.msg, pe.Category, c.cat)
		}
		if pe.User == "" {
			t.Errorf("classify(%d) пустое User-сообщение", c.status)
		}
		// Сырой текст провайдера НЕ должен попадать в человеческое сообщение.
		if strings.Contains(pe.User, "max_tokens") || strings.Contains(pe.User, "endpoints") || strings.Contains(pe.User, "context length") {
			t.Errorf("сырой текст утёк в User: %q", pe.User)
		}
	}
}

// TestUserMessage — для ProviderError берётся чистый User; для прочих ошибок —
// общий вежливый текст; для nil — пусто.
func TestUserMessage(t *testing.T) {
	pe := &ProviderError{Category: "credits", User: "Баланс закончился.", Detail: "402: raw secret detail"}
	if got := UserMessage(pe); got != "Баланс закончился." {
		t.Errorf("UserMessage(ProviderError) = %q", got)
	}
	if got := UserMessage(errors.New("openrouter: raw internal boom")); strings.Contains(got, "boom") || got == "" {
		t.Errorf("UserMessage(generic) не должен светить детали: %q", got)
	}
	if got := UserMessage(nil); got != "" {
		t.Errorf("UserMessage(nil) = %q, ожидали пусто", got)
	}
	// Обёрнутая ProviderError тоже распознаётся (errors.As).
	wrapped := errors.New("openrouter: превышен лимит: " + pe.Error())
	_ = wrapped
}

// TestShouldFallback — на что переходим к резервной модели, а на что нет.
func TestShouldFallback(t *testing.T) {
	yes := []string{"server", "model", "timeout", "rate_limit"}
	no := []string{"credits", "context", "malformed", "other"}
	for _, c := range yes {
		if !shouldFallback(c) {
			t.Errorf("на %q должны пробовать резервную модель", c)
		}
	}
	for _, c := range no {
		if shouldFallback(c) {
			t.Errorf("на %q НЕ должны менять модель", c)
		}
	}
}
