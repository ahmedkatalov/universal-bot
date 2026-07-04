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

// loginResponse покрывает оба режима ответа /login: сразу токен, либо
// mode=select_profile со списком профилей (если у аккаунта их несколько).
type loginResponse struct {
	Token       string `json:"token"`
	AccessToken string `json:"access_token"`
	Mode        string `json:"mode"`
	User        struct {
		ID string `json:"id"`
	} `json:"user"`
	Profiles []struct {
		ID        string `json:"id"`
		UserID    string `json:"user_id"`
		IsPrimary bool   `json:"is_primary"`
	} `json:"profiles"`
}

func (c *Client) login(ctx context.Context) error {
	body, err := c.postJSON(ctx, "/api/auth/login", map[string]string{
		"email":    c.email,
		"password": c.password,
	})
	if err != nil {
		return err
	}

	var parsed loginResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return fmt.Errorf("cmf login: не удалось разобрать ответ: %w", err)
	}

	token := firstNonEmpty(parsed.Token, parsed.AccessToken)

	// Аккаунт с несколькими профилями: выбираем основной (или первый) и
	// дозапрашиваем токен через /select-profile — коду бота не нужен код
	// с почты, обычного логина email+пароль достаточно.
	if token == "" && parsed.Mode == "select_profile" && len(parsed.Profiles) > 0 {
		chosen := parsed.Profiles[0]
		for _, p := range parsed.Profiles {
			if p.IsPrimary {
				chosen = p
				break
			}
		}
		userID := firstNonEmpty(chosen.UserID, parsed.User.ID)
		selBody, err := c.postJSON(ctx, "/api/auth/select-profile", map[string]string{
			"user_id":    userID,
			"profile_id": chosen.ID,
		})
		if err != nil {
			return fmt.Errorf("cmf select-profile: %w", err)
		}
		var sel loginResponse
		if err := json.Unmarshal(selBody, &sel); err != nil {
			return fmt.Errorf("cmf select-profile: не удалось разобрать ответ: %w", err)
		}
		token = firstNonEmpty(sel.Token, sel.AccessToken)
	}

	if token == "" {
		return fmt.Errorf("cmf login: в ответе нет токена: %s", truncate(string(body), 200))
	}
	c.tokenMu.Lock()
	c.token = token
	c.tokenMu.Unlock()
	return nil
}

// postJSON отправляет POST с JSON-телом и возвращает тело ответа (200 OK).
func (c *Client) postJSON(ctx context.Context, path string, payload any) ([]byte, error) {
	data, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cmf %s: %w", path, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("cmf %s вернул %d: %s", path, resp.StatusCode, truncate(string(body), 200))
	}
	return body, nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// get выполняет GET с токеном, при 401 перелогинивается и повторяет один раз.
// branchID (если не пустой) уходит в заголовок X-Branch-ID — его требуют
// роуты с RequireBranch (например, /contract-payments).
func (c *Client) get(ctx context.Context, path string, query url.Values, branchID string) ([]byte, error) {
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
		if branchID != "" {
			req.Header.Set("X-Branch-ID", branchID)
		}

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
	body, err := c.get(ctx, "/api/clients/lookup", q, "")
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

// ContractRef — договор клиента.
type ContractRef struct {
	ID          string `json:"id"`
	BranchID    string `json:"branch_id"`
	Number      int64  `json:"number"`
	ProductName string `json:"product_name"`
	Remaining   int64  `json:"remaining"`
}

// ClientContracts возвращает договоры клиента.
func (c *Client) ClientContracts(ctx context.Context, clientID string) ([]ContractRef, error) {
	body, err := c.get(ctx, "/api/contracts/client/"+url.PathEscape(clientID)+"/contracts-summary", nil, "")
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

// ContractPayments возвращает платежи договора за период. branchID уходит
// в X-Branch-ID (роут /contract-payments требует его).
func (c *Client) ContractPayments(ctx context.Context, contractID, branchID string, from, to time.Time) ([]Payment, error) {
	q := url.Values{}
	q.Set("contract_id", contractID)
	q.Set("date_from", from.Format("2006-01-02"))
	q.Set("date_to", to.Format("2006-01-02"))
	q.Set("limit", "200")
	body, err := c.get(ctx, "/api/contract-payments/", q, branchID)
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
		payments, err := c.ContractPayments(ctx, contract.ID, contract.BranchID, from, to)
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

// AddPayment вносит платёж по договору в программу (POST /contract-payments).
// amountRub — сумма в рублях; paidAt — дата операции. branchID уходит в
// X-Branch-ID. Возвращает ошибку, если программа отклонила запрос.
func (c *Client) AddPayment(ctx context.Context, contractID, branchID string, amountRub int64, paidAt time.Time) error {
	payload := map[string]any{
		"contract_id": contractID,
		"amount":      amountRub,
		"paid_at":     paidAt.Format("2006-01-02"),
		"type":        "regular",
		"comment":     "Внесено ботом по чеку из WhatsApp",
	}
	data, _ := json.Marshal(payload)
	for attempt := 0; attempt < 2; attempt++ {
		c.tokenMu.Lock()
		token := c.token
		c.tokenMu.Unlock()
		if token == "" {
			if err := c.login(ctx); err != nil {
				return err
			}
			continue
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/contract-payments/", bytes.NewReader(data))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		if branchID != "" {
			req.Header.Set("X-Branch-ID", branchID)
		}
		resp, err := c.http.Do(req)
		if err != nil {
			return fmt.Errorf("cmf add payment: %w", err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode == http.StatusUnauthorized {
			c.tokenMu.Lock()
			c.token = ""
			c.tokenMu.Unlock()
			continue
		}
		if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
			return fmt.Errorf("программа отклонила платёж (%d): %s", resp.StatusCode, truncate(string(body), 200))
		}
		return nil
	}
	return fmt.Errorf("cmf add payment: не удалось авторизоваться")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
