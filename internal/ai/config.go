package ai

import (
	"fmt"
	"net/url"
	"os"
	"strings"
)

func envOr(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

// NewFromEnv поддерживает прежние OPENROUTER_* настройки. Предпочтительный
// ключ для ODIRouter — ODIROUTER_API_KEY; значение никогда не логируется.
func NewFromEnv() (*Assistant, error) {
	key := envOr("ODIROUTER_API_KEY", os.Getenv("OPENROUTER_API_KEY"))
	base := envOr("ODIROUTER_BASE_URL", envOr("OPENROUTER_BASE_URL", defaultBaseURL))
	model := envOr("ODIROUTER_MODEL", envOr("OPENROUTER_MODEL", defaultModel))
	vision := envOr("ODIROUTER_VISION_MODEL", os.Getenv("OPENROUTER_VISION_MODEL"))
	if key == "" {
		key = strings.TrimSpace(os.Getenv("OPENAI_API_KEY"))
		if key != "" {
			base = envOr("OPENAI_BASE_URL", "https://api.openai.com/v1")
			model = envOr("OPENAI_MODEL", defaultModel)
			vision = os.Getenv("OPENAI_VISION_MODEL")
		}
	}
	if key == "" {
		return nil, nil
	}
	if base == "https://odirouter.ai/api/v1" || base == "https://odirouter.ai/v1" {
		base = defaultBaseURL
	}
	u, err := url.Parse(base)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" {
		return nil, fmt.Errorf("неверный базовый URL ИИ: нужен http(s) адрес без пароля и параметров запроса")
	}
	a := New(key, model, vision, base)
	switch a.apiMode {
	case "auto", "responses", "chat_completions":
	default:
		return nil, fmt.Errorf("OPENROUTER_API_MODE: допустимы auto, responses, chat_completions")
	}
	for _, effort := range []string{a.reasoning, a.extractReasoning} {
		switch effort {
		case "low", "medium", "high", "xhigh", "max":
		default:
			return nil, fmt.Errorf("уровень рассуждений GPT: допустимы low, medium, high, xhigh, max")
		}
	}
	return a, nil
}

func (a *Assistant) Description() string {
	mode := "chat_completions"
	if a.usesResponses(a.model) {
		mode = "responses"
	}
	return fmt.Sprintf("%s; модель %s; зрение %s; API %s", a.baseURL, a.model, a.visionModel, mode)
}
