// Сверка чеков с программой рассрочек (cmf): каждый чек из рабочей группы
// становится "наблюдением". Бот определяет клиента (имя пишут в подписи к
// чеку или отдельным сообщением), ищет его в программе с учётом опечаток,
// при неоднозначности переспрашивает прямо в группе, а затем следит, внесли
// ли платёж в программу — и напоминает, если забыли.
package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode"

	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"

	"whatsapp-bot/internal/ai"
	"whatsapp-bot/internal/cmf"
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
	for _, w := range strings.Fields(name) {
		// слова с цифрами (суммы "25.000", "20т") в имя не берём
		if strings.IndexFunc(w, func(r rune) bool { return r >= '0' && r <= '9' }) >= 0 {
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
	// 1. Подпись к чеку.
	if strings.TrimSpace(caption) != "" {
		if name := b.cmfExtractClientName(ctx, caption); name != "" {
			return name, parser.ExtractAmount(caption)
		}
		// ИИ не выделил, но подпись сама похожа на ФИО ("цихаев саляхь").
		if name, ok := looksLikeName(caption); ok {
			return name, parser.ExtractAmount(caption)
		}
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
	// 3. Имя из durable-очереди (FIFO) — самое РАННЕЕ по времени сообщения из
	// написанных перед чеком, не старше окна спаривания. Очередь в БD, поэтому
	// переживает рестарт; забор атомарный (не отдаст одно имя двум чекам).
	since := time.Now().Add(-b.pairWindow)
	if name, amount, ok, err := b.db.TakePendingName(ctx, msg.Info.Chat.String(), msg.Info.Sender.String(), since); err != nil {
		fmt.Println("Очередь имён: ошибка выборки:", err)
	} else if ok {
		return name, amount
	}
	return "", 0
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
// Возвращает true, если сообщение — имя (обработано).
func (b *Bot) handleNameMessage(ctx context.Context, msg *events.Message, text string, rawID int) bool {
	chat := msg.Info.Chat
	sender := msg.Info.Sender.String()
	quotedID := extractQuotedStanzaID(msg)
	since := time.Now().Add(-b.pairWindow)
	amount := parser.ExtractAmount(text) // сумма из того же сообщения, если была

	// Есть ли ждущий чек (ответ на чек или неподтверждённый чек от отправителя)?
	hasUnconfirmed, _ := b.db.HasUnconfirmedReceiptFrom(ctx, chat.String(), sender, since)
	checkNearby := quotedID != "" || hasUnconfirmed

	name, ok := looksLikeName(text)
	if !ok {
		// Рядом с чеком часто пишут ФИО и сумму НА РАЗНЫХ СТРОКАХ
		// («Магамадов Алха\n22.000₽ ✅») — это подпись к чеку (кому засчитать),
		// а не отдельный платёж. Берём ФИО из строки. Без чека рядом не трогаем —
		// пусть парсер разберёт как обычный платёж.
		if !checkNearby {
			return false
		}
		if ln, found := firstNameLine(text); found {
			name = ln
		} else {
			return false
		}
	}

	// Уверены ли, что это ВООБЩЕ ФИО человека, а не фраза, случайно прошедшая
	// эвристику («Доброе Утро», «Новый Клиент»)? Уверенно — если совпал известный
	// алиас. Иначе спрашиваем ИИ: он подтвердит/нормализует настоящее ФИО или
	// вернёт пусто для не-имени. Без этого любая 2–5-словная фраза с заглавной
	// перехватывалась как «клиент» ДО ИИ — и привязывала чужой чек к выдуманному
	// человеку либо плодила мусорные контакты. Без ассистента доверяем только
	// известным алиасам, остальное НЕ перехватываем (чек останется на проверку).
	if _, known := b.aliases.ResolveName(name); !known {
		confirmed := ""
		if b.assistant != nil {
			confirmed = b.cmfExtractClientName(ctx, text)
		}
		if confirmed == "" {
			return false // не уверены, что это имя — пусть идёт дальше (платёж/болтовня)
		}
		name = confirmed
	}

	if !checkNearby {
		// Чека рядом нет. Если это «ФИО+сумма» и рядом было фото пачки денег —
		// это НАЛИЧКА, записываем сразу.
		if amount > 0 && b.consumePendingCash(chat, sender) {
			b.recordCashPayment(ctx, chat, name, amount, rawID, msg.Info.Timestamp)
			return true
		}
		// Иначе имя (возможно с суммой) — в очередь: перед будущим чеком или
		// фото наличных.
		b.enqueuePendingName(ctx, msg, name, amount, rawID)
		return true
	}

	canonical, _ := b.aliases.ResolveName(name)
	var contactIDPtr *int
	if cid, err := b.db.GetOrCreateContact(ctx, canonical); err == nil {
		contactIDPtr = &cid
	}

	updated := false
	if quotedID != "" {
		if found, _, err := b.db.ReattributeReceiptByMessage(ctx, quotedID, canonical, contactIDPtr); err == nil && found {
			updated = true
			// Сверка по этому чеку тоже переключается на нового клиента.
			if b.reresolveWatchByCheck(ctx, chat, quotedID, canonical) {
				fmt.Printf("Чек переатрибутирован на клиента %q (свайп с ФИО на чек), сверка обновлена\n", canonical)
				return true
			}
		} else {
			// Свайп пришёлся НЕ на чек (например, на вопрос бота, чья привязка уже
			// вытеснена из askMap) — не привязываем вслепую к самому старому чеку
			// (это дало бы неверную атрибуцию). Запоминаем имя как ждущее.
			b.enqueuePendingName(ctx, msg, name, amount, rawID)
			return true
		}
	}
	if !updated {
		// Имя без свайпа: самый старый неподтверждённый чек от отправителя
		// (FIFO по порядку чата).
		found, _, err := b.db.ReattributeOldestUnconfirmedReceipt(ctx, chat.String(), sender, since, canonical, contactIDPtr)
		if err != nil || !found {
			// Не нашли чек — на всякий случай запомним имя как ждущее.
			b.enqueuePendingName(ctx, msg, name, amount, rawID)
			return true
		}
	}
	fmt.Printf("Чек переатрибутирован на клиента %q (ФИО написали рядом с чеком)\n", canonical)

	// Дополнительно обновляем наблюдение сверки, если программа подключена.
	if b.cmf != nil {
		if watchID, wok, err := b.db.LatestNonameWatch(ctx, chat.String(), msg.Info.Sender.String(), since); err == nil && wok {
			_ = b.db.UpdateCmfWatch(ctx, watchID, canonical, "", "", "", "lookup")
			var amount float64
			if ws, err := b.db.ListCmfWatches(ctx, []string{"lookup"}, 50); err == nil {
				for _, w := range ws {
					if w.ID == watchID {
						amount = w.Amount
					}
				}
			}
			go b.cmfResolveWatch(context.Background(), watchID, chat, canonical, amount)
		}
	}
	return true
}

// extractQuotedStanzaID возвращает id сообщения, на которое ответили (свайп).
func extractQuotedStanzaID(msg *events.Message) string {
	ext := msg.Message.GetExtendedTextMessage()
	if ext == nil {
		return ""
	}
	if ci := ext.GetContextInfo(); ci != nil {
		return ci.GetStanzaID()
	}
	return ""
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
	ext := msg.Message.GetExtendedTextMessage()
	if ext == nil {
		return ""
	}
	ci := ext.GetContextInfo()
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
	clients, kind, err := b.cmfLookupWithTypos(ctx, clientText)
	if err != nil {
		// Программа не ответила — оставляем «lookup»: фоновая проверка повторит.
		fmt.Println("cmf lookup:", err)
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
	ticker := time.NewTicker(30 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		b.cmfCheckDue(ctx)
		cancel()
	}
}

// cmfRetryLookups — чеки, по которым программа не ответила при поиске клиента
// (статус lookup, старше 10 минут), ищем ещё раз.
func (b *Bot) cmfRetryLookups(ctx context.Context) {
	ws, err := b.db.ListCmfWatches(ctx, []string{"lookup"}, 20)
	if err != nil {
		return
	}
	for _, w := range ws {
		if w.ClientText == "" || time.Since(w.CreatedAt) < 10*time.Minute || time.Since(w.CreatedAt) > 72*time.Hour {
			continue
		}
		jid, err := types.ParseJID(w.GroupJID)
		if err != nil {
			continue
		}
		b.cmfResolveWatch(ctx, w.ID, jid, w.ClientText, w.Amount)
	}
}

func (b *Bot) cmfCheckDue(ctx context.Context) {
	b.cmfRetryLookups(ctx)
	due, err := b.db.DueCmfWatches(ctx, time.Now().Add(-cmfRemindAfter()), 30)
	if err != nil {
		fmt.Println("cmf: ошибка выборки наблюдений:", err)
		return
	}
	// Собираем неотмеченные по ГРУППЕ, чтобы послать ОДНО напоминание со списком
	// клиентов, а не по сообщению на каждый чек (меньше шума, как просил владелец).
	type rem struct {
		client string
		amount float64
		date   time.Time
	}
	byGroup := map[string][]rem{}
	var order []string
	for _, w := range due {
		found, err := b.cmf.HasPaymentAround(ctx, w.ClientID, w.Amount, w.TxDate, 5)
		if err != nil {
			fmt.Println("cmf: ошибка проверки платежа:", err)
			continue
		}
		if found {
			_ = b.db.UpdateCmfWatch(ctx, w.ID, "", "", "", "", "found")
			fmt.Printf("cmf: платёж %s на %.0f ₽ найден в программе\n", w.ClientName, w.Amount)
			continue
		}
		_ = b.db.UpdateCmfWatch(ctx, w.ID, "", "", "", "", "reminded")
		if _, ok := byGroup[w.GroupJID]; !ok {
			order = append(order, w.GroupJID)
		}
		byGroup[w.GroupJID] = append(byGroup[w.GroupJID], rem{w.ClientName, w.Amount, w.TxDate})
	}

	for _, gj := range order {
		jid, err := types.ParseJID(gj)
		if err != nil {
			continue
		}
		if b.groupSilent(jid) {
			continue // в этой группе бот молчит — напоминания не шлём
		}
		items := byGroup[gj]
		var sb strings.Builder
		if len(items) == 1 {
			it := items[0]
			fmt.Fprintf(&sb, "⏰ Занесите, пожалуйста, оплату в программу: чек от %s на %.0f ₽ — клиент %s (в рассрочке пока НЕ отмечен).",
				it.date.Format("02.01"), it.amount, it.client)
		} else {
			sb.WriteString("⏰ Занесите, пожалуйста, оплаты этих клиентов в программу (чеки пришли, но в рассрочке пока НЕ отмечены):")
			for _, it := range items {
				fmt.Fprintf(&sb, "\n• %s — %.0f ₽ (чек %s)", it.client, it.amount, it.date.Format("02.01"))
			}
		}
		b.sendText(jid, sb.String())
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
			"'проверь сверку'. Не отвечай про сверку по памяти — всегда вызывай этот инструмент.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"from_date": map[string]any{"type": "string", "description": "Начало периода YYYY-MM-DD (пусто = сегодня)"},
				"to_date":   map[string]any{"type": "string", "description": "Конец периода YYYY-MM-DD (пусто = сегодня)"},
				"group":     map[string]any{"type": "string", "description": "Название группы (пусто = все)"},
			},
			"required": []string{},
		},
		Handle: func(ctx context.Context, input json.RawMessage) (string, error) {
			var args struct {
				FromDate string `json:"from_date"`
				ToDate   string `json:"to_date"`
				Group    string `json:"group"`
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
			return b.cmfReconcile(ctx, from, to, groupJID)
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
				"date":        map[string]any{"type": "string", "description": "Дата платежа YYYY-MM-DD (пусто = сегодня)"},
				"contract_id": map[string]any{"type": "string", "description": "ID договора, если у клиента их несколько (из предыдущего ответа инструмента)"},
			},
			"required": []string{"client_name", "amount"},
		},
		Handle: func(ctx context.Context, input json.RawMessage) (string, error) {
			var args struct {
				ClientName string  `json:"client_name"`
				Amount     float64 `json:"amount"`
				Date       string  `json:"date"`
				ContractID string  `json:"contract_id"`
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

			if err := b.cmf.AddPayment(ctx, chosen.ID, branchID, int64(args.Amount+0.5), paidAt); err != nil {
				return "", err
			}
			return fmt.Sprintf("✅ Внёс платёж в программу: %s, %.0f ₽, договор №%d, дата %s.",
				clients[0].FullName, args.Amount, chosen.Number, paidAt.Format("02.01.2006")), nil
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
			"watch_id — номер наблюдения из cmf_status (если ни его, ни message_id нет — последний неясный чек в ЭТОЙ группе). " +
			"client_name — ФИО клиента как в программе.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"watch_id":    map[string]any{"type": "integer", "description": "Номер наблюдения (0 = по message_id или последний неясный в этой группе)"},
				"message_id":  map[string]any{"type": "string", "description": "id сообщения, на которое ответили свайпом (вопрос бота или чек)"},
				"client_name": map[string]any{"type": "string", "description": "ФИО клиента в программе"},
			},
			"required": []string{"client_name"},
		},
		Handle: func(ctx context.Context, input json.RawMessage) (string, error) {
			var args struct {
				WatchID    int    `json:"watch_id"`
				MessageID  string `json:"message_id"`
				ClientName string `json:"client_name"`
			}
			if err := json.Unmarshal(input, &args); err != nil {
				return "", err
			}
			if b.cmf == nil {
				return "", fmt.Errorf("интеграция с программой не настроена (CMF_API_URL/CMF_EMAIL/CMF_PASSWORD)")
			}
			watchID := args.WatchID
			if watchID == 0 && strings.TrimSpace(args.MessageID) != "" {
				mid := strings.TrimSpace(args.MessageID)
				if id, ok, _ := b.db.CmfWatchByAsk(ctx, mid); ok {
					watchID = id
				} else if id, ok, _ := b.db.CmfWatchByCheckMessage(ctx, mid); ok {
					watchID = id
				}
			}
			if watchID == 0 {
				ws, err := b.db.ListCmfWatches(ctx, []string{"ambiguous", "noname", "unmatched"}, 50)
				if err != nil {
					return "", err
				}
				for _, w := range ws {
					if w.GroupJID == chat.String() {
						watchID = w.ID
						break
					}
				}
			}
			if watchID == 0 {
				return "", fmt.Errorf("не понял, о каком чеке речь — ответь свайпом на чек или вопрос бота, или укажи watch_id из cmf_status")
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
				return fmt.Sprintf("Чек #%d на %.0f ₽ привязан к клиенту %s — слежу, чтобы платёж внесли в программу.", w.ID, w.Amount, plan.Client.FullName), nil
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
