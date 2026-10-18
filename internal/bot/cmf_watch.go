// Сверка чеков с программой рассрочек (cmf): каждый чек из рабочей группы
// становится "наблюдением". Бот определяет клиента (имя пишут в подписи к
// чеку или отдельным сообщением), ищет его в программе с учётом опечаток,
// при неоднозначности переспрашивает прямо в группе, а затем следит, внесли
// ли платёж в программу — и напоминает, если забыли.
package bot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode"

	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"

	"whatsapp-bot/internal/ai"
	"whatsapp-bot/internal/cmf"
	"whatsapp-bot/internal/db"
	"whatsapp-bot/internal/parser"
)

const settingUnmatchedBranch = "cmf_unmatched_branch"

// cmfRemindAfter — сколько ждать внесения платежа в программу, прежде чем
// напомнить в группу. Настраивается CMF_REMIND_HOURS (по умолчанию 24).
func cmfRemindAfter() time.Duration {
	if s := os.Getenv("CMF_REMIND_HOURS"); s != "" {
		if h, err := strconv.Atoi(s); err == nil && h > 0 {
			return time.Duration(h) * time.Hour
		}
	}
	return 24 * time.Hour
}

// nameStopwords — короткие реплики, которые НЕ являются именем клиента.
var nameStopwords = map[string]bool{
	"ок": true, "окей": true, "да": true, "нет": true, "спасибо": true, "спс": true,
	"привет": true, "хорошо": true, "хор": true, "понял": true, "поняла": true,
	"готово": true, "всё": true, "все": true, "ясно": true, "давай": true, "жду": true,
	"плюс": true, "принял": true, "принято": true, "ладно": true, "ок.": true,
	"верно": true, "точно": true, "правильно": true, "угу": true, "ага": true,
	// служебные слова, часто идущие рядом с оплатой («... за 2 месяца», «оплата»)
	"за": true, "месяц": true, "месяца": true, "месяцев": true, "мес": true,
	"оплата": true, "оплатил": true, "оплатила": true, "оплату": true, "нал": true,
	"наличка": true, "наличкой": true, "наличными": true, "офис": true,
	// местоимения и частые слова, из-за которых фразы типа «Его чек» ошибочно
	// принимались за ФИО клиента
	"его": true, "её": true, "ее": true, "их": true, "он": true, "она": true,
	"оно": true, "они": true, "мы": true, "вы": true, "ты": true, "я": true,
	"это": true, "этот": true, "эта": true, "эти": true, "тот": true, "та": true,
	"те": true, "вот": true, "тут": true, "там": true, "здесь": true,
	"чек": true, "чеки": true, "чека": true, "деньги": true, "сумма": true,
	"перевод": true, "платёж": true, "платеж": true, "текст": true, "без": true,
	"имени": true, "имя": true, "тебе": true, "мне": true, "ему": true, "ей": true,
	"нам": true, "вам": true, "и": true, "в": true, "на": true, "от": true,
	// глаголы-действия и наречия времени, которые работники пишут рядом с чеком
	// («перевёл сегодня», «скинул только что») — это НЕ ФИО клиента.
	"перевёл": true, "перевел": true, "перевела": true, "перевели": true,
	"перевожу": true, "переведено": true, "скинул": true, "скинула": true,
	"скинули": true, "скинь": true, "кинул": true, "кинула": true, "кинули": true,
	"отправил": true, "отправила": true, "отправили": true, "отправлено": true,
	"заплатил": true, "заплатила": true, "заплатили": true, "оплачено": true,
	"плачу": true, "сделал": true, "сделала": true, "сделали": true, "внёс": true,
	"внес": true, "внесла": true, "сдал": true, "сдала": true, "принёс": true,
	"принес": true, "забрал": true, "взял": true, "отдал": true,
	"сегодня": true, "вчера": true, "сейчас": true, "только": true, "что": true,
	"уже": true, "ещё": true, "еще": true, "тоже": true, "также": true,
	// слова-обращения к боту и служебные из фраз-вопросов («Записал чек?»,
	// «клиент X сумма Y дата Z») — чтобы вопрос/предложение не превращались в ФИО
	"записал": true, "записала": true, "записали": true, "запиши": true,
	"записать": true, "запись": true, "запиши-ка": true, "внёс?": true,
	"клиент": true, "клиента": true, "клиенту": true, "клиентом": true,
	"сумму": true, "суммы": true, "суммой": true, "дата": true, "дату": true,
	"даты": true, "датой": true, "число": true, "числа": true, "время": true,
	"секунд": true, "секунда": true, "секунды": true, "минут": true, "минута": true,
	"минуты": true, "час": true, "часов": true, "почему": true, "зачем": true,
	"когда": true, "где": true, "сколько": true, "чей": true, "чья": true,
	"чьё": true, "чье": true, "чьи": true, "как": true, "какой": true, "какая": true,
	"джарвис": true, "бот": true, "проверь": true, "покажи": true, "скажи": true,
	"посчитал": true, "посчитала": true, "посчитай": true, "посчитать": true,
	"считал": true, "считай": true, "посчитано": true, "почему-то": true,
	// частицы/союзы — никогда не имена, часто в фразах-вопросах
	"а": true, "но": true, "ну": true, "же": true, "ли": true, "бы": true,
	"или": true, "то": true, "ни": true, "не": true, "разве": true, "неужели": true,
}

// looksLikeName проверяет, похожа ли строка на ФИО клиента. Требует 2+ слова
// (реальные подписи к чекам — это ФИО: "цихаев саляхь", "Атабаев Турпал"),
// без команд, вопросов, цифр и стоп-слов. Возвращает очищенное имя.
func looksLikeName(text string) (string, bool) {
	name := strings.TrimSpace(text)
	if name == "" || len([]rune(name)) > 60 || strings.ContainsAny(name, "?/\n@") {
		return "", false
	}
	var nameWords []string
	prevNum := false
	for _, w := range strings.Fields(name) {
		// слова с цифрами (суммы "25.000", "20т") в имя не берём
		if strings.IndexFunc(w, func(r rune) bool { return r >= '0' && r <= '9' }) >= 0 {
			prevNum = true
			continue
		}
		// хвост суммы «15 т.р.», «15 тыс.руб», «15000 руб.» — не часть ФИО
		afterNum := prevNum
		prevNum = false
		if parser.IsMoneyWord(w, afterNum) {
			prevNum = afterNum // «15 тыс. руб.» — хвост из нескольких слов
			continue
		}
		// Обрезаем НЕбуквенные края слова: эмодзи/знаки, прилипшие к имени
		// («акъуб✅» -> «акъуб», «Р.» -> «Р»). Слово без букв (✅, стрелка) -> пусто.
		clean := strings.TrimFunc(w, func(r rune) bool { return !unicode.IsLetter(r) })
		if clean == "" {
			continue
		}
		if nameStopwords[strings.ToLower(clean)] {
			continue
		}
		nameWords = append(nameWords, clean)
	}
	// Меньше 2 слов — это, скорее, реплика ("Ок"), а не ФИО клиента.
	if len(nameWords) < 2 || len(nameWords) > 5 {
		return "", false
	}
	return strings.Join(nameWords, " "), true
}

// firstNameLine ищет ФИО клиента в МНОГОСТРОЧНОМ сообщении построчно
// («Магамадов Алха\n22.000₽ ✅» -> «Магамадов Алха»). Нужна, когда рядом с чеком
// пишут имя и сумму/галочку на разных строках, и looksLikeName по всему тексту
// не срабатывает из-за переноса строки.
func firstNameLine(text string) (string, bool) {
	for _, line := range strings.Split(text, "\n") {
		if name, ok := looksLikeName(line); ok {
			return name, true
		}
	}
	return "", false
}

// resolveReceiptPayer определяет ФИО клиента, написанное РЯДОМ с чеком:
// подпись к фото, текст-ответ (свайп на чек) или отдельное имя, присланное
// прямо перед чеком. Это и есть "чей чек" — важнее получателя на чеке.
// Возвращает ФИО клиента и сумму, если она была в подписи/имени рядом (нужна
// для фото наличных: «ФИО+сумма» + фото денег = наличка на эту сумму). Для
// обычного чека сумма берётся с самого чека, и это значение игнорируется.
func (b *Bot) resolveReceiptPayer(ctx context.Context, msg *events.Message, caption string) (string, float64) {
	found := func(name string, amount float64) (string, float64) {
		// Имя этого клиента, написанное ПЕРЕД чеком, уже использовано — снимаем
		// его из очереди, иначе оно достанется следующему чеку другого клиента.
		b.consumeQueuedSameName(ctx, msg.Info.Chat, msg.Info.Sender.String(), name)
		return name, amount
	}
	// 1. Подпись к чеку.
	if name, amount := b.payerFromCaption(ctx, caption); name != "" {
		return found(name, amount)
	}
	// 2. Чек — это ответ (свайп) на сообщение с именем.
	if quoted := extractQuotedText(msg); strings.TrimSpace(quoted) != "" {
		if name := b.cmfExtractClientName(ctx, quoted); name != "" {
			return found(name, parser.ExtractAmount(quoted))
		}
		if name, ok := looksLikeName(quoted); ok {
			return found(name, parser.ExtractAmount(quoted))
		}
	}
	// 3. Имя из очереди «написали перед чеком» берётся уже при разборе чека
	// (takeQueuedPayer), когда известна его сумма: «Ахмед 15000» не достанется
	// чеку на 20000.
	return "", 0
}

// payerFromCaption — ФИО клиента из подписи к фото чека. Подпись-команда боту
// («Джарвис, внеси в программу») — не ФИО клиента.
func (b *Bot) payerFromCaption(ctx context.Context, caption string) (string, float64) {
	if stripped, addressed := b.stripBotName(caption); addressed {
		caption = strings.TrimSpace(strings.TrimLeft(strings.TrimPrefix(strings.TrimPrefix(stripped, "это"), "Это"), " ,:-—"))
	}
	if isCommandText(caption) || strings.TrimSpace(caption) == "" {
		return "", 0
	}
	if name := b.cmfExtractClientName(ctx, caption); name != "" {
		return name, parser.ExtractAmount(caption)
	}
	// ИИ не выделил, но подпись сама похожа на ФИО ("цихаев саляхь").
	if name, ok := looksLikeName(caption); ok {
		return name, parser.ExtractAmount(caption)
	}
	return "", 0
}

// takeQueuedPayer — имя из очереди «написали перед чеком» для чека на эту сумму.
// Возвращает и id сообщения с именем: если его потом удалят или изменят, чек
// узнает об этом.
func (b *Bot) takeQueuedPayer(ctx context.Context, chat types.JID, senderJID string, amount float64) (string, int, bool) {
	if senderJID == "" || b.db == nil {
		return "", 0, false
	}
	p, ok, err := b.db.TakePendingNameFor(ctx, chat.String(), senderJID, time.Now().Add(-b.pairWindow), amount)
	if err != nil {
		fmt.Println("Очередь имён: ошибка выборки:", err)
		return "", 0, false
	}
	return p.Name, p.RawID, ok
}

// consumeQueuedSameName — снять из очереди имя того же клиента (оно уже
// использовано подписью или свайпом).
func (b *Bot) consumeQueuedSameName(ctx context.Context, chat types.JID, senderJID, name string) {
	if b.db == nil || name == "" {
		return
	}
	ps, err := b.db.PendingNamesOf(ctx, chat.String(), senderJID, time.Now().Add(-b.pairWindow))
	if err != nil {
		return
	}
	for _, p := range ps {
		if sameClientName(p.Name, name) || normCyr(p.Name) == normCyr(name) {
			_ = b.db.ConsumePendingNameByID(ctx, p.ID)
			return
		}
	}
}

// enqueuePendingName кладёт имя в durable-очередь (имя ПЕРЕД будущим чеком).
// amount — сумма из того же сообщения (0, если её не было). Порядок — по времени
// сообщения WhatsApp (msg.Info.Timestamp), не по времени обработки.
func (b *Bot) enqueuePendingName(ctx context.Context, msg *events.Message, name string, amount float64, rawID int) {
	if err := b.db.EnqueuePendingName(ctx, msg.Info.Chat.String(), msg.Info.Sender.String(),
		name, amount, rawID, msg.Info.Timestamp); err != nil {
		fmt.Println("Очередь имён: ошибка сохранения:", err)
	}
}

// consumePendingNameCash достаёт из durable-очереди самое РАННЕЕ ждущее имя С
// СУММОЙ от отправителя (для случая «фото наличных пришло после ФИО+сумма»).
// Порядок совпадает с resolveReceiptPayer (FIFO) — чтобы фото денег и приходящие
// следом чеки разбирали очередь в одном порядке, а не крест-накрест.
func (b *Bot) consumePendingNameCash(ctx context.Context, chat types.JID, senderJID string) (string, float64, bool) {
	since := time.Now().Add(-cashLinkWindow)
	name, amount, ok, err := b.db.TakePendingNameWithAmount(ctx, chat.String(), senderJID, since)
	if err != nil {
		fmt.Println("Очередь имён (нал): ошибка выборки:", err)
		return "", 0, false
	}
	return name, amount, ok
}

// handleNameMessage разбирает сообщение-имя (ФИО без чека) и по порядку
// сообщений (FIFO) решает: это имя ПОСЛЕ чека (есть ждущий чек -> привязать)
// или ПЕРЕД чеком (нет -> в очередь). Свайп на конкретный чек имеет приоритет.
// Возвращает true, если сообщение — только имя (обработано). «ФИО + сумма» —
// это ПЛАТЁЖ: он записывается (возвращаем false), а имя заодно ждёт чек НА ТУ ЖЕ
// сумму (текст + чек одного платежа сводятся в отчётах в один).
func (b *Bot) handleNameMessage(ctx context.Context, msg *events.Message, text string, rawID int) bool {
	chat := msg.Info.Chat
	sender := msg.Info.Sender.String()
	quotedID := extractQuotedStanzaID(msg)
	since := time.Now().Add(-b.pairWindow)
	amount := parser.ExtractAmount(text) // сумма из того же сообщения, если была
	amountLines := countAmountLines(text)

	// Есть ли ждущий чек (неподтверждённый чек от отправителя)?
	hasUnconfirmed, _ := b.db.HasUnconfirmedReceiptFrom(ctx, chat.String(), sender, since)
	checkNearby := quotedID != "" || hasUnconfirmed

	// Наличка («Исаев Ислам нал 5000», «Ахмед 10000 у Дени») и список из
	// нескольких платежей — это платежи, а не подпись к чеку. Если рядом ждёт чек,
	// подпишем его строкой с ТОЙ ЖЕ суммой, остальное запишется как платежи.
	// Решаем по строкам с суммой, а не по сумме всего текста: у «Исаев Харон\n
	// 10000\n5000» общая сумма не читается (числа на разных строках), а это всё
	// равно два платежа — подписью к чеку его не съедаем.
	if amountLines > 1 || ((amount > 0 || amountLines > 0) && parser.IsCash(text)) {
		if checkNearby && amountLines > 1 {
			b.labelCheckFromLines(ctx, msg, text, rawID, since)
		}
		return false
	}

	name, ok := looksLikeName(text)
	nameText := text // откуда взято ФИО (показать ИИ для подтверждения)
	if !ok {
		// Рядом с чеком часто пишут ФИО и сумму НА РАЗНЫХ СТРОКАХ
		// («Магамадов Алха\n22.000₽ ✅») — это подпись к чеку (кому засчитать),
		// а не отдельный платёж. Берём ФИО из строки. Без чека рядом не трогаем —
		// пусть парсер разберёт как обычный платёж.
		if !checkNearby {
			return false
		}
		// ФИО платежа — со строки суммы или из заголовка её блока, а не первая
		// строка-имя: в «Магомедов Ахмед\nИсаев Харон 15000» 15000 — Исаева.
		if p, found := amountLinePayer(text); found {
			name, nameText = p.name, p.nameText
		} else if ln, found := firstNameLine(text); found {
			name = ln
		} else {
			return false
		}
	}

	// Уверены ли, что это ВООБЩЕ ФИО человека, а не фраза, случайно прошедшая
	// эвристику («Доброе Утро», «Новый Клиент»)? Уверенно — если совпал известный
	// алиас. Иначе спрашиваем ИИ: он подтвердит/нормализует настоящее ФИО или
	// вернёт пусто для не-имени. Без ассистента доверяем только известным
	// алиасам, остальное НЕ перехватываем (чек останется на проверку).
	if _, known := b.aliases.ResolveName(name); !known {
		confirmed := ""
		if b.assistant != nil {
			confirmed = b.cmfExtractClientName(ctx, nameText)
		}
		if confirmed == "" {
			return false // не уверены, что это имя — пусть идёт дальше (платёж/болтовня)
		}
		name = confirmed
	}

	canonical, _ := b.aliases.ResolveName(name)
	contact := func() *int {
		if cid, err := b.db.GetOrCreateContact(ctx, canonical); err == nil {
			return &cid
		}
		return nil
	}

	// Свайп на конкретное сообщение — имя относится к НЕМУ.
	if quotedID != "" {
		if found, _, err := b.db.ReattributeReceiptByMessageFrom(ctx, quotedID, canonical, contact(), rawID); err == nil && found {
			_ = b.db.MarkMessageParsed(ctx, rawID)
			if !b.reresolveWatchByCheck(ctx, chat, quotedID, canonical) {
				fmt.Printf("Чек переатрибутирован на клиента %q (свайп с ФИО на чек)\n", canonical)
			}
			return true
		}
		// Свайп на текстовый платёж — уточнили ФИО его клиента.
		if amount == 0 {
			if txs, err := b.db.TextTxByMessage(ctx, quotedID); err == nil && len(txs) == 1 {
				if cid := contact(); cid != nil {
					_ = b.db.SetTxClientFrom(ctx, txs[0], *cid, canonical, rawID)
					_ = b.db.MarkMessageParsed(ctx, rawID)
					fmt.Printf("Клиент текстового платежа уточнён свайпом: %q\n", canonical)
					return true
				}
			}
		}
		// Свайп на что-то другое — это не имя для БУДУЩЕГО чека отправителя.
		return false
	}

	if amount > 0 {
		// «ФИО + сумма» рядом с ждущим чеком на ТУ ЖЕ сумму — подпись к нему.
		if hasUnconfirmed {
			if checkAmt, ok, _ := b.db.OldestUnconfirmedReceiptAmount(ctx, chat.String(), sender, since); ok && (checkAmt <= 0 || math.Abs(checkAmt-amount) < 1 || amountLinesMatch(text, checkAmt)) {
				if found, _, checkRaw, err := b.db.ReattributeOldestUnconfirmedReceiptFrom(ctx, chat.String(), sender, since, canonical, contact(), rawID, 0); err == nil && found {
					_ = b.db.MarkMessageParsed(ctx, rawID)
					b.syncWatchAfterLabel(ctx, chat, checkRaw, canonical)
					fmt.Printf("Чек переатрибутирован на клиента %q (ФИО+сумма рядом с чеком)\n", canonical)
					return true
				}
			}
		}
		// Иначе это платёж: запишется как текстовый, а имя подождёт чек на эту сумму.
		b.enqueuePendingName(ctx, msg, name, amount, rawID)
		return false
	}

	if !checkNearby {
		// Чека рядом нет — имя (перед чеком) в очередь.
		b.enqueuePendingName(ctx, msg, name, 0, rawID)
		_ = b.db.MarkMessageParsed(ctx, rawID)
		return true
	}

	// Имя без свайпа: самый старый неподтверждённый чек от отправителя
	// (FIFO по порядку чата).
	found, _, checkRaw, err := b.db.ReattributeOldestUnconfirmedReceiptFrom(ctx, chat.String(), sender, since, canonical, contact(), rawID, 0)
	if err != nil || !found {
		b.enqueuePendingName(ctx, msg, name, 0, rawID)
		_ = b.db.MarkMessageParsed(ctx, rawID)
		return true
	}
	_ = b.db.MarkMessageParsed(ctx, rawID)
	fmt.Printf("Чек переатрибутирован на клиента %q (ФИО написали рядом с чеком)\n", canonical)
	b.syncWatchAfterLabel(ctx, chat, checkRaw, canonical)
	return true
}

// countAmountLines — сколько строк сообщения содержат сумму (список платежей).
func countAmountLines(text string) int {
	n := 0
	for _, ln := range strings.Split(text, "\n") {
		if parser.ExtractAmount(ln) > 0 {
			n++
		}
	}
	return n
}

// nearAmount — суммы совпадают (с точностью до рубля).
func nearAmount(a, b float64) bool { return a > 0 && b > 0 && math.Abs(a-b) < 1 }

// textHasAmount — есть ли в строке платёж на сумму amt: сумма, которую читает
// разбор платежей («Исаев Харон за 2 месяца 30000» — 30000, а не первое число
// 2), или первая сумма строки (ExtractAmount: «22т», «22.000»).
func textHasAmount(text string, amt float64) bool {
	if nearAmount(parser.ExtractAmount(text), amt) {
		return true
	}
	for _, tr := range parser.ParseMessage(text).Transactions {
		if nearAmount(tr.Amount, amt) {
			return true
		}
	}
	return false
}

// amountLinesMatch — у каждой строки текста с суммой есть платёж на сумму amt
// (и такая строка есть): «ФИО сумма» называет только чек на ТУ ЖЕ сумму.
func amountLinesMatch(text string, amt float64) bool {
	found := false
	for _, ln := range strings.Split(text, "\n") {
		if parser.ExtractAmount(ln) <= 0 {
			continue
		}
		if !textHasAmount(ln, amt) {
			return false
		}
		found = true
	}
	return found
}

// nonEmptyLines — непустые строки сообщения (как их видит разбор платежей).
func nonEmptyLines(text string) []string {
	var out []string
	for _, ln := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		if ln = strings.TrimSpace(ln); ln != "" {
			out = append(out, ln)
		}
	}
	return out
}

// cashMarkOnly — в строке, кроме пометки налички («Наличка:», «нал», «у Дени»,
// «сдал в офис»), нет ФИО: это пометка к платежам вокруг, а не заголовок блока
// клиента («Исаев Харон у Дени»).
func cashMarkOnly(line string) bool {
	words := strings.Fields(line)
	var rest []string
	for i := 0; i < len(words); i++ {
		w := strings.ToLower(strings.TrimFunc(words[i], func(r rune) bool { return !unicode.IsLetter(r) }))
		switch {
		case w == "у" || (w == "на" || w == "из") && i+1 < len(words) && strings.HasPrefix(strings.ToLower(words[i+1]), "рук"):
			i++ // «у Дени», «на руки», «из рук»
		case strings.HasPrefix(w, "налич"), strings.HasPrefix(w, "кэш"), strings.HasPrefix(w, "офис"),
			w == "нал", w == "налом", w == "наликом", w == "нальком", w == "кеш", w == "cash", w == "живыми":
		default:
			rest = append(rest, words[i])
		}
	}
	_, isName := looksLikeName(strings.Join(rest, " "))
	return !isName
}

// paymentsCash — платежи сообщения (как их читает парсер) и какие из них
// наличные. Кроме пометки в строке самого платежа и в заголовке его блока
// («Исаев Харон у Дени»), учитываем пометки отдельной строкой: «Наличка» над
// блоком и «наличка» последней строкой. Парсер такие пометки теряет (заголовок —
// при смене ФИО, последнюю строку — если платежей больше одного), а модель
// читает их как наличку всего блока. Где граница пометки неясна — считаем
// наличкой: такой платёж чек не называет (бот спросит «чей чек?»), а не
// посчитает его второй раз рядом с наличкой.
func paymentsCash(text string) ([]parser.Transaction, []bool) {
	lines := nonEmptyLines(text)
	trs := parser.ParseMessage(strings.Join(lines, "\n")).Transactions
	// Какой платёж из какой строки: парсер идёт по строкам по порядку и из
	// строки берёт не больше одного платежа.
	txAt := make([]int, len(lines))
	prev := 0
	for i := range lines {
		txAt[i] = -1
		if n := len(parser.ParseMessage(strings.Join(lines[:i+1], "\n")).Transactions); n > prev {
			txAt[i], prev = prev, n
		}
	}
	cash := make([]bool, len(trs))
	for k, tr := range trs {
		cash[k] = tr.IsCash || parser.IsCash(tr.Line) || parser.IsCash(tr.RawName)
	}
	markBelow := func(i int, wholeBlockOnly bool) {
		seen := false
		for j := i + 1; j < len(lines); j++ {
			if k := txAt[j]; k >= 0 && k < len(cash) {
				seen, cash[k] = true, true
				continue
			}
			if wholeBlockOnly && seen && !parser.IsCash(lines[j]) {
				return // новый блок другого клиента
			}
		}
	}
	for i, ln := range lines {
		if txAt[i] >= 0 || !parser.IsCash(ln) {
			continue
		}
		below := false
		for j := i + 1; j < len(lines) && !below; j++ {
			below = txAt[j] >= 0
		}
		switch {
		case !below || cashMarkOnly(ln) && !strings.HasSuffix(ln, ":"):
			// «наличка» в конце или посреди сообщения: это хвост блока выше или
			// заголовок ниже — не угадываем, наличка всё сообщение.
			for k := range cash {
				cash[k] = true
			}
		case cashMarkOnly(ln):
			markBelow(i, false) // «Наличка:» — заголовок: всё, что ниже
		default:
			markBelow(i, true) // «Исаев Харон у Дени» — его блок
		}
	}
	return trs, cash
}

// checkNameHit — безналичный платёж на сумму чека, чьё ФИО может назвать чек.
type checkNameHit struct {
	name     string // ФИО
	nameText string // откуда оно взято (его можно показать ИИ для подтверждения)
	n        int    // сколько таких платежей этого ФИО в сообщении (столько чеков оно называет)
}

// nameForCheckAmount ищет в сообщении НЕналичный платёж на сумму amt — строку
// «ФИО сумма» или блок «ФИО\nсумма\nсумма» (формат парсера) — и возвращает его
// ФИО. seen — платёж на эту сумму в сообщении вообще есть. ok=false — такого
// платежа с ФИО нет (он наличный или без ФИО) или таких платежей у РАЗНЫХ людей
// несколько (не угадываем — спросим «чей чек?»).
func nameForCheckAmount(text string, amt float64) (hit checkNameHit, seen, ok bool) {
	if amt <= 0 {
		return hit, false, false
	}
	trs, cash := paymentsCash(text)
	for k, tr := range trs {
		if !nearAmount(tr.Amount, amt) && !nearAmount(parser.ExtractAmount(tr.Line), amt) {
			continue
		}
		seen = true
		if cash[k] {
			continue
		}
		// ФИО — из самой строки платежа («Магомедов Ахмед 15000») или из
		// заголовка блока («Исаев Харон» над строкой «10000»).
		n, src, found := "", "", false
		if n, found = looksLikeName(tr.RawName); found {
			src = tr.RawName
		} else if n, found = looksLikeName(tr.Line); found {
			src = tr.Line
		}
		if !found {
			continue
		}
		if ok && normCyr(n) != normCyr(hit.name) {
			return checkNameHit{}, true, false
		}
		if !ok {
			hit, ok = checkNameHit{name: n, nameText: src}, true
		}
		hit.n++
	}
	return hit, seen, ok
}

// amountLinePayer — ФИО платежа в сообщении с ОДНОЙ строкой суммы: из самой
// строки («Магомедов Ахмед\nИсаев Харон 15000» — Исаев Харон) или из
// заголовка её блока («Магамадов Алха\n22.000₽ ✅»), как читает парсер.
func amountLinePayer(text string) (checkNameHit, bool) {
	if countAmountLines(text) != 1 {
		return checkNameHit{}, false
	}
	trs := parser.ParseMessage(text).Transactions
	if len(trs) != 1 {
		return checkNameHit{}, false
	}
	hit, _, ok := nameForCheckAmount(text, trs[0].Amount)
	return hit, ok
}

// checkNameSource — из какого текста соседнего сообщения можно взять ФИО
// клиента чека на сумму checkAmt. Правило то же, что у handleNameMessage и
// очереди имён: «ФИО сумма» называет только чек на ТУ ЖЕ сумму; наличка чек не
// называет (это другой платёж); в многострочном тексте ФИО берётся у платежа с
// суммой чека (его строка или заголовок блока «ФИО\nсумма»), а не с первой
// строки-имени. Сообщение без суммы называет чек любой суммы. exact — сумма в
// сообщении совпала с суммой чека; n — сколько чеков на эту сумму это ФИО
// называет (по числу таких платежей в тексте).
func checkNameSource(text string, checkAmt float64) (src string, exact bool, n int, ok bool) {
	lines := countAmountLines(text)
	if lines > 1 {
		if hit, _, found := nameForCheckAmount(text, checkAmt); found {
			return hit.nameText, true, hit.n, true
		}
		return "", false, 0, false
	}
	if parser.IsCash(text) {
		return "", false, 0, false
	}
	if lines == 0 {
		return text, false, 0, true
	}
	if !amountLinesMatch(text, checkAmt) {
		return "", false, 0, false
	}
	if len(nonEmptyLines(text)) > 1 {
		hit, seen, found := nameForCheckAmount(text, checkAmt)
		if found {
			return hit.nameText, true, 1, true
		}
		if seen {
			return "", false, 0, false // платёж на сумму чека есть, но без ФИО
		}
	}
	return text, true, 1, true
}

// isCardOwner — это ФИО владельца карты (получателя, напечатанного на чеке):
// он не клиент этого чека.
func isCardOwner(name, cardOwner string) bool {
	return cardOwner != "" && strings.EqualFold(strings.TrimSpace(name), strings.TrimSpace(cardOwner))
}

// labelCheckFromLines — в сообщении-списке платежей («Магомедов Ахмед 15000\n
// Исаев Дени 20000 нал», «Исаев Харон\n10000\n5000 нал») ищем платёж на сумму
// чека и подписываем им чек: свайп — тот чек, на который ответили; иначе —
// самый старый ждущий чек отправителя.
func (b *Bot) labelCheckFromLines(ctx context.Context, msg *events.Message, text string, rawID int, since time.Time) {
	chat, sender := msg.Info.Chat, msg.Info.Sender.String()
	// ФИО со строки — клиент, только если он известен или его подтвердил ИИ
	// (заголовок «Карта Хадижат», «Карта Сбер» — не клиент), и это не владелец
	// карты, напечатанный на чеке. Иначе чек не подписываем — бот спросит.
	client := func(nameText, cardOwner string) (string, *int, bool) {
		canonical, ok := b.confirmClientName(ctx, nameText)
		if !ok || isCardOwner(canonical, cardOwner) {
			return "", nil, false
		}
		if id, err := b.db.GetOrCreateContact(ctx, canonical); err == nil {
			return canonical, &id, true
		}
		return canonical, nil, true
	}
	if quotedID := extractQuotedStanzaID(msg); quotedID != "" {
		// Свайп на чек: подписываем ЕГО строкой на его сумму. Свайп не на чек —
		// чужим ждущим чекам список не подпись.
		r, err := b.db.ReceiptByWaMessageID(ctx, quotedID)
		if err != nil || r.ID == 0 {
			return
		}
		hit, _, ok := nameForCheckAmount(text, r.Amount)
		if !ok {
			return
		}
		owner, _ := b.db.ReceiptCardOwnerByMessage(ctx, quotedID)
		canonical, cid, ok := client(hit.nameText, owner)
		if !ok {
			return
		}
		if found, _, err := b.db.ReattributeReceiptByMessageFrom(ctx, quotedID, canonical, cid, rawID); err == nil && found {
			b.reresolveWatchByCheck(ctx, chat, quotedID, canonical)
		}
		return
	}
	checkAmt, owner, ok, _ := b.db.OldestUnconfirmedReceiptCard(ctx, chat.String(), sender, since)
	if !ok || checkAmt <= 0 {
		return
	}
	hit, _, ok := nameForCheckAmount(text, checkAmt)
	if !ok {
		return
	}
	canonical, cid, ok := client(hit.nameText, owner)
	if !ok {
		return
	}
	if found, _, checkRaw, err := b.db.ReattributeOldestUnconfirmedReceiptFrom(ctx, chat.String(), sender, since, canonical, cid, rawID, checkAmt); err == nil && found {
		b.syncWatchAfterLabel(ctx, chat, checkRaw, canonical)
	}
}

// syncWatchAfterLabel — клиента чека назвали отдельным сообщением: сверка с
// программой по ЭТОМУ чеку тоже узнаёт клиента.
func (b *Bot) syncWatchAfterLabel(ctx context.Context, chat types.JID, checkRawID int, canonical string) {
	if b.cmf == nil || checkRawID == 0 {
		return
	}
	wid, ok, err := b.db.CmfWatchByRawMessage(ctx, checkRawID)
	if err != nil {
		return
	}
	if !ok {
		b.ensureMissingWatchesAsync() // чек стал засчитанным только сейчас
		return
	}
	w, ok, err := b.db.CmfWatchByID(ctx, wid)
	if err != nil || !ok || w.Status == "found" || w.Status == "deleted" {
		return
	}
	_ = b.db.UpdateCmfWatch(ctx, wid, canonical, "", "", "", "lookup")
	go b.cmfResolveWatch(context.Background(), wid, chat, canonical, w.Amount)
}

// ensureMissingWatches — засчитанным чекам без сверки заводим наблюдение.
func (b *Bot) ensureMissingWatches(ctx context.Context) {
	if b.cmf == nil || b.db == nil {
		return
	}
	if !b.ensureMu.TryLock() {
		return // уже идёт
	}
	defer b.ensureMu.Unlock()
	ms, err := b.db.ReceiptsWithoutWatch(ctx, time.Now().AddDate(0, 0, -3), 50)
	if err != nil {
		return
	}
	for _, m := range ms {
		jid, err := types.ParseJID(m.GroupJID)
		if err != nil {
			continue
		}
		b.cmfWatchReceipt(ctx, jid, m.SenderJID, m.Client, m.Amount, m.TxDate, m.RawID)
	}
}

// ensureMissingWatchesAsync — то же в фоне (из разбора сообщения не ждём программу).
func (b *Bot) ensureMissingWatchesAsync() {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		b.ensureMissingWatches(ctx)
	}()
}

// repairNonameWatches — сверка «без клиента», хотя клиента чека уже назвали
// (ответ на «чей чек?» до этого обновления не доходил до сверки): узнаём клиента.
func (b *Bot) repairNonameWatches(ctx context.Context) {
	ws, err := b.db.NonameWatchesWithClient(ctx, 30)
	if err != nil {
		return
	}
	for _, w := range ws {
		if ctx.Err() != nil {
			return
		}
		jid, err := types.ParseJID(w.GroupJID)
		if err != nil {
			continue
		}
		_ = b.db.UpdateCmfWatch(ctx, w.ID, w.ClientText, "", "", "", "lookup")
		b.cmfResolveWatch(ctx, w.ID, jid, w.ClientText, w.Amount)
		if b.cmfDown() {
			return
		}
	}
}

// syncWatchByCheckWa — клиента чека назвали ответом на «чей чек?» или правкой:
// сверка по этому чеку ищет его в программе (раньше оставалась «без имени»
// навсегда и не напоминала).
func (b *Bot) syncWatchByCheckWa(ctx context.Context, chat types.JID, checkWa, canonical string) {
	b.syncWatchByCheck(ctx, chat, checkWa, canonical, true)
}

// syncWatchByCheckNow — то же, но наблюдение заводится сразу (для команд
// ассистента: ответ владельцу и так идёт после них).
func (b *Bot) syncWatchByCheckNow(ctx context.Context, chat types.JID, checkWa, canonical string) {
	b.syncWatchByCheck(ctx, chat, checkWa, canonical, false)
}

func (b *Bot) syncWatchByCheck(ctx context.Context, chat types.JID, checkWa, canonical string, async bool) {
	if b.cmf == nil || checkWa == "" || canonical == "" {
		return
	}
	// Чек стал полным только сейчас (клиента назвали по чеку без получателя,
	// «это не дубль») — наблюдения за ним ещё нет: заводим.
	if info, ok, err := b.db.ReceiptWatchInfoByMessage(ctx, checkWa); err == nil && ok && !info.HasWatch && info.Countable {
		if copyOf, _ := b.db.IsGroupForwardCopy(ctx, info.RawID); !copyOf {
			run := func() {
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
				defer cancel()
				b.cmfWatchReceipt(ctx, chat, info.SenderJID, canonical, info.Amount, info.TxDate, info.RawID)
			}
			if async {
				go run() // поиск в программе — в фоне, разбор сообщений его не ждёт
			} else {
				run()
			}
			return
		}
	}
	if ref, err := b.db.RawMessageByWaID(ctx, checkWa); err == nil && ref.Found && !ref.Deleted {
		b.syncWatchAfterLabel(ctx, chat, ref.ID, canonical)
	}
}

// extractQuotedStanzaID возвращает id сообщения, на которое ответили (свайп).
func extractQuotedStanzaID(msg *events.Message) string {
	if ci := contextInfoOf(msg); ci != nil {
		return ci.GetStanzaID()
	}
	return ""
}

// contextInfoOf — сведения об ответе (свайпе) у любого вида сообщения: текст,
// фото, документ (PDF-чек), видео. Чек часто присылают ОТВЕТОМ на сообщение с
// именем — у фото/документа цитата лежит в их собственном ContextInfo.
func contextInfoOf(msg *events.Message) *waProto.ContextInfo {
	m := msg.Message
	if m == nil {
		return nil
	}
	if e := m.GetExtendedTextMessage(); e != nil && e.GetContextInfo() != nil {
		return e.GetContextInfo()
	}
	if i := m.GetImageMessage(); i != nil && i.GetContextInfo() != nil {
		return i.GetContextInfo()
	}
	if d := m.GetDocumentMessage(); d != nil && d.GetContextInfo() != nil {
		return d.GetContextInfo()
	}
	if dc := m.GetDocumentWithCaptionMessage(); dc != nil {
		if d := dc.GetMessage().GetDocumentMessage(); d != nil && d.GetContextInfo() != nil {
			return d.GetContextInfo()
		}
	}
	if v := m.GetVideoMessage(); v != nil && v.GetContextInfo() != nil {
		return v.GetContextInfo()
	}
	return nil
}

// cmfWatchReceipt заводит наблюдение сверки за уже разобранным чеком (с учётом
// вижна) и сразу пытается сопоставить клиента. clientText — ФИО, написанное
// рядом с чеком; если пусто, берётся получатель с чека.
func (b *Bot) cmfWatchReceipt(ctx context.Context, chat types.JID, senderJID, clientText string, amount float64, txDate time.Time, rawID int) {
	if b.cmf == nil || amount == 0 {
		return
	}
	status := "noname"
	if clientText != "" {
		status = "lookup"
	}
	watchID, err := b.db.InsertCmfWatch(ctx, rawID, chat.String(), senderJID, clientText, amount, txDate, status)
	if errors.Is(err, db.ErrWatchExists) {
		return
	}
	if err != nil {
		fmt.Println("cmf: не удалось создать наблюдение:", err)
		return
	}
	if clientText != "" {
		b.cmfResolveWatch(ctx, watchID, chat, clientText, amount)
	}
}

// extractQuotedText возвращает текст сообщения, на которое ответили (свайп).
func extractQuotedText(msg *events.Message) string {
	ci := contextInfoOf(msg)
	if ci == nil {
		return ""
	}
	q := ci.GetQuotedMessage()
	if q == nil {
		return ""
	}
	if c := q.GetConversation(); c != "" {
		return c
	}
	if e := q.GetExtendedTextMessage(); e != nil {
		return e.GetText()
	}
	if img := q.GetImageMessage(); img != nil {
		return img.GetCaption()
	}
	return ""
}

// cmfResolveWatch ищет клиента в программе по имени. Точное совпадение всей
// строки -> привязываем и следим за платежом. Нечёткое (по словам, при опечатке)
// НЕ привязываем вслепую — иначе напоминание/«внесён» уйдёт на, возможно, не
// того клиента (тёзку/однофамильца): спрашиваем в группе ответом на сам чек
// («за кого этот платёж?»), ответ понимает applyCmfWatchAnswer.
func (b *Bot) cmfResolveWatch(ctx context.Context, watchID int, chat types.JID, clientText string, amount float64) {
	// Поиск в программе — не дольше минуты (программа может висеть).
	lctx, cancel := context.WithTimeout(ctx, time.Minute)
	clients, kind, err := b.cmfLookupWithTypos(lctx, clientText)
	cancel()
	b.cmfNoteResult(err, chat)
	if err != nil {
		// Программа не ответила — оставляем «lookup»: фоновая проверка повторит.
		fmt.Println("cmf lookup:", err)
		_ = b.db.TouchCmfWatch(context.Background(), watchID)
		return
	}
	if kind == cmfExact || kind == cmfStrong {
		// Точное ИЛИ уверенное нечёткое (опечатка/склонение, но кандидат явно один)
		// — привязываем к клиенту и ждём его платёж в программе. Это и есть «как
		// человек предположить»: «Каталова»/«Котолов» → «Ахмед Каталов».
		_ = b.db.UpdateCmfWatch(ctx, watchID, "", clients[0].ID, clients[0].FullName, "", "watch")
		b.clearOpenAskFor(chat.String(), watchID)
		fmt.Printf("cmf: чек на %.0f ₽ привязан к клиенту %s (совпадение: %v), ждём платёж\n", amount, clients[0].FullName, kind)
		return
	}
	w, wok, _ := b.db.CmfWatchByID(ctx, watchID)
	if wok && (w.Deleted || w.Status == "deleted") {
		return
	}
	// Плательщик с чека раньше уже платил за конкретного клиента (ответили в
	// группе) — как человек, помним, но коротко говорим, чтобы можно было
	// поправить. Только если среди найденных нет его самого.
	if wok {
		if c, ok := b.payerFor(ctx, clientText, clients); ok {
			b.bindCmfWatch(ctx, chat, w, c, false)
			b.cmfSay(ctx, chat, w, fmt.Sprintf("✅ Чек на %s — за «%s», как в прошлый раз. Если не так — ответьте на это сообщение ФИО клиента.", rub0(amount), c.FullName))
			return
		}
	}
	waMsgID, senderJID := "", ""
	if wok {
		waMsgID, senderJID = w.WaMessageID, w.SenderJID
	}
	if kind == cmfNoMatch {
		_ = b.db.UpdateCmfWatch(ctx, watchID, "", "", "", "", "unmatched")
	} else {
		_ = b.db.UpdateCmfWatch(ctx, watchID, "", "", "", "", "ambiguous")
	}
	text, opts := cmfAskText(clientText, amount, clients)
	if kind == cmfNoMatch {
		if branch, _ := b.db.SettingGet(ctx, settingUnmatchedBranch); branch != "" {
			text += " Пока отнёс к точке «" + branch + "»."
		}
	}
	b.askCmfWatch(ctx, chat, watchID, waMsgID, senderJID, text, opts)
}

// cmfWatcherLoop — фоновая сверка: раз в полчаса проверяет наблюдения старше
// CMF_REMIND_HOURS — внесён ли платёж в программу; если нет, напоминает в группу.
func (b *Bot) cmfWatcherLoop() {
	if b.cmf == nil {
		return
	}
	run := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		b.cmfCheckDue(ctx)
	}
	time.Sleep(2 * time.Minute) // после запуска не ждём полчаса
	run()
	ticker := time.NewTicker(30 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		run()
	}
}

// cmfRetryLookups — чеки, по которым программа не ответила при поиске клиента
// (статус lookup, старше 10 минут), ищем ещё раз: самые давно проверенные
// первыми, до 30 дней (раньше через 3 суток чек бросался навсегда).
func (b *Bot) cmfRetryLookups(ctx context.Context) { b.cmfRetryLookupsN(ctx, 20) }

// cmfRetryLookupsAll — связь вернулась: перепроверить все отложенные.
func (b *Bot) cmfRetryLookupsAll(ctx context.Context) { b.cmfRetryLookupsN(ctx, 300) }

func (b *Bot) cmfRetryLookupsN(ctx context.Context, limit int) {
	ws, err := b.db.LookupWatchesForRetry(ctx, limit)
	if err != nil {
		return
	}
	for _, w := range ws {
		if ctx.Err() != nil {
			return
		}
		if w.ClientText == "" || time.Since(w.CreatedAt) < 10*time.Minute || time.Since(w.CreatedAt) > 30*24*time.Hour {
			continue
		}
		jid, err := types.ParseJID(w.GroupJID)
		if err != nil {
			continue
		}
		b.cmfResolveWatch(ctx, w.ID, jid, w.ClientText, w.Amount)
		if b.cmfDown() {
			return // программа всё ещё не отвечает — остальные подождут
		}
	}
}

func (b *Bot) cmfCheckDue(ctx context.Context) {
	b.ensureMissingWatches(ctx)
	b.repairNonameWatches(ctx)
	b.cmfRetryLookups(ctx)
	due, err := b.db.DueCmfWatches(ctx, time.Now().Add(-cmfRemindAfter()), 30)
	if err != nil {
		fmt.Println("cmf: ошибка выборки наблюдений:", err)
		return
	}
	// Записи после обращений к программе — со своим сроком: если общий срок
	// прохода истёк, отправленное напоминание всё равно должно запомниться.
	wctx, wcancel := context.WithTimeout(context.Background(), time.Minute)
	defer wcancel()
	// Собираем неотмеченные по ГРУППЕ, чтобы послать ОДНО напоминание со списком
	// клиентов, а не по сообщению на каждый чек (меньше шума, как просил владелец).
	type rem struct {
		id     int
		client string
		amount float64
		date   time.Time
		check  bool   // похожая оплата есть, но сверка не уверена — «проверьте»
		group  string // группа чека (для напоминания владельцу в личку)
	}
	byGroup := map[string][]rem{}
	var order []string
	// Судим чеки клиента вместе, как сверка: одна оплата — один чек.
	verdicts := b.judgeClientWatches(ctx, due)
	// Копии одного чека (в группе СБ и в основной) — одно напоминание.
	type itemKey struct {
		client string
		item   int
	}
	// Напоминание — в группе, где боту можно писать (копия в заглушённой группе
	// напоминание не «съедает»).
	primary := map[itemKey]int{}
	primarySilent := map[itemKey]bool{}
	for _, w := range due {
		v, ok := verdicts[w.ID]
		if !ok {
			continue
		}
		k := itemKey{w.ClientID, v.item}
		silent := true
		if jid, err := types.ParseJID(w.GroupJID); err == nil {
			silent = b.groupSilent(jid)
		}
		p, seen := primary[k]
		if !seen || primarySilent[k] && !silent || primarySilent[k] == silent && w.ID < p {
			primary[k], primarySilent[k] = w.ID, silent
		}
	}
	var copies []int // копии напомненного чека — помечаем вместе с ним
	for _, w := range due {
		needCheck := false
		if v, ok := verdicts[w.ID]; ok {
			if v.v.entered() {
				_ = b.db.UpdateCmfWatch(wctx, w.ID, "", "", "", "", "found")
				fmt.Printf("cmf: платёж %s на %.0f ₽ найден в программе\n", w.ClientName, w.Amount)
				continue
			}
			needCheck = v.v.needsCheck()
			if primary[itemKey{w.ClientID, v.item}] != w.ID {
				copies = append(copies, w.ID) // тот же чек в другой группе: напоминание по первому
				continue
			}
		} else {
			gj, _ := types.ParseJID(w.GroupJID)
			found, err := b.cmf.HasPaymentAround(ctx, w.ClientID, w.Amount, w.TxDate, 5)
			b.cmfNoteResult(err, gj)
			if err != nil {
				fmt.Println("cmf: ошибка проверки платежа:", err)
				// Отметка «проверяли»: очередь идёт дальше, не застревая на нём.
				_ = b.db.TouchCmfWatch(wctx, w.ID)
				continue
			}
			if found {
				_ = b.db.UpdateCmfWatch(wctx, w.ID, "", "", "", "", "found")
				fmt.Printf("cmf: платёж %s на %.0f ₽ найден в программе\n", w.ClientName, w.Amount)
				continue
			}
		}
		// Группа чека заглушена — напоминаем в группе копии, а если её нет —
		// владельцу в личку (иначе напоминание пропало бы навсегда).
		target := w.GroupJID
		if jid, err := types.ParseJID(target); err == nil && b.groupSilent(jid) {
			target = "dm"
			if gs, err := b.db.ForwardCopyGroups(ctx, w.WaMessageID); err == nil {
				for _, g := range gs {
					if gj, err := types.ParseJID(g); err == nil && !b.groupSilent(gj) {
						target = g
						break
					}
				}
			}
		}
		if _, ok := byGroup[target]; !ok {
			order = append(order, target)
		}
		byGroup[target] = append(byGroup[target], rem{w.ID, w.ClientName, w.Amount, w.TxDate, needCheck, w.GroupJID})
	}
	for _, id := range copies {
		_ = b.db.UpdateCmfWatch(wctx, id, "", "", "", "", "reminded")
	}

	groupNames := map[types.JID]string{}
	if len(byGroup["dm"]) > 0 {
		groupNames = b.joinedGroups(ctx)
	}
	for _, gj := range order {
		items := byGroup[gj]
		markAll := func(askID string) {
			for _, it := range items {
				_ = b.db.UpdateCmfWatch(wctx, it.id, "", "", "", "", "reminded")
				if askID != "" {
					_ = b.db.SetCmfWatchAsk(wctx, it.id, askID, "")
				}
			}
		}
		if gj == "dm" {
			var sb strings.Builder
			sb.WriteString("⏰ В программе пока НЕ отмечены (в группе я молчу, поэтому пишу вам):")
			for _, it := range items {
				g := ""
				if jid, err := types.ParseJID(it.group); err == nil && groupNames[jid] != "" {
					g = " — группа «" + groupNames[jid] + "»"
				}
				fmt.Fprintf(&sb, "\n• %s — %.0f ₽ (чек %s)%s", it.client, it.amount, it.date.Format("02.01"), g)
			}
			if b.notifyOwners(sb.String()) {
				markAll("")
			} // владельца нет — чек ждёт, пока в группе снова можно писать
			continue
		}
		jid, err := types.ParseJID(gj)
		if err != nil {
			continue
		}
		var sb strings.Builder
		if len(items) == 1 {
			it := items[0]
			if it.check {
				fmt.Fprintf(&sb, "⏰ Проверьте в программе: чек от %s на %.0f ₽ — клиент %s. Похожая оплата там есть, но не уверен, что она за этот чек.",
					it.date.Format("02.01"), it.amount, it.client)
			} else {
				fmt.Fprintf(&sb, "⏰ Занесите, пожалуйста, оплату в программу: чек от %s на %.0f ₽ — клиент %s (в рассрочке пока НЕ отмечен).",
					it.date.Format("02.01"), it.amount, it.client)
			}
			// Ответом на сам чек и со связью: поправку («это не он») поймём по этому чеку.
			if w, ok, err := b.db.CmfWatchByID(wctx, it.id); err == nil && ok && w.GroupJID == gj {
				markAll("")
				b.cmfSay(wctx, jid, w, sb.String())
				continue
			}
		} else {
			sb.WriteString("⏰ Занесите, пожалуйста, оплаты этих клиентов в программу (чеки пришли, но в рассрочке пока НЕ отмечены):")
			for _, it := range items {
				note := ""
				if it.check {
					note = " — похожая оплата есть, проверьте"
				}
				fmt.Fprintf(&sb, "\n• %s — %.0f ₽ (чек %s)%s", it.client, it.amount, it.date.Format("02.01"), note)
			}
		}
		markAll(b.sendTextReturnID(jid, sb.String()))
	}

	// Уже напомненные: внесли с тех пор — закрываем (иначе «внеси в программу»
	// потом брал бы их дату). Понемногу за проход, самые давно проверенные.
	if reminded, err := b.db.RemindedCmfWatches(ctx, time.Now().AddDate(0, 0, -14), 10); err == nil && len(reminded) > 0 {
		vs := b.judgeClientWatches(ctx, reminded)
		for _, w := range reminded {
			if v, ok := vs[w.ID]; ok && v.v.entered() {
				_ = b.db.UpdateCmfWatch(wctx, w.ID, "", "", "", "", "found")
			} else {
				_ = b.db.TouchCmfWatch(wctx, w.ID)
			}
		}
	}
}

// cmfExtractClientName вытаскивает имя плательщика из подписи к чеку через ИИ.
func (b *Bot) cmfExtractClientName(ctx context.Context, caption string) string {
	if b.assistant == nil {
		// Без ИИ не выделяем имя из подписи наугад: вернуть всю подпись
		// («спасибо», «с карты Пияна перевёл») означало бы записать мусор в
		// клиенты и затереть настоящего получателя. Пусть решает looksLikeName
		// у вызывающего — он требует правдоподобное ФИО.
		return ""
	}
	system := "Из подписи к банковскому чеку выдели имя КЛИЕНТА, который сделал платёж по своей рассрочке. " +
		"Внимание: в подписи могут упоминаться посторонние имена (чья карта использовалась, кто переслал) — " +
		"нужен именно плательщик рассрочки. Пример: 'с карты Пияна Ахмед сделал оплату своей рассрочки' -> Ахмед. " +
		"'Саралиева Милана' -> Саралиева Милана. " +
		"Имя приведи в именительный падеж (кто?): 'брат Догаева Магомеда скинул' -> Догаев Магомед. " +
		"Суммы и лишние слова не включай. Верни СТРОГО JSON {\"client\":\"имя или пусто\"}."
	out, err := b.assistant.Complete(ctx, system, caption)
	if err != nil {
		fmt.Println("cmf: извлечение имени из подписи не удалось:", err)
		return ""
	}
	var parsed struct {
		Client string `json:"client"`
	}
	if block := extractJSONBlock(out); block != "" {
		_ = json.Unmarshal([]byte(block), &parsed)
	}
	return strings.TrimSpace(parsed.Client)
}

// ---- Инструменты ассистента ----

// cmfConnectionTool — «есть доступ к программе?»: проверяет связь ПО ФАКТУ
// (вход + пробный поиск), а не по наличию настроек. Раньше бот отвечал «да,
// подключён», ничего не проверив, — а каждый запрос падал.
func (b *Bot) cmfConnectionTool() ai.Tool {
	return ai.Tool{
		Name: "cmf_connection_check",
		Description: "Проверяет ПО ФАКТУ, работает ли связь с программой рассрочек (вход + пробный поиск клиента). " +
			"Вызывай на «есть доступ к программе?», «программа работает?», «почему сверка не идёт», и если сверка " +
			"вернула ошибку. Отвечай по результату, не по догадке.",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{}, "required": []string{}},
		Handle: func(ctx context.Context, input json.RawMessage) (string, error) {
			if b.cmf == nil {
				return "Программа НЕ подключена: в настройках бота не заданы CMF_API_URL / CMF_EMAIL / CMF_PASSWORD.", nil
			}
			pctx, cancel := context.WithTimeout(ctx, 40*time.Second)
			defer cancel()
			if err := b.cmf.Ping(pctx); err != nil {
				fmt.Println("cmf ping:", err)
				return "Связи с программой НЕТ: " + cmf.Human(err) + ".", nil
			}
			return "Связь с программой есть: вход и поиск клиентов работают.", nil
		},
	}
}

// cmfStatusTool — ЖИВАЯ сверка распознанных чеков из учёта с программой:
// какие внесены, какие нет, по каким клиент не найден. Работает по всему
// учёту (в т.ч. по чекам, сохранённым через "запомни"), а не по отдельной
// таблице наблюдений — поэтому реально отвечает "какие чеки не внесены".
func (b *Bot) cmfStatusTool() ai.Tool {
	return ai.Tool{
		Name: "cmf_check_receipts",
		Description: "Живая сверка чеков с программой рассрочек: берёт распознанные чеки из учёта за период " +
			"(по умолчанию сегодня) и проверяет по каждому, внесён ли платёж в программу. Показывает: внесённые, " +
			"НЕ внесённые (их надо добавить), и чеки, клиента которых нет в программе. Вызывай ВСЕГДА при вопросах " +
			"'какие чеки не внесены', 'какие сегодняшние чеки добавлены', 'чеки каких клиентов внесены', " +
			"'проверь сверку', 'внесён ли чек Каталова' (тогда укажи client). Не отвечай про сверку по памяти — всегда вызывай этот инструмент.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"from_date": map[string]any{"type": "string", "description": "Начало периода YYYY-MM-DD (пусто = сегодня)"},
				"to_date":   map[string]any{"type": "string", "description": "Конец периода YYYY-MM-DD (пусто = сегодня)"},
				"group":     map[string]any{"type": "string", "description": "Название группы (пусто = все)"},
				"client":    map[string]any{"type": "string", "description": "ФИО клиента, если спрашивают про его чек («внесён ли чек Каталова?») — проверю только его"},
			},
			"required": []string{},
		},
		Handle: func(ctx context.Context, input json.RawMessage) (string, error) {
			var args struct {
				FromDate string `json:"from_date"`
				ToDate   string `json:"to_date"`
				Group    string `json:"group"`
				Client   string `json:"client"`
			}
			_ = json.Unmarshal(input, &args)
			now := time.Now()
			from := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
			to := from.AddDate(0, 0, 1)
			if args.FromDate != "" {
				if t, err := time.ParseInLocation("2006-01-02", args.FromDate, time.Local); err == nil {
					from = t
				}
			}
			if args.ToDate != "" {
				if t, err := time.ParseInLocation("2006-01-02", args.ToDate, time.Local); err == nil {
					to = t.AddDate(0, 0, 1)
				}
			}
			groupJID := ""
			if strings.TrimSpace(args.Group) != "" {
				jid, _, err := b.resolveGroup(ctx, args.Group)
				if err != nil {
					return "", err
				}
				groupJID = jid.String()
			}
			if strings.TrimSpace(args.Client) != "" && args.FromDate == "" {
				from = from.AddDate(0, 0, -30) // про чек клиента — за последний месяц
			}
			return b.cmfReconcileFor(ctx, from, to, groupJID, args.Client)
		},
	}
}

// cmfReconcile и «человеческое» сопоставление — в reconcile.go / reconcile_match.go.

// cmfLookupWithTypos и cmfFuzzyByWords вынесены в cmf_fuzzy.go (сопоставление с
// допуском на опечатки в буквах и склонения).

// cmfAddPaymentTool — внести платёж по чеку в программу рассрочек.
func (b *Bot) cmfAddPaymentTool() ai.Tool {
	return ai.Tool{
		Name: "cmf_add_payment",
		Description: "Вносит платёж клиента в программу рассрочек (записывает оплату по договору). Вызывай, когда " +
			"владелец просит внести/добавить платёж в программу: 'внеси чек Миланы на 25000 в программу', " +
			"'добавь платёж Ахмеда Каталова 14000'. Находит клиента и его договор; если договоров несколько — " +
			"вернёт список, тогда переспроси, по какому вносить (укажи contract_id). ЭТО ЗАПИСЬ В ПРОГРАММУ — " +
			"вызывай только по явной просьбе внести, не по своей инициативе.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"client_name": map[string]any{"type": "string", "description": "Имя клиента в программе"},
				"amount":      map[string]any{"type": "number", "description": "Сумма платежа в рублях"},
				"date":        map[string]any{"type": "string", "description": "Дата операции на чеке YYYY-MM-DD (пусто = дата чека этого клиента на эту сумму из учёта, иначе сегодня)"},
				"contract_id": map[string]any{"type": "string", "description": "ID договора, если у клиента их несколько (из предыдущего ответа инструмента)"},
				"confirm_duplicate": map[string]any{"type": "boolean",
					"description": "true — владелец ЯВНО подтвердил, что нужно внести ещё один такой же платёж, хотя похожий уже есть"},
			},
			"required": []string{"client_name", "amount"},
		},
		Handle: func(ctx context.Context, input json.RawMessage) (string, error) {
			var args struct {
				ClientName string  `json:"client_name"`
				Amount     float64 `json:"amount"`
				Date       string  `json:"date"`
				ContractID string  `json:"contract_id"`
				Confirm    bool    `json:"confirm_duplicate"`
			}
			if err := json.Unmarshal(input, &args); err != nil {
				return "", err
			}
			if b.cmf == nil {
				return "", fmt.Errorf("интеграция с программой не настроена")
			}
			if args.Amount <= 0 {
				return "", fmt.Errorf("нужна сумма платежа")
			}
			paidAt := time.Now()
			dateNote := ""
			if args.Date != "" {
				if t, err := time.ParseInLocation("2006-01-02", args.Date, time.Local); err == nil {
					paidAt = t
				}
			}

			// Определяем клиента и договор.
			var branchID, contractID string
			if strings.TrimSpace(args.ContractID) != "" {
				contractID = strings.TrimSpace(args.ContractID)
				// branch неизвестен по id — найдём среди договоров клиента ниже
			}
			clients, kind, err := b.cmfLookupWithTypos(ctx, strings.TrimSpace(args.ClientName))
			if err != nil {
				return "", err
			}
			if len(clients) == 0 {
				return fmt.Sprintf("Клиента %q в программе не нашёл.", args.ClientName), nil
			}
			if len(clients) > 1 && contractID == "" {
				var names []string
				for _, c := range clients {
					names = append(names, c.FullName)
				}
				return "Под это имя подходит несколько клиентов: " + strings.Join(names, "; ") + ". Уточни полное имя.", nil
			}
			// ЗАПИСЬ в программу — на СЛАБОМ совпадении не вносим вслепую (можно
			// записать не тому). Просим подтвердить точное имя найденного кандидата.
			if kind == cmfWeak && contractID == "" {
				return fmt.Sprintf("Точного совпадения по «%s» в программе нет, похоже на «%s». Если это он — повтори с его ПОЛНЫМ именем (как в программе), тогда внесу.", args.ClientName, clients[0].FullName), nil
			}
			contracts, err := b.cmf.ClientContracts(ctx, clients[0].ID)
			if err != nil {
				return "", err
			}
			if len(contracts) == 0 {
				return fmt.Sprintf("У клиента %s нет договоров в программе.", clients[0].FullName), nil
			}
			var chosen *cmf.ContractRef
			if contractID != "" {
				for i := range contracts {
					if contracts[i].ID == contractID {
						chosen = &contracts[i]
						break
					}
				}
				if chosen == nil {
					return "", fmt.Errorf("договор %s у клиента не найден", contractID)
				}
			} else if len(contracts) == 1 {
				chosen = &contracts[0]
			} else {
				var lines []string
				for _, c := range contracts {
					lines = append(lines, fmt.Sprintf("договор №%d (%s, остаток %d ₽) — contract_id=%s", c.Number, c.ProductName, c.Remaining, c.ID))
				}
				return fmt.Sprintf("У клиента %s несколько договоров — уточни, по какому вносить:\n- %s", clients[0].FullName, strings.Join(lines, "\n- ")), nil
			}
			branchID = chosen.BranchID

			// «Внеси чек …» без даты — дата операции с ЧЕКА этого клиента на эту
			// сумму (иначе напоминание потом решит, что оплата не внесена).
			// Какой чек вносим: невнесённый по сверке (старые уже внесённые
			// закрываем), при явной дате — чек этого дня.
			watchID := 0
			var enteredDates []time.Time // чеки, которые сверка уже видит внесёнными
			if open, err := b.db.OpenWatchesForClientAmount(ctx, clients[0].ID, args.Amount); err == nil && len(open) > 0 {
				// Внесённые по сверке чеки не берём. Если владелец подтвердил «ещё
				// один», а невнесённых по сверке нет (единственную оплату сверка
				// отдала не тому из двух одинаковых чеков) — берём самый поздний.
				vs := b.judgeClientWatches(ctx, open)
				var pending []db.CmfWatch
				for _, w := range open {
					if v, ok := vs[w.ID]; ok && v.v.entered() {
						enteredDates = append(enteredDates, w.TxDate)
						continue
					}
					pending = append(pending, w)
				}
				if args.Confirm && len(pending) == 0 {
					pending = open[len(open)-1:]
				}
				if len(pending) == 0 && len(enteredDates) > 0 && args.Date == "" && !args.Confirm {
					var ds []string
					for _, d := range enteredDates {
						ds = append(ds, d.In(time.Local).Format("02.01"))
					}
					return fmt.Sprintf("ПОХОЖЕ НА ПОВТОР: чек %s на %.0f ₽ (%s) в программе уже внесён. Не внёс ещё раз. Если нужен ещё один платёж — повтори с confirm_duplicate=true.",
						clients[0].FullName, args.Amount, strings.Join(ds, ", ")), nil
				}
				if args.Date != "" {
					for _, w := range pending {
						if w.TxDate.In(time.Local).Format("2006-01-02") == paidAt.Format("2006-01-02") {
							watchID = w.ID
							break
						}
					}
				} else if len(pending) > 0 {
					watchID, paidAt, dateNote = pending[0].ID, pending[0].TxDate, " — по чеку"
				}
			}

			// Такой платёж уже есть в программе (внесли вручную, повторная просьба,
			// прошлая попытка оборвалась) — второй раз без подтверждения не пишем.
			if !args.Confirm {
				key := fmt.Sprintf("%s|%.0f|%s", clients[0].ID, args.Amount, paidAt.Format("2006-01-02"))
				if at, ok := b.cmfRecentWrite(key); ok {
					return fmt.Sprintf("ПОХОЖЕ НА ПОВТОР: я уже вносил %s %.0f ₽ (дата %s) в %s. Не внёс ещё раз. Спроси владельца; если нужен ещё один платёж — повтори с confirm_duplicate=true.",
						clients[0].FullName, args.Amount, paidAt.Format("02.01.2006"), at.Format("15:04")), nil
				}
				fromDay := paidAt.AddDate(0, 0, -5)
				for _, d := range enteredDates {
					if d.AddDate(0, 0, -5).Before(fromDay) {
						fromDay = d.AddDate(0, 0, -5)
					}
				}
				pays, err := b.cmf.PaymentsBetween(ctx, clients[0].ID, fromDay, time.Now().AddDate(0, 0, 1))
				if err != nil {
					return fmt.Sprintf("Не внёс: не смог проверить, нет ли этого платежа в программе уже (%s). Попробуй чуть позже.", cmf.Human(err)), nil
				}
				want := itemKop(args.Amount)
				for _, p := range pays {
					// Как сверка: с точностью до рубля (копейки при вводе отбрасывают),
					// в рублях или в копейках.
					if sameMoney(want, payKop(p, false)) || sameMoney(want, payKop(p, true)) {
						when := effDate(p)
						return fmt.Sprintf("ПОХОЖЕ НА ПОВТОР: в программе уже есть %.0f ₽ от %s%s у %s. Не внёс. Спроси владельца: внести ещё один? Если да — повтори с confirm_duplicate=true.",
							args.Amount, dayOrDash(when), contractLabel(p), clients[0].FullName), nil
					}
				}
			}

			sendRub := int64(math.Round(args.Amount))
			key := fmt.Sprintf("%s|%.0f|%s", clients[0].ID, args.Amount, paidAt.Format("2006-01-02"))
			if err := b.cmf.AddPayment(ctx, chosen.ID, branchID, sendRub, paidAt); err != nil {
				b.cmfNoteWrite(key) // могло записаться до обрыва связи — повтор переспросит
				return "", err
			}
			b.cmfNoteWrite(key)
			if watchID != 0 {
				_ = b.db.UpdateCmfWatch(ctx, watchID, "", "", "", "", "found")
			}
			return fmt.Sprintf("✅ Внёс платёж в программу: %s, %d ₽, договор №%d, дата %s%s.",
				clients[0].FullName, sendRub, chosen.Number, paidAt.Format("02.01.2006"), dateNote), nil
		},
	}
}

// cmfResolveTool — вручную указать, чей чек (ответ на вопрос бота или команда).
func (b *Bot) cmfResolveTool(chat types.JID) ai.Tool {
	return ai.Tool{
		Name: "cmf_resolve",
		Description: "Указывает, какому клиенту программы относится чек из сверки. Вызывай, когда владелец отвечает " +
			"на вопрос бота «за кого этот платёж» или говорит «этот чек — Ахмед Каталов Нажудович». " +
			"Если владелец ответил свайпом — передай message_id из [Контекст ответа: … id сообщения …] (это вопрос бота или сам чек). " +
			"Без свайпа передай amount (сумму чека) и/или payer (подпись), если владелец их назвал: привяжу, только если такой неясный чек один. " +
			"client_name — ФИО клиента как в программе.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"watch_id":    map[string]any{"type": "integer", "description": "Номер наблюдения (0 = по message_id или по сумме/плательщику)"},
				"message_id":  map[string]any{"type": "string", "description": "id сообщения, на которое ответили свайпом (вопрос бота, его подтверждение/напоминание или сам чек)"},
				"client_name": map[string]any{"type": "string", "description": "ФИО клиента в программе"},
				"amount":      map[string]any{"type": "number", "description": "Сумма чека, если владелец её назвал (чтобы найти нужный чек)"},
				"payer":       map[string]any{"type": "string", "description": "Подпись/плательщик чека, если владелец его назвал"},
			},
			"required": []string{"client_name"},
		},
		Handle: func(ctx context.Context, input json.RawMessage) (string, error) {
			var args struct {
				WatchID    int     `json:"watch_id"`
				MessageID  string  `json:"message_id"`
				ClientName string  `json:"client_name"`
				Amount     float64 `json:"amount"`
				Payer      string  `json:"payer"`
			}
			if err := json.Unmarshal(input, &args); err != nil {
				return "", err
			}
			if b.cmf == nil {
				return "", fmt.Errorf("интеграция с программой не настроена (CMF_API_URL/CMF_EMAIL/CMF_PASSWORD)")
			}
			watchID := args.WatchID
			if mid := strings.TrimSpace(args.MessageID); watchID == 0 && mid != "" {
				if refs, _ := b.db.CmfWatchesByAsk(ctx, mid); len(refs) == 1 {
					watchID = refs[0].WatchID
				} else if id, ok, _ := b.db.CmfWatchByCheckMessage(ctx, mid); ok {
					watchID = id
				}
				if watchID == 0 {
					// Свайп на сообщение, которое не относится к одному чеку — НЕ угадываем.
					return "", fmt.Errorf("не понял, к какому чеку относится это сообщение — ответь свайпом на сам чек или вопрос бота по нему, или укажи watch_id")
				}
			}
			if watchID == 0 {
				ws, err := b.db.ListCmfWatches(ctx, []string{"ambiguous", "noname"}, 100)
				if err != nil {
					return "", err
				}
				var match []db.CmfWatch
				for _, w := range ws {
					if w.GroupJID != chat.String() {
						continue
					}
					if args.Amount > 0 && math.Abs(args.Amount-w.Amount) >= 1 {
						continue
					}
					if p := strings.TrimSpace(args.Payer); p != "" && !sameClientName(p, w.ClientText) && !nameCovers(p, w.ClientText) {
						continue
					}
					match = append(match, w)
				}
				switch len(match) {
				case 1:
					watchID = match[0].ID
				case 0:
					return "", fmt.Errorf("неясных чеков с такими данными в этой группе нет — уточни сумму или ответь свайпом на чек")
				default:
					var lines []string
					for _, w := range match {
						lines = append(lines, fmt.Sprintf("#%d — %.0f ₽ — «%s» — %s", w.ID, w.Amount, w.ClientText, w.TxDate.In(reconLoc).Format("02.01")))
					}
					return "Неясных чеков несколько — спроси владельца, какой, и вызови снова с watch_id:\n" + strings.Join(lines, "\n"), nil
				}
			}
			w, ok, err := b.db.CmfWatchByID(ctx, watchID)
			if err != nil || !ok {
				return "", fmt.Errorf("наблюдение #%d не найдено", watchID)
			}
			plan := planCmfAnswer(cmfAnswer{Kind: ansClient, Name: strings.TrimSpace(args.ClientName)}, nil, b.cmfLookupFunc(ctx))
			switch plan.Action {
			case actBind:
				gj := chat
				if j, err := types.ParseJID(w.GroupJID); err == nil {
					gj = j
				}
				b.bindCmfWatch(ctx, gj, w, plan.Client, false)
				return fmt.Sprintf("Чек #%d на %.0f ₽ (подпись «%s») привязан к клиенту %s — слежу, чтобы платёж внесли в программу.", w.ID, w.Amount, w.ClientText, plan.Client.FullName), nil
			case actAskAgain:
				var names []string
				for _, c := range plan.Options {
					names = append(names, c.FullName)
				}
				return "Под это имя подходит не один клиент или имя неполное: " + strings.Join(names, "; ") + " — уточни полное ФИО.", nil
			case actLookupError:
				return "", plan.Err
			default:
				return fmt.Sprintf("Клиента %q в программе не нашёл — проверь написание.", args.ClientName), nil
			}
		},
	}
}

// cmfBranchTool — "запомни: чеки, которых нет в программе, относятся к точке X".
func (b *Bot) cmfBranchTool() ai.Tool {
	return ai.Tool{
		Name: "cmf_set_unmatched_branch",
		Description: "Запоминает, к какой точке (филиалу) относить чеки, клиентов которых нет в программе. " +
			"Вызывай при 'запомни: чеки которых нет в программе относятся к главной точке'. " +
			"Пустое название = показать текущую настройку.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"branch": map[string]any{"type": "string", "description": "Название точки (пусто = показать текущую)"},
			},
			"required": []string{},
		},
		Handle: func(ctx context.Context, input json.RawMessage) (string, error) {
			var args struct {
				Branch string `json:"branch"`
			}
			_ = json.Unmarshal(input, &args)
			if strings.TrimSpace(args.Branch) == "" {
				cur, err := b.db.SettingGet(ctx, settingUnmatchedBranch)
				if err != nil {
					return "", err
				}
				if cur == "" {
					return "Точка для чеков вне программы пока не задана.", nil
				}
				return "Чеки, которых нет в программе, относятся к точке «" + cur + "».", nil
			}
			if err := b.db.SettingSet(ctx, settingUnmatchedBranch, strings.TrimSpace(args.Branch)); err != nil {
				return "", err
			}
			return "Запомнил: чеки, которых нет в программе, относятся к точке «" + strings.TrimSpace(args.Branch) + "».", nil
		},
	}
}

// cmfNoteWrite / cmfRecentWrite — журнал собственных записей в программу за
// сутки: повторная просьба внести то же самое не пишет второй раз, даже если
// программа ещё не показывает первую запись.
func (b *Bot) cmfNoteWrite(key string) {
	b.cmfWritesMu.Lock()
	defer b.cmfWritesMu.Unlock()
	if b.cmfWrites == nil {
		b.cmfWrites = map[string]time.Time{}
	}
	for k, t := range b.cmfWrites {
		if time.Since(t) > 24*time.Hour {
			delete(b.cmfWrites, k)
		}
	}
	b.cmfWrites[key] = time.Now()
}

func (b *Bot) cmfRecentWrite(key string) (time.Time, bool) {
	b.cmfWritesMu.Lock()
	defer b.cmfWritesMu.Unlock()
	t, ok := b.cmfWrites[key]
	if !ok || time.Since(t) > 24*time.Hour {
		return time.Time{}, false
	}
	return t, true
}
