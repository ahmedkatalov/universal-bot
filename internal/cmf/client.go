// Package cmf — клиент API приложения рассрочек (cmf): поиск клиентов,
// договоры, платежи. Бот сверяет чеки из WhatsApp-групп с платежами,
// внесёнными в программу, и напоминает о забытых.
package cmf

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

type Client struct {
	baseURL  string
	email    string
	password string
	http     *http.Client

	tokenMu sync.Mutex
	token   string
}

// NewFromEnv создаёт клиента из CMF_API_URL / CMF_EMAIL / CMF_PASSWORD.
// Возвращает nil, если переменные не заданы (интеграция выключена).
func NewFromEnv() *Client {
	base := strings.TrimRight(strings.TrimSpace(os.Getenv("CMF_API_URL")), "/")
	email := strings.TrimSpace(os.Getenv("CMF_EMAIL"))
	pass := strings.TrimSpace(os.Getenv("CMF_PASSWORD"))
	if base == "" || email == "" || pass == "" {
		return nil
	}
	return &Client{
		baseURL:  base,
		email:    email,
		password: pass,
		http:     &http.Client{Timeout: 30 * time.Second},
	}
}

func (c *Client) login(ctx context.Context) error {
	payload, _ := json.Marshal(map[string]string{"email": c.email, "password": c.password})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/auth/login", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("cmf login: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("cmf login вернул %d: %s", resp.StatusCode, truncate(string(body), 200))
	}
	var parsed struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return fmt.Errorf("cmf login: не удалось разобрать ответ: %w", err)
	}
	token := parsed.Token
	if token == "" {
		token = parsed.AccessToken
	}
	if token == "" {
		return fmt.Errorf("cmf login: в ответе нет токена (у бота должен быть аккаунт с одним профилем): %s", truncate(string(body), 200))
	}
	c.tokenMu.Lock()
	c.token = token
	c.tokenMu.Unlock()
	return nil
}

// get выполняет GET с токеном, при 401 перелогинивается и повторяет один раз.
func (c *Client) get(ctx context.Context, path string, query url.Values) ([]byte, error) {
	for attempt := 0; attempt < 2; attempt++ {
		c.tokenMu.Lock()
		token := c.token
		c.tokenMu.Unlock()
		if token == "" {
			if err := c.login(ctx); err != nil {
				return nil, err
			}
			c.tokenMu.Lock()
			token = c.token
			c.tokenMu.Unlock()
		}

		u := c.baseURL + path
		if len(query) > 0 {
			u += "?" + query.Encode()
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token)

		resp, err := c.http.Do(req)
		if err != nil {
			return nil, fmt.Errorf("cmf: %w", err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode == http.StatusUnauthorized {
			c.tokenMu.Lock()
			c.token = ""
			c.tokenMu.Unlock()
			continue
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("cmf %s вернул %d: %s", path, resp.StatusCode, truncate(string(body), 200))
		}
		return body, nil
	}
	return nil, fmt.Errorf("cmf: не удалось авторизоваться")
}

// looseItems достаёт массив объектов из ответа, который может быть голым
// массивом или обёрткой {"items": [...]} / {"data": [...]}.
func looseItems(body []byte) []json.RawMessage {
	var arr []json.RawMessage
	if err := json.Unmarshal(body, &arr); err == nil {
		return arr
	}
	var wrapped map[string]json.RawMessage
	if err := json.Unmarshal(body, &wrapped); err == nil {
		for _, key := range []string{"items", "data", "clients", "results"} {
			if raw, ok := wrapped[key]; ok {
				if err := json.Unmarshal(raw, &arr); err == nil {
					return arr
				}
			}
		}
	}
	return nil
}

// ClientInfo — клиент из cmf.
type ClientInfo struct {
	ID       string `json:"id"`
	FullName string `json:"full_name"`
	Phone    string `json:"phone"`
}

// LookupClients ищет клиентов по подстроке имени (регистронезависимо).
func (c *Client) LookupClients(ctx context.Context, fullName string) ([]ClientInfo, error) {
	q := url.Values{}
	q.Set("full_name", fullName)
	q.Set("limit", "10")
	body, err := c.get(ctx, "/api/clients/lookup", q)
	if err != nil {
		return nil, err
	}
	var out []ClientInfo
	for _, raw := range looseItems(body) {
		var ci ClientInfo
		if err := json.Unmarshal(raw, &ci); err == nil && ci.ID != "" {
			out = append(out, ci)
		}
	}
	return out, nil
}

// ContractRef — договор клиента (id + точка).
type ContractRef struct {
	ID       string `json:"id"`
	BranchID string `json:"branch_id"`
}

// ClientContracts возвращает договоры клиента.
func (c *Client) ClientContracts(ctx context.Context, clientID string) ([]ContractRef, error) {
	body, err := c.get(ctx, "/api/contracts/client/"+url.PathEscape(clientID)+"/contracts-summary", nil)
	if err != nil {
		return nil, err
	}
	var out []ContractRef
	for _, raw := range looseItems(body) {
		var cr ContractRef
		if err := json.Unmarshal(raw, &cr); err == nil && cr.ID != "" {
			out = append(out, cr)
		}
	}
	return out, nil
}

// Payment — платёж по договору.
type Payment struct {
	Amount int64     `json:"amount"`
	PaidAt time.Time `json:"paid_at"`
}

// ContractPayments возвращает платежи договора за период.
func (c *Client) ContractPayments(ctx context.Context, contractID string, from, to time.Time) ([]Payment, error) {
	q := url.Values{}
	q.Set("contract_id", contractID)
	q.Set("date_from", from.Format("2006-01-02"))
	q.Set("date_to", to.Format("2006-01-02"))
	q.Set("limit", "200")
	body, err := c.get(ctx, "/api/contract-payments/", q)
	if err != nil {
		return nil, err
	}
	var out []Payment
	for _, raw := range looseItems(body) {
		var p Payment
		if err := json.Unmarshal(raw, &p); err == nil {
			out = append(out, p)
		}
	}
	return out, nil
}

// HasPaymentAround проверяет, есть ли у клиента платёж на данную сумму
// в окне ±windowDays вокруг даты чека. Сумма сравнивается и в рублях,
// и в копейках (на случай, если cmf хранит в минорных единицах).
func (c *Client) HasPaymentAround(ctx context.Context, clientID string, amount float64, txDate time.Time, windowDays int) (bool, error) {
	contracts, err := c.ClientContracts(ctx, clientID)
	if err != nil {
		return false, err
	}
	from := txDate.AddDate(0, 0, -windowDays)
	to := txDate.AddDate(0, 0, windowDays)
	wantRub := int64(amount + 0.5)
	wantKop := int64(amount*100 + 0.5)
	for _, contract := range contracts {
		payments, err := c.ContractPayments(ctx, contract.ID, from, to)
		if err != nil {
			return false, err
		}
		for _, p := range payments {
			if p.Amount == wantRub || p.Amount == wantKop {
				return true, nil
			}
		}
	}
	return false, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
