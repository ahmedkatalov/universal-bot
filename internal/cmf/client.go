// Package cmf — клиент API приложения рассрочек (cmf): поиск клиентов,
// договоры, платежи. Бот сверяет чеки из WhatsApp-групп с платежами,
// внесёнными в программу, и напоминает о забытых.
package cmf

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"strconv"
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

	// Короткий кэш поиска клиентов: сверка за период ищет одни и те же имена/
	// основы много раз («ещё раз» — снова все), и без кэша бот засыпал бы
	// программу сотнями одинаковых запросов (риск упереться в лимит частоты).
	cacheMu     sync.Mutex
	lookupCache map[string]lookupEntry
}

type lookupEntry struct {
	at  time.Time
	res []ClientInfo
}

const lookupCacheTTL = 3 * time.Minute

// readRetries — сколько раз пробуем ЧИТАЮЩИЙ запрос при временном сбое (сеть,
// перегрузка, 5xx). Запись платежа НЕ повторяем: при таймауте платёж мог уже
// записаться, и повтор задвоил бы его в программе.
const readRetries = 3

// NewFromEnv создаёт клиента из CMF_API_URL / CMF_EMAIL / CMF_PASSWORD.
// Возвращает nil, если переменные не заданы (интеграция выключена).
func NewFromEnv() *Client {
	base := strings.TrimRight(strings.TrimSpace(os.Getenv("CMF_API_URL")), "/")
	// Пути запросов уже начинаются с /api. Если адрес задали с хвостом «/api»
	// (частая ошибка), получилось бы /api/api/... → 404 на КАЖДЫЙ запрос.
	base = strings.TrimRight(strings.TrimSuffix(base, "/api"), "/")
	email := strings.TrimSpace(os.Getenv("CMF_EMAIL"))
	pass := strings.TrimSpace(os.Getenv("CMF_PASSWORD"))
	if base == "" || email == "" || pass == "" {
		return nil
	}
	return &Client{
		baseURL:     base,
		email:       email,
		password:    pass,
		http:        &http.Client{Timeout: 30 * time.Second},
		lookupCache: map[string]lookupEntry{},
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
	body, err := c.postJSON(ctx, "вход", "/api/auth/login", map[string]string{
		"email":    c.email,
		"password": c.password,
	})
	if err != nil {
		return err
	}

	var parsed loginResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return &APIError{Op: "вход", Status: http.StatusOK, Body: "непонятный ответ: " + truncate(string(body), 120)}
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
		selBody, err := c.postJSON(ctx, "вход", "/api/auth/select-profile", map[string]string{
			"user_id":    userID,
			"profile_id": chosen.ID,
		})
		if err != nil {
			return err
		}
		var sel loginResponse
		if err := json.Unmarshal(selBody, &sel); err != nil {
			return &APIError{Op: "вход", Status: http.StatusOK, Body: "непонятный ответ выбора профиля"}
		}
		token = firstNonEmpty(sel.Token, sel.AccessToken)
	}

	if token == "" {
		return &APIError{Op: "вход", Status: http.StatusOK, Body: "в ответе нет токена"}
	}
	c.tokenMu.Lock()
	c.token = token
	c.tokenMu.Unlock()
	return nil
}

// backoff — пауза перед повтором временно не удавшегося запроса.
func backoff(attempt int) time.Duration {
	return time.Duration(attempt*attempt) * 500 * time.Millisecond // 0.5s, 2s, 4.5s
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

// postJSON отправляет POST с JSON-телом (используется для ВХОДА — его повторять
// безопасно) и возвращает тело ответа (200 OK). Временные сбои повторяет.
func (c *Client) postJSON(ctx context.Context, op, path string, payload any) ([]byte, error) {
	data, _ := json.Marshal(payload)
	var lastErr error
	for attempt := 0; attempt < readRetries; attempt++ {
		if attempt > 0 {
			if err := sleepCtx(ctx, backoff(attempt)); err != nil {
				return nil, &APIError{Op: op, Err: err}
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(data))
		if err != nil {
			return nil, &APIError{Op: op, Err: err}
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := c.http.Do(req)
		if err != nil {
			lastErr = &APIError{Op: op, Err: err}
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			ae := &APIError{Op: op, Status: resp.StatusCode, Body: truncate(string(body), 200)}
			if ae.Transient() {
				lastErr = ae
				continue
			}
			return nil, ae
		}
		return body, nil
	}
	return nil, lastErr
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// ensureToken возвращает действующий токен, при необходимости входя заново.
func (c *Client) ensureToken(ctx context.Context) (string, error) {
	c.tokenMu.Lock()
	t := c.token
	c.tokenMu.Unlock()
	if t != "" {
		return t, nil
	}
	if err := c.login(ctx); err != nil {
		return "", err
	}
	c.tokenMu.Lock()
	t = c.token
	c.tokenMu.Unlock()
	return t, nil
}

func (c *Client) clearToken() {
	c.tokenMu.Lock()
	c.token = ""
	c.tokenMu.Unlock()
}

// get выполняет ЧИТАЮЩИЙ GET с токеном: при 401 один раз перелогинивается, при
// временных сбоях (сеть, 429, 5xx) повторяет с паузой. op — что делаем (для
// понятной причины ошибки). branchID (если не пустой) уходит в X-Branch-ID —
// его требуют роуты с RequireBranch (например, /contract-payments).
func (c *Client) get(ctx context.Context, op, path string, query url.Values, branchID string) ([]byte, error) {
	var lastErr error
	relogged := false
	for attempt := 0; attempt <= readRetries; attempt++ {
		if attempt > 0 && lastErr != nil {
			if err := sleepCtx(ctx, backoff(attempt)); err != nil {
				return nil, &APIError{Op: op, Err: err}
			}
		}
		token, err := c.ensureToken(ctx)
		if err != nil {
			return nil, err
		}

		u := c.baseURL + path
		if len(query) > 0 {
			u += "?" + query.Encode()
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, &APIError{Op: op, Err: err}
		}
		req.Header.Set("Authorization", "Bearer "+token)
		if branchID != "" {
			req.Header.Set("X-Branch-ID", branchID)
		}

		resp, err := c.http.Do(req)
		if err != nil {
			lastErr = &APIError{Op: op, Err: err}
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode == http.StatusUnauthorized {
			c.clearToken()
			if relogged {
				return nil, &APIError{Op: op, Status: resp.StatusCode, Body: truncate(string(body), 200)}
			}
			relogged = true
			lastErr = nil // повтор сразу, с новым входом
			continue
		}
		if resp.StatusCode != http.StatusOK {
			ae := &APIError{Op: op, Status: resp.StatusCode, Body: truncate(string(body), 200)}
			if ae.Transient() {
				lastErr = ae
				continue
			}
			return nil, ae
		}
		return body, nil
	}
	if lastErr == nil {
		lastErr = &APIError{Op: op, Status: http.StatusUnauthorized}
	}
	return nil, lastErr
}

// Ping проверяет связь с программой ПО-НАСТОЯЩЕМУ: свежий вход + пробный поиск.
// nil — программа отвечает, логин верный, путь поиска существует. Используется
// при старте (в лог) и когда владелец спрашивает «есть доступ к программе?».
func (c *Client) Ping(ctx context.Context) error {
	c.clearToken()
	if err := c.login(ctx); err != nil {
		return err
	}
	q := url.Values{}
	q.Set("full_name", "ива")
	q.Set("limit", "1")
	_, err := c.get(ctx, "поиск клиента", "/api/clients/lookup", q, "")
	var ae *APIError
	if errors.As(err, &ae) && (ae.Status == http.StatusBadRequest || ae.Status == http.StatusUnprocessableEntity) {
		return nil // сервер ответил (формат пробного запроса не понравился) — связь есть
	}
	return err
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

// --- Толерантный разбор ответов программы ---
// Реальный API рассрочек может называть поля чуть иначе, чем ожидается, или
// возвращать суммы строкой ("25000"/"25000.00"), а даты — в разных форматах.
// Строгий разбор в таких случаях МОЛЧА давал бы пустые имена и нулевые суммы
// (тогда сверка показывала бы «не внесён» на всё). Поэтому парсим гибко: берём
// первое непустое из набора возможных ключей и понимаем число как число ИЛИ
// строку. Известные (текущие) ключи всегда идут первыми — поведение не меняется.

// jsonStr достаёт строковое значение по первому подходящему ключу.
func jsonStr(m map[string]json.RawMessage, keys ...string) string {
	for _, k := range keys {
		if raw, ok := m[k]; ok {
			var s string
			if json.Unmarshal(raw, &s) == nil && strings.TrimSpace(s) != "" {
				return strings.TrimSpace(s)
			}
			// число, пришедшее там, где ждём строку (например, id как число)
			var n json.Number
			if json.Unmarshal(raw, &n) == nil && n.String() != "" {
				return n.String()
			}
		}
	}
	return ""
}

// jsonInt читает целое (сумму/номер) как число ИЛИ строку ("25000", "25000.00").
func jsonInt(m map[string]json.RawMessage, keys ...string) int64 {
	for _, k := range keys {
		raw, ok := m[k]
		if !ok {
			continue
		}
		// null/пусто в этом ключе — НЕ считаем за 0, а пробуем следующий ключ.
		// Иначе `{"amount":null,"sum":"25000"}` вернул бы 0 (json.Unmarshal null
		// в float64 не ошибка и оставляет 0), и запасной ключ не сработал бы.
		if t := strings.TrimSpace(string(raw)); t == "" || t == "null" {
			continue
		}
		var f float64
		if json.Unmarshal(raw, &f) == nil {
			return int64(math.Round(f))
		}
		var s string
		if json.Unmarshal(raw, &s) == nil {
			s = strings.TrimSpace(strings.ReplaceAll(s, " ", ""))
			s = strings.ReplaceAll(s, ",", ".") // запятая — десятичный разделитель
			if v, err := strconv.ParseFloat(s, 64); err == nil {
				return int64(math.Round(v))
			}
		}
	}
	return 0
}

// cmfLoc — часовой пояс программы (Москва) для дат без пояса.
var cmfLoc = time.FixedZone("MSK", 3*3600)

// jsonTime читает дату/время в нескольких форматах (RFC3339, «2006-01-02»,
// unix-секунды). Пустое/непонятное -> нулевое время (для сопоставления не
// критично: период фильтруется на стороне программы, сверяем по сумме).
func jsonTime(m map[string]json.RawMessage, keys ...string) time.Time {
	for _, k := range keys {
		raw, ok := m[k]
		if !ok {
			continue
		}
		var s string
		if json.Unmarshal(raw, &s) == nil && s != "" {
			if t, err := time.Parse(time.RFC3339, s); err == nil {
				return t
			}
			// Время без пояса программа пишет по Москве: «2026-08-21 22:00» — это
			// 21-е, а не 22-е число.
			for _, layout := range []string{"2006-01-02T15:04:05", "2006-01-02 15:04:05", "2006-01-02T15:04", "2006-01-02 15:04"} {
				if t, err := time.ParseInLocation(layout, s, cmfLoc); err == nil {
					return t
				}
			}
			for _, layout := range []string{"2006-01-02", "02.01.2006"} {
				if t, err := time.Parse(layout, s); err == nil {
					return t
				}
			}
		}
		var unix int64
		if json.Unmarshal(raw, &unix) == nil && unix > 0 {
			return time.Unix(unix, 0)
		}
	}
	return time.Time{}
}

// ClientInfo — клиент из cmf. Теги json нужны для json.Marshal (сохранение
// кандидатов в наблюдение); чтение идёт через UnmarshalJSON (толерантно к ключам).
type ClientInfo struct {
	ID       string `json:"id"`
	FullName string `json:"full_name"`
	Phone    string `json:"phone"`
}

func (ci *ClientInfo) UnmarshalJSON(data []byte) error {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		return err
	}
	ci.ID = jsonStr(m, "id", "client_id", "uuid", "_id")
	ci.FullName = jsonStr(m, "full_name", "fullName", "name", "fio", "client_name", "client_full_name")
	ci.Phone = jsonStr(m, "phone", "phone_number", "tel", "mobile")
	return nil
}

// LookupClients ищет клиентов по подстроке имени (регистронезависимо).
func (c *Client) LookupClients(ctx context.Context, fullName string) ([]ClientInfo, error) {
	key := strings.ToLower(strings.TrimSpace(fullName))
	c.cacheMu.Lock()
	if e, ok := c.lookupCache[key]; ok && time.Since(e.at) < lookupCacheTTL {
		c.cacheMu.Unlock()
		return append([]ClientInfo(nil), e.res...), nil
	}
	c.cacheMu.Unlock()

	q := url.Values{}
	q.Set("full_name", fullName)
	q.Set("limit", "10")
	body, err := c.get(ctx, "поиск клиента", "/api/clients/lookup", q, "")
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
	c.cacheMu.Lock()
	if c.lookupCache == nil || len(c.lookupCache) > 2000 {
		c.lookupCache = map[string]lookupEntry{} // ленивая инициализация + предохранитель от роста
	}
	c.lookupCache[key] = lookupEntry{at: time.Now(), res: append([]ClientInfo(nil), out...)}
	c.cacheMu.Unlock()
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

func (cr *ContractRef) UnmarshalJSON(data []byte) error {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		return err
	}
	cr.ID = jsonStr(m, "id", "contract_id", "uuid", "_id")
	cr.BranchID = jsonStr(m, "branch_id", "branchId", "branch", "point_id")
	cr.Number = jsonInt(m, "number", "contract_number", "num")
	cr.ProductName = jsonStr(m, "product_name", "productName", "product", "name")
	cr.Remaining = jsonInt(m, "remaining", "remaining_amount", "balance", "debt")
	return nil
}

// ClientContracts возвращает договоры клиента.
func (c *Client) ClientContracts(ctx context.Context, clientID string) ([]ContractRef, error) {
	body, err := c.get(ctx, "договоры", "/api/contracts/client/"+url.PathEscape(clientID)+"/contracts-summary", nil, "")
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
	Amount    int64     `json:"amount"`     // в единицах программы (рубли ИЛИ копейки)
	PaidAt    time.Time `json:"paid_at"`    // дата оплаты (как указал оператор)
	CreatedAt time.Time `json:"created_at"` // когда ВНЕСЛИ в программу (если программа отдаёт)

	// Договор (рассрочка), к которому относится оплата — заполняет PaymentsBetween,
	// чтобы сверка показывала, в КАКУЮ рассрочку клиента ушла оплата.
	ContractID     string `json:"contract_id,omitempty"`
	ContractNumber int64  `json:"contract_number,omitempty"`
	Product        string `json:"product,omitempty"`
}

func (p *Payment) UnmarshalJSON(data []byte) error {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		return err
	}
	p.Amount = jsonInt(m, "amount", "sum", "value", "amount_rub", "paid_amount", "payment_amount")
	p.PaidAt = jsonTime(m, "paid_at", "paidAt", "date", "payment_date", "created_at")
	p.CreatedAt = jsonTime(m, "created_at", "createdAt", "entered_at", "inserted_at")
	p.ContractID = jsonStr(m, "contract_id", "contractId")
	return nil
}

// ContractPayments возвращает платежи договора за период. branchID уходит
// в X-Branch-ID (роут /contract-payments требует его).
func (c *Client) ContractPayments(ctx context.Context, contractID, branchID string, from, to time.Time) ([]Payment, error) {
	q := url.Values{}
	q.Set("contract_id", contractID)
	q.Set("date_from", from.Format("2006-01-02"))
	q.Set("date_to", to.Format("2006-01-02"))
	q.Set("limit", "200")
	body, err := c.get(ctx, "платежи", "/api/contract-payments/", q, branchID)
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

// PaymentsBetween возвращает ВСЕ платежи клиента (по всем договорам) за период
// [from, to]. Основа для сверки: собираем все внесённые оплаты клиента разом и
// сопоставляем с его чеками 1:1 (потребляя каждый платёж один раз).
func (c *Client) PaymentsBetween(ctx context.Context, clientID string, from, to time.Time) ([]Payment, error) {
	contracts, err := c.ClientContracts(ctx, clientID)
	if err != nil {
		return nil, err
	}
	var out []Payment
	for _, contract := range contracts {
		payments, err := c.ContractPayments(ctx, contract.ID, contract.BranchID, from, to)
		if err != nil {
			return nil, err
		}
		for i := range payments {
			payments[i].ContractID = contract.ID
			payments[i].ContractNumber = contract.Number
			payments[i].Product = contract.ProductName
		}
		out = append(out, payments...)
	}
	return out, nil
}

// PaymentsAround возвращает платежи клиента в окне ±windowDays вокруг даты чека.
func (c *Client) PaymentsAround(ctx context.Context, clientID string, txDate time.Time, windowDays int) ([]Payment, error) {
	return c.PaymentsBetween(ctx, clientID, txDate.AddDate(0, 0, -windowDays), txDate.AddDate(0, 0, windowDays))
}

// HasPaymentAround проверяет, есть ли у клиента ОДИНОЧНЫЙ платёж на данную сумму
// в окне ±windowDays вокруг даты чека. Используется для напоминания «чек не
// внесён». Сравниваем точно (в рублях и в копейках — cmf может хранить минорные);
// агрегатную сумму НЕ используем: несвязанные платежи клиента в окне легко
// сложатся в сумму чека и НАСТОЯЩИЙ невнесённый чек молча пропадёт из напоминаний
// (ложный пропуск для напоминания хуже лишнего пинга).
func (c *Client) HasPaymentAround(ctx context.Context, clientID string, amount float64, txDate time.Time, windowDays int) (bool, error) {
	pays, err := c.PaymentsAround(ctx, clientID, txDate, windowDays)
	if err != nil {
		return false, err
	}
	wantRub := int64(amount + 0.5)
	wantKop := int64(amount*100 + 0.5)
	for _, p := range pays {
		if p.Amount == wantRub || p.Amount == wantKop {
			return true, nil
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
	// ЗАПИСЬ: при сетевом сбое/таймауте НЕ повторяем — платёж мог уже записаться,
	// и повтор задвоил бы его. Повтор только после 401 (протухший вход), когда
	// программа точно ничего не записала.
	for attempt := 0; attempt < 2; attempt++ {
		token, err := c.ensureToken(ctx)
		if err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/contract-payments/", bytes.NewReader(data))
		if err != nil {
			return &APIError{Op: "внесение", Err: err}
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		if branchID != "" {
			req.Header.Set("X-Branch-ID", branchID)
		}
		resp, err := c.http.Do(req)
		if err != nil {
			return fmt.Errorf("связь с программой оборвалась при внесении — ПРОВЕРЬ в программе, записался ли платёж, прежде чем вносить снова: %w", &APIError{Op: "внесение", Err: err})
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode == http.StatusUnauthorized {
			c.clearToken()
			continue
		}
		if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
			return fmt.Errorf("программа отклонила платёж: %w", &APIError{Op: "внесение", Status: resp.StatusCode, Body: truncate(string(body), 200)})
		}
		return nil
	}
	return &APIError{Op: "внесение", Status: http.StatusUnauthorized}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
