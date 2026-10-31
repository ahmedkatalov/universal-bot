package ai

import (
	"strings"
	"testing"
)

func TestProviderConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name             string
		env              map[string]string
		base, model      string
		wantNil, wantErr bool
	}{
		{name: "disabled", wantNil: true},
		{name: "ODIRouter", env: map[string]string{"ODIROUTER_API_KEY": "odi-key"}, base: defaultBaseURL, model: defaultModel},
		{name: "legacy key and old URL", env: map[string]string{"OPENROUTER_API_KEY": "odi-key", "OPENROUTER_BASE_URL": "https://odirouter.ai/api/v1"}, base: defaultBaseURL, model: defaultModel},
		{name: "direct OpenAI key stays at OpenAI", env: map[string]string{"OPENAI_API_KEY": "openai-key"}, base: "https://api.openai.com/v1", model: defaultModel},
		{name: "explicit configuration", env: map[string]string{"ODIROUTER_API_KEY": "odi-key", "OPENROUTER_BASE_URL": "https://example.com/v1", "OPENROUTER_MODEL": "gpt-custom"}, base: "https://example.com/v1", model: "gpt-custom"},
		{name: "invalid protocol", env: map[string]string{"ODIROUTER_API_KEY": "odi-key", "OPENROUTER_API_MODE": "invalid"}, wantErr: true},
		{name: "unsupported reasoning", env: map[string]string{"ODIROUTER_API_KEY": "odi-key", "OPENROUTER_REASONING_EFFORT": "none"}, wantErr: true},
		{name: "credential in URL rejected", env: map[string]string{"ODIROUTER_API_KEY": "odi-key", "OPENROUTER_BASE_URL": "https://user:secret@example.com/v1"}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, k := range []string{"ODIROUTER_API_KEY", "ODIROUTER_BASE_URL", "ODIROUTER_MODEL", "ODIROUTER_VISION_MODEL", "OPENROUTER_API_KEY", "OPENROUTER_BASE_URL", "OPENROUTER_MODEL", "OPENROUTER_VISION_MODEL", "OPENAI_API_KEY", "OPENAI_BASE_URL", "OPENAI_MODEL", "OPENAI_VISION_MODEL", "OPENROUTER_API_MODE", "OPENROUTER_REASONING_EFFORT", "OPENROUTER_EXTRACT_REASONING_EFFORT"} {
				t.Setenv(k, "")
			}
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			a, err := NewFromEnv()
			if (err != nil) != tc.wantErr {
				t.Fatalf("configuration error: %v", err)
			}
			if tc.wantErr {
				return
			}
			if tc.wantNil {
				if a != nil {
					t.Error("missing key should disable AI")
				}
				return
			}
			if a.baseURL != tc.base || a.model != tc.model || a.visionModel != tc.model || !a.usesResponses(a.model) {
				t.Errorf("wrong configuration: %s", a.Description())
			}
			if strings.Contains(a.Description(), a.apiKey) {
				t.Error("key exposed in description")
			}
		})
	}
}
