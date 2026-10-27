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
// cleanNameCaption — подпись к чеку состоит ТОЛЬКО из ФИО (плюс, может быть,
// суммы и валюты): тогда клиента можно взять без запроса к ИИ. Если в подписи
// есть посторонние слова (глаголы, «с карты», «брат» и т.п. — их looksLikeName
// отбрасывает как стоп-слова), считаем подпись шумной и возвращаем ok=false,
// чтобы её разобрал ИИ (он отличит плательщика от упомянутых посторонних).
func cleanNameCaption(caption string) (string, bool) {
	name, ok := looksLikeName(caption)
	if !ok {
		return "", false
	}
	nameSet := map[string]bool{}
	for _, w := range strings.Fields(name) {
		nameSet[strings.ToLower(w)] = true
	}
	for _, w := range strings.Fields(caption) {
		if strings.IndexFunc(w, func(r rune) bool { return r >= '0' && r <= '9' }) >= 0 {
			continue // число (сумма)
		}
		clean := strings.TrimFunc(w, func(r rune) bool { return !unicode.IsLetter(r) })
		if clean == "" {
			continue // эмодзи/знаки
		}
		if parser.IsMoneyWord(clean, true) {
			continue // «руб», «тыс», «₽»
		}
		if !nameSet[strings.ToLower(clean)] {
			return "", false // слово не из имени и не сумма — подпись шумная, нужен ИИ
		}
	}
	return name, true
}

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
//
// Имя этого клиента, написанное ПЕРЕД чеком, снимает из очереди уже разбор
// чека (handleBankReceipt → consumeQueuedSameName), когда известна сумма чека:
// снимается запись на ЭТУ сумму, а не первая по имени.
func (b *Bot) resolveReceiptPayer(ctx context.Context, msg *events.Message, caption string) (string, float64) {
	// 1. Подпись к чеку.
	if name, amount := b.payerFromCaption(ctx, caption); name != "" {
		return name, amount
	}
	// 2. Чек — это ответ (свайп) на сообщение с именем.
	if quoted := extractQuotedText(msg); strings.TrimSpace(quoted) != "" {
		if name := b.cmfExtractClientName(ctx, quoted); name != "" {
			return name, parser.ExtractAmount(quoted)
		}
		if name, ok := looksLikeName(quoted); ok {
			return name, parser.ExtractAmount(quoted)
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
	// Чистая подпись-ФИО («Магомедов Ахмед», «Магомедов Ахмед 15000») —
	// распознаём на месте, без лишнего запроса к ИИ (ускоряет ответ на чек).
	// Шумные подписи («с карты Пияна Ахмед оплатил») по-прежнему разбирает ИИ.
	if name, ok := cleanNameCaption(caption); ok {
		if canon, _ := b.aliases.ResolveName(name); canon != "" {
			return canon, parser.ExtractAmount(caption)
		}
		return name, parser.ExtractAmount(caption)
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

// takeQueuedBreakdown — чек на сумму, которую текст перед ним расписал по
// частям одного клиента («Исаев Харон 15000 + 20000», затем один чек на
// 35000): части ждут в очереди имён каждая свою сумму, а пришёл один чек на
// всё. Это тот же платёж: чек берёт клиента, части из очереди снимаются, а
// платежи текста — подпись чека (их снимает dropBreakdownTexts, когда чек
// записан в сбор). Только если в очереди ждут ВСЕ части сообщения и
// безналичные платежи сообщения целиком — разбивка суммы чека (одного
// клиента); наличка рядом — отдельный платёж, она остаётся.
func (b *Bot) takeQueuedBreakdown(ctx context.Context, chat types.JID, senderJID string, amount float64) (string, int, bool) {
	if senderJID == "" || b.db == nil || amount <= 0 {
		return "", 0, false
	}
	ps, err := b.db.PendingNamesOf(ctx, chat.String(), senderJID, time.Now().Add(-b.pairWindow))
	if err != nil {
		return "", 0, false
	}
	byRaw := map[int][]db.PendingName{}
	var order []int
	for _, p := range ps {
		if p.RawID <= 0 || p.Amount <= 0 {
			continue
		}
		if _, seen := byRaw[p.RawID]; !seen {
			order = append(order, p.RawID)
		}
		byRaw[p.RawID] = append(byRaw[p.RawID], p)
	}
	for _, rid := range order {
		parts := byRaw[rid]
		if len(parts) < 2 {
			continue
		}
		total, same := 0.0, true
		for _, p := range parts {
			total += p.Amount
			same = same && normCyr(p.Name) == normCyr(parts[0].Name)
		}
		if !same || !nearAmount(total, amount) {
			continue
		}
		body, _, err := b.db.RawBodyByID(ctx, rid)
		if err != nil {
			continue
		}
		if _, ok := breakdownOfCheck(body, amount); !ok {
			continue
		}
		for _, p := range parts {
			_ = b.db.ConsumePendingNameByID(ctx, p.ID)
		}
		return parts[0].Name, rid, true
	}
	return "", 0, false
}

// dropBreakdownTexts — чек записан на клиента по разбивке, написанной до
// него: платежи этого текста — тот же платёж, что чек, отдельно не считаются.
// Разбор текста ИИ мог ещё идти — дожидаемся его, чтобы снять и его итог.
// amount — сумма чека.
func (b *Bot) dropBreakdownTexts(ctx context.Context, rawID int, amount float64) {
	b.aiRetryMu.Lock()
	f, busy := b.aiInFlight[rawID]
	b.aiRetryMu.Unlock()
	if busy {
		select {
		case <-f.done:
		case <-time.After(time.Minute):
		}
	}
	body, _, err := b.db.RawBodyByID(ctx, rawID)
	if err != nil {
		return
	}
	n, err := b.db.IgnoreTextTxOfRaw(ctx, rawID, breakdownAmounts(body, amount))
	if err != nil {
		fmt.Println("Разбивка суммы чека: не снял платежи текста:", err)
		return
	}
	// Без налички текст целиком — сам чек: разбирать его больше нечего. С
	// наличкой её ещё надо записать (ИИ мог не прочитать текст — он перечитает,
	// а безналичные платежи снова снимет dropCaptionParts).
	if !textCashPays(body) {
		_ = b.db.MarkMessageParsed(ctx, rawID)
	}
	fmt.Printf("Разбивка суммы чека (сообщение %d): %d платеж(а) текста — это сам чек\n", rawID, n)
}

// breakdownAmounts — суммы платежей, которыми текст мог записать чек на amount,
// расписанный по частям: безналичные части и сама сумма чека (ИИ мог прочитать
// разбивку одним платежом). Наличку не трогаем.
func breakdownAmounts(text string, amount float64) []float64 {
	out := []float64{amount}
	for _, p := range textPays(text) {
		if !p.cash && p.amount > 0 {
			out = append(out, p.amount)
		}
	}
	return out
}

// dropCaptionParts — платежи текста записаны, а сам текст назвал чек
// разбивкой его суммы по частям и в нём же отдельная наличка («Исаев Харон
// 15000 + 20000\n5000 нал» на чек 35000): безналичные платежи текста — это сам
// чек (снимаем, иначе 35000 посчитаются дважды), наличка — отдельный платёж
// (остаётся). Вызывается после каждой записи платежей сообщения: свайп, правка,
// перечитывание после сбоя ИИ.
func (b *Bot) dropCaptionParts(ctx context.Context, rawID int, text string) {
	if b.db == nil || rawID <= 0 || !(countAmountLines(text) > 1 || severalAmounts(text)) {
		return
	}
	amts, err := b.db.ConfirmedReceiptAmountsNamedBy(ctx, rawID)
	if err != nil {
		return
	}
	for _, a := range amts {
		if _, ok := breakdownOfCheck(text, a); !ok {
			continue
		}
		if n, err := b.db.IgnoreTextTxOfRaw(ctx, rawID, breakdownAmounts(text, a)); err == nil && n > 0 {
			fmt.Printf("Разбивка суммы чека (сообщение %d): %d безналичных платеж(а) текста — это сам чек на %.0f ₽\n", rawID, n, a)
		}
		return
	}
}

// consumeQueuedSameName — снять из очереди имя того же клиента: оно уже
// использовано подписью или свайпом чека на сумму amount (иначе оно
// достанется следующему чеку другого клиента). У клиента в очереди может быть
// несколько записей с разными суммами («Исаев Харон 7500 и 5000»): снимаем
// запись на сумму чека, иначе — имя без суммы, иначе — части одного
// сообщения, которые вместе дают сумму чека («15000 + 20000» и чек 35000),
// иначе — первую запись клиента (опечатка в сумме: лучше потом спросить про
// его следующий чек, чем молча отдать ему чек другого клиента).
// breakdownRaw — сообщение, расписавшее сумму этого чека по частям: его
// безналичные платежи — сам чек (их снимет dropBreakdownTexts).
func (b *Bot) consumeQueuedSameName(ctx context.Context, chat types.JID, senderJID, name string, amount float64) (breakdownRaw int) {
	if b.db == nil || name == "" || senderJID == "" {
		return 0
	}
	ps, err := b.db.PendingNamesOf(ctx, chat.String(), senderJID, time.Now().Add(-b.pairWindow))
	if err != nil {
		return 0
	}
	var mine []db.PendingName
	for _, p := range ps {
		if sameClientName(p.Name, name) || normCyr(p.Name) == normCyr(name) {
			mine = append(mine, p)
		}
	}
	if len(mine) == 0 {
		return 0
	}
	consume := func(ps ...db.PendingName) {
		for _, p := range ps {
			_ = b.db.ConsumePendingNameByID(ctx, p.ID)
		}
	}
	for _, p := range mine {
		if nearAmount(p.Amount, amount) {
			consume(p)
			return 0
		}
	}
	for _, p := range mine {
		if p.Amount <= 0 {
			consume(p)
			return 0
		}
	}
	if amount > 0 {
		byRaw := map[int][]db.PendingName{}
		var order []int
		for _, p := range mine {
			if p.RawID <= 0 {
				continue
			}
			if _, seen := byRaw[p.RawID]; !seen {
				order = append(order, p.RawID)
			}
			byRaw[p.RawID] = append(byRaw[p.RawID], p)
		}
		for _, rid := range order {
			total := 0.0
			for _, p := range byRaw[rid] {
				total += p.Amount
			}
			if len(byRaw[rid]) > 1 && nearAmount(total, amount) {
				consume(byRaw[rid]...)
				if body, _, err := b.db.RawBodyByID(ctx, rid); err == nil {
					if _, ok := breakdownOfCheck(body, amount); ok {
						return rid
					}
				}
				return 0
			}
		}
	}
	consume(mine[0])
	return 0
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

	// Наличка («Исаев Ислам нал 5000», «Ахмед 10000 у Дени», «… Мансур взял»,
	// «… отдал Адаму») и список из нескольких платежей — это платежи, а не
	// подпись к чеку. Если рядом ждёт чек, подпишем его платежом на ТУ ЖЕ сумму,
	// остальное запишется как платежи. Считаем суммы, а не только строки: у
	// «Исаев Харон\n10000\n5000» общая сумма не читается (числа на разных
	// строках), а в «Исаев Харон 15000 20000», «Магомедов Ахмед 7500 и 5000»,
	// «Исаев Харон 15000, Магомедов Ахмед 10000» сумм несколько в одной строке —
	// это всё равно несколько платежей, подписью к чеку их не съедаем.
	multi := amountLines > 1 || severalAmounts(text)
	if multi || ((amount > 0 || amountLines > 0) && textCash(text)) {
		var used []float64
		if checkNearby && multi {
			labeled, caption := b.labelCheckFromLines(ctx, msg, text, rawID, since)
			if caption {
				// Сумма чека по частям одного клиента («Исаев Харон 15000 +
				// 20000» на чек 35000) — подпись чека: это тот же платёж.
				if !textCashPays(text) {
					_ = b.db.MarkMessageParsed(ctx, rawID)
					return true
				}
				// Рядом с разбивкой — наличка («…\n5000 нал»): это отдельный
				// платёж, его записываем; безналичные платежи текста — сам чек,
				// их после записи снимет dropCaptionParts. В очередь имён части
				// не ставим — их чек уже назван.
				return false
			}
			used = append(used, labeled...)
			// Платёж на сумму другого ждущего чека отправителя назовёт ЕГО
			// (соседнее сообщение) — будущему чеку он не достаётся.
			if amts, err := b.db.UnconfirmedReceiptAmountsFrom(ctx, chat.String(), sender, since); err == nil {
				used = append(used, amts...)
			}
		}
		// Список перед чеками: остальные его платежи ждут свои чеки (на ту же
		// сумму) в очереди имён. Свайп на что-то другое — не имя для будущего чека.
		if multi && quotedID == "" {
			b.enqueueListPayers(ctx, msg, text, rawID, used...)
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
		// Чек мог взять из очереди ФИО, написанное после «Чей он?» (см.
		// crossCandidate): тогда его новое ФИО покажет, что то было ответом.
		cand, hasCand := b.crossCandidateOf(chat.String(), sender)
		var prev db.CrossCheck
		if hasCand {
			prev, _ = b.db.CrossCheckOf(ctx, quotedID, 0)
		}
		cid := contact()
		if re, err := b.db.ReattributeReceiptByMessageRel(ctx, quotedID, canonical, cid, rawID); err == nil && re.Found {
			_ = b.db.MarkMessageParsed(ctx, rawID)
			// Чеки «того же перевода» прежнего клиента снова отдельные — говорим.
			if len(re.Released) > 0 {
				_, qSender, _, _ := b.db.ReceiptReplyRef(ctx, re.ReceiptID)
				b.reportReleasedHolds(chat, quotedID, qSender, canonical, re.Released)
			}
			if hasCand {
				b.crossCandidateResolved(ctx, chat, cand, prev, canonical, cid)
			}
			// Тот же перевод того же клиента уже в сборе (экран без даты или
			// полный чек) — второй раз не считаем, спрашиваем; тот же чек в
			// другой группе на другого клиента — «чей он?».
			if same := b.afterCheckNamed(ctx, chat, quotedID, 0); same.Found && same.VictimIsThis {
				return true
			}
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
					fmt.Printf("Чек переатрибутирован на клиента %q (ФИО+сумма рядом с чеком)\n", canonical)
					if !b.labeledCheckHeld(ctx, chat, checkRaw) {
						b.syncWatchAfterLabel(ctx, chat, checkRaw, canonical)
					}
					return true
				}
			}
		}
		// Иначе это платёж: запишется как текстовый, а имя подождёт чек на эту
		// сумму — если только чек этого клиента на эту сумму уже не записан
		// (текст после чека с подписью — тот же платёж) и не ждёт рядом другой
		// чек на эту сумму (его назовёт это же сообщение, как соседнее, — в
		// очереди имя досталось бы потом чужому чеку той же суммы).
		if b.checkedAlready(ctx, msg, canonical, amount, rawID) == 0 && !b.waitingCheckOf(ctx, chat.String(), sender, since, amount) {
			b.enqueuePendingName(ctx, msg, name, amount, rawID)
		}
		return false
	}

	// Чека, ждущего клиента, нет — имя перед следующим чеком (в очередь). Но
	// если последний чек отправителя взял из очереди ФИО, написанное после
	// «Чей он?», это ФИО — клиент того чека, а то было ответом (crossCandidate).
	// Свёрнутый экран этого работника, только что признанный тем же переводом,
	// что полный чек, — ФИО может быть и про него (nameAfterHeldScreen: тот же
	// клиент — «уже учтён», другой — имя ждёт чек, а бот спрашивает).
	queueName := func(afterHeldScreen bool) bool {
		if b.crossCandidateNextName(ctx, msg, canonical, contact(), rawID) {
			_ = b.db.MarkMessageParsed(ctx, rawID)
			return true
		}
		if afterHeldScreen && b.nameAfterHeldScreen(ctx, msg, name, canonical, contact(), rawID) {
			return true
		}
		b.enqueuePendingName(ctx, msg, name, 0, rawID)
		_ = b.db.MarkMessageParsed(ctx, rawID)
		b.noteCrossCandidate(msg, text, canonical, rawID)
		return true
	}
	if !checkNearby {
		return queueName(true)
	}

	// Имя без свайпа: самый старый неподтверждённый чек от отправителя
	// (FIFO по порядку чата).
	found, _, checkRaw, err := b.db.ReattributeOldestUnconfirmedReceiptFrom(ctx, chat.String(), sender, since, canonical, contact(), rawID, 0)
	if err != nil || !found {
		return queueName(false)
	}
	_ = b.db.MarkMessageParsed(ctx, rawID)
	fmt.Printf("Чек переатрибутирован на клиента %q (ФИО написали рядом с чеком)\n", canonical)
	if !b.labeledCheckHeld(ctx, chat, checkRaw) {
		b.syncWatchAfterLabel(ctx, chat, checkRaw, canonical)
	}
	return true
}

// labeledCheckHeld — чек сообщения checkRaw назвали ФИО, написанным рядом
// (FIFO, список): проверки после того, как чек стал чьим (afterCheckNamed), —
// тот же перевод того же клиента мог уже войти в сбор другим чеком. true —
// этот чек в сбор не пошёл (повтор или ждёт ответа в «НЕ вошло»).
func (b *Bot) labeledCheckHeld(ctx context.Context, chat types.JID, checkRaw int) bool {
	if checkRaw <= 0 {
		return false
	}
	same := b.afterCheckNamed(ctx, chat, "", checkRaw)
	return same.Found && same.VictimIsThis
}

// Наличка, написанная через того, кто её забрал, без слова «нал»: «Мансур
// взял», «Адам забрал», «Мансур собрал», «принёс», «отдал Адаму», «передала к
// Дени», «отдал мне». Так её читают и разбор платежей (ИИ, хвост строки у
// парсера), и вопрос «у кого наличка?».
var (
	collectorTakeWords = map[string]bool{
		"взял": true, "взяла": true, "забрал": true, "забрала": true, "собрал": true, "собрала": true,
		"принёс": true, "принес": true, "принесла": true, "занёс": true, "занес": true, "занесла": true,
	}
	collectorGiveWords = map[string]bool{"отдал": true, "отдала": true, "передал": true, "передала": true}
	// после «отдал/передал» — не тот, кому отдали: «передал через сбер», «отдал сегодня».
	notCollectorWords = map[string]bool{
		"через": true, "на": true, "по": true, "за": true, "с": true, "со": true, "из": true, "от": true,
		"до": true, "в": true, "во": true, "сегодня": true, "вчера": true, "сейчас": true, "уже": true,
		"только": true, "всё": true, "все": true, "полностью": true, "частично": true, "деньги": true,
		"сумму": true, "оплату": true, "долг": true, "остаток": true,
		"сбером": true, "сбербанком": true, "тинькоффом": true, "тиньком": true, "втб": true, "альфой": true, "себе": true,
		"всю": true, "весь": true, "сразу": true, "половину": true, "предоплату": true, "часть": true, "досрочно": true,
		"заранее": true, "вовремя": true, "свои": true, "мои": true, "больше": true, "меньше": true, "раньше": true,
		"позже": true, "назад": true, "обратно": true, "взнос": true, "наличку": true, "наличные": true,
	}
	// collectorPronouns — «отдал мне», «передал ему»: деньги у человека.
	collectorPronouns = map[string]bool{"мне": true, "ему": true, "ей": true, "нам": true, "им": true, "вам": true, "тебе": true}
)

// collectorCash — в тексте сказано, что деньги принесли или забрали
// («принёс», «занёс», «Мансур взял», «я забрал», «… забрал мансур»), или кому
// их отдали («отдал Адаму», «передал мансуру», «отдал к Дени», «отдал мне»).
// Это наличка — отдельный платёж. Обычная подпись перевода — не наличка:
// «Исаев Харон отдал ещё 15000», «отдал последние 15000», «отдал всю сумму»
// (не кому, а как отдал), «Исаев Харон взял айфон, первый взнос 15000» (взял
// вещь, а не деньги).
func collectorCash(text string) bool {
	for _, ln := range nonEmptyLines(text) {
		if lineCollector(ln) {
			return true
		}
	}
	return false
}

// collectorGoods — вещи, которые берут в рассрочку: «взял айфон», «забрал
// телефон» — взяли товар, а не деньги.
var collectorGoods = map[string]bool{
	"айфон": true, "айфона": true, "iphone": true, "телефон": true, "телефона": true, "смартфон": true,
	"самсунг": true, "ноутбук": true, "ноут": true, "планшет": true, "товар": true, "технику": true,
	"холодильник": true, "телевизор": true, "машину": true, "часы": true, "наушники": true,
	"чек": true, "квитанцию": true, "сдачу": true,
}

// lineCollector — collectorCash для одной строки.
func lineCollector(line string) bool {
	ws := strings.FieldsFunc(line, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
	low := func(i int) string { return strings.ToLower(ws[i]) }
	// person — слово может быть именем человека (не сумма, не служебное слово).
	person := func(i int) bool {
		w := low(i)
		return strings.IndexFunc(w, unicode.IsDigit) < 0 && len([]rune(w)) > 1 &&
			!nameStopwords[w] && !notCollectorWords[w] && !pairNoiseWords[w] && !collectorPronouns[w] &&
			!collectorTakeWords[w] && !collectorGiveWords[w] && !parser.IsMoneyWord(w, true)
	}
	capital := func(i int) bool {
		r := []rune(ws[i])
		return len(r) > 0 && unicode.IsUpper(r[0])
	}
	// amountBefore — сумма уже написана до слова i: «ФИО 10000 забрал мансур».
	amountBefore := func(i int) bool {
		for k := 0; k < i; k++ {
			if strings.IndexFunc(ws[k], unicode.IsDigit) >= 0 {
				return true
			}
		}
		return false
	}
	for i := range ws {
		w := low(i)
		switch {
		case collectorTakeWords[w]:
			// «взял айфон», «телефон забрал» — взяли вещь (товар в рассрочку).
			if i+1 < len(ws) && collectorGoods[low(i+1)] || i > 0 && collectorGoods[low(i-1)] {
				continue
			}
			// «Исаев Харон взял телефон 15000» — слово с маленькой буквы сразу после
			// глагола, до суммы, — что взяли. После суммы это тот, кто забрал
			// («10000 забрал мансур»).
			if i+1 < len(ws) && person(i+1) && !capital(i+1) && !amountBefore(i) {
				continue
			}
			// «принёс 10000», «10000 занёс», «я забрал», «Мансур взял» — наличка.
			return true
		case collectorGiveWords[w]:
			j := i + 1
			viaK := j < len(ws) && low(j) == "к"
			if viaK {
				j++
			}
			if j >= len(ws) {
				continue
			}
			// «отдал мне», «отдал к Дени», «отдал Адаму». С маленькой буквы — имя в
			// дательном («отдал адаму», «передал мансуру») или последнее слово
			// после суммы («10000 отдал адам»). «отдал ещё/последние/всю сумму/
			// досрочно 15000» — не кому, а как отдал.
			if collectorPronouns[low(j)] {
				return true
			}
			if person(j) && (viaK || capital(j) || collectorDative(low(j)) || j+1 == len(ws) && amountBefore(i)) {
				return true
			}
		}
	}
	return false
}

// collectorDative — слово похоже на имя в дательном падеже: «адаму»,
// «мансуру», «мадине», «дени». Прилагательные («последние», «вторую»,
// «полное») — нет.
func collectorDative(w string) bool {
	r := []rune(w)
	if len(r) < 3 {
		return false
	}
	for _, suf := range []string{"ую", "юю", "ие", "ые", "ое", "ее", "ей", "ой", "ий", "ый"} {
		if strings.HasSuffix(w, suf) {
			return false
		}
	}
	switch r[len(r)-1] {
	case 'у', 'ю', 'е', 'и':
		return true
	}
	return false
}

// textCash — текст помечен как наличка так же, как его читает разбор платежей:
// «нал», «наличка», «у Дени», «офис» (parser.IsCash), пометка в строке платежа
// по разбору парсера или «кто забрал» («Мансур взял», «отдал Адаму»). Наличка —
// отдельный платёж: такой текст не называет чек и не ждёт его в очереди имён.
func textCash(text string) bool {
	if parser.IsCash(text) || collectorCash(text) {
		return true
	}
	for _, tr := range parser.ParseMessage(text).Transactions {
		if tr.IsCash {
			return true
		}
	}
	return false
}

// amountPair — платёж внутри строки: ФИО перед суммой (как написано) и сумма.
type amountPair struct {
	name   string // "" — ФИО перед суммой не написано
	amount float64
}

// pairJoinWords — союзы списка между суммами («7500 и 5000», «15000 плюс 5000»).
var pairJoinWords = map[string]bool{"и": true, "плюс": true, "ещё": true, "еще": true}

// pairNoiseWords — пометки к сумме, которые не ФИО: банк, карта, перевод.
var pairNoiseWords = map[string]bool{
	"сбер": true, "сбербанк": true, "втб": true, "альфа": true, "тинькофф": true, "тинек": true,
	"озон": true, "карта": true, "карту": true, "карты": true, "картой": true, "переводом": true,
	"наличкой": true, "налом": true, "наличные": true, "кэш": true, "кеш": true, "cash": true,
}

// balanceWords — остаток, долг, итог перед суммой («15000, остаток 30000»,
// «15000 из 45000», «15000 + 20000 итого 35000»): такая сумма — не ещё один
// платёж строки, а «ФИО сумма» с пометкой.
var balanceWords = map[string]bool{
	"остаток": true, "остатка": true, "осталось": true, "остался": true, "осталась": true,
	"останется": true, "ост": true, "долг": true, "долга": true, "должен": true, "должна": true,
	"из": true, "было": true, "всего": true, "итого": true, "итог": true,
}

// debtPaidWords — «долг», «остаток» после союза списка («15000 + долг 5000»,
// «15000 и долг 5000 закрыл», «плюс остаток 30000») — оплатили и долг: это
// второй платёж строки. Без союза («15000, долг 5000», «остаток 30000») —
// пометка остатка.
var debtPaidWords = map[string]bool{"долг": true, "долга": true, "остаток": true, "остатка": true, "ост": true}

func init() {
	// Слова остатка — и не ФИО («Исаев Харон 15000, остаток 30000» — клиент
	// «Исаев Харон»).
	for w := range balanceWords {
		nameStopwords[w] = true
	}
}

// gapNameWords — слова ФИО между суммами строки, есть ли между ними союз
// списка («+», «,», «;», «/», «и») и пометка остатка («остаток», «из»).
// strong — союз сложения («+», «и», «плюс», «ещё»), а не просто запятая.
// Хвосты сумм («т.р.», «руб»), числа, пометки и служебные слова — не ФИО.
func gapNameWords(gap string, afterNumber bool) (words []string, joined, balance, strong bool) {
	joined = strings.ContainsAny(gap, "+,;/&")
	// strong — союз «+», «и», «плюс», «ещё» (а не просто запятая): «+ долг 5000»
	// — оплатили и долг.
	strong = strings.ContainsAny(gap, "+&")
	debtOnly := true
	tail := afterNumber
	for _, w := range strings.Fields(gap) {
		if tail && parser.IsMoneyWord(w, true) {
			continue
		}
		tail = false
		clean := strings.TrimFunc(w, func(r rune) bool { return !unicode.IsLetter(r) })
		lw := strings.ToLower(clean)
		switch {
		case lw == "":
		case pairJoinWords[lw]:
			joined, strong = true, true
		case balanceWords[lw]:
			balance = true
			debtOnly = debtOnly && debtPaidWords[lw]
		case strings.IndexFunc(w, unicode.IsDigit) >= 0, nameStopwords[lw], pairNoiseWords[lw]:
		default:
			words = append(words, clean)
		}
	}
	if balance && debtOnly && strong {
		balance = false // «15000 + долг 5000» — второй платёж
	}
	return words, joined, balance, strong
}

// lineAmountPairs — платежи строки по её суммам: каждая сумма — платёж ФИО,
// написанного перед ней («Исаев Харон 15000 + 20000» — два платежа Исаева,
// «Исаев Харон 15000, Магомедов Ахмед 10000» — Исаева и Магомедова). Строка,
// начинающаяся с суммы, — платежи ФИО заголовка блока block; если же после
// последней суммы ещё стоит ФИО («15000 Исаев Харон 20000 Магомедов Ахмед» —
// сумма перед именем), чьи суммы — не угадываем: ФИО у пар пустые. Та же сумма
// повтором без союза между ними («15000 (15т)») — один платёж. Сумма после
// пометки остатка («15000, остаток 30000», «15000 из 45000») — не платёж (а
// строку, которая с такой пометки начинается, рядом с платежами отбрасывает
// textPays). Итог частей одного ФИО («15000 + 20000 = 35000», «35000
// (15000+20000)») — тоже не ещё один платёж: платежи — части.
// Учитываются только суммы, похожие на деньги («за 2 месяца» — не платёж).
func lineAmountPairs(line, block string) []amountPair {
	var out []amountPair
	var plus []bool // перед суммой — союз сложения («+», «и», «плюс»)
	name, prevEnd := "", 0
	startsWithAmount := false
	sameName := true // все суммы строки — одного ФИО (новое ФИО не написано)
	for _, h := range parser.AmountsAt(line) {
		if !h.Real {
			continue
		}
		words, joined, balance, strong := gapNameWords(line[prevEnd:h.Start], len(out) > 0)
		prevEnd = h.End
		switch {
		case len(out) > 0 && balance && len(words) == 0:
			continue // «15000, остаток 30000» — остаток, а не второй платёж
		case len(out) == 0:
			name = strings.Join(words, " ")
			if name == "" {
				name, startsWithAmount = block, true
			}
		case len(words) > 0:
			name = strings.Join(words, " ")
			sameName = false
		case !joined && nearAmount(out[len(out)-1].amount, h.Value):
			continue // «15000 (15т)» — та же сумма ещё раз
		}
		out = append(out, amountPair{name: name, amount: h.Value})
		plus = append(plus, strong)
	}
	if tail, _, _, _ := gapNameWords(line[prevEnd:], len(out) > 0); startsWithAmount && len(tail) > 0 {
		for i := range out {
			out[i].name = ""
		}
	}
	if len(out) >= 3 && sameName {
		// Итог частей: последняя сумма после «=» («15000 + 20000 = 35000») или
		// первая, за которой части в скобках («35000 (15000+20000)»). Сумма,
		// прибавленная союзом («5000 + 10000 + 15000»), — не итог, а платёж.
		total := 0.0
		for _, p := range out {
			total += p.amount
		}
		last := len(out) - 1
		switch {
		case !plus[last] && nearAmount(out[last].amount, total-out[last].amount):
			out = out[:last]
		case !plus[1] && nearAmount(out[0].amount, total-out[0].amount):
			out = out[1:]
		}
	}
	return out
}

// severalAmounts — в какой-то строке текста несколько сумм-платежей
// («Исаев Харон 15000 20000», «15000 + 20000», «7500 и 5000», «Исаев 15000,
// Магомедов 10000»). Такой текст — список платежей, а не подпись к одному чеку.
func severalAmounts(text string) bool {
	for _, ln := range nonEmptyLines(text) {
		if len(lineAmountPairs(ln, "")) > 1 {
			return true
		}
	}
	return false
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

// balanceLine — строка начинается с пометки остатка или итога перед первой
// суммой, без ФИО: «остаток 30000», «итого 35000», «из 45000».
func balanceLine(line string) bool {
	for _, h := range parser.AmountsAt(line) {
		if h.Real {
			words, _, balance, _ := gapNameWords(line[:h.Start], false)
			return balance && len(words) == 0
		}
	}
	return false
}

// hasRealAmount — в строке есть сумма, похожая на деньги (с суффиксом тысяч
// или от 100 ₽).
func hasRealAmount(text string) bool {
	for _, h := range parser.AmountsAt(text) {
		if h.Real {
			return true
		}
	}
	return false
}

// textHasAmount — есть ли в строке платёж на сумму amt. Сумма платежа —
// сумма пары «ФИО сумма» (lineAmountPairs): «Исаев Харон за 2 месяца 30000» —
// 30000, а не 2; в «Исаев Харон 15000, остаток 30000», «15000 из 45000»,
// «15000 (было 20000)» — 15000, а не остаток (разбор платежей берёт последнее
// число строки). Строка без сумм, похожих на деньги, — первое число или сумма
// разбора платежей.
func textHasAmount(text string, amt float64) bool {
	if hasRealAmount(text) {
		for _, p := range lineAmountPairs(text, "") {
			if nearAmount(p.amount, amt) {
				return true
			}
		}
		return false
	}
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
		cash[k] = tr.IsCash || textCash(tr.Line) || textCash(tr.RawName)
	}
	markBelow := func(i int, wholeBlockOnly bool) {
		seen := false
		for j := i + 1; j < len(lines); j++ {
			if k := txAt[j]; k >= 0 && k < len(cash) {
				seen, cash[k] = true, true
				continue
			}
			if wholeBlockOnly && seen && !textCash(lines[j]) {
				return // новый блок другого клиента
			}
		}
	}
	for i, ln := range lines {
		// «Мансур взял», «отдал Адаму» отдельной строкой — та же пометка налички.
		if txAt[i] >= 0 || !textCash(ln) {
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

// textPay — платёж сообщения так, как его видит подпись к чеку: сумма, ФИО
// (из строки «ФИО сумма», заголовка блока «ФИО\nсумма» или пары «ФИО сумма»
// в строке с несколькими суммами) и наличный ли он.
type textPay struct {
	name       string  // ФИО ("" — не написано)
	nameText   string  // откуда оно взято
	amount     float64 // сумма платежа
	lineAmount float64 // первая сумма его строки (строка с одной суммой)
	cash       bool
}

// is — платёж на сумму amt.
func (p textPay) is(amt float64) bool {
	return nearAmount(p.amount, amt) || nearAmount(p.lineAmount, amt)
}

// textPays — платежи сообщения по строкам. Строка с несколькими суммами
// («Исаев Харон 15000 20000», «Исаев Харон 15000, Магомедов Ахмед 10000»,
// «7500 и 5000» под заголовком «Магомедов Ахмед») — платежи по парам «ФИО
// сумма»: ФИО — написанное перед суммой. Пометка налички в этой строке или
// отдельной строкой без суммы — граница неясна, наличка вся строка. Строка,
// которая начинается с пометки остатка или итога («остаток 30000», «итого
// 35000», «из 45000» под блоком клиента), рядом с платежами — пометка, а не
// ещё платёж (парсер берёт её платежом ФИО заголовка).
func textPays(text string) []textPay {
	var out []textPay
	lines := nonEmptyLines(text)
	skipBalance := false // в сообщении есть платёж, кроме строк остатка
	for _, ln := range lines {
		if hasRealAmount(ln) && !balanceLine(ln) {
			skipBalance = true
			break
		}
	}
	trs, cash := paymentsCash(text)
	for k, tr := range trs {
		pairs := lineAmountPairs(tr.Line, "")
		if len(pairs) > 1 {
			continue // строку с несколькими суммами разбираем ниже — по парам «ФИО сумма»
		}
		if skipBalance && balanceLine(tr.Line) {
			continue
		}
		p := textPay{amount: tr.Amount, lineAmount: parser.ExtractAmount(tr.Line), cash: cash[k]}
		if len(pairs) == 1 {
			// Сумма платежа — сумма пары, а не остаток: в «Исаев Харон 15000
			// остаток 30000» платёж 15000 (парсер берёт 30000).
			p.amount = pairs[0].amount
		}
		// ФИО — из самой строки платежа («Магомедов Ахмед 15000»; перед суммой
		// пары — даже если парсер из-за остатка в конце строки взял ФИО
		// заголовка) или из заголовка блока («Исаев Харон» над строкой «10000»).
		pairName := ""
		if len(pairs) == 1 {
			pairName = pairs[0].name
		}
		for _, src := range []string{pairName, tr.RawName, tr.Line} {
			if n, found := looksLikeName(src); found {
				p.name, p.nameText = n, src
				break
			}
		}
		out = append(out, p)
	}
	looseCash := false
	for _, ln := range lines {
		if parser.ExtractAmount(ln) <= 0 && textCash(ln) {
			looseCash = true
		}
	}
	block := ""
	for _, ln := range lines {
		if parser.ExtractAmount(ln) <= 0 {
			if n, isName := looksLikeName(ln); isName {
				block = n
			}
			continue
		}
		pairs := lineAmountPairs(ln, block)
		if len(pairs) < 2 || skipBalance && balanceLine(ln) {
			continue
		}
		lineCash := looseCash || textCash(ln)
		for _, pr := range pairs {
			p := textPay{amount: pr.amount, cash: lineCash}
			if n, found := looksLikeName(pr.name); found {
				p.name, p.nameText = n, pr.name
			}
			out = append(out, p)
		}
	}
	return out
}

// nameForCheckAmount ищет в сообщении НЕналичный платёж на сумму amt — строку
// «ФИО сумма», блок «ФИО\nсумма\nсумма» (формат парсера) или пару «ФИО сумма»
// в строке с несколькими суммами — и возвращает его ФИО. seen — платёж на эту
// сумму в сообщении вообще есть. ok=false — такого платежа с ФИО нет (он
// наличный или без ФИО) или таких платежей у РАЗНЫХ людей несколько (не
// угадываем — спросим «чей чек?»).
func nameForCheckAmount(text string, amt float64) (hit checkNameHit, seen, ok bool) {
	if amt <= 0 {
		return hit, false, false
	}
	for _, p := range textPays(text) {
		if !p.is(amt) {
			continue
		}
		seen = true
		if p.cash || p.name == "" {
			continue
		}
		if ok && normCyr(p.name) != normCyr(hit.name) {
			return checkNameHit{}, true, false
		}
		if !ok {
			hit, ok = checkNameHit{name: p.name, nameText: p.nameText}, true
		}
		hit.n++
	}
	return hit, seen, ok
}

// breakdownOfCheck — сообщение рядом с чеком расписывает ЕГО сумму по частям
// («Исаев Харон 15000 + 20000», «Исаев Харон\n15000\n20000» на чек 35000):
// безналичные платежи (их хотя бы два) — одного ФИО и вместе дают сумму чека.
// Это подпись чека (один платёж), а не ещё платежи сверх него. Наличка в том
// же сообщении («…\n5000 нал») — отдельный платёж, не часть чека (её
// записываем, см. textCashPays).
func breakdownOfCheck(text string, amt float64) (checkNameHit, bool) {
	if amt <= 0 {
		return checkNameHit{}, false
	}
	var hit checkNameHit
	total, parts := 0.0, 0
	for _, p := range textPays(text) {
		if p.cash {
			continue
		}
		if p.name == "" || p.amount <= 0 {
			return checkNameHit{}, false
		}
		if hit.name != "" && normCyr(p.name) != normCyr(hit.name) {
			return checkNameHit{}, false
		}
		if hit.name == "" {
			hit = checkNameHit{name: p.name, nameText: p.nameText, n: 1}
		}
		total += p.amount
		parts++
	}
	return hit, parts >= 2 && nearAmount(total, amt)
}

// textCashPays — в сообщении есть наличный платёж (отдельный от чеков).
func textCashPays(text string) bool {
	for _, p := range textPays(text) {
		if p.cash {
			return true
		}
	}
	return false
}

// amountLinePayer — ФИО платежа в сообщении с ОДНОЙ строкой суммы: из самой
// строки («Магомедов Ахмед\nИсаев Харон 15000» — Исаев Харон) или из
// заголовка её блока («Магамадов Алха\n22.000₽ ✅»), как читает парсер. Сумма
// платежа — сумма пары строки, а не остаток («Исаев Харон 15000, остаток
// 30000» — 15000).
func amountLinePayer(text string) (checkNameHit, bool) {
	if countAmountLines(text) != 1 {
		return checkNameHit{}, false
	}
	trs := parser.ParseMessage(text).Transactions
	pays := textPays(text)
	if len(trs) != 1 || len(pays) != 1 {
		return checkNameHit{}, false
	}
	hit, _, ok := nameForCheckAmount(text, pays[0].amount)
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
	// Список платежей — несколько строк с суммой или несколько сумм в строке
	// («Исаев Харон 15000 20000», «Магомедов Ахмед 7500 и 5000»): ФИО берём
	// только у платежа на сумму чека.
	if lines > 1 || severalAmounts(text) {
		if hit, _, found := nameForCheckAmount(text, checkAmt); found {
			return hit.nameText, true, hit.n, true
		}
		return "", false, 0, false
	}
	// Наличка («нал», «у Дени», «Мансур взял», «отдал Адаму») — другой платёж.
	if textCash(text) {
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
// каждый ждущий чек отправителя, на сумму которого в списке есть платёж с
// одним ФИО. Список, который расписывает сумму самого старого ждущего чека
// (или чека под свайпом) по частям одного клиента («Исаев Харон 15000 +
// 20000» на чек 35000), — его подпись. labeled — суммы подписанных чеков;
// caption — сообщение целиком подпись чека (его платежи — это сам чек, второй
// раз их не записываем).
func (b *Bot) labelCheckFromLines(ctx context.Context, msg *events.Message, text string, rawID int, since time.Time) (labeled []float64, caption bool) {
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
	// Платёж списка на сумму чека, иначе — разбивка суммы чека по частям.
	hitFor := func(amt float64) (checkNameHit, bool, bool) {
		if hit, _, ok := nameForCheckAmount(text, amt); ok {
			return hit, false, true
		}
		hit, ok := breakdownOfCheck(text, amt)
		return hit, true, ok
	}
	if quotedID := extractQuotedStanzaID(msg); quotedID != "" {
		// Свайп на чек: подписываем ЕГО строкой на его сумму. Свайп не на чек —
		// чужим ждущим чекам список не подпись.
		r, err := b.db.ReceiptByWaMessageID(ctx, quotedID)
		if err != nil || r.ID == 0 {
			return nil, false
		}
		hit, whole, ok := hitFor(r.Amount)
		if !ok {
			return nil, false
		}
		owner, _ := b.db.ReceiptCardOwnerByMessage(ctx, quotedID)
		canonical, cid, ok := client(hit.nameText, owner)
		if !ok {
			return nil, false
		}
		if found, _, err := b.db.ReattributeReceiptByMessageFrom(ctx, quotedID, canonical, cid, rawID); err == nil && found {
			if same := b.afterCheckNamed(ctx, chat, quotedID, 0); !(same.Found && same.VictimIsThis) {
				b.reresolveWatchByCheck(ctx, chat, quotedID, canonical)
			}
			return []float64{r.Amount}, whole
		}
		return nil, false
	}
	checks, err := b.db.UnconfirmedReceiptCardsFrom(ctx, chat.String(), sender, since)
	if err != nil || len(checks) == 0 {
		return nil, false
	}
	label := func(c db.WaitingCheck, nameText string) bool {
		canonical, cid, ok := client(nameText, c.Owner)
		if !ok {
			return false
		}
		found, _, checkRaw, err := b.db.ReattributeOldestUnconfirmedReceiptFrom(ctx, chat.String(), sender, since, canonical, cid, rawID, c.Amount)
		if err != nil || !found {
			return false
		}
		if !b.labeledCheckHeld(ctx, chat, checkRaw) {
			b.syncWatchAfterLabel(ctx, chat, checkRaw, canonical)
		}
		return true
	}
	// Самый старый ждущий чек: список может расписывать его сумму по частям
	// одного клиента — тогда сообщение целиком его подпись.
	if oldest := checks[0]; oldest.Amount > 0 {
		if _, _, ok := nameForCheckAmount(text, oldest.Amount); !ok {
			if hit, ok := breakdownOfCheck(text, oldest.Amount); ok {
				if b.listMatchesOtherCheck(ctx, chat.String(), sender, since, text) {
					// Без свайпа разбивка «15000 + 20000» неоднозначна, если среди
					// ждущих чеков отправителя есть чек на одну из частей: это может
					// быть он и отдельный платёж. Этот чек не угадываем — бот спросит.
					checks = checks[1:]
				} else if label(oldest, hit.nameText) {
					return []float64{oldest.Amount}, true
				} else {
					return nil, false
				}
			}
		}
	}
	// Каждый ждущий чек отправителя (по порядку чата) получает платёж списка на
	// ЕГО сумму с одним ФИО: «Исаев Харон 15000\nМагомедов Ахмед 10000» после
	// чеков 15000 и 10000 называет оба, и следующее голое имя достаётся
	// следующему чеку, а не второму чеку списка. Платежей одного ФИО на сумму —
	// столько, сколько чеков этой суммы они называют.
	left := map[int64]int{}
	stop := map[int64]bool{}
	for _, c := range checks {
		key := int64(math.Round(c.Amount))
		if c.Amount <= 0 || stop[key] {
			continue
		}
		hit, _, ok := nameForCheckAmount(text, c.Amount)
		if !ok {
			stop[key] = true
			continue
		}
		n, seen := left[key]
		if !seen {
			n = hit.n
		}
		if n <= 0 || !label(c, hit.nameText) {
			// Чек этой суммы не подписали — более поздний той же суммы не трогаем
			// (подпись идёт к самому старому чеку суммы).
			stop[key] = true
			continue
		}
		left[key] = n - 1
		labeled = append(labeled, c.Amount)
	}
	return labeled, false
}

// waitingCheckOf — у отправителя ждёт чек без клиента на сумму amount.
func (b *Bot) waitingCheckOf(ctx context.Context, chat, sender string, since time.Time, amount float64) bool {
	amts, err := b.db.UnconfirmedReceiptAmountsFrom(ctx, chat, sender, since)
	if err != nil {
		return false
	}
	for _, a := range amts {
		if nearAmount(a, amount) {
			return true
		}
	}
	return false
}

// listMatchesOtherCheck — у отправителя есть ждущий чек на сумму одного из
// платежей текста.
func (b *Bot) listMatchesOtherCheck(ctx context.Context, chat, sender string, since time.Time, text string) bool {
	amts, err := b.db.UnconfirmedReceiptAmountsFrom(ctx, chat, sender, since)
	if err != nil {
		return true
	}
	for _, p := range textPays(text) {
		if p.cash {
			continue // наличка — не часть разбивки чека
		}
		for _, a := range amts {
			if p.is(a) {
				return true
			}
		}
	}
	return false
}

// enqueueListPayers — список платежей («Исаев Харон 7500 и 5000», «Исаев
// Харон 15000\nМагомедов Ахмед 10000»): каждый безналичный платёж с ФИО ждёт
// в очереди имён свой чек на ТУ ЖЕ сумму — как одиночный «ФИО сумма», чтобы
// чек получил клиента сразу, а не голое имя следующего клиента. Платежи на
// суммы used уже подписали чеки (по одному на сумму) — их не ставим. На
// сумму, у которой в списке разные ФИО или наличка, не ставим: не угадываем,
// бот спросит.
func (b *Bot) enqueueListPayers(ctx context.Context, msg *events.Message, text string, rawID int, used ...float64) {
	done := map[int64]bool{}
	confirmed := map[string]string{}
	for _, p := range textPays(text) {
		key := int64(math.Round(p.amount))
		if p.amount <= 0 || done[key] {
			continue
		}
		done[key] = true
		hit, _, ok := nameForCheckAmount(text, p.amount)
		if !ok {
			continue
		}
		n := hit.n
		for k, u := range used {
			if nearAmount(u, p.amount) {
				n, used[k] = n-1, 0
			}
		}
		if n <= 0 {
			continue
		}
		canonical, seen := confirmed[hit.nameText]
		if !seen {
			canonical, _ = b.confirmClientName(ctx, hit.nameText)
			confirmed[hit.nameText] = canonical
		}
		if canonical == "" {
			continue
		}
		for n -= b.checkedAlready(ctx, msg, canonical, p.amount, rawID); n > 0; n-- {
			b.enqueuePendingName(ctx, msg, canonical, p.amount, rawID)
		}
	}
}

// checkedAlready — сколько свежих чеков отправителя уже записаны на этого
// клиента на эту сумму (подписью, свайпом, именем рядом — не этим
// сообщением). Текст после них — сводка тех же платежей («Исаев Харон
// 15000\nМагомедов Ахмед 10000» после чеков с подписями): его платёж не ждёт
// ещё один чек в очереди имён, иначе следующий чек той же суммы молча уйдёт
// этому клиенту, а не тому, чьё имя напишут рядом.
func (b *Bot) checkedAlready(ctx context.Context, msg *events.Message, name string, amount float64, rawID int) int {
	n, err := b.db.ConfirmedReceiptCountFrom(ctx, msg.Info.Chat.String(), msg.Info.Sender.String(),
		time.Now().Add(-b.pairWindow), name, amount, rawID)
	if err != nil {
		return 0
	}
	return n
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
