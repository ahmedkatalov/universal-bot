package bot

// Сквозной стенд для тестов: настоящий бот без WhatsApp. Сообщения подаются
// в handleGroupMessage / handlePrivateMessage / handleBankReceipt, всё, что бот
// «отправил», собирается в e.out. База — настоящий Postgres (TEST_DATABASE_URL),
// программа рассрочек — фейковый HTTP-сервер, ИИ — фейковый OpenRouter, ответы
// которого задаёт тест (e.ai). Без TEST_DATABASE_URL тесты пропускаются.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"

	"whatsapp-bot/internal/ai"
	"whatsapp-bot/internal/cmf"
	"whatsapp-bot/internal/db"
	"whatsapp-bot/internal/parser"
)

// e2eSent — сообщение, которое бот «отправил».
type e2eSent struct {
	Chat, Text, Quoted, ID string
}

// aiCall — один запрос бота к ИИ: системный промпт, сообщения, инструменты.
type aiCall struct {
	System   string
	Messages []map[string]any
	Tools    []string
	Raw      map[string]any
}

// aiReply — что вернёт фейковый ИИ: текст и/или вызовы инструментов.
type aiReply struct {
	Text      string
	ToolCalls []aiToolCall
}

type aiToolCall struct {
	Name string
	Args string // JSON
}

type e2eBot struct {
	*Bot
	t     *testing.T
	posts *fakePosts // что бот внёс в программу (POST)
	mu    sync.Mutex
	out   []e2eSent
	next  int
	calls []aiCall
	// ai — как отвечает фейковый ИИ на запрос (nil — ИИ выключен).
	ai func(c aiCall) aiReply
}

type e2eOpts struct {
	CMFClients []cmf.ClientInfo // клиенты программы (nil — программа выключена)
	CMFPays    map[string][]map[string]any
	WithAI     bool
	Admins     []string // номера админов отчётности (пусто — все админы)
	Owners     []string
	Groups     map[string]string // jid → название
}

// newE2E — свежий бот на настоящей базе. Каждый вызов чистит наблюдения
// сверки и память о плательщиках, чтобы тесты не влияли друг на друга.
func newE2E(t *testing.T, o e2eOpts) *e2eBot {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL не задан — пропускаю сквозной тест")
	}
	d, err := db.Connect(context.Background(), url)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	ctx := context.Background()
	if err := d.TestResetAll(ctx); err != nil {
		t.Fatalf("очистка тестовой базы: %v", err)
	}

	e := &e2eBot{t: t}
	b := &Bot{
		db:          d,
		aliases:     parser.NewAliasMap(),
		botName:     "Джарвис",
		history:     make(map[string][]ai.Turn),
		pending:     make(map[string][]pendingReceipt),
		sentMsgs:    make(map[string][]string),
		pendingCash: make(map[string]time.Time),
		clarify:     newClarifyState(),
		pairWindow:  15 * time.Minute,
		muted:       map[string]bool{},
		seenMsgs:    map[string]time.Time{},
		selfUser:    "79990000000",
	}
	b.lastProactive = make(map[string]time.Time)
	if len(o.Admins) > 0 {
		b.reportAdmins = map[string]bool{}
		for _, a := range o.Admins {
			b.reportAdmins[normalizePhone(a)] = true
		}
	}
	if len(o.Owners) > 0 {
		b.ownerNumbers = map[string]bool{}
		for _, a := range o.Owners {
			b.ownerNumbers[normalizePhone(a)] = true
		}
	}
	b.groupsCache = map[types.JID]string{}
	for j, n := range o.Groups {
		jid, _ := types.ParseJID(j)
		b.groupsCache[jid] = n
	}
	b.groupsFetched = time.Now().Add(100 * 365 * 24 * time.Hour)
	b.sendHook = func(chat types.JID, text, quotedID string) string {
		e.mu.Lock()
		defer e.mu.Unlock()
		e.next++
		id := fmt.Sprintf("BOT-%d-%d", time.Now().UnixNano(), e.next)
		e.out = append(e.out, e2eSent{chat.String(), text, quotedID, id})
		return id
	}
	if o.CMFClients != nil {
		e.posts = &fakePosts{}
		b.cmf = fakeCMFProgram(t, o.CMFClients, o.CMFPays, e.posts)
	}
	if o.WithAI {
		b.assistant = e.fakeAI()
	}
	e.Bot = b
	return e
}

// fakeCMFWithPays — программа: поиск клиентов по подстроке, договоры и оплаты.
// pays: client id → список оплат ({"amount":..,"paid_at":"2026-08-10",...}).
func fakeCMFWithPays(t *testing.T, clients []cmf.ClientInfo, pays map[string][]map[string]any) *cmf.Client {
	return fakeCMFProgram(t, clients, pays, nil)
}

// fakePosts — платежи, которые бот записал в фейковую программу.
type fakePosts struct {
	mu   sync.Mutex
	list []map[string]any
}

func (p *fakePosts) n() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.list)
}

// fakeCMFProgram — программа, которая ещё и принимает внесение платежей (POST):
// внесённое сразу видно в оплатах договора.
func fakeCMFProgram(t *testing.T, clients []cmf.ClientInfo, pays map[string][]map[string]any, posts *fakePosts) *cmf.Client {
	t.Helper()
	var mu sync.Mutex
	if pays == nil {
		pays = map[string][]map[string]any{}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/api/contract-payments"):
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			cid := strings.TrimPrefix(fmt.Sprint(body["contract_id"]), "c-")
			pays[cid] = append(pays[cid], map[string]any{"amount": body["amount"], "paid_at": body["paid_at"]})
			if posts != nil {
				posts.mu.Lock()
				posts.list = append(posts.list, body)
				posts.mu.Unlock()
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "p1"})
		case r.URL.Path == "/api/auth/login":
			_ = json.NewEncoder(w).Encode(map[string]string{"token": "t"})
		case r.URL.Path == "/api/clients/lookup":
			q := strings.ToLower(r.URL.Query().Get("full_name"))
			out := []map[string]string{}
			for _, c := range clients {
				if strings.Contains(strings.ToLower(c.FullName), q) {
					out = append(out, map[string]string{"id": c.ID, "full_name": c.FullName, "phone": c.Phone})
				}
			}
			_ = json.NewEncoder(w).Encode(out)
		case strings.HasPrefix(r.URL.Path, "/api/contracts/client/"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/contracts/client/"), "/contracts-summary")
			_ = json.NewEncoder(w).Encode([]map[string]any{{"id": "c-" + id, "branch_id": "b1", "number": 100, "product_name": "товар"}})
		case strings.HasPrefix(r.URL.Path, "/api/contract-payments"):
			cid := strings.TrimPrefix(r.URL.Query().Get("contract_id"), "c-")
			out := pays[cid]
			if out == nil {
				out = []map[string]any{}
			}
			_ = json.NewEncoder(w).Encode(out)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	t.Setenv("CMF_API_URL", srv.URL)
	t.Setenv("CMF_EMAIL", "bot@x")
	t.Setenv("CMF_PASSWORD", "p")
	return cmf.NewFromEnv()
}

// fakeAI — фейковый OpenRouter: отдаёт то, что вернёт e.ai.
func (e *e2eBot) fakeAI() *ai.Assistant {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req map[string]any
		_ = json.Unmarshal(body, &req)
		c := aiCall{Raw: req}
		if msgs, ok := req["messages"].([]any); ok {
			for _, m := range msgs {
				mm, _ := m.(map[string]any)
				if mm["role"] == "system" {
					c.System += fmt.Sprint(contentText(mm["content"]))
					continue
				}
				c.Messages = append(c.Messages, mm)
			}
		}
		if tools, ok := req["tools"].([]any); ok {
			for _, tl := range tools {
				if fm, ok := tl.(map[string]any)["function"].(map[string]any); ok {
					c.Tools = append(c.Tools, fmt.Sprint(fm["name"]))
				}
			}
		}
		e.mu.Lock()
		e.calls = append(e.calls, c)
		f := e.ai
		e.mu.Unlock()
		rep := aiReply{Text: "ок"}
		if f != nil {
			rep = f(c)
		}
		msg := map[string]any{"role": "assistant", "content": rep.Text}
		finish := "stop"
		if len(rep.ToolCalls) > 0 {
			var tcs []map[string]any
			for k, tc := range rep.ToolCalls {
				tcs = append(tcs, map[string]any{"id": fmt.Sprintf("call_%d", k), "type": "function",
					"function": map[string]any{"name": tc.Name, "arguments": tc.Args}})
			}
			msg["tool_calls"] = tcs
			finish = "tool_calls"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": msg, "finish_reason": finish}},
		})
	}))
	e.t.Cleanup(srv.Close)
	return ai.New("test-key", "test-model", "test-vision", srv.URL)
}

func contentText(c any) string {
	switch v := c.(type) {
	case string:
		return v
	case []any:
		var sb strings.Builder
		for _, p := range v {
			if pm, ok := p.(map[string]any); ok {
				sb.WriteString(fmt.Sprint(pm["text"]))
			}
		}
		return sb.String()
	}
	return ""
}

// lastUserText — текст последнего пользовательского сообщения в запросе к ИИ.
func (c aiCall) lastUserText() string {
	for k := len(c.Messages) - 1; k >= 0; k-- {
		if c.Messages[k]["role"] == "user" {
			return contentText(c.Messages[k]["content"])
		}
	}
	return ""
}

// hasToolResult — в запросе уже есть результат инструмента (второй круг).
func (c aiCall) hasToolResult() bool {
	for _, m := range c.Messages {
		if m["role"] == "tool" {
			return true
		}
	}
	return false
}

// toolResults — результаты инструментов в запросе.
func (c aiCall) toolResults() []string {
	var out []string
	for _, m := range c.Messages {
		if m["role"] == "tool" {
			out = append(out, contentText(m["content"]))
		}
	}
	return out
}

func (e *e2eBot) sent() []e2eSent {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]e2eSent(nil), e.out...)
}

func (e *e2eBot) last() e2eSent {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.out) == 0 {
		return e2eSent{}
	}
	return e.out[len(e.out)-1]
}

// sentSince — что бот отправил после n-го сообщения.
func (e *e2eBot) sentSince(n int) []e2eSent {
	e.mu.Lock()
	defer e.mu.Unlock()
	if n > len(e.out) {
		return nil
	}
	return append([]e2eSent(nil), e.out[n:]...)
}

// waitIdle — подождать фоновые горутины (ассистент, ИИ-разбор, сверка).
func (e *e2eBot) waitIdle() { time.Sleep(400 * time.Millisecond) }

var e2eSeq int

func e2eID(prefix string) string {
	e2eSeq++
	return fmt.Sprintf("%s-%d-%d", prefix, time.Now().UnixNano(), e2eSeq)
}

// e2eGroup — новая группа с уникальным JID.
func e2eGroup(name string) types.JID {
	return types.NewJID(e2eID("g"+name), types.GroupServer)
}

// user — JID участника по номеру.
func user(phone string) types.JID { return types.NewJID(phone, types.DefaultUserServer) }

// groupText — текстовое сообщение в группе (как пришло бы из WhatsApp).
func groupText(grp, sender types.JID, pushName, text string) *events.Message {
	return &events.Message{
		Info: types.MessageInfo{
			MessageSource: types.MessageSource{Chat: grp, Sender: sender, IsGroup: true},
			ID:            e2eID("M"),
			PushName:      pushName,
			Timestamp:     time.Now(),
		},
		Message: &waProto.Message{Conversation: proto.String(text)},
	}
}

// groupReply — ответ свайпом на сообщение quotedID (автор quotedSender, текст quotedText).
func groupReply(grp, sender types.JID, pushName, text, quotedID string, quotedSender types.JID, quotedText string) *events.Message {
	return &events.Message{
		Info: types.MessageInfo{
			MessageSource: types.MessageSource{Chat: grp, Sender: sender, IsGroup: true},
			ID:            e2eID("M"),
			PushName:      pushName,
			Timestamp:     time.Now(),
		},
		Message: &waProto.Message{ExtendedTextMessage: &waProto.ExtendedTextMessage{
			Text: proto.String(text),
			ContextInfo: &waProto.ContextInfo{
				StanzaID:      proto.String(quotedID),
				Participant:   proto.String(quotedSender.String()),
				QuotedMessage: &waProto.Message{Conversation: proto.String(quotedText)},
			},
		}},
	}
}

// privateText — сообщение боту в личку.
func privateText(sender types.JID, pushName, text string) *events.Message {
	return &events.Message{
		Info: types.MessageInfo{
			MessageSource: types.MessageSource{Chat: sender, Sender: sender},
			ID:            e2eID("P"),
			PushName:      pushName,
			Timestamp:     time.Now(),
		},
		Message: &waProto.Message{Conversation: proto.String(text)},
	}
}

// sendGroup — подать сообщение в группу и дождаться фоновой обработки.
func (e *e2eBot) sendGroup(msg *events.Message) {
	e.handleGroupMessage(context.Background(), msg)
	e.waitIdle()
}

// event — подать событие целиком (удаление, правка) и дождаться обработки.
func (e *e2eBot) event(msg *events.Message) {
	e.handleEvent(msg)
	e.waitIdle()
}

// sendReceipt — чек в группе: распознанный текст (как после OCR) и подпись.
// Возвращает id сообщения с чеком.
func (e *e2eBot) sendReceipt(grp, sender types.JID, pushName, ocrText, caption string) string {
	ctx := context.Background()
	waID := e2eID("CHK")
	text := ocrText
	if caption != "" {
		text = caption + "\n" + ocrText
	}
	rawID, _, err := e.db.SaveRawMessage(ctx, waID, grp.String(), sender.String(), sender.User, pushName, text, true, "", time.Now())
	if err != nil {
		e.t.Fatal(err)
	}
	msg := &events.Message{Info: types.MessageInfo{
		MessageSource: types.MessageSource{Chat: grp, Sender: sender, IsGroup: true},
		ID:            waID, PushName: pushName, Timestamp: time.Now(),
	}, Message: &waProto.Message{ImageMessage: &waProto.ImageMessage{Caption: proto.String(caption)}}}
	payer, payerAmount := e.resolveReceiptPayer(ctx, msg, caption)
	e.handleBankReceipt(ctx, grp, sender.String(), waID, text, rawID, time.Now(), nil, ".jpg", payer, payerAmount)
	e.waitIdle()
	return waID
}

// botJID — JID самого бота (для свайпа на его сообщения).
func (e *e2eBot) botJID() types.JID { return user(e.selfUser) }

// TestE2EHarnessSmoke — стенд работает: текстовый платёж без ИИ записывается.
func TestE2EHarnessSmoke(t *testing.T) {
	e := newE2E(t, e2eOpts{})
	grp := e2eGroup("smoke")
	e.sendGroup(groupText(grp, user("79001112233"), "Сафаи", "Ахмед 5000"))
	now := time.Now()
	from := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()) // текстовые платежи датируются днём
	sums, err := e.db.SummaryForPeriod(context.Background(), from, from.AddDate(0, 0, 1), []string{grp.String()})
	if err != nil {
		t.Fatal(err)
	}
	var total float64
	for _, s := range sums {
		total += s.Total
	}
	if total != 5000 {
		t.Errorf("сбор %v, ожидали 5000 (%+v)", total, sums)
	}
}

// TestE2EHarnessAI — фейковый ИИ: обращение по имени получает ответ ассистента,
// а инструменты вызываются по-настоящему.
func TestE2EHarnessAI(t *testing.T) {
	e := newE2E(t, e2eOpts{WithAI: true})
	grp := e2eGroup("ai")
	e.ai = func(c aiCall) aiReply {
		if !c.hasToolResult() && strings.Contains(c.lastUserText(), "сбор") {
			return aiReply{ToolCalls: []aiToolCall{{Name: "send_finance_report", Args: `{"from_date":"2026-10-01","to_date":"2026-10-31","format":"text"}`}}}
		}
		if c.hasToolResult() {
			return aiReply{Text: "Итог: " + strings.Join(c.toolResults(), " ")}
		}
		return aiReply{Text: "Привет! Чем помочь?"}
	}
	n := len(e.sent())
	e.sendGroup(groupText(grp, user("79001112233"), "Ахмед", "Джарвис привет"))
	if got := e.sentSince(n); len(got) != 1 || got[0].Text != "Привет! Чем помочь?" {
		t.Fatalf("ответ ассистента: %+v", got)
	}
	n = len(e.sent())
	e.sendGroup(groupText(grp, user("79001112233"), "Ахмед", "Джарвис какой сбор за октябрь"))
	if got := e.sentSince(n); len(got) != 1 || !strings.HasPrefix(got[0].Text, "Итог:") {
		t.Fatalf("ответ с инструментом: %+v", got)
	}
}
