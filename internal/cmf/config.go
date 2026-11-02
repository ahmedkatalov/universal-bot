package cmf

import (
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// ConfigurationError contains field names only: neither passwords nor a URL
// accidentally containing credentials may be copied into logs or a chat.
type ConfigurationError struct {
	Missing    []string
	InvalidURL bool
}

func (e *ConfigurationError) Error() string {
	if len(e.Missing) > 0 {
		return "не заполнены настройки программы: " + strings.Join(e.Missing, ", ")
	}
	return "неверный CMF_API_URL: нужен адрес http:// или https:// без Markdown-ссылок, логина, пароля, параметров и фрагмента"
}

// NewFromEnv preserves the optional-integration API used by existing callers.
// Diagnostics and startup should use NewFromEnvChecked to retain the reason.
func NewFromEnv() *Client {
	c, _ := NewFromEnvChecked()
	return c
}

// NewFromEnvChecked distinguishes a deliberately disabled integration (all
// three fields empty) from a partially configured or malformed connection.
func NewFromEnvChecked() (*Client, error) {
	base := strings.TrimSpace(os.Getenv("CMF_API_URL"))
	email := strings.TrimSpace(os.Getenv("CMF_EMAIL"))
	pass := strings.TrimSpace(os.Getenv("CMF_PASSWORD"))
	if base == "" && email == "" && pass == "" {
		return nil, nil
	}
	var missing []string
	for _, item := range []struct{ key, value string }{{"CMF_API_URL", base}, {"CMF_EMAIL", email}, {"CMF_PASSWORD", pass}} {
		if item.value == "" {
			missing = append(missing, item.key)
		}
	}
	if len(missing) > 0 {
		return nil, &ConfigurationError{Missing: missing}
	}
	u, err := url.Parse(base)
	if err != nil || u == nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, &ConfigurationError{InvalidURL: true}
	}
	if u.Port() != "" {
		// url.Parse checks syntax; NewRequest also checks invalid host/ports.
		if _, err := http.NewRequest(http.MethodGet, u.String(), nil); err != nil {
			return nil, &ConfigurationError{InvalidURL: true}
		}
	}
	u.Path = strings.TrimRight(strings.TrimSuffix(strings.TrimRight(u.Path, "/"), "/api"), "/")
	u.RawPath = ""
	return &Client{
		baseURL:     strings.TrimRight(u.String(), "/"),
		email:       email,
		password:    pass,
		http:        &http.Client{Timeout: 30 * time.Second},
		lookupCache: map[string]lookupEntry{},
	}, nil
}
