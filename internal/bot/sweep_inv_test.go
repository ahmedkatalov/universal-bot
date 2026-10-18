package bot

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
	"sync/atomic"
	"testing"
	"time"

	"whatsapp-bot/internal/ai"
	"whatsapp-bot/internal/cmf"
	"whatsapp-bot/internal/db"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.mau.fi/whatsmeow"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
	"path/filepath"
)

// Регрессии разбора жалоб владельца (программа, скорость, команды, поиск чека).

// 1. [server] CMF unreachable (e.g. bot's program password changed) is completely silent, and checks that arrive during the outage are dropped for good after 72 h

func TestINV_01_CmfOutageSilentAndAbandoned(t *testing.T) {
	ctx := context.Background()
	grp, emp := e2eGroup("zzout"), user("79281119001")
	e := newE2E(t, e2eOpts{WithAI: true})
	e.ai = aiNamesAndPayments("Каталов Ахмед")
	var locked atomic.Bool // вход в программу отвечает 401
	locked.Store(true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/auth/login":
			if locked.Load() {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"token": "t"})
		case r.URL.Path == "/api/clients/lookup":
			_ = json.NewEncoder(w).Encode([]map[string]string{{"id": "1", "full_name": "Каталов Ахмед Нажудович"}})
		case strings.HasPrefix(r.URL.Path, "/api/contracts/client/"):
			_ = json.NewEncoder(w).Encode([]map[string]any{{"id": "c-1", "branch_id": "b1"}})
		case strings.HasPrefix(r.URL.Path, "/api/contract-payments"):
			_ = json.NewEncoder(w).Encode([]map[string]any{})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	t.Setenv("CMF_API_URL", srv.URL)
	t.Setenv("CMF_EMAIL", "bot@x")
	t.Setenv("CMF_PASSWORD", "p")
	e.cmf = cmf.NewFromEnv()

	ck := e.sendReceipt(grp, emp, "Сафаи", sberCheckOn(time.Now().AddDate(0, 0, -1), "14000.00", "10:00:00"), "Каталов Ахмед")
	n := 0 // предупреждение может прийти сразу, при первом же обращении к программе
	e.remindNow() // +30 ч фоновой сверки
	e.remindNow() // +60 ч
	warned := false
	for _, m := range e.sentSince(n) {
		if strings.Contains(strings.ToLower(m.Text), "программ") {
			warned = true
		}
	}
	st, _, _ := e.db.TestCmfWatchState(ctx, ck)
	t.Logf("60 ч без связи: статус чека %q, сообщений от бота %d", st, len(e.sentSince(n)))
	if !warned {
		t.Errorf("60 ч программа не пускает бота — никому ни слова (статус чека %q)", st)
	}
	locked.Store(false)  // пароль поправили — связь есть
	got := e.remindNow() // +90 ч; чек так и не внесён — должны напомнить
	st, cid, _ := e.db.TestCmfWatchState(ctx, ck)
	if !hasReminder(got) || st != "reminded" {
		t.Errorf("связь вернулась, а невнесённый чек навсегда брошен: status=%q client=%q, напоминаний нет", st, cid)
	}
}

// 2. [server] (Production only) Answering «🤔 Чей это чек?» records the client but never updates the CMF watch, so the check stays 'noname' and is never checked against the program

// Passes on HEAD (fixed there); the same scenario on 7d11619 leaves status="noname".
func TestINV_02_ClarifyAnswerReachesWatch(t *testing.T) {
	ctx := context.Background()
	grp, emp := e2eGroup("zzcl"), user("79281119006")
	e := newE2E(t, e2eOpts{WithAI: true, CMFClients: katalov})
	e.ai = aiNamesAndPayments("Каталов Ахмед")
	e.groupsCache[grp] = "Оплата"
	ck := e.sendReceipt(grp, emp, "Сафаи", sberCheckOn(time.Now().AddDate(0, 0, -1), "14000.00", "10:00:00"), "")
	st0, _, _ := e.db.TestCmfWatchState(ctx, ck)
	e.age(grp.String())
	e.clarifyTick(ctx)
	q := e.lastWith("Чей это чек")
	if q.ID == "" {
		t.Fatalf("нет вопроса «чей чек»: %+v", e.sent())
	}
	e.sendGroup(groupReply(grp, emp, "Сафаи", "Каталов Ахмед", q.ID, e.botJID(), q.Text))
	time.Sleep(300 * time.Millisecond)
	st, cid, _ := e.db.TestCmfWatchState(ctx, ck)
	got := e.remindNow()
	t.Logf("до ответа: %q; после ответа: status=%q client=%q; напоминание: %v", st0, st, cid, hasReminder(got))
	if cid != "1" || !hasReminder(got) {
		t.Errorf("клиента назвали ответом на «Чей это чек?», а сверка его не узнала: status=%q", st)
	}
}

// 3. [server] Program lookups run synchronously inside the WhatsApp event handler, so one reply to «за кого этот платёж?» freezes the whole bot for 21 s (slow program) or 2 min (hung program)

func TestINV_03_CmfReplyBlocksMessageHandler(t *testing.T) {
	grp, emp := e2eGroup("zzblk"), user("79281119002")
	e := newE2E(t, e2eOpts{WithAI: true})
	e.ai = aiNamesAndPayments("Иванов Иван", "Каталов Ахмед")
	var slow atomic.Int64 // задержка каждого ответа программы, нс
	var lookups atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if d := time.Duration(slow.Load()); d > 0 {
			select {
			case <-time.After(d):
			case <-r.Context().Done():
				return
			}
		}
		switch r.URL.Path {
		case "/api/auth/login":
			_ = json.NewEncoder(w).Encode(map[string]string{"token": "t"})
		case "/api/clients/lookup":
			lookups.Add(1)
			out := []map[string]string{}
			if strings.Contains("каталов ахмед нажудович", strings.ToLower(r.URL.Query().Get("full_name"))) {
				out = append(out, map[string]string{"id": "1", "full_name": "Каталов Ахмед Нажудович"})
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
	e.cmf = cmf.NewFromEnv()

	e.sendReceipt(grp, emp, "Сафаи", sberCheckOn(time.Now(), "14000.00", "10:00:00"), "Иванов Иван")
	ask := e.lastWith("За кого этот платёж")
	if ask.ID == "" {
		t.Fatalf("нет вопроса: %+v", e.sent())
	}
	lat := 3 * time.Second // программа отвечает по 3 с (ZZ_LAT=35s — программа висит)
	if s := os.Getenv("ZZ_LAT"); s != "" {
		lat, _ = time.ParseDuration(s)
	}
	slow.Store(int64(lat))
	lookups.Store(0)
	reply := groupReply(grp, emp, "Сафаи", "Каталова Ахмеда", ask.ID, e.botJID(), ask.Text)
	start := time.Now()
	e.handleGroupMessage(context.Background(), reply) // так его зовёт whatsmeow — синхронно
	took := time.Since(start)
	t.Logf("обработка одного ответа: %v, поисков в программе %d; ответ бота: %q", took, lookups.Load(), e.last().Text)
	if took > 5*time.Second {
		t.Errorf("один ответ на «за кого этот платёж?» держит обработчик WhatsApp %v — все группы и личка ждут", took)
	}
}

// 4. [server] Errors in the typo-tolerant client search are swallowed, so a rate-limited program looks like «такого клиента в программе не нашёл» and the watch gets the terminal status 'unmatched'

func TestINV_04_CmfFuzzyErrorsBecomeNotFound(t *testing.T) {
	ctx := context.Background()
	grp, emp := e2eGroup("zzrl"), user("79281119003")
	e := newE2E(t, e2eOpts{WithAI: true})
	e.ai = aiNamesAndPayments("Иса Ахмедов")
	var limited atomic.Bool
	limited.Store(true)
	var lookups atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/auth/login":
			_ = json.NewEncoder(w).Encode(map[string]string{"token": "t"})
		case r.URL.Path == "/api/clients/lookup":
			if k := lookups.Add(1); limited.Load() && k > 1 {
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
			out := []map[string]string{}
			if strings.Contains("ахмедов иса магомедович", strings.ToLower(r.URL.Query().Get("full_name"))) {
				out = append(out, map[string]string{"id": "7", "full_name": "Ахмедов Иса Магомедович"})
			}
			_ = json.NewEncoder(w).Encode(out)
		case strings.HasPrefix(r.URL.Path, "/api/contracts/client/"):
			_ = json.NewEncoder(w).Encode([]map[string]any{{"id": "c-7", "branch_id": "b1"}})
		case strings.HasPrefix(r.URL.Path, "/api/contract-payments"):
			_ = json.NewEncoder(w).Encode([]map[string]any{})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	t.Setenv("CMF_API_URL", srv.URL)
	t.Setenv("CMF_EMAIL", "bot@x")
	t.Setenv("CMF_PASSWORD", "p")
	e.cmf = cmf.NewFromEnv()

	start := time.Now()
	ck := e.sendReceipt(grp, emp, "Сафаи", sberCheckOn(time.Now().AddDate(0, 0, -1), "14000.00", "10:00:00"), "Иса Ахмедов")
	for i := 0; i < 60; i++ { // разбор ждёт повторов 429 (≈7 с на каждый поиск по слову)
		if st, _, _ := e.db.TestCmfWatchState(ctx, ck); st != "lookup" {
			break
		}
		time.Sleep(time.Second)
	}
	st, _, _ := e.db.TestCmfWatchState(ctx, ck)
	t.Logf("через %v: статус %q, поисков %d; бот написал: %q", time.Since(start).Round(time.Second), st, lookups.Load(), e.last().Text)
	if st == "unmatched" || strings.Contains(e.last().Text, "не нашёл") {
		t.Errorf("программа ответила 429, а бот сказал группе «такого клиента в программе не нашёл» (статус %q)", st)
	}
	limited.Store(false) // программа снова отвечает
	got := e.remindNow()
	st, cid, _ := e.db.TestCmfWatchState(ctx, ck)
	if cid != "7" || !hasReminder(got) {
		t.Errorf("после восстановления связи чек не проверяется: status=%q client=%q", st, cid)
	}
}

// 5. [server] 30 watches that always fail block the whole reminder queue (DueCmfWatches takes the 30 oldest, and an error just means continue, forever)

func TestINV_05_CmfStuckWatchesBlockQueue(t *testing.T) {
	ctx := context.Background()
	grp, emp := e2eGroup("zzhol"), user("79281119004")
	e := newE2E(t, e2eOpts{WithAI: true})
	e.ai = aiNamesAndPayments("Каталов Ахмед", "Ахмедов Иса")
	names := map[string]string{"1": "Каталов Ахмед Нажудович", "2": "Ахмедов Иса Магомедович"}
	var broken atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/auth/login":
			_ = json.NewEncoder(w).Encode(map[string]string{"token": "t"})
		case r.URL.Path == "/api/clients/lookup":
			q := strings.ToLower(r.URL.Query().Get("full_name"))
			out := []map[string]string{}
			for id, n := range names {
				if strings.Contains(strings.ToLower(n), q) {
					out = append(out, map[string]string{"id": id, "full_name": n})
				}
			}
			_ = json.NewEncoder(w).Encode(out)
		case strings.HasPrefix(r.URL.Path, "/api/contracts/client/"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/contracts/client/"), "/contracts-summary")
			_ = json.NewEncoder(w).Encode([]map[string]any{{"id": "c-" + id + "-0", "branch_id": "b1"}, {"id": "c-" + id + "-1", "branch_id": "b1"}})
		case strings.HasPrefix(r.URL.Path, "/api/contract-payments"):
			if broken.Load() && r.URL.Query().Get("contract_id") == "c-2-1" {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":"X-Branch-ID required"}`))
				return
			}
			_ = json.NewEncoder(w).Encode([]map[string]any{})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	t.Setenv("CMF_API_URL", srv.URL)
	t.Setenv("CMF_EMAIL", "bot@x")
	t.Setenv("CMF_PASSWORD", "p")
	e.cmf = cmf.NewFromEnv()

	for i := 0; i < 30; i++ { // 30 чеков клиента со «сломанным» договором
		e.sendReceipt(grp, emp, "Сафаи", sberCheckOn(time.Now().AddDate(0, 0, -1), fmt.Sprintf("%d.00", 5000+i*100), "10:00:00"), "Ахмедов Иса")
	}
	if err := e.db.TestAgeCmfWatches(ctx, time.Hour); err != nil { // они старше нового чека
		t.Fatal(err)
	}
	broken.Store(true)
	ck := e.sendReceipt(grp, emp, "Сафаи", sberCheckOn(time.Now().AddDate(0, 0, -1), "14000.00", "12:00:00"), "Каталов Ахмед")
	var got []e2eSent
	for i := 0; i < 3; i++ { // три круга фоновой сверки
		got = append(got, e.remindNow()...)
	}
	st, cid, _ := e.db.TestCmfWatchState(ctx, ck)
	t.Logf("новый чек Каталова: status=%q client=%q; напоминание: %v", st, cid, hasReminder(got))
	if st != "reminded" {
		t.Errorf("новый невнесённый чек ни разу не проверен: status=%q (впереди очереди 30 «вечно падающих»)", st)
	}
}

// 6. [HEAD] (HEAD only) The reminder cycle now re-checks up to 100 already-reminded checks before the new ones: about 13× more program requests in the same 5-min budget, and on overrun nothing new is reminded while sent reminders are not saved and repeat every 30 min

func TestINV_06_RemindStarvedByRemindedBacklog(t *testing.T) {
	ctx := context.Background()
	grp1, grp2 := e2eGroup("zzs1"), e2eGroup("zzs2")
	e := newE2E(t, e2eOpts{WithAI: true})
	pool, err := pgxpool.New(ctx, os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	const abc = "абвгдежзиклмнопрстуфхцчшэюя"
	ab := []rune(abc)
	var words []string
	for i := 0; i < 200; i++ {
		words = append(words, string(ab[i%len(ab)])+string(ab[(i/len(ab))%len(ab)]))
	}
	for _, q := range []string{
		`INSERT INTO contacts (canonical_name) SELECT 'Клиентов' || w || ' Ахмед' FROM unnest($1::text[]) w`,
		`WITH s AS (SELECT g, now() - make_interval(secs => (g::float8 / 600 * 100 * 86400)::int) AS at FROM generate_series(1, 600) g),
     rm AS (INSERT INTO raw_messages (wa_message_id, wa_group_jid, sender_jid, sender_name, body, has_media, received_at, parsed, created_at)
            SELECT 'ZZ' || g, CASE WHEN g % 2 = 0 THEN $2 ELSE $3 END, '79001112233@s.whatsapp.net', 'Сафаи', 'чек', true, at, true, at FROM s
            RETURNING id, wa_message_id, wa_group_jid, created_at)
     INSERT INTO bank_receipts (raw_message_id, bank, recipient_raw, contact_id, amount, doc_number, group_jid, tx_date, created_at, client_confirmed)
     SELECT rm.id, 'Сбер', c.canonical_name, c.id, 5000 + (substr(rm.wa_message_id, 3)::int % 30) * 500, 'D' || rm.wa_message_id,
            rm.wa_group_jid, rm.created_at, rm.created_at, true
     FROM rm JOIN contacts c ON c.id = 1 + (substr(rm.wa_message_id, 3)::int % 200) WHERE cardinality($1::text[]) > 0`,
		`WITH pick AS (SELECT br.raw_message_id, rm.wa_group_jid, br.amount, br.tx_date, br.contact_id, c.canonical_name, br.created_at,
            row_number() OVER (PARTITION BY br.created_at < now() - interval '25 hours' ORDER BY br.created_at DESC) AS k,
            br.created_at < now() - interval '25 hours' AS old
        FROM bank_receipts br JOIN raw_messages rm ON rm.id = br.raw_message_id JOIN contacts c ON c.id = br.contact_id
        WHERE cardinality($1::text[]) > 0)
     INSERT INTO cmf_watch (raw_message_id, group_jid, sender_jid, client_text, client_id, client_name, amount, tx_date, status, created_at)
     SELECT raw_message_id, wa_group_jid, '79001112233@s.whatsapp.net', canonical_name, (999 + contact_id)::text, canonical_name, amount, tx_date,
            CASE WHEN NOT old THEN 'watch' WHEN k <= 5 THEN 'watch' WHEN k <= 105 THEN 'reminded' ELSE 'found' END, created_at
     FROM pick`,
	} {
		args := []any{words}
		if strings.Contains(q, "$2") {
			args = append(args, grp1.String(), grp2.String())
		}
		if _, err := pool.Exec(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	const lat = 20 * time.Millisecond // каждый ответ программы
	var reqs atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(lat):
		case <-r.Context().Done():
			return
		}
		reqs.Add(1)
		switch {
		case r.URL.Path == "/api/auth/login":
			_ = json.NewEncoder(w).Encode(map[string]string{"token": "t"})
		case strings.HasPrefix(r.URL.Path, "/api/contracts/client/"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/contracts/client/"), "/contracts-summary")
			_ = json.NewEncoder(w).Encode([]map[string]any{{"id": "c-" + id + "-0", "branch_id": "b1"}, {"id": "c-" + id + "-1", "branch_id": "b1"}})
		default:
			_ = json.NewEncoder(w).Encode([]map[string]any{})
		}
	}))
	t.Cleanup(srv.Close)
	t.Setenv("CMF_API_URL", srv.URL)
	t.Setenv("CMF_EMAIL", "bot@x")
	t.Setenv("CMF_PASSWORD", "p")
	e.cmf = cmf.NewFromEnv()

	due, _ := e.db.DueCmfWatches(ctx, time.Now().Add(-cmfRemindAfter()), 30)
	oldReqs := len(due) * 3 // 7d11619: по каждому due — договоры + 2 договора
	budget := time.Duration(oldReqs*8) * lat
	cctx, cancel := context.WithTimeout(ctx, budget)
	n := len(e.sent())
	start := time.Now()
	e.cmfCheckDue(cctx)
	cancel()
	took := time.Since(start)
	e.waitIdle()
	reminded := 0
	for _, w := range due {
		if v, ok, _ := e.db.CmfWatchByID(ctx, w.ID); ok && v.Status == "reminded" {
			reminded++
		}
	}
	msgs := 0
	for _, m := range e.sentSince(n) {
		if strings.Contains(m.Text, "⏰") {
			msgs++
		}
	}
	t.Logf("due %d; бюджет %v (7d11619 нужно %d запросов ≈ %v); круг %v, запросов %d; reminded %d из %d; ⏰-сообщений %d",
		len(due), budget, oldReqs, time.Duration(oldReqs)*lat, took, reqs.Load(), reminded, len(due), msgs)
	if reminded != len(due) {
		t.Errorf("новые невнесённые чеки не напомнены/не отмечены: reminded %d из %d, ⏰-сообщений %d", reminded, len(due), msgs)
	}
}

// 7. [server] cmf_check_receipts makes 4+ strictly sequential program requests per client, and there is no tool to check one check or client, so «внесён ли чек X?» means a full-day reconcile that takes minutes

func TestINV_07_ReconcileDayCost(t *testing.T) {
	ctx := context.Background()
	grp1, grp2 := e2eGroup("zzr1"), e2eGroup("zzr2")
	e := newE2E(t, e2eOpts{WithAI: true})
	pool, err := pgxpool.New(ctx, os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	const abc = "абвгдежзиклмнопрстуфхцчшэюя"
	ab := []rune(abc)
	var words []string
	names := map[string]string{}
	for i := 0; i < 300; i++ {
		w := string(ab[i%len(ab)]) + string(ab[(i/len(ab))%len(ab)])
		words = append(words, w)
		names[fmt.Sprint(1000+i)] = "Клиентов" + w + " Ахмед"
	}
	if _, err := pool.Exec(ctx, `INSERT INTO contacts (canonical_name) SELECT 'Клиентов' || w || ' Ахмед' FROM unnest($1::text[]) w`, words); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
    WITH s AS (SELECT g, now() - make_interval(secs => (g::float8 / 6000 * 100 * 86400)::int) AS at FROM generate_series(1, 6000) g),
    rm AS (INSERT INTO raw_messages (wa_message_id, wa_group_jid, sender_jid, sender_name, body, has_media, received_at, parsed, created_at)
           SELECT 'ZZ' || g, CASE WHEN g % 2 = 0 THEN $1 ELSE $2 END, '79001112233@s.whatsapp.net', 'Сафаи', 'чек', true, at, true, at FROM s
           RETURNING id, wa_message_id, wa_group_jid, created_at)
    INSERT INTO bank_receipts (raw_message_id, bank, recipient_raw, contact_id, amount, doc_number, group_jid, tx_date, created_at, client_confirmed)
    SELECT rm.id, 'Сбер', c.canonical_name, c.id, 5000 + (substr(rm.wa_message_id, 3)::int % 30) * 500, 'D' || rm.wa_message_id,
           rm.wa_group_jid, rm.created_at, rm.created_at, true
    FROM rm JOIN contacts c ON c.id = 1 + (substr(rm.wa_message_id, 3)::int % 300)`, grp1.String(), grp2.String()); err != nil {
		t.Fatal(err)
	}
	const lat = 100 * time.Millisecond // каждый ответ программы
	var reqs atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(lat):
		case <-r.Context().Done():
			return
		}
		reqs.Add(1)
		switch {
		case r.URL.Path == "/api/auth/login":
			_ = json.NewEncoder(w).Encode(map[string]string{"token": "t"})
		case r.URL.Path == "/api/clients/lookup":
			q := strings.ToLower(r.URL.Query().Get("full_name"))
			out := []map[string]string{}
			for id, n := range names {
				if strings.Contains(strings.ToLower(n), q) && len(out) < 10 {
					out = append(out, map[string]string{"id": id, "full_name": n})
				}
			}
			_ = json.NewEncoder(w).Encode(out)
		case strings.HasPrefix(r.URL.Path, "/api/contracts/client/"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/contracts/client/"), "/contracts-summary")
			_ = json.NewEncoder(w).Encode([]map[string]any{{"id": "c-" + id + "-0", "branch_id": "b1"}, {"id": "c-" + id + "-1", "branch_id": "b1"}})
		case strings.HasPrefix(r.URL.Path, "/api/contract-payments"):
			_ = json.NewEncoder(w).Encode([]map[string]any{})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	t.Setenv("CMF_API_URL", srv.URL)
	t.Setenv("CMF_EMAIL", "bot@x")
	t.Setenv("CMF_PASSWORD", "p")
	e.cmf = cmf.NewFromEnv()

	day := time.Now().AddDate(0, 0, -1).Format("2006-01-02")
	start := time.Now()
	out := e.tool("cmf_check_receipts", grp1, `{"from_date":"`+day+`","to_date":"`+day+`"}`)
	took := time.Since(start)
	head := out
	if i := strings.Index(head, "\n"); i > 0 {
		head = head[:i]
	}
	n := time.Duration(reqs.Load())
	t.Logf("сверка за %s: %v при %v на запрос, запросов %d подряд; %q", day, took, lat, n, head)
	t.Logf("  при 300 мс на запрос ≈ %v, при 1 с ≈ %v", took+n*200*time.Millisecond, took+n*900*time.Millisecond)
	if took > 15*time.Second {
		t.Errorf("«проверь, всё ли внесено за вчера» отвечает %v (%d запросов к программе подряд)", took, n)
	}
}

// 8. [server] In a muted group the reminder is dropped but the check is still marked 'reminded', so it is never reminded anywhere, even after unmuting

func TestINV_08_CmfMutedGroupDropsReminderForever(t *testing.T) {
	ctx := context.Background()
	grp, emp := e2eGroup("zzmute"), user("79281119005")
	e := newE2E(t, e2eOpts{WithAI: true, CMFClients: katalov})
	e.ai = aiNamesAndPayments("Каталов Ахмед")
	ck := e.sendReceipt(grp, emp, "Сафаи", sberCheckOn(time.Now().AddDate(0, 0, -1), "14000.00", "10:00:00"), "Каталов Ахмед")
	if err := e.setMuted(ctx, grp.String(), true); err != nil {
		t.Fatal(err)
	}
	got := e.remindNow()
	st, _, _ := e.db.TestCmfWatchState(ctx, ck)
	t.Logf("молчание: отправлено %d, статус %q", len(got), st)
	if err := e.setMuted(ctx, grp.String(), false); err != nil {
		t.Fatal(err)
	}
	got = append(got, e.remindNow()...)
	st, _, _ = e.db.TestCmfWatchState(ctx, ck)
	t.Logf("после снятия молчания: всего отправлено %d, статус %q", len(got), st)
	if !hasReminder(got) {
		t.Errorf("невнесённый чек так и не напомнен никому (status=%q)", st)
	}
}

// 9. [server] Every «ФИО сумма» and every check holds the single WhatsApp event queue on a synchronous AI call, so a «Джарвис» reply waits behind all of them

func TestINV_09_F1OwnerReplyWaitsBehindEmployeePayments(t *testing.T) {
	g1, g2 := e2eGroup("f1a"), e2eGroup("f1b")
	e := newE2E(t, e2eOpts{WithAI: true, Admins: []string{"79287836800"}})
	const lat = 700 * time.Millisecond // живая модель отвечает секунды; здесь 0.7 с
	names := []string{"Магомедов Ахмед", "Исаев Ислам", "Каталов Харон", "Абдулаев Рахман", "Джабраилов Шамиль", "Асхабов Хусен"}
	base := aiNamesAndPayments(names...)
	e.ai = func(c aiCall) aiReply {
		time.Sleep(lat)
		if len(c.Tools) > 0 {
			return aiReply{Text: "Привет! Чем помочь?"}
		}
		return base(c)
	}
	owner := user("79287836800")
	waitReply := func(from int) time.Time {
		for dead := time.Now().Add(60 * time.Second); time.Now().Before(dead); time.Sleep(10 * time.Millisecond) {
			for _, m := range e.sentSince(from) {
				if strings.Contains(m.Text, "Привет! Чем помочь?") {
					return time.Now()
				}
			}
		}
		t.Fatal("нет ответа ассистента")
		return time.Time{}
	}
	// Как whatsmeow (client.go handlerQueueLoop): следующее событие ждёт возврата обработчика.
	queue := make(chan func(), 32)
	go func() {
		for f := range queue {
			f()
		}
	}()
	defer close(queue)

	n := len(e.sent())
	st := time.Now()
	queue <- func() {
		e.handleGroupMessage(context.Background(), groupText(g1, owner, "Ахмед", "Джарвис привет"))
	}
	alone := waitReply(n).Sub(st)
	e.waitIdle()

	for i, nm := range names {
		msg := groupText([]types.JID{g1, g2}[i%2], user(fmt.Sprintf("7928111000%d", i)), "Сотрудник", fmt.Sprintf("%s %d", nm, 15000+1000*i))
		queue <- func() { e.handleGroupMessage(context.Background(), msg) }
	}
	n = len(e.sent())
	st = time.Now()
	queue <- func() {
		e.handleGroupMessage(context.Background(), groupText(g1, owner, "Ахмед", "Джарвис привет"))
	}
	waited := waitReply(n).Sub(st)
	e.waitIdle()
	t.Logf("ответ владельцу: без очереди %v, после 6 платежей сотрудников %v", alone.Round(time.Millisecond), waited.Round(time.Millisecond))
	if waited > 2*alone+500*time.Millisecond {
		t.Errorf("владелец ждал %v вместо ~%v: каждое «ФИО сумма» держит очередь всех чатов на вызов ИИ (handleNameMessage → cmfExtractClientName)",
			waited.Round(time.Millisecond), alone.Round(time.Millisecond))
	}
}

// 10. [server] AI calls have no deadline: 3 attempts × 90 s × every fallback model, so when the provider fails, one payment freezes all chats for minutes

// файл импортирует net/http, net/http/httptest, sync, whatsapp-bot/internal/ai
func TestINV_10_F2OverloadedAIHoldsQueue(t *testing.T) {
	t.Setenv("OPENROUTER_FALLBACK_MODELS", "fallback-1,fallback-2")
	grp := e2eGroup("f2")
	e := newE2E(t, e2eOpts{WithAI: true, Admins: []string{"79287836800"}})
	var mu sync.Mutex
	reqs := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		reqs++
		mu.Unlock()
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"message":"Provider returned error: overloaded"}}`))
	}))
	t.Cleanup(srv.Close)
	e.assistant = ai.New("test-key", "test-model", "test-vision", srv.URL)
	st := time.Now()
	e.handleGroupMessage(context.Background(), groupText(grp, user("79283330001"), "Сотрудник", "Магомедов Ахмед 15000"))
	blocked := time.Since(st)
	mu.Lock()
	n := reqs
	mu.Unlock()
	t.Logf("платёж при перегрузке ИИ держал очередь событий %v; запросов к ИИ к этому моменту: %d", blocked.Round(time.Millisecond), n)
	if blocked > time.Second {
		t.Errorf("один платёж при перегрузке ИИ держит все чаты %v (ретраи × резервные модели, без дедлайна)", blocked.Round(time.Millisecond))
	}
}

// 11. [server] «Проверь, занесли ли чеки в программу» takes tens of seconds to minutes: each client's lookup and payments in the program are fetched one by one, plus a quadratic ФИО comparison

// файл импортирует encoding/json, net/http, net/http/httptest, sync, whatsapp-bot/internal/cmf, whatsapp-bot/internal/db
func TestINV_11_F3CmfCheckReceiptsSequential(t *testing.T) {
	grp, emp := e2eGroup("f3"), user("79281119903")
	e := newE2E(t, e2eOpts{WithAI: true, Admins: []string{"79287836800"}})
	e.groupsCache[grp] = "Оплата"
	ctx := context.Background()
	surn := []string{"Магомедов", "Исаев", "Каталов", "Абдулаев", "Джабраилов", "Асхабов", "Усумов", "Манаев"}
	first := []string{"Ахмед", "Ислам", "Харон", "Рахман", "Шамиль"}
	var clients []cmf.ClientInfo
	for i := 0; i < 40; i++ {
		name := surn[i%len(surn)] + " " + first[i/len(surn)]
		clients = append(clients, cmf.ClientInfo{ID: fmt.Sprint(100 + i), FullName: name + " Русланович"})
		rawID, _, err := e.db.SaveRawMessage(ctx, e2eID("F3"), grp.String(), emp.String(), emp.User, "Сафаи", name, true, "", time.Now())
		if err != nil {
			t.Fatal(err)
		}
		cid, _ := e.db.GetOrCreateContact(ctx, name)
		if err := e.db.InsertBankReceipt(ctx, db.BankReceiptInput{RawMessageID: rawID, RecipientRaw: name, ContactID: &cid,
			Amount: float64(5000 + 100*i), GroupJID: grp.String(), TxDate: time.Now(), ClientConfirmed: true}); err != nil {
			t.Fatal(err)
		}
	}
	var mu sync.Mutex
	reqs := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		reqs++
		mu.Unlock()
		time.Sleep(150 * time.Millisecond)
		switch {
		case r.URL.Path == "/api/auth/login":
			_ = json.NewEncoder(w).Encode(map[string]string{"token": "t"})
		case r.URL.Path == "/api/clients/lookup":
			q := strings.ToLower(r.URL.Query().Get("full_name"))
			out := []map[string]string{}
			for _, c := range clients {
				if strings.Contains(strings.ToLower(c.FullName), q) {
					out = append(out, map[string]string{"id": c.ID, "full_name": c.FullName})
				}
			}
			_ = json.NewEncoder(w).Encode(out)
		case strings.HasPrefix(r.URL.Path, "/api/contracts/client/"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/contracts/client/"), "/contracts-summary")
			_ = json.NewEncoder(w).Encode([]map[string]any{{"id": "c-" + id, "branch_id": "b1", "number": 100}})
		case strings.HasPrefix(r.URL.Path, "/api/contract-payments"):
			_ = json.NewEncoder(w).Encode([]map[string]any{})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	t.Setenv("CMF_API_URL", srv.URL)
	t.Setenv("CMF_EMAIL", "bot@x")
	t.Setenv("CMF_PASSWORD", "p")
	e.cmf = cmf.NewFromEnv()
	st := time.Now()
	out := e.tool("cmf_check_receipts", grp, `{}`)
	took := time.Since(st)
	mu.Lock()
	n := reqs
	mu.Unlock()
	t.Logf("сверка 40 клиентов за день: %v, запросов в программу %d; %q", took.Round(time.Millisecond), n, strings.SplitN(out, "\n", 2)[0])
	if took > 10*time.Second {
		t.Errorf("«проверь программу» за один день отвечает %v — %d запросов в программу идут по одному", took.Round(time.Second), n)
	}
}

// 12. [server] An employee's answer to «🔎 За кого этот платёж?» is processed inside the message queue: one AI call plus up to 7 program requests in a row

// файл импортирует encoding/json, net/http, net/http/httptest, sync, whatsapp-bot/internal/cmf
func TestINV_12_F5CmfAnswerInHandler(t *testing.T) {
	grp, emp := e2eGroup("f5"), user("79281119905")
	e := newE2E(t, e2eOpts{WithAI: true, Admins: []string{"79287836800"}})
	e.groupsCache[grp] = "Оплата"
	clients := []cmf.ClientInfo{{ID: "11", FullName: "Магомедов Ахмед Русланович"}, {ID: "12", FullName: "Магомедов Ахмед Исаевич"},
		{ID: "13", FullName: "Исаева Медина Ахмедовна"}}
	var mu sync.Mutex
	reqs := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		reqs++
		mu.Unlock()
		time.Sleep(300 * time.Millisecond)
		switch {
		case r.URL.Path == "/api/auth/login":
			_ = json.NewEncoder(w).Encode(map[string]string{"token": "t"})
		case r.URL.Path == "/api/clients/lookup":
			q := strings.ToLower(r.URL.Query().Get("full_name"))
			out := []map[string]string{}
			for _, c := range clients {
				if strings.Contains(strings.ToLower(c.FullName), q) {
					out = append(out, map[string]string{"id": c.ID, "full_name": c.FullName})
				}
			}
			_ = json.NewEncoder(w).Encode(out)
		case strings.HasPrefix(r.URL.Path, "/api/contracts/client/"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/contracts/client/"), "/contracts-summary")
			_ = json.NewEncoder(w).Encode([]map[string]any{{"id": "c-" + id, "branch_id": "b1", "number": 100}})
		default:
			_ = json.NewEncoder(w).Encode([]map[string]any{})
		}
	}))
	t.Cleanup(srv.Close)
	t.Setenv("CMF_API_URL", srv.URL)
	t.Setenv("CMF_EMAIL", "bot@x")
	t.Setenv("CMF_PASSWORD", "p")
	e.cmf = cmf.NewFromEnv()
	base := aiNamesAndPayments("Магомедов Ахмед")
	slow := false
	e.ai = func(c aiCall) aiReply {
		if strings.Contains(c.System, "за какого клиента программы рассрочек") {
			if slow {
				time.Sleep(700 * time.Millisecond)
			}
			return aiReply{Text: `{"kind":"client","pick":0,"name":"Исаева Мадина"}`}
		}
		return base(c)
	}
	e.sendReceipt(grp, emp, "Сафаи", sberCheck("15000.00", "10:00:00"), "Магомедов Ахмед")
	var q e2eSent
	for i := 0; i < 100 && q.ID == ""; i++ {
		time.Sleep(100 * time.Millisecond)
		q = e.lastWith("За кого этот платёж")
	}
	if q.ID == "" {
		t.Fatalf("нет вопроса «за кого платёж»: %+v", e.sent())
	}
	slow = true
	mu.Lock()
	before := reqs
	mu.Unlock()
	st := time.Now()
	e.handleGroupMessage(context.Background(), groupReply(grp, emp, "Сафаи", "нет, это Исаевой Мадины", q.ID, e.botJID(), q.Text))
	blocked := time.Since(st)
	mu.Lock()
	n := reqs - before
	mu.Unlock()
	t.Logf("ответ на «за кого платёж» держал очередь событий %v, запросов в программу %d", blocked.Round(time.Millisecond), n)
	if blocked > time.Second {
		t.Errorf("ответ сотрудника держит очередь всех чатов %v (ИИ + %d запросов в программу синхронно)", blocked.Round(time.Millisecond), n)
	}
}

// 13. [HEAD] (HEAD only, not on the server) The background «занесите в программу» check became heavy: about 37 s of CPU on every 30-minute round

// файл импортирует encoding/json, net/http, net/http/httptest, os, github.com/jackc/pgx/v5, github.com/jackc/pgx/v5/pgxpool, whatsapp-bot/internal/cmf
func TestINV_13_F4CmfCheckDueCPU(t *testing.T) {
	grp, emp := e2eGroup("f4"), user("79281119904")
	e := newE2E(t, e2eOpts{WithAI: true, Admins: []string{"79287836800"}})
	e.groupsCache[grp] = "Оплата"
	ctx := context.Background()
	p, err := pgxpool.New(ctx, os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	surn := []string{"Магомедов", "Исаев", "Каталов", "Абдулаев", "Джабраилов", "Асхабов", "Усумов", "Манаев", "Дебишов", "Цихаев",
		"Атабаев", "Саралиев", "Догаев", "Альмурзаев", "Магамадов", "Хадаев", "Ибрагимов", "Садулаев", "Умхадижиев", "Солтамурадов"}
	first := []string{"Ахмед", "Ислам", "Харон", "Рахман", "Шамиль", "Хусен", "Адам", "Ибрагим", "Сулейман", "Рауф"}
	var names []string
	var cids []int
	for _, s := range surn {
		for _, f := range first {
			id, _ := e.db.GetOrCreateContact(ctx, s+" "+f)
			names, cids = append(names, s+" "+f), append(cids, id)
		}
	}
	// 105 дней учёта: ~35 чеков и ~80 текстов в день (как обычная работа).
	var raws, brs, txs, cws [][]any
	id := 0
	now := time.Now()
	for d := 0; d < 105; d++ {
		for k := 0; k < 115; k++ {
			id++
			at := now.AddDate(0, 0, -d).Add(-time.Duration(k) * time.Minute)
			ci := (d*7 + k) % len(names)
			media := k < 35
			raws = append(raws, []any{id, fmt.Sprintf("F4-%d", id), grp.String(), emp.String(), names[ci], media, at, true})
			if media {
				brs = append(brs, []any{id, names[ci], cids[ci], 5000 + 500*(k%20), grp.String(), at, true, at})
				if d >= 1 && d < 60 && k%4 == 0 {
					st := "found"
					if d == 1 || k%8 == 0 {
						st = map[bool]string{true: "watch", false: "reminded"}[d == 1]
					}
					cws = append(cws, []any{id, grp.String(), emp.String(), names[ci], fmt.Sprint(cids[ci]), names[ci] + " Русланович", 5000 + 500*(k%20), at, st, at})
				}
			} else {
				txs = append(txs, []any{cids[ci], names[ci], 3000 + 500*(k%20), id, at, k%3 == 0, at})
			}
		}
	}
	copyRows := func(tbl string, cols []string, rows [][]any) {
		if _, err := p.CopyFrom(ctx, pgx.Identifier{tbl}, cols, pgx.CopyFromRows(rows)); err != nil {
			t.Fatal(tbl, err)
		}
	}
	copyRows("raw_messages", []string{"id", "wa_message_id", "wa_group_jid", "sender_jid", "body", "has_media", "received_at", "parsed"}, raws)
	_, _ = p.Exec(ctx, `SELECT setval('raw_messages_id_seq', (SELECT max(id) FROM raw_messages))`)
	copyRows("bank_receipts", []string{"raw_message_id", "recipient_raw", "contact_id", "amount", "group_jid", "tx_date", "client_confirmed", "created_at"}, brs)
	copyRows("transactions", []string{"contact_id", "raw_name", "amount", "raw_message_id", "tx_date", "is_cash", "created_at"}, txs)
	copyRows("cmf_watch", []string{"raw_message_id", "group_jid", "sender_jid", "client_text", "client_id", "client_name", "amount", "tx_date", "status", "created_at"}, cws)
	_, _ = p.Exec(ctx, `ANALYZE`)
	// Программа отвечает мгновенно — меряем только работу самого бота.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/auth/login":
			_ = json.NewEncoder(w).Encode(map[string]string{"token": "t"})
		case strings.HasPrefix(r.URL.Path, "/api/contracts/client/"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/contracts/client/"), "/contracts-summary")
			_ = json.NewEncoder(w).Encode([]map[string]any{{"id": "c-" + id, "branch_id": "b1", "number": 100}})
		default:
			_ = json.NewEncoder(w).Encode([]map[string]any{})
		}
	}))
	t.Cleanup(srv.Close)
	t.Setenv("CMF_API_URL", srv.URL)
	t.Setenv("CMF_EMAIL", "bot@x")
	t.Setenv("CMF_PASSWORD", "p")
	e.cmf = cmf.NewFromEnv()
	st := time.Now()
	e.cmfCheckDue(ctx)
	took := time.Since(st)
	t.Logf("один круг напоминаний (%d чеков, %d текстов в базе): %v", len(brs), len(txs), took.Round(time.Millisecond))
	if took > 5*time.Second {
		t.Errorf("круг напоминаний «занесите в программу» занимает %v процессора при мгновенной программе (judgeDueWatches → sameClientName по всему учёту)", took.Round(time.Second))
	}
}

// 14. [HEAD] (HEAD only, not on the server) An ФИО reply to a check that has no reconciliation watch runs ensureMissingWatches inside the queue: up to 50 old checks are matched against the program synchronously

// файл импортирует encoding/json, net/http, net/http/httptest, sync, whatsapp-bot/internal/cmf, whatsapp-bot/internal/db
func TestINV_14_F6EnsureWatchesInHandler(t *testing.T) {
	grp, emp := e2eGroup("f6"), user("79281119906")
	e := newE2E(t, e2eOpts{WithAI: true, Admins: []string{"79287836800"}})
	e.groupsCache[grp] = "Оплата"
	base := aiNamesAndPayments("Магомедов Ахмед")
	e.ai = func(c aiCall) aiReply {
		if strings.Contains(c.System, "модуль разбора банковских чеков") {
			return aiReply{Text: `{}`} // сумма не читается
		}
		return base(c)
	}
	ctx := context.Background()
	surn := []string{"Исаев", "Каталов", "Абдулаев", "Джабраилов", "Асхабов", "Усумов", "Манаев", "Дебишов", "Цихаев", "Атабаев"}
	first := []string{"Ислам", "Харон", "Рахман", "Шамиль", "Хусен"}
	clients := []cmf.ClientInfo{{ID: "1", FullName: "Магомедов Ахмед Исаевич"}}
	for i := 0; i < 50; i++ {
		name := surn[i%len(surn)] + " " + first[i/len(surn)]
		clients = append(clients, cmf.ClientInfo{ID: fmt.Sprint(500 + i), FullName: name + " Русланович"})
		// Чек, подтянутый «перечитай группу»: клиент из подписи, сверки нет.
		rawID, _, err := e.db.SaveRawMessage(ctx, e2eID("IMP"), grp.String(), emp.String(), emp.User, "Сафаи", name, true, "", time.Now().AddDate(0, 0, -20))
		if err != nil {
			t.Fatal(err)
		}
		cid, _ := e.db.GetOrCreateContact(ctx, name)
		if err := e.db.InsertBankReceipt(ctx, db.BankReceiptInput{RawMessageID: rawID, RecipientRaw: name, ContactID: &cid,
			Amount: float64(5000 + i), GroupJID: grp.String(), TxDate: time.Now().AddDate(0, 0, -20), ClientConfirmed: true}); err != nil {
			t.Fatal(err)
		}
	}
	var mu sync.Mutex
	reqs := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		reqs++
		mu.Unlock()
		time.Sleep(150 * time.Millisecond)
		switch {
		case r.URL.Path == "/api/auth/login":
			_ = json.NewEncoder(w).Encode(map[string]string{"token": "t"})
		case r.URL.Path == "/api/clients/lookup":
			q := strings.ToLower(r.URL.Query().Get("full_name"))
			out := []map[string]string{}
			for _, c := range clients {
				if strings.Contains(strings.ToLower(c.FullName), q) {
					out = append(out, map[string]string{"id": c.ID, "full_name": c.FullName})
				}
			}
			_ = json.NewEncoder(w).Encode(out)
		default:
			_ = json.NewEncoder(w).Encode([]map[string]any{})
		}
	}))
	t.Cleanup(srv.Close)
	t.Setenv("CMF_API_URL", srv.URL)
	t.Setenv("CMF_EMAIL", "bot@x")
	t.Setenv("CMF_PASSWORD", "p")
	e.cmf = cmf.NewFromEnv()
	ck := e.sendReceipt(grp, emp, "Сафаи", "СберБанк\nЧек по операции\nФИО получателя\nХадижат Ибрагимовна И.\nНомер операции в СБП\n"+e2eID("OP"), "")
	mu.Lock()
	before := reqs
	mu.Unlock()
	st := time.Now()
	e.handleGroupMessage(ctx, groupReply(grp, emp, "Сафаи", "Магомедов Ахмед", ck, emp, ""))
	blocked := time.Since(st)
	mu.Lock()
	n := reqs - before
	mu.Unlock()
	t.Logf("свайп ФИО на чек держал очередь событий %v; запросов в программу %d", blocked.Round(time.Millisecond), n)
	if blocked > time.Second {
		t.Errorf("подпись ФИО к чеку держит очередь всех чатов %v: ensureMissingWatches синхронно сверяет старые чеки (%d запросов)", blocked.Round(time.Second), n)
	}
}

// 15. [server] «Джарвис, пересчитай» waits for up to 15 three-read AI parses, one at a time, inside the reply

// файл импортирует sync
func TestINV_15_F7RecountInline(t *testing.T) {
	grp := e2eGroup("f7")
	e := newE2E(t, e2eOpts{WithAI: true, Admins: []string{"79287836800"}})
	ctx := context.Background()
	for i := 0; i < 20; i++ { // тексты, оставшиеся неразобранными (ИИ лежал)
		if _, _, err := e.db.SaveRawMessage(ctx, e2eID("UNP"), grp.String(), "79281234567@s.whatsapp.net", "", "Сотрудник",
			fmt.Sprintf("от Магомедова %d т перевёл вчера", 10+i), false, "", time.Now().Add(-time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	const lat = 700 * time.Millisecond
	base := aiNamesAndPayments("Магомедов Ахмед")
	var mu sync.Mutex
	parses := 0
	e.ai = func(c aiCall) aiReply {
		if len(c.Tools) > 0 {
			if !c.hasToolResult() {
				return aiReply{ToolCalls: []aiToolCall{{Name: "recount_everything", Args: `{}`}}}
			}
			return aiReply{Text: "Готово."}
		}
		if strings.Contains(c.System, "модуль разбора платежей") {
			mu.Lock()
			parses++
			mu.Unlock()
		}
		time.Sleep(lat)
		return base(c)
	}
	n := len(e.sent())
	st := time.Now()
	e.handleGroupMessage(ctx, groupText(grp, user("79287836800"), "Ахмед", "Джарвис пересчитай всё"))
	var took time.Duration
	for dead := time.Now().Add(120 * time.Second); time.Now().Before(dead) && took == 0; time.Sleep(20 * time.Millisecond) {
		for _, m := range e.sentSince(n) {
			if strings.Contains(m.Text, "Готово") {
				took = time.Since(st)
			}
		}
	}
	if took == 0 {
		t.Fatal("нет ответа на пересчёт")
	}
	mu.Lock()
	defer mu.Unlock()
	t.Logf("«пересчитай»: ответ через %v, чтений ИИ %d (одно чтение %v)", took.Round(time.Millisecond), parses, lat)
	if took > 5*lat {
		t.Errorf("«пересчитай» отвечает через %v: инструмент по одному ждёт %d разборов ИИ", took.Round(time.Millisecond), parses/3)
	}
}

// 16. [server] Команда владельца свайпом на «🤔 Чей это чек?» (и в подписи к фото) записывается как ФИО клиента — команда не выполняется, чек уходит в сбор под «клиентом» «Молчи группе»

// imports: context, fmt?, strings, sync, testing, time
func TestINV_16_CommandRecordedAsClientName(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		swipe bool
		text  string
	}{
		{true, "Молчи в группе"},              // свайп на вопрос бота (7d11619 и HEAD)
		{true, "Джарвис, Перечитай группу"},   // свайп с обращением по имени (регрессия HEAD)
		{false, "Джарвис, внеси в программу"}, // подпись к фото чека (7d11619 и HEAD)
	}
	for _, tc := range cases {
		grp, emp, owner := e2eGroup("cmdname"), user("79001112299"), user(ownerPhone)
		e := newE2E(t, e2eOpts{WithAI: true, Admins: []string{ownerPhone}})
		e.groupsCache[grp] = "Оплата"
		var mu sync.Mutex
		var tools []string
		e.ai = func(c aiCall) aiReply {
			switch {
			case strings.Contains(c.System, "выдели имя КЛИЕНТА"):
				return aiReply{Text: `{"client":""}`} // модель: это не ФИО
			case len(c.Tools) == 0:
				return aiReply{Text: `{}`}
			case c.hasToolResult():
				return aiReply{Text: "Готово"}
			}
			name, args := "reread_group", `{}`
			if strings.Contains(strings.ToLower(c.lastUserText()), "молчи") {
				name, args = "set_group_silence", `{"silence":true}`
			}
			mu.Lock()
			tools = append(tools, name)
			mu.Unlock()
			return aiReply{ToolCalls: []aiToolCall{{Name: name, Args: args}}}
		}
		n := 0
		if tc.swipe {
			e.sendReceipt(grp, emp, "Сафаи", sberCheck("5000", "10:00:00"), "")
			e.age(grp.String())
			e.clarifyTick(ctx)
			q := e.lastWith("Чей это чек")
			if q.ID == "" {
				t.Fatalf("нет вопроса «Чей это чек»: %+v", e.sent())
			}
			n = len(e.sent())
			e.sendGroup(groupReply(grp, owner, "Ахмед", tc.text, q.ID, e.botJID(), q.Text))
		} else {
			e.sendReceipt(grp, owner, "Ахмед", sberCheck("7000", "11:00:00"), tc.text)
		}
		time.Sleep(300 * time.Millisecond)
		totals := dayTotals(e, grp)
		for name := range totals {
			low := strings.ToLower(name)
			if strings.Contains(low, "молчи") || strings.Contains(low, "перечитай") || strings.Contains(low, "внеси") {
				var said []string
				for _, m := range e.sentSince(n) {
					said = append(said, m.Text)
				}
				mu.Lock()
				t.Errorf("%q: команда записана клиентом %q (сбор %v); инструменты ассистента: %v; бот ответил: %q", tc.text, name, totals, tools, said)
				mu.Unlock()
			}
		}
	}
}

// 17. [server] Чеки разбираются синхронно в единственной очереди событий WhatsApp: «Джарвис, …» и личка ждут, пока ИИ прочитает каждый чек перед ними

// imports: context, fmt, strings, sync, testing, time
func TestINV_17_OwnerCommandWaitsBehindChecks(t *testing.T) {
	ctx := context.Background()
	grp, emp, owner := e2eGroup("lat"), user("79001115566"), user(ownerPhone)
	e := newE2E(t, e2eOpts{WithAI: true, Admins: []string{ownerPhone}})
	e.groupsCache[grp] = "Оплата"
	const lat = 1500 * time.Millisecond // один ответ ИИ (в жизни 2–15 с, зрение — дольше)
	var mu sync.Mutex
	requests := 0
	e.ai = func(c aiCall) aiReply {
		mu.Lock()
		requests++
		mu.Unlock()
		time.Sleep(lat)
		switch {
		case strings.Contains(c.System, "выдели имя КЛИЕНТА"):
			return aiReply{Text: `{"client":"Каталов Ахмед"}`}
		case strings.Contains(c.System, "модуль разбора изображений"):
			return aiReply{Text: `{"kind":"receipt","bank":"Сбербанк","recipient":"Хадижат Ибрагимовна И.","amount":5000,"datetime":"` +
				time.Now().Format("2006-01-02") + ` 10:00:00","doc_number":"` + e2eID("D") + `"}`}
		case len(c.Tools) > 0:
			return aiReply{Text: "Сбор за сегодня: …"}
		}
		return aiReply{Text: `{}`}
	}
	const checks = 6
	start := time.Now()
	for k := 0; k < checks; k++ { // чеки разбираются в очереди группы, как handleGroupMessage
		k := k
		e.enqueueChat(grp.String(), func() {
			waID := e2eID("CHK")
			ocr := sberCheck(fmt.Sprint(5000+k), fmt.Sprintf("10:%02d:00", k))
			caption := "Каталов Ахмед"
			rawID, _, _ := e.db.SaveRawMessage(ctx, waID, grp.String(), emp.String(), emp.String(), "Сафаи", caption+"\n"+ocr, true, "", time.Now())
			msg := groupText(grp, emp, "Сафаи", caption)
			msg.Info.ID = waID
			payer, pa := e.resolveReceiptPayer(ctx, msg, caption)
			e.handleBankReceipt(ctx, grp, emp.String(), waID, caption+"\n"+ocr, rawID, time.Now(), []byte("\xff\xd8\xff\xe0jpeg"), ".jpg", payer, pa)
		})
	}
	checksDone := time.Since(start)
	mu.Lock()
	perCheck := float64(requests) / checks
	mu.Unlock()
	n := len(e.sent())
	defer e.drainQueues(2 * time.Minute)
	e.handleGroupMessage(ctx, groupText(grp, owner, "Ахмед", "Джарвис, скинь отчёт за сегодня"))
	var replied time.Duration
	for i := 0; i < 300 && replied == 0; i++ {
		if len(e.sentSince(n)) > 0 {
			replied = time.Since(start)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if replied == 0 || replied > 5*time.Second {
		t.Errorf("ответ на «Джарвис, скинь отчёт» через %v: %d чеков перед ним держали очередь %v (%.1f запроса к ИИ на чек, ответ ИИ %v)",
			replied.Round(time.Millisecond), checks, checksDone.Round(time.Millisecond), perCheck, lat)
	}
}

// 18. [server] 30 «вечно ошибочных» наблюдений сверки (например 403 «нет прав на точку») навсегда занимают окно проверки: новые чеки больше никогда не сверяются с программой и не напоминаются

// imports: context, encoding/json, fmt, net/http, net/http/httptest, strings, sync, testing, time, whatsapp-bot/internal/cmf
func TestINV_18_ReminderStarvedByFailingWatches(t *testing.T) {
	ctx := context.Background()
	grp, emp := e2eGroup("stv"), user("79001119900")
	e := newE2E(t, e2eOpts{WithAI: true})
	e.groupsCache[grp] = "Оплата"
	e.ai = func(c aiCall) aiReply {
		u := c.lastUserText()
		if strings.Contains(c.System, "выдели имя КЛИЕНТА") {
			for _, n := range []string{"Исаев Харон", "Каталов Ахмед"} {
				if strings.Contains(u, n) {
					return aiReply{Text: `{"client":"` + n + `"}`}
				}
			}
		}
		return aiReply{Text: `{}`}
	}
	clients := []cmf.ClientInfo{{ID: "1", FullName: "Каталов Ахмед Нажудович"}, {ID: "2", FullName: "Исаев Харон Русланович"}}
	var mu sync.Mutex
	reqs := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		reqs++
		mu.Unlock()
		switch {
		case r.URL.Path == "/api/auth/login":
			_ = json.NewEncoder(w).Encode(map[string]string{"token": "t"})
		case r.URL.Path == "/api/clients/lookup":
			q := strings.ToLower(r.URL.Query().Get("full_name"))
			out := []map[string]string{}
			for _, c := range clients {
				if strings.Contains(strings.ToLower(c.FullName), q) {
					out = append(out, map[string]string{"id": c.ID, "full_name": c.FullName})
				}
			}
			_ = json.NewEncoder(w).Encode(out)
		case strings.HasPrefix(r.URL.Path, "/api/contracts/client/"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/contracts/client/"), "/contracts-summary")
			br := "b1"
			if id == "2" {
				br = "b2" // Исаев — клиент точки, на которую у пользователя бота нет прав
			}
			_ = json.NewEncoder(w).Encode([]map[string]any{{"id": "c-" + id, "branch_id": br, "number": 100}})
		case strings.HasPrefix(r.URL.Path, "/api/contract-payments"):
			if r.Header.Get("X-Branch-ID") == "b2" {
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"error":"no access to branch"}`))
				return
			}
			_ = json.NewEncoder(w).Encode([]map[string]any{})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	t.Setenv("CMF_API_URL", srv.URL)
	t.Setenv("CMF_EMAIL", "bot@x")
	t.Setenv("CMF_PASSWORD", "p")
	e.cmf = cmf.NewFromEnv()

	day := time.Now().AddDate(0, 0, -2)
	for i := 0; i < 30; i++ {
		e.sendReceipt(grp, emp, "Сафаи", sberCheckOn(day, fmt.Sprint(3000+i), fmt.Sprintf("09:%02d:00", i)), "Исаев Харон")
	}
	if err := e.db.TestAgeCmfWatches(ctx, 48*time.Hour); err != nil {
		t.Fatal(err)
	}
	ck := e.sendReceipt(grp, emp, "Сафаи", sberCheckOn(time.Now().AddDate(0, 0, -1), "14000.00", "10:00:00"), "Каталов Ахмед")
	var got []e2eSent
	for tick := 0; tick < 3; tick++ { // три фоновых прохода (раз в 30 минут)
		got = append(got, e.remindNow()...)
	}
	st, _, _ := e.db.TestCmfWatchState(ctx, ck)
	if st != "reminded" || !hasReminder(got) {
		mu.Lock()
		t.Errorf("невнесённый чек Каталова на 14000 за 3 прохода не проверен (status=%s, напоминаний %d, запросов к программе %d)", st, len(got), reqs)
		mu.Unlock()
	}
}

// 19. [server] Фото чека, пришедшее при сбое ИИ (402 — кончился баланс OpenRouter), молча выпадает из учёта без очереди на повтор, и «перечитай группу» его не перечитывает

// imports: context, encoding/json, io, net/http, net/http/httptest, path/filepath, strings, sync, testing, time, whatsapp-bot/internal/ai
func TestINV_19_RereadGroupDoesNotRereadDroppedCheckPhoto(t *testing.T) {
	ctx := context.Background()
	grp, emp := e2eGroup("rrd"), user("79001113344")
	e := newE2E(t, e2eOpts{Admins: []string{ownerPhone}})
	e.reportDir = filepath.Join(t.TempDir(), "reports")
	e.groupsCache[grp] = "Оплата"
	var mu sync.Mutex
	visionDown := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		down := visionDown
		mu.Unlock()
		s := string(body)
		answer := func(text string) {
			_ = json.NewEncoder(w).Encode(map[string]any{"choices": []map[string]any{{"message": map[string]any{"role": "assistant", "content": text}, "finish_reason": "stop"}}})
		}
		switch {
		case down && (strings.Contains(s, "image_url") || strings.Contains(s, "модуль разбора банковских чеков")):
			w.WriteHeader(http.StatusPaymentRequired)
			_, _ = w.Write([]byte(`{"error":{"message":"Insufficient credits"}}`))
		case strings.Contains(s, "image_url"):
			answer(`{"kind":"receipt","bank":"Сбербанк","recipient":"Хадижат Ибрагимовна И.","amount":14000,"datetime":"` + time.Now().Format("2006-01-02") + ` 10:00:00","doc_number":"A123"}`)
		case strings.Contains(s, "выдели имя"):
			answer(`{"client":""}`)
		default:
			answer(`{}`)
		}
	}))
	defer srv.Close()
	e.assistant = ai.New("k", "m", "v", srv.URL)

	img := []byte("\xff\xd8\xff\xe0fakejpeg")
	waID := e2eID("CHK")
	path := e.saveMediaFile(waID, img, ".jpg")
	ocr := "Каталов Ахмед\nс6ер 0нлайн ч3к п0 0пер4ции" // подпись + каша Tesseract
	rawID, _, err := e.db.SaveRawMessage(ctx, waID, grp.String(), emp.String(), emp.String(), "Сафаи", ocr, true, path, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	e.handleBankReceipt(ctx, grp, emp.String(), waID, ocr, rawID, time.Now(), img, ".jpg", "Каталов Ахмед", 0)
	mu.Lock()
	visionDown = false // баланс пополнили
	mu.Unlock()
	out := e.tool("reread_group", grp, `{}`)
	e.retryDueReceipts()
	if id, _ := e.db.ReceiptByWaMessageID(ctx, waID); !id.Found {
		t.Errorf("чек на 14000 (Каталов Ахмед) не записан и после «перечитай группу», хотя зрение снова работает; в группу ничего не писалось: %+v; ответ инструмента: %q",
			e.sent(), out)
	}
}

// 20. [server] «Найди / Покажи / Внеси чек Каталова» свайпом на «⏰ Занесите оплату в программу» понимается как ответ «клиент»: команда не выполняется, чек отвязывается от клиента, напоминания по нему прекращаются

// imports: context, strings, sync, testing, time
func TestINV_20_FindCommandOnReminderUnbindsCheck(t *testing.T) {
	ctx := context.Background()
	for _, prefix := range []string{"", "Джарвис, "} {
		grp, emp, owner := e2eGroup("fcr"), user("79281118888"), user(ownerPhone)
		e := newE2E(t, e2eOpts{WithAI: true, Admins: []string{ownerPhone}, CMFClients: katalov})
		e.groupsCache[grp] = "Оплата"
		var mu sync.Mutex
		var tools []string
		e.ai = func(c aiCall) aiReply {
			switch {
			case strings.Contains(c.System, "выдели имя КЛИЕНТА"):
				if strings.Contains(c.lastUserText(), "Каталов Ахмед") {
					return aiReply{Text: `{"client":"Каталов Ахмед"}`}
				}
				return aiReply{Text: `{"client":""}`}
			case strings.Contains(c.System, "за какого клиента программы рассрочек"):
				return aiReply{Text: `{"kind":"other"}`}
			case len(c.Tools) == 0:
				return aiReply{Text: `{}`}
			case c.hasToolResult():
				return aiReply{Text: "Вот чек"}
			}
			mu.Lock()
			tools = append(tools, "send_client_receipt")
			mu.Unlock()
			return aiReply{ToolCalls: []aiToolCall{{Name: "send_client_receipt", Args: `{"person":"Каталов Ахмед"}`}}}
		}
		ck := e.sendReceipt(grp, emp, "Сафаи", sberCheckOn(time.Now().AddDate(0, 0, -1), "14000.00", "10:00:00"), "Каталов Ахмед")
		rem := e.remindNow()
		if !hasReminder(rem) {
			t.Fatalf("нет напоминания: %+v", e.sent())
		}
		r := rem[len(rem)-1]
		n := len(e.sent())
		e.sendGroup(groupReply(grp, owner, "Ахмед", prefix+"Найди чек Каталова", r.ID, e.botJID(), r.Text))
		time.Sleep(300 * time.Millisecond)
		var said []string
		for _, m := range e.sentSince(n) {
			said = append(said, m.Text)
		}
		st, cid, _ := e.db.TestCmfWatchState(ctx, ck)
		again := hasReminder(e.remindNow())
		mu.Lock()
		_ = again // напоминание по чеку — одно; главное, что клиент не сброшен
		if len(tools) == 0 || cid != "1" {
			t.Errorf("%q: инструменты %v; чек сверки status=%s client=%q (был Каталов, id 1); напоминание снова: %v; бот ответил %q",
				prefix+"Найди чек Каталова", tools, st, cid, again, said)
		}
		mu.Unlock()
	}
}

// 21. [HEAD] HEAD: ответ на вопрос бота синхронно ходит в программу рассрочек прямо в обработчике WhatsApp (новые пути syncWatchByCheckWa / ensureMissingWatches)

// imports: context, encoding/json, net/http, net/http/httptest, sync, testing, time, whatsapp-bot/internal/cmf
func TestINV_21_AnswerBlocksOnCMF(t *testing.T) {
	grp, emp := e2eGroup("abc"), user("79001112277")
	e := newE2E(t, e2eOpts{WithAI: true})
	e.groupsCache[grp] = "Оплата"
	e.ai = func(c aiCall) aiReply { return aiReply{Text: `{}`} }
	var mu sync.Mutex
	reqs := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		reqs++
		mu.Unlock()
		time.Sleep(time.Second) // программа отвечает за 1 с
		switch {
		case r.URL.Path == "/api/auth/login":
			_ = json.NewEncoder(w).Encode(map[string]string{"token": "t"})
		case r.URL.Path == "/api/clients/lookup":
			_ = json.NewEncoder(w).Encode([]map[string]string{{"id": "1", "full_name": "Каталов Ахмед Нажудович"}})
		default:
			_ = json.NewEncoder(w).Encode([]map[string]any{})
		}
	}))
	defer srv.Close()
	t.Setenv("CMF_API_URL", srv.URL)
	t.Setenv("CMF_EMAIL", "bot@x")
	t.Setenv("CMF_PASSWORD", "p")
	e.cmf = cmf.NewFromEnv()
	ocr := "СберБанк\nЧек по операции\nФИО получателя\nХадижат Ибрагимовна И.\nНомер операции в СБП\n" + e2eID("OP") // без суммы
	e.sendReceipt(grp, emp, "Сафаи", ocr, "")
	q := e.lastWith("не разобрал")
	if q.ID == "" {
		t.Fatalf("нет вопроса про сумму: %+v", e.sent())
	}
	mu.Lock()
	reqs = 0
	mu.Unlock()
	t0 := time.Now()
	e.handleGroupMessage(context.Background(), groupReply(grp, emp, "Сафаи", "Каталов Ахмед 14000", q.ID, e.botJID(), q.Text))
	took := time.Since(t0)
	mu.Lock()
	defer mu.Unlock()
	if took > 500*time.Millisecond {
		t.Errorf("ответ «Каталов Ахмед 14000» держал обработчик сообщений WhatsApp %v: %d синхронных запросов в программу", took.Round(time.Millisecond), reqs)
	}
}

// 22. [server] Каждый запрос ассистента (и каждый шаг с инструментом) несёт некэшируемый список ВСЕХ людей учёта и построчную сводку за месяц: ~25 тыс. токенов

// imports: context, testing, time, whatsapp-bot/internal/db
func TestINV_22_PromptGrowsWithContacts(t *testing.T) {
	ctx := context.Background()
	grp := e2eGroup("pr")
	e := newE2E(t, e2eOpts{Admins: []string{ownerPhone}})
	e.groupsCache[grp] = "Оплата"
	surn := []string{"Каталов", "Исаев", "Магомедов", "Хасанов", "Дудаев", "Алиев", "Абдулаев", "Саралиев", "Мусиев", "Альмурзаев"}
	first := []string{"Ахмед", "Ислам", "Магомед", "Дени", "Харон", "Руслан", "Адам", "Мансур", "Зелимхан", "Аслан", "Ибрагим", "Хасан", "Умар", "Шамиль", "Рамзан"}
	pat := []string{"Нажудович", "Русланович", "Ахмедович", "Исламович", "Магомедович", "Адамович", "Хасанович", "Умарович", "Шамилевич", "Рамзанович"}
	const contacts, paying = 1500, 700
	for i := 0; i < contacts; i++ {
		name := surn[i%10] + " " + first[(i/10)%15] + " " + pat[(i/150)%10]
		cid, err := e.db.GetOrCreateContact(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		if i < paying {
			rid, _, err := e.db.SaveRawMessage(ctx, e2eID("T"), grp.String(), "x@s.whatsapp.net", "", "Сафаи", name+" 15000", false, "", time.Now())
			if err != nil {
				t.Fatal(err)
			}
			if err := e.db.InsertTransaction(ctx, db.TransactionInput{ContactID: cid, RawName: name, Amount: 15000, RawMessageID: rid, TxDate: time.Now()}); err != nil {
				t.Fatal(err)
			}
		}
	}
	static, dynamic := e.buildAssistantSystemPromptFor(ctx, true)
	if n := len([]rune(dynamic)); n > 20000 {
		t.Errorf("некэшируемая часть промпта ассистента: %d символов (~%d токенов) на КАЖДЫЙ запрос и каждый шаг инструментов (кэшируемая — %d символов); %d людей, %d с платежами за месяц",
			n, n/3, len([]rune(static)), contacts, paying)
	}
}

// 23. [server] Подтверждение «Молчу 🤐» после «Джарвис, молчи в группе» бот сам глушит: команда выполнена, но владелец не видит ответа

// imports: testing
func TestINV_23_SilenceConfirmationSwallowed(t *testing.T) {
	grp, owner := e2eGroup("mlc"), user(ownerPhone)
	e := newE2E(t, e2eOpts{WithAI: true, Admins: []string{ownerPhone}})
	e.groupsCache[grp] = "Оплата"
	e.ai = func(c aiCall) aiReply {
		if len(c.Tools) == 0 {
			return aiReply{Text: `{}`}
		}
		if !c.hasToolResult() {
			return aiReply{ToolCalls: []aiToolCall{{Name: "set_group_silence", Args: `{"silence":true}`}}}
		}
		return aiReply{Text: "Молчу 🤐"}
	}
	n := len(e.sent())
	e.sendGroup(groupText(grp, owner, "Ахмед", "Джарвис, молчи в группе"))
	if e.groupSilent(grp) && len(e.sentSince(n)) == 0 {
		t.Errorf("группа заглушена, но владелец не получил никакого ответа на команду")
	}
}

// 24. [server] «Перечитай группу» never re-reads saved check photos/PDFs that failed recognition (AI outage / empty balance): photos with no receipt row are never retried, and receipts that used up 6 retries stay 'failed' for good

// internal/bot/zz_inv_rescan_test.go (imports there: pgxpool, ai, httptest, …)
func TestINV_24_RereadSkipsFailedMedia(t *testing.T) {
	t.Setenv("RECEIPT_VISION_READS", "1")
	grp := e2eGroup("klnt")
	e := newE2E(t, e2eOpts{Admins: []string{"79280000001"}, Groups: map[string]string{grp.String(): "Оплата КЛНТ"}})
	e.reportDir = filepath.Join(t.TempDir(), "reports")
	var mu sync.Mutex
	down, vision := true, 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		isVision := strings.Contains(string(body), "image_url")
		mu.Lock()
		dn := down
		if isVision {
			vision++
		}
		mu.Unlock()
		if dn {
			w.WriteHeader(http.StatusPaymentRequired)
			_, _ = w.Write([]byte(`{"error":{"message":"Insufficient credits"}}`))
			return
		}
		content := `{"payments":[]}`
		if isVision {
			content = `{"kind":"receipt","bank":"Сбербанк","recipient":"Мансур Х.","amount":15000,"doc_number":"` +
				fmt.Sprint(time.Now().UnixNano()) + `","datetime":"` + time.Now().Format("2006-01-02 15:04") + `"}`
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []map[string]any{{"message": map[string]any{"role": "assistant", "content": content}, "finish_reason": "stop"}}})
	}))
	t.Cleanup(srv.Close)
	e.assistant = ai.New("k", "m", "v", srv.URL)
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	img := []byte("\xff\xd8\xff\xe0 jpeg of a receipt")
	emp := user("79001112233")
	waA := e2eID("IMG")
	rawA, _, _ := e.db.SaveRawMessage(ctx, waA, grp.String(), emp.String(), "79001112233", "Сафаи", "Каталов Ахмед", true, e.saveMediaFile(waA, img, ".jpg"), time.Now())
	e.handleBankReceipt(ctx, grp, emp.String(), waA, "Каталов Ахмед", rawA, time.Now(), img, ".jpg", "Каталов Ахмед", 0)
	waB := e2eID("IMG")
	ocrB := "Исаев Харон\nСбербанк Онлайн\nЧек по операции\nПеревод клиенту СберБанка\nФИО получателя Мансур Х.\nСумма перевода ###"
	rawB, _, _ := e.db.SaveRawMessage(ctx, waB, grp.String(), emp.String(), "79001112233", "Сафаи", ocrB, true, e.saveMediaFile(waB, img, ".jpg"), time.Now())
	e.handleBankReceipt(ctx, grp, emp.String(), waB, ocrB, rawB, time.Now(), img, ".jpg", "Исаев Харон", 0)
	for i := 0; i < 8; i++ {
		_, _ = pool.Exec(ctx, `UPDATE bank_receipts SET next_retry_at = now() - interval '1 minute' WHERE recognition_status='pending'`)
		e.retryDueReceipts()
	}
	var st string
	_ = pool.QueryRow(ctx, `SELECT COALESCE(string_agg(raw_message_id||':'||amount||':'||recognition_status||':'||recognition_attempts, ', '),'') FROM bank_receipts`).Scan(&st)
	t.Logf("чеки после сбоя (raw:сумма:статус:попыток): [%s]", st)
	mu.Lock()
	down = false
	v0 := vision
	mu.Unlock()
	out := e.tool("reread_group", grp, `{}`)
	t.Logf("reread_group → %s", out)
	mu.Lock()
	calls := vision - v0
	mu.Unlock()
	var cnt int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM bank_receipts WHERE raw_message_id IN ($1,$2) AND amount > 0`, rawA, rawB).Scan(&cnt)
	if cnt != 2 {
		t.Errorf("после «перечитай группу» распознано чеков %d из 2; запросов к зрению: %d — сохранённые фото заново не читаются", cnt, calls)
	}
}

// 25. [server] Once a group has 7+ checks without a ФИО, the bot stops asking «Чей это чек?» in that group, for new checks too, and those checks are never checked against the program

func TestINV_25_ClarifyCutoffSilent(t *testing.T) {
	ctx := context.Background()
	grp := e2eGroup("klnt")
	e := newE2E(t, e2eOpts{WithAI: true, Admins: []string{"79280000001"}, Groups: map[string]string{grp.String(): "Оплата КЛНТ"}})
	e.ai = func(c aiCall) aiReply { return aiReply{Text: `{"client":""}`} }
	emp := user("79001112233")
	asksAfter := func() int {
		e.age(grp.String())
		n := len(e.sent())
		for i := 0; i < 3; i++ {
			e.clarifyTick(ctx)
		}
		k := 0
		for _, s := range e.sentSince(n) {
			if strings.Contains(s.Text, "Чей это чек") {
				k++
			}
		}
		return k
	}
	for i := 0; i < 7; i++ {
		e.sendReceipt(grp, emp, "Сафаи", sberCheck(fmt.Sprintf("%d.00", 10010+i*10), fmt.Sprintf("09:0%d:00", i)), "")
	}
	batch := asksAfter()
	e.sendReceipt(grp, emp, "Сафаи", sberCheck("15000.00", "13:00:00"), "")
	fresh := asksAfter()
	t.Logf("вопросов «чей чек?»: по пачке из 7 — %d, по новому чеку — %d (при 6 чеках в пачке бот спрашивает все 6)", batch, fresh)
	if batch == 0 || fresh == 0 {
		t.Errorf("«чей чек?» не задаётся (пачка: %d, новый: %d) — чеки без клиента молча вне сбора и вне сверки", batch, fresh)
	}
}

// 26. [server] «Перечитай группу» records plans and questions as payments with the raw parser (+45 000 ₽ phantom in the сбор after an AI outage)

func TestINV_26_RereadRecordsNonPayments(t *testing.T) {
	owner := "79280000001"
	grp := e2eGroup("klnt")
	e := newE2E(t, e2eOpts{Admins: []string{owner}, Groups: map[string]string{grp.String(): "Оплата КЛНТ"}})
	var mu sync.Mutex
	down := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		dn := down
		mu.Unlock()
		if dn {
			w.WriteHeader(http.StatusPaymentRequired)
			_, _ = w.Write([]byte(`{"error":{"message":"Insufficient credits"}}`))
			return
		}
		msg := map[string]any{"role": "assistant", "content": `{"payments":[]}`}
		finish := "stop"
		if _, hasTools := req["tools"]; hasTools {
			msg = map[string]any{"role": "assistant", "content": "", "tool_calls": []map[string]any{{"id": "c1", "type": "function",
				"function": map[string]any{"name": "reread_group", "arguments": `{"group":"Оплата КЛНТ"}`}}}}
			finish = "tool_calls"
			for _, m := range req["messages"].([]any) {
				if mm, _ := m.(map[string]any); mm["role"] == "tool" {
					msg, finish = map[string]any{"role": "assistant", "content": "Перечитал: " + contentText(mm["content"])}, "stop"
				}
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []map[string]any{{"message": msg, "finish_reason": finish}}})
	}))
	t.Cleanup(srv.Close)
	e.assistant = ai.New("k", "m", "v", srv.URL)
	ctx := context.Background()
	emp := user("79001112233")
	for _, s := range []string{"Исаев Харон завтра принесёт 20000", "Исаев Харон 20000?", "сказал взять 5000"} {
		e.sendGroup(groupText(grp, emp, "Сафаи", s))
	}
	total := func() float64 {
		now := time.Now()
		from := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
		sums, err := e.db.SummaryForPeriod(ctx, from, from.AddDate(0, 0, 1), []string{grp.String()})
		if err != nil {
			t.Fatal(err)
		}
		var tot float64
		for _, s := range sums {
			tot += s.Total
			t.Logf("  в сборе: %q %.0f", s.CanonicalName, s.Total)
		}
		return tot
	}
	before := total()
	mu.Lock()
	down = false
	mu.Unlock()
	n := len(e.sent())
	e.handlePrivateMessage(ctx, privateText(user(owner), "Ахмед", "Джарвис перечитай группу Оплата КЛНТ"))
	for _, s := range e.sentSince(n) {
		t.Logf("бот владельцу: %s", s.Text)
	}
	if after := total(); after != before {
		t.Errorf("«перечитай группу» добавил в сбор %.0f ₽ из «завтра принесёт», «20000?», «сказал взять» (было %.0f)", after-before, before)
	}
}

// 27. [server] A check photo the bot failed to download (network drop or expired media link) disappears: no question in the group, «непонятых нет — всё разобрано», no retry, and a re-delivery from the phone is skipped as «уже было»

func TestINV_27_UndownloadedCheckInvisible(t *testing.T) {
	owner := "79280000001"
	grp := e2eGroup("klnt")
	e := newE2E(t, e2eOpts{WithAI: true, Admins: []string{owner}, Groups: map[string]string{grp.String(): "Оплата КЛНТ"}})
	e.reportDir = filepath.Join(t.TempDir(), "reports")
	dev := *store.NoopDevice
	e.client = whatsmeow.NewClient(&dev, nil)
	ctx := context.Background()
	emp := user("79001112233")
	photo := func(caption string) *events.Message {
		return &events.Message{
			Info: types.MessageInfo{MessageSource: types.MessageSource{Chat: grp, Sender: emp, IsGroup: true},
				ID: e2eID("IMG"), PushName: "Сафаи", Timestamp: time.Now().Add(-time.Hour)},
			Message: &waProto.Message{ImageMessage: &waProto.ImageMessage{Caption: proto.String(caption), Mimetype: proto.String("image/jpeg"),
				DirectPath: proto.String("/v/t62.7118-24/x.enc"), MediaKey: make([]byte, 32), FileSHA256: make([]byte, 32), FileEncSHA256: make([]byte, 32)}},
		}
	}
	chk := photo("")
	named := photo("Каталов Ахмед")
	n0 := len(e.sent())
	e.handleGroupMessage(ctx, chk)
	e.handleGroupMessage(ctx, named)
	e.waitIdle()
	toGroup := e.sentSince(n0)
	unclear := e.tool("list_unclear_receipts", grp, `{}`)
	files := e.tool("send_unclear_files", grp, `{}`)
	t.Logf("в группу после двух нескачанных чеков: %+v", toGroup)
	t.Logf("list_unclear_receipts → %s", unclear)
	t.Logf("send_unclear_files → %s", files)
	if len(toGroup) == 0 {
		t.Errorf("два чека не скачались — в группе ни вопроса, ни просьбы переслать")
	}
	e.onDemandMu.Lock()
	e.onDemandUntil, e.onDemandReplyTo = time.Now().Add(3*time.Minute), user(owner)
	e.onDemandMu.Unlock()
	n := len(e.sent())
	e.handleEvent(&events.HistorySync{Data: &waHistorySync.HistorySync{SyncType: waHistorySync.HistorySync_ON_DEMAND.Enum(),
		Conversations: []*waHistorySync.Conversation{{ID: proto.String(grp.String()), Messages: []*waHistorySync.HistorySyncMsg{{
			Message: &waWeb.WebMessageInfo{Key: &waCommon.MessageKey{RemoteJID: proto.String(grp.String()), FromMe: proto.Bool(false),
				ID: proto.String(chk.Info.ID), Participant: proto.String(emp.String())},
				Message: chk.Message, MessageTimestamp: proto.Uint64(uint64(chk.Info.Timestamp.Unix()))}}}}}}})
	time.Sleep(800 * time.Millisecond)
	t.Logf("после пакета истории бот написал: %+v", e.sentSince(n))
	if strings.Contains(unclear, "Непонятых чеков нет") {
		t.Errorf("бот говорит «%s», хотя фото чека не скачано и не разобрано", unclear)
	}
	if len(e.sentSince(n)) == 0 {
		t.Errorf("телефон прислал это сообщение снова — бот его пропустил и ничего не сообщил")
	}
}

// 28. [server] Fetching history from the phone cannot reach recent gaps and reports nothing: anchored at the OLDEST saved message, request failures swallowed, «nothing new» never reported, phone replies after 3 minutes dropped

func TestINV_28_HistoryRescanBlind(t *testing.T) {
	owner := "79280000001"
	grp := e2eGroup("klnt")
	e := newE2E(t, e2eOpts{Admins: []string{owner}, Groups: map[string]string{grp.String(): "Оплата КЛНТ"}})
	dev := *store.NoopDevice
	e.client = whatsmeow.NewClient(&dev, nil)
	ctx := context.Background()
	emp := user("79001112233")
	now := time.Now()
	hist := func(msgs ...*events.Message) *events.HistorySync {
		var hm []*waHistorySync.HistorySyncMsg
		for _, m := range msgs {
			hm = append(hm, &waHistorySync.HistorySyncMsg{Message: &waWeb.WebMessageInfo{
				Key:     &waCommon.MessageKey{RemoteJID: proto.String(grp.String()), FromMe: proto.Bool(false), ID: proto.String(m.Info.ID), Participant: proto.String(emp.String())},
				Message: m.Message, MessageTimestamp: proto.Uint64(uint64(m.Info.Timestamp.Unix()))}})
		}
		return &events.HistorySync{Data: &waHistorySync.HistorySync{SyncType: waHistorySync.HistorySync_ON_DEMAND.Enum(),
			Conversations: []*waHistorySync.Conversation{{ID: proto.String(grp.String()), Messages: hm}}}}
	}
	old := groupText(grp, emp, "Сафаи", "первое сообщение")
	old.Info.Timestamp = now.AddDate(0, -2, 0)
	e.sendGroup(old)
	known := groupText(grp, emp, "Сафаи", "Каталов Ахмед 15000")
	e.sendGroup(known)
	gapStart := now.AddDate(0, 0, -2)
	waID, ts, _, _, _ := e.db.GapAnchor(ctx, grp.String(), 14) // так бот выбирает, с чего просить историю
	req := e.client.BuildHistorySyncRequest(&types.MessageInfo{MessageSource: types.MessageSource{Chat: grp}, ID: waID, Timestamp: ts}, 200)
	anchor := time.Unix(req.GetProtocolMessage().GetPeerDataOperationRequestMessage().GetHistorySyncOnDemandRequest().GetOldestMsgTimestampMS(), 0)
	out := e.tool("reread_group", grp, `{}`)
	e.onDemandMu.Lock()
	e.onDemandUntil, e.onDemandReplyTo = time.Now().Add(3*time.Minute), user(owner)
	e.onDemandMu.Unlock()
	n := len(e.sent())
	e.handleEvent(hist(known))
	time.Sleep(500 * time.Millisecond)
	nothingNew := e.sentSince(n)
	e.onDemandMu.Lock()
	e.onDemandUntil = time.Now().Add(-time.Minute)
	e.onDemandMu.Unlock()
	late := groupText(grp, emp, "Сафаи", "Магомедов Ислам 30000")
	late.Info.Timestamp = now.AddDate(0, 0, -1)
	e.handleEvent(hist(late))
	time.Sleep(500 * time.Millisecond)
	_, lateSaved, _ := e.db.SaveRawMessage(ctx, late.Info.ID, grp.String(), emp.String(), "", "", "probe", false, "", late.Info.Timestamp)
	t.Logf("(a) якорь %s — телефон отдаст только сообщения СТАРШЕ; дыра с %s", anchor.Format("02.01.2006"), gapStart.Format("02.01.2006"))
	t.Logf("(b) reread_group при недоступном телефоне → %q", out)
	t.Logf("(c) история без нового → бот написал %d сообщений", len(nothingNew))
	t.Logf("(d) опоздавший ответ телефона сохранён: %v", lateSaved)
	if anchor.Before(gapStart) {
		t.Errorf("история запрашивается до %s — пропущенное с %s так не подтянуть", anchor.Format("02.01"), gapStart.Format("02.01"))
	}
	if !strings.Contains(strings.ToLower(out), "истори") {
		t.Errorf("запрос истории не ушёл, а владельцу об этом ни слова")
	}
	if len(nothingNew) == 0 {
		t.Errorf("телефон прислал историю без нового — обещанного «напишу, сколько нашёл» нет")
	}
	if !lateSaved {
		t.Errorf("ответ телефона после 3-минутного окна выброшен молча")
	}
}

// 29. [server] No way to search for a specific check by amount or client across all states: duplicate and deleted checks are invisible, and receipt_details presents a deleted check as a normal one

func TestINV_29_FindCheckAcrossStates(t *testing.T) {
	grp := e2eGroup("klnt")
	e := newE2E(t, e2eOpts{Groups: map[string]string{grp.String(): "Оплата КЛНТ"}})
	e.reportDir = filepath.Join(t.TempDir(), "reports")
	safai, rasul := user("79001112233"), user("79004445566")
	chkA := sberCheck("15000.00", "10:00:00")
	e.sendReceipt(grp, safai, "Сафаи", chkA, "Каталов Ахмед")
	e.sendReceipt(grp, rasul, "Расул", chkA, "Каталов Ахмед")
	waC := e.sendReceipt(grp, rasul, "Расул", sberCheck("15000.00", "12:00:00"), "Исаев Харон")
	e.event(&events.Message{Info: types.MessageInfo{MessageSource: types.MessageSource{Chat: grp, Sender: rasul, IsGroup: true}, ID: e2eID("R"), Timestamp: time.Now()},
		Message: &waProto.Message{ProtocolMessage: &waProto.ProtocolMessage{Type: waProto.ProtocolMessage_REVOKE.Enum(),
			Key: &waProto.MessageKey{RemoteJID: proto.String(grp.String()), ID: proto.String(waC), Participant: proto.String(rasul.String())}}}})
	today := time.Now().Format("2006-01-02")
	var katalov, isaev strings.Builder
	for _, c := range []struct{ name, args string }{
		{"receipts_ledger", `{"from_date":"` + today + `","to_date":"` + today + `","person":"Каталов"}`},
		{"receipt_details", `{"person":"Каталов"}`},
		{"send_client_receipt", `{"person":"Каталов"}`},
		{"find_check", `{"name":"Каталов"}`},
	} {
		out := e.tool(c.name, grp, c.args)
		t.Logf("%s %s →\n%s", c.name, c.args, out)
		katalov.WriteString(out)
	}
	for _, c := range []struct{ name, args string }{
		{"receipts_ledger", `{"from_date":"` + today + `","to_date":"` + today + `","person":"Исаев"}`},
		{"receipt_details", `{"person":"Исаев"}`},
		{"send_client_receipt", `{"person":"Исаев"}`},
		{"list_unclear_receipts", `{}`},
		{"find_check", `{"name":"Исаев","amount":15000}`},
	} {
		out := e.tool(c.name, grp, c.args)
		t.Logf("%s %s →\n%s", c.name, c.args, out)
		isaev.WriteString(out)
	}
	var withAmount []string
	for _, tl := range e.assistantTools(context.Background(), grp, types.JID{}, true, true) {
		if props, ok := tl.InputSchema["properties"].(map[string]any); ok {
			if _, ok := props["amount"]; ok {
				withAmount = append(withAmount, tl.Name)
			}
		}
	}
	t.Logf("инструменты с параметром amount (все — запись/правка, ни одного поиска): %v", withAmount)
	if !strings.Contains(katalov.String(), "Расул") {
		t.Errorf("копия чека Каталова от Расула нигде не видна («не вошёл: повтор чека Сафаи») — владелец думает, что чек потерян")
	}
	if !strings.Contains(strings.ToLower(isaev.String()), "удал") {
		t.Errorf("чек Исаева на 15000 (сообщение удалено) по клиенту не находится — бот отвечает «не нашла» без причины")
	}
}

// 30. [server] «Перечитай группу» blocks the reply while it synchronously runs up to 15 old messages through the AI (×3 reads each), yet tells the owner this happened «в фоне»

func TestINV_30_RereadSlowSync(t *testing.T) {
	owner := "79280000001"
	grp := e2eGroup("klnt")
	e := newE2E(t, e2eOpts{Admins: []string{owner}, Groups: map[string]string{grp.String(): "Оплата КЛНТ"}})
	var mu sync.Mutex
	down, delay, calls := true, time.Duration(0), 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		dn, d := down, delay
		calls++
		mu.Unlock()
		time.Sleep(d)
		if dn {
			w.WriteHeader(http.StatusPaymentRequired)
			_, _ = w.Write([]byte(`{"error":{"message":"Insufficient credits"}}`))
			return
		}
		msg := map[string]any{"role": "assistant", "content": `{"payments":[]}`}
		finish := "stop"
		if _, hasTools := req["tools"]; hasTools {
			msg = map[string]any{"role": "assistant", "content": "", "tool_calls": []map[string]any{{"id": "c1", "type": "function",
				"function": map[string]any{"name": "reread_group", "arguments": `{"group":"Оплата КЛНТ"}`}}}}
			finish = "tool_calls"
			for _, m := range req["messages"].([]any) {
				if mm, _ := m.(map[string]any); mm["role"] == "tool" {
					msg, finish = map[string]any{"role": "assistant", "content": "Перечитал: " + contentText(mm["content"])}, "stop"
				}
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []map[string]any{{"message": msg, "finish_reason": finish}}})
	}))
	t.Cleanup(srv.Close)
	e.assistant = ai.New("k", "m", "v", srv.URL)
	emp := user("79001112233")
	for i := 0; i < 20; i++ {
		e.handleGroupMessage(context.Background(), groupText(grp, emp, "Сафаи", fmt.Sprintf("буду в офисе к 15:30, клиент номер %d ждёт", i+1)))
	}
	time.Sleep(600 * time.Millisecond)
	mu.Lock()
	down, delay, calls = false, time.Second, 0
	mu.Unlock()
	n := len(e.sent())
	start := time.Now()
	e.handlePrivateMessage(context.Background(), privateText(user(owner), "Ахмед", "Джарвис перечитай группу Оплата КЛНТ"))
	took := time.Since(start)
	mu.Lock()
	c := calls
	mu.Unlock()
	for _, s := range e.sentSince(n) {
		t.Logf("бот владельцу через %v (%d запросов к ИИ): %s", took.Round(100*time.Millisecond), c, s.Text)
	}
	if took > 10*time.Second {
		t.Errorf("ответ на «перечитай группу» через %v при задержке ИИ 1 с; запросов к ИИ: %d", took.Round(100*time.Millisecond), c)
	}
}
