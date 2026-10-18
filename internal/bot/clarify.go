// Проактивный вопрос "чей это чек": если у чека нет подтверждённого клиента
// (никто не написал ФИО рядом) и это не массовый импорт, бот сам спрашивает
// в группе, показывая данные чека, и ждёт ответа — вместо того чтобы молча
// оставить чек непонятым.
package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"

	"whatsapp-bot/internal/db"
	"whatsapp-bot/internal/parser"
)

const (
	clarifyGrace    = 90 * time.Second // ждём ФИО рядом с чеком столько, прежде чем спросить
	clarifyMaxAsk   = 6                // если непонятых чеков больше — это массовый импорт, не спамим вопросами
	clarifyPerCycle = 2                // сколько вопросов задаём за один проход (чтобы не флудить)
)

// clarifyMap хранит соответствие "id вопроса бота" -> "id сообщения чека",
// чтобы ответ владельца на вопрос привязать к нужному чеку.
type clarifyState struct {
	mu           sync.Mutex
	askMap       map[string]string // botQuestionMsgID -> receiptWaMessageID (чей чек)
	cashAskMap   map[string]int    // botQuestionMsgID -> transactionID (у кого наличка)
	dupAskMap    map[string]int    // botQuestionMsgID -> transactionID (повтор налички?)
	askOrder     []string          // порядок вставки ключей askMap (для вытеснения старых)
	cashAskOrder []string          // порядок вставки ключей cashAskMap
	dupAskOrder  []string          // порядок вставки ключей dupAskMap

	// lastAsk — последний заданный ботом вопрос В КАЖДОЙ ГРУППЕ, чтобы понять
	// ПРОСТОЙ ответ, написанный следом БЕЗ свайпа («10000р» под вопросом «какая
	// сумма?»). Ключ — JID группы.
	lastAsk map[string]openAsk
	// receiptAsks — открытые вопросы «чей чек?» группы по порядку (ответ без
	// свайпа идёт к самому раннему или к чеку на названную сумму).
	receiptAsks map[string][]openAsk
	// paymentAsks — «❓ Чья это наличка 27000?»: id вопроса → сообщение-платёж.
	paymentAsks map[string]paymentAsk
	// dupFixAsks — «этот чек уже присылали, в первый раз он на X, сейчас
	// подписан Y»: id предупреждения → какой чек и на кого поправить.
	dupFixAsks map[string]dupFixAsk
}

// dupFixAsk — повторно присланный чек подписан другим клиентом.
type dupFixAsk struct {
	checkWa string // первый (засчитанный) чек
	name    string // клиент из новой подписи
}

// paymentAsk — платёж из текста, по которому бот спросил, чей он.
type paymentAsk struct {
	rawID  int
	amount float64
	cash   bool
	txDate time.Time
}

// openAsk — открытый вопрос бота в группе (для ответа без свайпа).
type openAsk struct {
	kind        string // "receipt" | "cash_collector" | "cash_dup" | "cmf_watch"
	receiptWaID string // для receipt — id сообщения-чека
	txID        int    // для cash_* — id транзакции
	watchID     int    // для cmf_watch — наблюдение сверки («за кого платёж?»)
	senderJID   string // чей чек/наличка (ответ без свайпа принимаем от него или от владельца)
	amount      float64
	wantName    bool // для cmf_watch — бот прямо попросил написать ФИО
	needAmount  bool // по чеку не прочиталась сумма
	needName    bool // по чеку не прочитан клиент
	at          time.Time
}

func newClarifyState() *clarifyState {
	return &clarifyState{
		askMap:     make(map[string]string),
		cashAskMap: make(map[string]int),
		dupAskMap:  make(map[string]int),
		lastAsk:    make(map[string]openAsk),

		receiptAsks: make(map[string][]openAsk),
		paymentAsks: make(map[string]paymentAsk),
		dupFixAsks:  make(map[string]dupFixAsk),
	}
}

// clarifyMapCap — максимум запоминаемых связей «вопрос бота -> чек/наличка».
const clarifyMapCap = 500

// registerCashAsk запоминает связь «id вопроса про наличку -> id транзакции».
// При переполнении удаляет САМЫЕ СТАРЫЕ ключи (по порядку вставки), а не весь
// map целиком — иначе ответ на любой не-самый-новый вопрос теряет привязку.
func (b *Bot) registerCashAsk(botMsgID string, txID int) {
	if botMsgID == "" {
		return
	}
	b.clarify.mu.Lock()
	defer b.clarify.mu.Unlock()
	if _, exists := b.clarify.cashAskMap[botMsgID]; !exists {
		b.clarify.cashAskOrder = append(b.clarify.cashAskOrder, botMsgID)
	}
	b.clarify.cashAskMap[botMsgID] = txID
	for len(b.clarify.cashAskMap) > clarifyMapCap && len(b.clarify.cashAskOrder) > 0 {
		oldest := b.clarify.cashAskOrder[0]
		b.clarify.cashAskOrder = b.clarify.cashAskOrder[1:]
		delete(b.clarify.cashAskMap, oldest)
	}
}

// registerDupAsk запоминает связь «id вопроса про повтор налички -> id транзакции».
func (b *Bot) registerDupAsk(botMsgID string, txID int) {
	if botMsgID == "" {
		return
	}
	b.clarify.mu.Lock()
	defer b.clarify.mu.Unlock()
	if _, exists := b.clarify.dupAskMap[botMsgID]; !exists {
		b.clarify.dupAskOrder = append(b.clarify.dupAskOrder, botMsgID)
	}
	b.clarify.dupAskMap[botMsgID] = txID
	for len(b.clarify.dupAskMap) > clarifyMapCap && len(b.clarify.dupAskOrder) > 0 {
		oldest := b.clarify.dupAskOrder[0]
		b.clarify.dupAskOrder = b.clarify.dupAskOrder[1:]
		delete(b.clarify.dupAskMap, oldest)
	}
}

// cashDupCheckEnabled — спрашивать ли про повторную наличку (та же сумма тому же
// клиенту за 30 дней — возможно, случайный дубль). По умолчанию ВКЛ. Отключить
// целиком: CASH_DUP_CHECK=0.
func cashDupCheckEnabled() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("CASH_DUP_CHECK")))
	return v == "" || v == "1" || v == "true" || v == "yes" || v == "on"
}

// cashDupCheckOn — фактически включена ли проверка повторной налички. Требует и
// общий выключатель уточнений (askReceiptsEnabled), и свой CASH_DUP_CHECK: иначе
// придержанная наличка осталась бы без вопроса и навсегда вне сбора.
func (b *Bot) cashDupCheckOn() bool {
	return askReceiptsEnabled() && cashDupCheckEnabled()
}

// Повтор налички ловится и при ином написании клиента: «Магамедов Ахмед нал
// 15000», а следом «Магомедов Ахмед нал 15000» (опечатку исправили, отправив
// заново) — тот же клиент, придержать и спросить. Сравнение строгое, как для
// всех денежных решений: «Омаров» и «Умаров» — разные клиенты.
func init() { db.SameCashClient = sameClientMoney }

// clarifyLoop раз в 45 секунд проверяет группы на чеки без клиента и задаёт
// по ним вопросы (аккуратно, не спамя).
func (b *Bot) clarifyLoop() {
	ticker := time.NewTicker(45 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		b.clarifyTick(ctx)
		cancel()
	}
}

// askReceiptsEnabled — включены ли уточняющие вопросы по чекам (чей чек, не
// разобрал сумму, у кого наличка, неполный чек). По умолчанию ВКЛ; выключить
// целиком: ASK_UNNAMED_RECEIPTS=0.
func askReceiptsEnabled() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("ASK_UNNAMED_RECEIPTS")))
	return !(v == "0" || v == "false" || v == "no")
}

func (b *Bot) clarifyTick(ctx context.Context) {
	// Без ИИ имена в ответах про чеки не разбираем — эти вопросы не задаём; про
	// наличку («у кого?», «новый или тот же?») спрашиваем и без ИИ: ответы
	// понимает код, а придержанная наличка без вопроса так и осталась бы вне сбора.
	withAI := b.assistant != nil
	if !askReceiptsEnabled() {
		return
	}
	before := time.Now().Add(-clarifyGrace)
	asked := 0
	for jid := range b.joinedGroups(ctx) {
		if asked >= clarifyPerCycle {
			break
		}
		if !b.isAllowedGroup(jid) {
			continue
		}
		if b.groupSilent(jid) {
			continue // владелец попросил молчать в этой группе — вопросов не задаём
		}
		// 1. Чеки без имени клиента — «чей это чек?».
		// Много чеков без ответа (сотрудник скинул пачку) — не замолкаем совсем:
		// спрашиваем про самые свежие по одному за цикл, остальные видны в
		// «нераспознанных» и в разделе «НЕ вошло».
		if n, err := b.db.CountUnconfirmed(ctx, jid.String(), before); withAI && err == nil && n > 0 {
			mass := n > clarifyMaxAsk
			limit := clarifyPerCycle - asked
			if mass {
				limit = 1
				fmt.Printf("Чей чек: %d чеков без ФИО в группе %s — спрашиваю по одному, свежие первыми\n", n, jid)
			}
			items, err := b.db.UnconfirmedReceiptsOrdered(ctx, jid.String(), before, limit, mass)
			if err == nil {
				for _, it := range items {
					// Прежде чем спрашивать — анализируем недавние сообщения этого
					// же отправителя: вдруг ФИО клиента написали рядом с чеком
					// (отдельной строкой/сообщением). Тогда привязываем молча.
					if b.tryResolveClientFromContext(ctx, jid, it) {
						continue
					}
					owner := it.CardOwner
					if owner == "" {
						owner = "не распознан"
					}
					text := fmt.Sprintf("🤔 Чей это чек? Получатель на чеке: %s, сумма %.0f ₽, %s. "+
						"Напишите имя клиента (кому засчитать) — можно просто следующим сообщением.", owner, it.Amount, it.TxDate.Format("02.01 15:04"))
					if b.askClarify(ctx, jid, text, it) {
						asked++
						// Имя клиента примем и без свайпа — следующим сообщением.
						b.setOpenAsk(jid.String(), openAsk{kind: "receipt", receiptWaID: it.WaMessageID, needName: true, senderJID: it.SenderJID, amount: it.Amount})
					}
				}
			}
		}
		if asked >= clarifyPerCycle {
			break
		}
		// 3. Наличка без ответственного — «у кого наличка / кто забрал?».
		if n, err := b.db.CountCashNeedingCollector(ctx, jid.String(), before); err == nil && n > 0 && n <= clarifyMaxAsk {
			items, err := b.db.CashNeedingCollector(ctx, jid.String(), before, clarifyPerCycle-asked)
			if err == nil {
				for _, it := range items {
					who := it.Client
					if who == "Наличка" || who == "(не указано)" || who == "" {
						who = "без имени клиента"
					}
					text := fmt.Sprintf("🤔 Наличка: %s — %.0f ₽. У кого она, кто забрал деньги? "+
						"Ответьте на это сообщение именем (например «у Дени» / «Мансур взял»).", who, it.Amount)
					botMsgID := b.sendReply(jid, text, it.WaMessageID, it.SenderJID)
					if botMsgID != "" {
						// Помечаем «спросили» ТОЛЬКО если вопрос реально ушёл. При
						// сбое отправки (botMsgID == "") оставляем платёж в очереди —
						// следующий цикл переспросит, иначе наличка навсегда без
						// ответственного.
						_ = b.db.MarkTxCollectorAsked(ctx, it.TxID, botMsgID)
						b.registerCashAsk(botMsgID, it.TxID)
						b.setOpenAsk(jid.String(), openAsk{kind: "cash_collector", txID: it.TxID, senderJID: it.SenderJID, amount: it.Amount})
						asked++
					}
				}
			}
		}
		if asked >= clarifyPerCycle {
			break
		}
		// 4. Повтор налички — «У клиента уже была та же сумма, это новый платёж?».
		// Без верхней границы n<=clarifyMaxAsk (как у других веток): придержанная
		// наличка исключена из сбора, поэтому её ОБЯЗАТЕЛЬНО нужно спросить, иначе
		// при накоплении >6 повторов ветка молча выключилась бы и деньги пропали
		// из сбора навсегда. Спам ограничен общим лимитом clarifyPerCycle за цикл.
		if b.cashDupCheckOn() {
			if n, err := b.db.CountCashDupNeedingConfirm(ctx, jid.String(), before); err == nil && n > 0 {
				items, err := b.db.CashDupNeedingConfirm(ctx, jid.String(), before, clarifyPerCycle-asked)
				if err == nil {
					for _, it := range items {
						when := "недавно"
						if !it.PrevWhen.IsZero() {
							when = "была " + it.PrevWhen.Format("02.01")
						}
						asWritten := ""
						if it.PrevName != "" {
							asWritten = " (записана как «" + it.PrevName + "»)"
						}
						text := fmt.Sprintf("🤔 У клиента %s уже %s наличка на %.0f ₽%s. Это НОВЫЙ платёж или тот же самый (повтор)? "+
							"Ответьте на это сообщение: «новый» — засчитаю отдельно, «тот же» — не считаю повтором.",
							it.Client, when, it.Amount, asWritten)
						botMsgID := b.sendReply(jid, text, it.WaMessageID, it.SenderJID)
						if botMsgID != "" {
							_ = b.db.MarkCashDupAsked(ctx, it.TxID, botMsgID)
							b.registerDupAsk(botMsgID, it.TxID)
							b.setOpenAsk(jid.String(), openAsk{kind: "cash_dup", txID: it.TxID, senderJID: it.SenderJID, amount: it.Amount})
							asked++
						}
					}
				}
			}
		}
		if asked >= clarifyPerCycle {
			break
		}
		// 2. Чеки, у которых не прочиталась сумма — «не смог разобрать, что на чеке?».
		if n, err := b.db.CountUnrecognized(ctx, jid.String(), before); withAI && err == nil && n > 0 {
			mass := n > clarifyMaxAsk
			limit := clarifyPerCycle - asked
			if mass {
				limit = 1
			}
			items, err := b.db.UnrecognizedReceiptsOrdered(ctx, jid.String(), before, limit, mass)
			if err == nil {
				for _, it := range items {
					text := "🤔 Не смог разобрать этот чек (не прочитал сумму). Напишите сумму " +
						"и ФИО клиента — например: «Ахмед Каталов 15000» (можно просто следующим сообщением)."
					if b.askClarify(ctx, jid, text, it) {
						asked++
						b.setOpenAsk(jid.String(), openAsk{kind: "receipt", receiptWaID: it.WaMessageID, needAmount: true, needName: true, senderJID: it.SenderJID})
					}
				}
			}
		}
	}
}

// tryResolveClientFromContext — прежде чем спросить «чей чек», анализирует
// недавние сообщения ТОГО ЖЕ отправителя: если рядом с чеком написали ФИО
// клиента (в т.ч. отдельной строкой «Магамадов Алха\n22.000₽»), привязывает
// чек к нему молча, без вопроса. Возвращает true, если удалось.
func (b *Bot) tryResolveClientFromContext(ctx context.Context, jid types.JID, it db.ClarifyReceipt) bool {
	if it.WaMessageID == "" || it.SenderJID == "" {
		return false
	}
	texts, err := b.db.RecentSenderTexts(ctx, jid.String(), it.SenderJID, time.Now().Add(-12*time.Minute), 15)
	if err != nil {
		return false
	}
	for _, t := range texts {
		name, ok := looksLikeName(t)
		if !ok {
			name, ok = firstNameLine(t)
		}
		if !ok {
			continue
		}
		// Уверены, что это ФИО, только если имя известно или его подтвердил ИИ
		// («Ассаламу алейкум брат», «Доброе утро всем» — не клиенты).
		if _, known := b.aliases.ResolveName(name); !known {
			confirmed := b.cmfExtractClientName(ctx, t)
			if confirmed == "" {
				continue
			}
			name = confirmed
		}
		canonical, _ := b.aliases.ResolveName(name)
		// Владелец карты (получатель на чеке) — это НЕ клиент, его не берём.
		if it.CardOwner != "" && strings.EqualFold(strings.TrimSpace(canonical), strings.TrimSpace(it.CardOwner)) {
			continue
		}
		var contactIDPtr *int
		if cid, err := b.db.GetOrCreateContact(ctx, canonical); err == nil {
			// Это ФИО уже «ушло» на другой чек того же отправителя («Иванов» +
			// чек1 + чек2: Иванов — это чек1). Второму чеку имя не приписываем —
			// про него честно спросим.
			if used, err := b.db.ClientAlreadyAttributedFrom(ctx, jid.String(), it.SenderJID, cid, it.WaMessageID, time.Now().Add(-30*time.Minute)); err == nil && used {
				continue
			}
			contactIDPtr = &cid
		}
		if found, _, _, err := b.db.FillReceiptByMessage(ctx, it.WaMessageID, canonical, contactIDPtr, 0); err == nil && found {
			fmt.Printf("Чек %s привязан к клиенту %q из соседнего сообщения (без вопроса)\n", it.WaMessageID, canonical)
			b.syncWatchByCheckWa(ctx, jid, it.WaMessageID, canonical)
			return true
		}
	}
	return false
}

// applyCashCollectorReply записывает ответственного (кто забрал наличку) из
// ответа владельца на вопрос «у кого наличка?».
func (b *Bot) applyCashCollectorReply(ctx context.Context, chat types.JID, txID int, text string, swipe bool) bool {
	collector := extractCollectorName(text)
	if collector == "" && swipe {
		// Детерминированный разбор не справился с нестандартной формулировкой —
		// спрашиваем ИИ (он поймёт «да Шамиль его забрал, он в офисе был» и т.п.).
		collector = b.aiCashCollector(ctx, text)
	}
	if collector == "" {
		return false
	}
	if canon, ok := b.aliases.ResolveName(collector); ok {
		collector = canon
	}
	// Без свайпа ответственного только ДОПИСЫВАЕМ (не перезаписываем уже названного).
	found, amount, client, err := b.db.SetTxCollector(ctx, txID, collector, !swipe)
	if err != nil || !found {
		return false
	}
	b.noteAnswer(ctx, "collector", strconv.Itoa(txID))
	b.sendText(chat, fmt.Sprintf("Записал: наличка %s %.0f ₽ — забрал %s.", client, amount, collector))
	return true
}

// aiCashCollector — ИИ-запас для ответа «у кого наличка / кто забрал»: достаёт
// имя забравшего из свободной формулировки, которую не осилил extractCollectorName
// («да её Шамиль в офисе принял» -> «Шамиль»). Пусто — не названо/ИИ недоступен.
func (b *Bot) aiCashCollector(ctx context.Context, reply string) string {
	if b.assistant == nil {
		return ""
	}
	sys := "Владельцу задали вопрос «у кого наличка / кто забрал деньги». Из его ответа выдели ИМЯ того, кто " +
		"ЗАБРАЛ/ПРИНЯЛ наличку — это человек ИЛИ место (офис, кафе, точка). Приведи в именительный падеж " +
		"(«Шамиля» -> «Шамиль»). Если в ответе никого не назвали — пусто. Верни СТРОГО JSON {\"collector\":\"имя или пусто\"}."
	out, err := b.assistant.Complete(ctx, sys, reply)
	if err != nil {
		return ""
	}
	var p struct {
		Collector string `json:"collector"`
	}
	if blk := extractJSONBlock(out); blk != "" {
		_ = json.Unmarshal([]byte(blk), &p)
	}
	return strings.TrimSpace(p.Collector)
}

// aiCashDupDecision — ИИ-запас для ответа на «повтор налички: новый или тот же?».
// Возвращает (resolved, isNew): resolved=false — ИИ тоже не понял, переспросим.
func (b *Bot) aiCashDupDecision(ctx context.Context, reply string) (resolved, isNew bool) {
	if b.assistant == nil {
		return false, false
	}
	sys := "Владельцу задали вопрос про ПОВТОР налички: это НОВЫЙ отдельный платёж или ТОТ ЖЕ (повтор, не считать)? " +
		"По его ответу реши. Верни СТРОГО JSON {\"decision\":\"new|same|unclear\"}: " +
		"new — новый/отдельный/засчитать/ещё один; same — тот же/повтор/дубль/не считать; unclear — непонятно."
	out, err := b.assistant.Complete(ctx, sys, reply)
	if err != nil {
		return false, false
	}
	var p struct {
		Decision string `json:"decision"`
	}
	if blk := extractJSONBlock(out); blk != "" {
		_ = json.Unmarshal([]byte(blk), &p)
	}
	switch strings.ToLower(strings.TrimSpace(p.Decision)) {
	case "new":
		return true, true
	case "same":
		return true, false
	}
	return false, false
}

// cashCollectorMarkers — слова-обёртки вокруг имени в ответе «у Дени», «Мансур взял».
var cashCollectorMarkers = map[string]bool{
	"у": true, "взял": true, "взяла": true, "забрал": true, "забрала": true,
	"отдал": true, "отдала": true, "к": true, "собрал": true, "собрала": true,
	"наличка": true, "нал": true, "наличными": true, "это": true, "руки": true, "руках": true,
}

// extractCollectorName вытаскивает имя ответственного из свободного ответа
// («у Дени» -> «Дени», «Мансур взял» -> «Мансур», «отдал Адаму» -> «Адаму»).
func extractCollectorName(text string) string {
	var words []string
	for _, w := range strings.Fields(text) {
		lw := strings.ToLower(strings.Trim(w, ".,!?()«»\""))
		if cashCollectorMarkers[lw] || lw == "" {
			continue
		}
		if strings.IndexFunc(w, func(r rune) bool { return r >= '0' && r <= '9' }) >= 0 {
			continue
		}
		words = append(words, w)
	}
	return strings.TrimSpace(strings.Join(words, " "))
}

// parseCashDupAnswer разбирает ответ на вопрос про повтор налички (по целым
// словам: «Усманов» — не «новый»). Возвращает (resolved, isNew): resolved=false —
// ответ непонятен (переспросим).
func parseCashDupAnswer(text string) (bool, bool) {
	t := strings.ToLower(strings.TrimSpace(text))
	toks := strings.FieldsFunc(t, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
	has := func(ws ...string) bool {
		for _, tk := range toks {
			for _, w := range ws {
				if tk == w {
					return true
				}
			}
		}
		return false
	}
	phrase := " " + strings.Join(toks, " ") + " "
	hasPhrase := func(ps ...string) bool {
		for _, p := range ps {
			if strings.Contains(phrase, " "+p+" ") {
				return true
			}
		}
		return false
	}
	// «тот же / повтор» — первыми: «не новый» содержит «новый».
	if hasPhrase("тот же", "та же", "то же", "тоже самое", "то же самое", "один и тот же", "не новый", "не новая",
		"не считай", "не считать", "не засчитывай") || has("повтор", "дубль", "дубликат", "старый", "ошибка", "ошибся", "ошиблась", "тоже") {
		return true, false
	}
	if t == "нет" || t == "неа" {
		return true, false
	}
	if has("новый", "новая", "новое", "новые", "отдельный", "отдельно", "отдельная", "другой", "другая", "другое") ||
		hasPhrase("ещё один", "еще один", "ещё одна", "еще одна", "ещё раз", "еще раз") {
		return true, true
	}
	if t == "да" {
		return true, true // «да» на «это новый?» — новый
	}
	return false, false
}

// askClarify отправляет вопрос цитатой на сам чек и запоминает связь
// «id вопроса -> id сообщения чека» (в памяти И в БД), чтобы привязать ответ
// владельца. Возвращает false, если вопрос не ушёл: тогда чек НЕ помечаем
// «спросили» — следующий цикл переспросит, иначе он завис бы навсегда.
func (b *Bot) askClarify(ctx context.Context, jid types.JID, text string, it db.ClarifyReceipt) bool {
	botMsgID := b.sendReply(jid, text, it.WaMessageID, it.SenderJID)
	if botMsgID == "" {
		return false
	}
	_ = b.db.MarkReceiptAsked(ctx, it.ID, botMsgID)
	b.registerClarifyAsk(botMsgID, it.WaMessageID)
	return true
}

// confirmReplies — ответы-подтверждения на вопрос бота («проверьте сумму» →
// «верно»): ничего не меняем, просто принимаем.
var confirmReplies = map[string]bool{
	"да": true, "верно": true, "все верно": true, "всё верно": true, "ок": true, "окей": true,
	"правильно": true, "да верно": true, "точно": true, "так и есть": true, "подтверждаю": true, "+": true,
	"да, верно": true, "все правильно": true, "всё правильно": true,
}

// isConfirmReply — «да / верно / ок» на вопрос бота.
func isConfirmReply(lower string) bool {
	return confirmReplies[strings.Trim(lower, ".,!)👍✅ ")]
}

// isUnknownReply — «не знаю / хз / не помню»: владелец не может ответить сейчас.
// Такой ответ НЕ должен превращаться в «имя клиента» и не снимает вопрос.
func isUnknownReply(lower string) bool {
	for _, m := range []string{"не знаю", "незнаю", "хз", "не помню", "непонятно", "не понял", "не в курсе", "без понятия"} {
		if strings.Contains(lower, m) {
			return true
		}
	}
	return false
}

// clarifyNameFromReply разбирает ОТВЕТ (свайпом) на вопрос бота про чек: имя
// клиента и/или сумму. deferToAI=true — ответ похож на вопрос или на целую
// фразу («Записал чек?», «клиент X сумма Y дата Z»): его нельзя грубо разбирать
// на «имя», лучше отдать ассистенту, который поймёт по смыслу (и запишет через
// fix_receipt). Так вопрос владельца перестаёт превращаться в ФИО клиента.
func clarifyNameFromReply(text string) (name string, amount float64, deferToAI bool) {
	amount = parser.ExtractAmount(text)
	// «Молчи в группе», «перечитай группу», «найди чек Каталова» — команда, не имя.
	if isCommandText(text) {
		return "", amount, true
	}
	// Вопрос («…?») — это не имя. Отдаём ассистенту.
	if strings.Contains(text, "?") {
		return "", amount, true
	}
	// Фраза-исправление, а не имя: ответ вроде «нет, это не Ахмед, а Магомед»
	// после отсева стоп-слов склеился бы в ложное ФИО «Ахмед Магомед». Если есть
	// явные маркеры фразы (отрицание «не/нет», ведущее противопоставление «а/но»,
	// или длиннее имени) — отдаём ассистенту: он поймёт, кого имел в виду владелец,
	// и запишет через fix_receipt, а не выдумает клиента из обрывков.
	if looksLikeCorrectionSentence(text) {
		return "", amount, true
	}
	// Чистое ФИО (2+ слов) — берём как есть.
	if n, ok := looksLikeName(text); ok {
		return n, amount, false
	}
	// Иначе собираем кандидатов в имя: слова без цифр, очищенные от знаков/эмодзи,
	// кроме служебных стоп-слов (глаголы, «клиент/сумма/дата», обращения к боту).
	fields := strings.Fields(text)
	var words []string
	for _, w := range fields {
		if strings.IndexFunc(w, func(r rune) bool { return r >= '0' && r <= '9' }) >= 0 {
			continue
		}
		clean := strings.TrimFunc(w, func(r rune) bool { return !unicode.IsLetter(r) })
		if clean == "" || nameStopwords[strings.ToLower(clean)] {
			continue
		}
		// Слово-кандидат в имя должно начинаться с ЗАГЛАВНОЙ (имя собственное).
		// Строчное слово в коротком ответе — это, скорее, частица/глагол из фразы
		// («а не посчитал»), а не ФИО: отдаём ассистенту, не выдумывая имя.
		if r := []rune(clean); !unicode.IsUpper(r[0]) {
			return "", amount, true
		}
		words = append(words, clean)
	}
	// Как имя принимаем ТОЛЬКО короткий ответ: 1–3 слова-имени в реплике из ≤4
	// слов («Ахмед», «Ахмед 15000»). Длинную фразу отдаём ассистенту — он
	// разберёт правильно, а не склеит служебные слова в «имя».
	switch {
	case len(words) == 0:
		return "", amount, false // имени нет; выше проверят, есть ли сумма
	case len(words) <= 3 && len(fields) <= 4:
		return strings.Join(words, " "), amount, false
	default:
		return "", amount, true
	}
}

// looksLikeCorrectionSentence распознаёт ответ-ИСПРАВЛЕНИЕ (а не ФИО): отрицание
// «не/нет» где угодно («это не Ахмед»), ведущая частица-противопоставление
// («а Магомед», «но Иван»), или длина больше имени (>4 слов). Такое нельзя грубо
// разбирать на имя — смысл теряется; отдаём ассистенту. Обычные ФИО (в т.ч. с
// ведущим «Клиент»/«Это» и отчеством) под эти маркеры не попадают.
func looksLikeCorrectionSentence(text string) bool {
	fields := strings.Fields(strings.TrimSpace(text))
	if len(fields) == 0 {
		return false
	}
	if len(fields) > 4 { // ФИО — максимум 3–4 слова; длиннее это фраза
		return true
	}
	norm := func(w string) string {
		return strings.ToLower(strings.TrimFunc(w, func(r rune) bool { return !unicode.IsLetter(r) }))
	}
	// Отрицание где угодно в коротком ответе = исправление («это НЕ Ахмед», «НЕ он»).
	for _, w := range fields {
		switch norm(w) {
		case "не", "нет", "неа":
			return true
		}
	}
	// Ведущее противопоставление («а Магомед», «но Иван», «ну Расул»).
	switch norm(fields[0]) {
	case "а", "но", "ну":
		return true
	}
	return false
}

// registerClarifyAsk запоминает связь «id вопроса бота -> id сообщения чека»,
// чтобы ответ владельца (свайпом на вопрос) привязался к нужному чеку.
func (b *Bot) registerClarifyAsk(botMsgID, receiptWaID string) {
	if botMsgID == "" || receiptWaID == "" {
		return
	}
	b.clarify.mu.Lock()
	if _, exists := b.clarify.askMap[botMsgID]; !exists {
		b.clarify.askOrder = append(b.clarify.askOrder, botMsgID)
	}
	b.clarify.askMap[botMsgID] = receiptWaID
	// Вытесняем самые старые связи, а не весь map целиком — иначе ответ на любой
	// не-самый-свежий вопрос «чей чек» теряет привязку и уходит в неверную
	// FIFO-атрибуцию.
	for len(b.clarify.askMap) > clarifyMapCap && len(b.clarify.askOrder) > 0 {
		oldest := b.clarify.askOrder[0]
		b.clarify.askOrder = b.clarify.askOrder[1:]
		delete(b.clarify.askMap, oldest)
	}
	b.clarify.mu.Unlock()
}

const (
	suspiciousMultiple = 12     // во сколько раз выше медианы = подозрительно
	suspiciousFloor    = 100000 // и не ниже этого порога (мелкие суммы не трогаем)
	suspiciousMinCount = 30     // минимум чеков в группе, чтобы медиана была надёжной
)

func suspiciousAmountCheckEnabled() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("SUSPICIOUS_AMOUNT_CHECK")))
	return v == "" || v == "1" || v == "true" || v == "yes" || v == "on"
}

// checkSuspiciousAmount спрашивает в группе, если сумма чека аномально большая
// для этой группы (вероятно, OCR/зрение приписали лишний ноль). Чек при этом
// уже в учёте — если владелец поправит, сумма исправится; если подтвердит —
// останется. Ловит именно грубые ошибки, чтобы не спамить.
func (b *Bot) checkSuspiciousAmount(ctx context.Context, chat types.JID, waMsgID, senderJID string, amount float64) {
	if !suspiciousAmountCheckEnabled() || waMsgID == "" || amount < suspiciousFloor {
		return
	}
	if b.groupSilent(chat) {
		return // в этой группе бот молчит по просьбе владельца
	}
	median, n, err := b.db.GroupAmountMedian(ctx, chat.String(), time.Now().AddDate(0, -3, 0))
	if err != nil || n < suspiciousMinCount || median <= 0 || amount <= median*suspiciousMultiple {
		return
	}
	text := fmt.Sprintf("🤔 Проверьте сумму: %.0f ₽ по этому чеку необычно большая для этой группы "+
		"(обычно около %.0f ₽). Если это ошибка распознавания — ответьте на это сообщение верной суммой; "+
		"если всё верно — напишите «верно».", amount, median)
	botMsgID := b.sendReply(chat, text, waMsgID, senderJID)
	if botMsgID != "" {
		// Персистим связь «вопрос -> чек»: иначе после перезапуска бота ответ
		// владельца («130000») не находил чек и завышенная сумма оставалась в сборе.
		_ = b.db.MarkReceiptAskedByMessage(ctx, waMsgID, botMsgID)
		b.registerClarifyAsk(botMsgID, waMsgID)
		// Поправленную сумму примем и без свайпа (следующим сообщением «130000»).
		b.setOpenAsk(chat.String(), openAsk{kind: "receipt", receiptWaID: waMsgID, needAmount: true, senderJID: senderJID})
	}
}

// handleClarifyReply — если владелец ответил (свайп) на вопрос бота "чей чек",
// привязывает названного клиента к тому чеку. Возвращает true, если обработано.
func (b *Bot) handleClarifyReply(ctx context.Context, msg *events.Message, text string) bool {
	quotedID := extractQuotedStanzaID(msg)
	if quotedID == "" {
		return false
	}

	// Ответ на вопрос сверки «за кого этот платёж?» (кто угодно из группы).
	if b.cmfReplyAnswer(ctx, msg, text, modeSwipe) {
		return true
	}

	// Ответ на «❓ Чья это наличка?» по текстовому платежу.
	b.clarify.mu.Lock()
	pa, isPayAsk := b.clarify.paymentAsks[quotedID]
	fix, isFixAsk := b.clarify.dupFixAsks[quotedID]
	b.clarify.mu.Unlock()
	if !isPayAsk {
		// Связь могла потеряться из памяти (рестарт бота) — ищем в БД.
		if p, ok, _ := b.db.WhoAskByMsg(ctx, quotedID); ok {
			pa, isPayAsk = paymentAsk{rawID: p.RawID, amount: p.Amount, cash: p.Cash, txDate: p.TxDate}, true
		}
	}
	if isPayAsk {
		return b.applyPaymentWhoAnswer(ctx, msg.Info.Chat, quotedID, pa, text, true)
	}
	// «Этот чек уже присылали… сейчас подписан Y» — «да» переносит чек на Y.
	if isFixAsk {
		return b.applyDupFixAnswer(ctx, msg.Info.Chat, quotedID, fix, text)
	}

	// Ответ на вопрос «наличка-повтор: новый или тот же?».
	b.clarify.mu.Lock()
	dupTxID, isDupAsk := b.clarify.dupAskMap[quotedID]
	b.clarify.mu.Unlock()
	if !isDupAsk {
		// Связь могла потеряться из памяти (рестарт бота) — ищем в БД по id
		// сообщения-вопроса, чтобы поздний ответ всё равно сработал.
		if id, ok, _ := b.db.DupTxByAskMsg(ctx, quotedID); ok {
			dupTxID, isDupAsk = id, true
		}
	}
	if isDupAsk {
		resolved, isNew := parseCashDupAnswer(text)
		if !resolved && b.assistant != nil {
			// Нестандартный ответ — пусть ИИ решит по смыслу «новый или тот же».
			if r, n := b.aiCashDupDecision(ctx, text); r {
				resolved, isNew = r, n
			}
		}
		if !resolved {
			// Переспрос тоже связываем с этой наличкой: ответ свайпом на него сработает.
			if id := b.sendTextReturnID(msg.Info.Chat, "Ответьте, пожалуйста, «новый» (засчитать отдельно) или «тот же» (повтор, не считать)."); id != "" {
				b.registerDupAsk(id, dupTxID)
				_ = b.db.MarkCashDupAsked(ctx, dupTxID, id)
			}
			return true // оставляем связь — можно ответить ещё раз на тот же вопрос
		}
		b.clarify.mu.Lock()
		delete(b.clarify.dupAskMap, quotedID)
		b.clarify.mu.Unlock()
		found, amount, client, err := b.db.ResolveCashDup(ctx, dupTxID, isNew)
		if err != nil || !found {
			return false
		}
		b.noteAnswer(ctx, "dup", strconv.Itoa(dupTxID))
		b.clearOpenAskTx(msg.Info.Chat.String(), "cash_dup", dupTxID)
		if isNew {
			b.sendText(msg.Info.Chat, fmt.Sprintf("Засчитал как новый платёж: наличка %s %.0f ₽.", client, amount))
		} else {
			b.sendText(msg.Info.Chat, fmt.Sprintf("Понял, это повтор — не считаю: %s %.0f ₽.", client, amount))
		}
		return true
	}

	// Ответ на вопрос «у кого наличка / кто забрал?» — записываем ответственного.
	b.clarify.mu.Lock()
	txID, isCashAsk := b.clarify.cashAskMap[quotedID]
	if isCashAsk {
		delete(b.clarify.cashAskMap, quotedID)
	}
	b.clarify.mu.Unlock()
	if !isCashAsk {
		// Связь могла потеряться из памяти (перезапуск бота) — ищем наличку в БД
		// по id сообщения-вопроса, чтобы ответ владельца всё равно сработал.
		if id, ok, _ := b.db.TxByCollectorAskMsg(ctx, quotedID); ok {
			txID, isCashAsk = id, true
		}
	}
	if isCashAsk {
		if b.applyCashCollectorReply(ctx, msg.Info.Chat, txID, text, true) {
			b.clearOpenAskTx(msg.Info.Chat.String(), "cash_collector", txID)
			return true
		}
		return false
	}

	// Связь «вопрос -> чек» НЕ снимаем при поиске: снимем только после успешной
	// записи. Иначе непонятный/неполный ответ («хз», имя без суммы) навсегда
	// отвязывал вопрос, и следующий нормальный ответ уходил в никуда.
	b.clarify.mu.Lock()
	receiptWaID, ok := b.clarify.askMap[quotedID]
	b.clarify.mu.Unlock()
	if !ok {
		// Связь могла потеряться (перезапуск бота) — ищем чек по id вопроса в БД.
		if waID, found, _ := b.db.ReceiptWaIDByAskMsg(ctx, quotedID); found {
			receiptWaID, ok = waID, true
		}
	}
	if !ok {
		return false
	}
	handled, done := b.applyReceiptAnswer(ctx, msg.Info.Chat, receiptWaID, text)
	if done {
		b.clarify.mu.Lock()
		delete(b.clarify.askMap, quotedID)
		b.clarify.mu.Unlock()
		b.clearOpenAsk(msg.Info.Chat.String())
	}
	return handled
}

// sendTextReturnID отправляет текст и возвращает id отправленного сообщения
// (нужно, чтобы привязать ответ владельца к конкретному вопросу).
func (b *Bot) sendTextReturnID(chat types.JID, text string) string {
	if b.sendHook != nil {
		return b.sendHook(chat, text, "")
	}
	resp, err := b.client.SendMessage(context.Background(), chat, &waProto.Message{
		Conversation: proto.String(text),
	})
	if err != nil {
		fmt.Println("Ошибка отправки вопроса:", err)
		return ""
	}
	b.rememberSent(chat, resp.ID)
	return resp.ID
}

// registerPaymentAsk — запомнить вопрос «чей это платёж?» по сообщению.
func (b *Bot) registerPaymentAsk(botMsgID string, a paymentAsk) {
	b.clarify.mu.Lock()
	b.clarify.paymentAsks[botMsgID] = a
	if len(b.clarify.paymentAsks) > clarifyMapCap {
		for k := range b.clarify.paymentAsks {
			delete(b.clarify.paymentAsks, k)
			break
		}
	}
	b.clarify.mu.Unlock()
}

// whoPlaceholders — «клиенты»-заглушки налички без имени.
var whoPlaceholders = map[string]bool{"Наличка": true, "(не указано)": true}

// applyPaymentWhoAnswer — ответ на «❓ чей это платёж?»: платёж того сообщения
// получает названного клиента (заглушка «Наличка» переименовывается, новый не
// добавляется). swipe=false — ответ без свайпа: сумма в нём допустима только
// та же, что в вопросе (иначе это новый платёж). Возвращает true, если ответ
// понят (или бот переспросил по этому вопросу).
func (b *Bot) applyPaymentWhoAnswer(ctx context.Context, chat types.JID, askID string, pa paymentAsk, text string, swipe bool) bool {
	t := strings.TrimSpace(text)
	// Исключённый владельцем платёж ответом не возвращаем.
	live, _ := b.db.TxRefsByRawID(ctx, pa.rawID)
	if len(live) == 0 {
		if any, _ := b.db.HasAnyTx(ctx, pa.rawID); any {
			b.closeWhoAsk(ctx, chat, pa)
			b.sendText(chat, "Этот платёж исключили из учёта — не записываю.")
			return true
		}
	}
	// Ответ списком («Магомедов Ахмед 20000\nИсаев Харон 7000») — по строке на сумму.
	var lines []string
	for _, ln := range strings.Split(t, "\n") {
		if ln = strings.TrimSpace(ln); ln != "" {
			lines = append(lines, ln)
		}
	}
	if len(lines) == 0 {
		return false
	}
	type pick struct {
		name, canonical string
		cid             int
		amount          float64
	}
	var picks []pick
	for _, ln := range lines {
		if strings.HasPrefix(strings.ToLower(ln), "у ") {
			return false // «у Дени» — это кто забрал деньги, а не клиент
		}
		name, amount, deferToAI := clarifyNameFromReply(ln)
		if deferToAI || name == "" {
			return false
		}
		if len(lines) > 1 && amount <= 0 {
			return false
		}
		if amount > 0 && (pa.amount <= 0 && !swipe && len(live) == 0 || pa.amount > 0 && math.Abs(pa.amount-amount) >= 1) {
			return false // названа другая сумма — это новый платёж, а не ответ
		}
		// Только настоящее ФИО («Мансур знает», «Позже скажу» — не клиент).
		canonical, ok := b.confirmClientName(ctx, name)
		if !ok {
			return false
		}
		cid, err := b.db.GetOrCreateContact(ctx, canonical)
		if err != nil {
			return false
		}
		picks = append(picks, pick{name, canonical, cid, amount})
	}
	if len(picks) > 1 && len(live) == 0 {
		return false
	}
	var done []string
	cash := pa.cash
	usedTx := map[int]bool{}
	for _, pk := range picks {
		amount := pk.amount
		if len(live) > 0 {
			// Платёж уже записан без имени — даём клиента ему (не добавляем второй).
			var cand []db.TxRef
			for _, tx := range live {
				if usedTx[tx.ID] || amount > 0 && math.Abs(tx.Amount-amount) >= 1 {
					continue
				}
				if whoPlaceholders[tx.Name] || len(live) == 1 {
					cand = append(cand, tx)
				}
			}
			if len(cand) > 1 {
				same := true // несколько одинаковых заглушек — берём первую
				for _, c := range cand {
					if math.Abs(c.Amount-cand[0].Amount) >= 1 {
						same = false
					}
				}
				if same && amount > 0 {
					cand = cand[:1]
				}
			}
			if len(cand) != 1 {
				var sums []string
				for _, tx := range live {
					if whoPlaceholders[tx.Name] {
						sums = append(sums, formatMoney(tx.Amount))
					}
				}
				if len(done) == 0 {
					b.sendText(chat, "Какой из платежей ("+strings.Join(sums, ", ")+" ₽)? Напишите «ФИО сумма».")
					return true
				}
				continue
			}
			tx := cand[0]
			usedTx[tx.ID] = true
			if err := b.db.SetTxClientFrom(ctx, tx.ID, pk.cid, pk.canonical, answerRawFrom(ctx)); err != nil {
				return false
			}
			if tx.Cash && b.cashDupCheckOn() {
				_ = b.db.RecheckCashRepeat(ctx, tx.ID) // та же наличка этого клиента за 30 дней — «повтор?»
			}
			cash = tx.Cash
			done = append(done, fmt.Sprintf("%s ₽ — %s", formatMoney(tx.Amount), pk.canonical))
			continue
		}
		if amount <= 0 {
			amount = pa.amount
		}
		if amount <= 0 {
			// В сообщении несколько сумм — перечитываем его с названным клиентом.
			if b.rereadWithClient(ctx, chat, pa, pk.canonical) == 0 {
				b.sendText(chat, "Понял, клиент "+pk.canonical+". А какая сумма? Напишите «ФИО сумма».")
				return true
			}
			now, _ := b.db.TxRefsByRawID(ctx, pa.rawID)
			for _, tx := range now {
				done = append(done, fmt.Sprintf("%s ₽ — %s", formatMoney(tx.Amount), tx.Name))
			}
			continue
		}
		card := ""
		if cash {
			card = "наличные"
		}
		if err := b.db.InsertTransaction(ctx, db.TransactionInput{
			ContactID: pk.cid, RawName: pk.canonical, Amount: amount, CardTo: card, IsCash: cash,
			RawMessageID: pa.rawID, TxDate: pa.txDate, DupCheck: b.cashDupCheckOn(),
			ClientSourceRawID: answerRawFrom(ctx),
		}); err != nil {
			fmt.Println("Ответ «чей платёж»: ошибка записи:", err)
			return false
		}
		done = append(done, fmt.Sprintf("%s ₽ — %s", formatMoney(amount), pk.canonical))
	}
	if len(done) == 0 {
		return false
	}
	_ = b.db.MarkMessageParsed(ctx, pa.rawID)
	b.noteAnswer(ctx, "who", strconv.Itoa(pa.rawID))
	// Вопрос закрываем, когда безымянных платежей в сообщении не осталось.
	open := false
	if now, err := b.db.TxRefsByRawID(ctx, pa.rawID); err == nil {
		for _, tx := range now {
			if whoPlaceholders[tx.Name] {
				open = true
			}
		}
	}
	if !open {
		b.closeWhoAsk(ctx, chat, pa)
	}
	kind := "перевод"
	if cash {
		kind = "наличка"
	}
	b.sendText(chat, fmt.Sprintf("Записал: %s %s.", kind, strings.Join(done, "; ")))
	_ = askID
	return true
}

// closeWhoAsk — на «❓ чей платёж?» ответили (или он больше не нужен).
func (b *Bot) closeWhoAsk(ctx context.Context, chat types.JID, pa paymentAsk) {
	_ = b.db.ClearWhoAsk(ctx, pa.rawID)
	b.clarify.mu.Lock()
	for k, v := range b.clarify.paymentAsks {
		if v.rawID == pa.rawID {
			delete(b.clarify.paymentAsks, k)
		}
	}
	if cur, ok := b.clarify.lastAsk[chat.String()]; ok && cur.kind == "payment_who" && cur.txID == pa.rawID {
		delete(b.clarify.lastAsk, chat.String())
	}
	b.clarify.mu.Unlock()
}

// rereadWithClient — «❓ чей платёж?» по сообщению с несколькими суммами: ИИ
// перечитывает его, зная клиента. Возвращает, сколько платежей записано.
func (b *Bot) rereadWithClient(ctx context.Context, chat types.JID, pa paymentAsk, canonical string) int {
	if b.assistant == nil {
		return 0
	}
	body, sender, err := b.db.RawBodyByID(ctx, pa.rawID)
	if err != nil || strings.TrimSpace(body) == "" {
		return 0
	}
	b.aiRescueUnparsed(ctx, chat, sender, []string{canonical + "\n" + body}, pa.rawID, pa.txDate, pa.cash, false)
	now, _ := b.db.TxRefsByRawID(ctx, pa.rawID)
	return len(now)
}

// registerDupFixAsk — запомнить предупреждение о повторном чеке с другим клиентом.
func (b *Bot) registerDupFixAsk(botMsgID string, a dupFixAsk) {
	if botMsgID == "" || a.checkWa == "" {
		return
	}
	b.clarify.mu.Lock()
	defer b.clarify.mu.Unlock()
	if len(b.clarify.dupFixAsks) >= clarifyMapCap {
		for k := range b.clarify.dupFixAsks {
			delete(b.clarify.dupFixAsks, k)
			break
		}
	}
	b.clarify.dupFixAsks[botMsgID] = a
}

// applyDupFixAnswer — ответ на предупреждение о повторном чеке: «да» — чек на
// клиента из новой подписи, ФИО — на этого клиента, «нет» — оставить как есть.
func (b *Bot) applyDupFixAnswer(ctx context.Context, chat types.JID, askID string, a dupFixAsk, text string) bool {
	low := cleanAnswer(text)
	toks := answerTokens(low)
	keep := func() bool {
		b.clarify.mu.Lock()
		delete(b.clarify.dupFixAsks, askID)
		b.clarify.mu.Unlock()
		b.sendText(chat, "Ок, оставил как было.")
		return true
	}
	// «да нет», «да не, всё правильно», «да, это повтор», «оставь» — оставить как есть.
	if hasNegation(toks) || strings.Contains(low, "повтор") || strings.Contains(low, "тот же") ||
		strings.Contains(low, "оставь") || strings.Contains(low, "оставьте") {
		return keep()
	}
	name := ""
	rest := toks
	if len(rest) > 0 && (yesWords[rest[0]] || ackWords[rest[0]]) {
		rest = rest[1:]
		for len(rest) > 0 && (rest[0] == "это" || rest[0] == "он" || rest[0] == "она" || rest[0] == "верно" ||
			rest[0] == "правильно" || rest[0] == "всё" || rest[0] == "все" || rest[0] == "да" || rest[0] == "так") {
			rest = rest[1:]
		}
		if len(rest) == 0 {
			name = a.name // «да» — клиент из новой подписи
		} else if c, ok := b.confirmClientName(ctx, strings.Join(rest, " ")); ok {
			name = c // «да, это Исаев Харон»
		} else {
			return false // «да …» с чем-то ещё — не ответ на этот вопрос
		}
	} else {
		// Свайп с ФИО — чек на этого клиента.
		c, ok := b.confirmClientName(ctx, strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(text), "это "), "Это ")))
		if !ok {
			return false
		}
		name = c
	}
	canonical, _ := b.aliases.ResolveName(name)
	var cid *int
	if id, err := b.db.GetOrCreateContact(ctx, canonical); err == nil {
		cid = &id
	}
	found, _, err := b.db.ReattributeReceiptByMessageFrom(ctx, a.checkWa, canonical, cid, 0)
	if err != nil || !found {
		return false
	}
	b.clarify.mu.Lock()
	delete(b.clarify.dupFixAsks, askID)
	b.clarify.mu.Unlock()
	b.reresolveWatchByCheck(ctx, chat, a.checkWa, canonical)
	b.sendText(chat, "Поправил: чек теперь на клиента "+canonical+".")
	return true
}
