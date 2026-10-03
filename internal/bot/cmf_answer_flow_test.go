package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/types"

	"whatsapp-bot/internal/cmf"
	"whatsapp-bot/internal/db"
	"whatsapp-bot/internal/parser"
)

// fakeCMF — программа рассрочек с поиском клиентов по подстроке ФИО.
func fakeCMF(t *testing.T, clients []cmf.ClientInfo) *cmf.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/auth/login":
			_ = json.NewEncoder(w).Encode(map[string]string{"token": "t"})
		case "/api/clients/lookup":
			q := strings.ToLower(r.URL.Query().Get("full_name"))
			var out []map[string]string
			for _, c := range clients {
				if strings.Contains(strings.ToLower(c.FullName), q) {
					out = append(out, map[string]string{"id": c.ID, "full_name": c.FullName})
				}
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

type sent struct{ chat, text, quoted, id string }

type flowBot struct {
	*Bot
	mu   sync.Mutex
	out  []sent
	next int
}

func (f *flowBot) last() sent {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.out) == 0 {
		return sent{}
	}
	return f.out[len(f.out)-1]
}

func newFlowBot(t *testing.T, clients []cmf.ClientInfo) *flowBot {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL не задан — пропускаю сквозной тест")
	}
	d, err := db.Connect(context.Background(), url)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	// Запомненные плательщики от прошлых прогонов — не в счёт.
	if err := d.SettingSet(context.Background(), settingPayerMap, ""); err != nil {
		t.Fatal(err)
	}
	f := &flowBot{}
	f.Bot = &Bot{db: d, aliases: parser.NewAliasMap(), cmf: fakeCMF(t, clients), botName: "Джарвис",
		clarify: newClarifyState(), muted: map[string]bool{}}
	f.Bot.sendHook = func(chat types.JID, text, quotedID string) string {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.next++
		id := fmt.Sprintf("BOTMSG-%d-%d", time.Now().UnixNano(), f.next)
		f.out = append(f.out, sent{chat.String(), text, quotedID, id})
		return id
	}
	return f
}

// Сквозной сценарий со скриншота: чек → вопрос ответом на чек → сотрудник
// отвечает свайпом «Альмурзаева Разет» → бот привязывает чек к этой клиентке,
// записывает её клиентом чека и запоминает плательщика; следующий чек от того
// же плательщика привязывается сам, без вопроса.
func TestCmfAnswerFlow(t *testing.T) {
	f := newFlowBot(t, []cmf.ClientInfo{
		{ID: "11", FullName: "Мусиева Марха Ахмедовна"},
		{ID: "12", FullName: "Сайдаева Марха Майрсолтаевна"},
		{ID: "77", FullName: "Альмурзаева Разет Ахмедовна"},
	})
	ctx := context.Background()
	grp := types.NewJID(fmt.Sprintf("flow%d", time.Now().UnixNano()), types.GroupServer)
	waID := fmt.Sprintf("CHECK-%d", time.Now().UnixNano())
	rawID, _, err := f.db.SaveRawMessage(ctx, waID, grp.String(), "emp@s.whatsapp.net", "79990000000", "Сафаи", "чек", true, "", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.InsertBankReceipt(ctx, db.BankReceiptInput{RawMessageID: rawID, RecipientRaw: "Альмурзаева Марха А", Amount: 19000,
		ClientConfirmed: true, GroupJID: grp.String(), TxDate: time.Now()}); err != nil {
		t.Fatal(err)
	}
	f.cmfWatchReceipt(ctx, grp, "emp@s.whatsapp.net", "Альмурзаева Марха А", 19000, time.Now(), rawID)

	ask := f.last()
	if ask.quoted != waID {
		t.Errorf("вопрос должен быть ответом на сам чек: %+v", ask)
	}
	if !strings.Contains(ask.text, "Альмурзаева Марха А") || !strings.Contains(ask.text, "та же фамилия") {
		t.Errorf("вопрос: %q", ask.text)
	}

	// Ответ свайпом.
	watchID, ok := f.cmfWatchByAsk(ctx, ask.id)
	if !ok {
		t.Fatal("вопрос не запомнен")
	}
	if !f.applyCmfWatchAnswer(ctx, grp, watchID, "Альмурзаева Разет", true) {
		t.Fatal("ответ не обработан")
	}
	reply := f.last()
	if !strings.Contains(reply.text, "Альмурзаева Разет Ахмедовна") || strings.Contains(reply.text, "привет") {
		t.Errorf("ответ бота: %q", reply.text)
	}
	w, _, _ := f.db.CmfWatchByID(ctx, watchID)
	if w.Status != "watch" || w.ClientID != "77" {
		t.Errorf("наблюдение: %+v", w)
	}
	if rs, err := f.db.ReceiptSenderByMessageID(ctx, waID); err != nil || !strings.Contains(rs.Recipient+" "+rs.Client, "Альмурзаева Разет") {
		t.Errorf("клиент чека в учёте: %+v %v", rs, err)
	}

	// Второй чек того же плательщика — без вопроса.
	before := len(f.out)
	waID2 := waID + "-2"
	rawID2, _, _ := f.db.SaveRawMessage(ctx, waID2, grp.String(), "emp@s.whatsapp.net", "79990000000", "Сафаи", "чек", true, "", time.Now())
	f.cmfWatchReceipt(ctx, grp, "emp@s.whatsapp.net", "Альмурзаева Марха А", 19000, time.Now(), rawID2)
	if len(f.out) != before {
		t.Errorf("по запомненному плательщику бот снова спросил: %q", f.last().text)
	}

	// Ответ без свайпа номером варианта на новый вопрос.
	waID3 := waID + "-3"
	rawID3, _, _ := f.db.SaveRawMessage(ctx, waID3, grp.String(), "emp@s.whatsapp.net", "79990000000", "Сафаи", "чек", true, "", time.Now())
	f.cmfWatchReceipt(ctx, grp, "emp@s.whatsapp.net", "Марха", 5000, time.Now(), rawID3)
	q := f.last()
	if !strings.Contains(q.text, "1. ") {
		t.Fatalf("ожидали вопрос с вариантами: %q", q.text)
	}
	if f.tryContextAnswer(ctx, grp, "Альмурзаева Разет") {
		t.Error("без свайпа новое ФИО не должно перехватываться (может быть имя перед новым чеком)")
	}
	if !f.tryContextAnswer(ctx, grp, "2") {
		t.Fatal("номер варианта без свайпа не понят")
	}
	if !strings.HasPrefix(f.last().text, "✅") {
		t.Errorf("ответ: %q", f.last().text)
	}
}
