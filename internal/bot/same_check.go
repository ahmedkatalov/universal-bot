// «Тот же чек или другой?»: чек стал полным/чьим не при приёме (автоповтор
// распознавания, ответ суммой или ФИО) и оказался тем же чеком, что уже учтён в
// группе. Сообщаем об этом в группе; если уверенности нет — спрашиваем, и ответ
// «другой» возвращает чек в сбор отдельным платежом.
package bot

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode"

	"whatsapp-bot/internal/db"

	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

// sameCheckAsk — «похоже, это тот же чек, что уже засчитан» по чеку checkWa.
type sameCheckAsk struct {
	checkWa string
}

func (b *Bot) registerSameCheckAsk(botMsgID string, a sameCheckAsk) {
	if botMsgID == "" || a.checkWa == "" {
		return
	}
	b.clarify.mu.Lock()
	if len(b.clarify.sameAsks) >= clarifyMapCap {
		for k := range b.clarify.sameAsks {
			delete(b.clarify.sameAsks, k)
			break
		}
	}
	b.clarify.sameAsks[botMsgID] = a
	b.clarify.mu.Unlock()
	// Связь «вопрос → чек» и в базе: ответ «другой» сработает и после
	// перезапуска бота (память вопросов пуста).
	if b.db != nil {
		if err := b.db.MarkSameCheckAsk(context.Background(), a.checkWa, botMsgID); err != nil {
			fmt.Println("Не запомнил вопрос «тот же чек?»:", err)
		}
	}
}

// reportSameCheck — сказать в группе, что чек checkWa (или его ничья копия)
// оказался повтором уже учтённого. reread — нашёл автоповтор распознавания.
// Возвращает id сообщения бота.
func (b *Bot) reportSameCheck(ctx context.Context, chat types.JID, checkWa, senderJID string, same db.SameCheck, reread bool) string {
	if !same.Found {
		return ""
	}
	if same.Parked {
		// Группа на молчании: чек ждёт в «НЕ вошло», спросим, когда можно будет
		// писать (askParkedSameChecks). Про «чей чек?» по нему не спрашиваем.
		b.dropReceiptAsk(chat.String(), same.VictimWa)
		return ""
	}
	if same.ScreenReplaced {
		// Повтором стал уже засчитанный экран без даты и номера, а в учёте
		// остался этот полный чек: говорим и даём путь назад.
		return b.reportScreenReplaced(ctx, chat, checkWa, senderJID, same)
	}
	if !same.VictimIsThis {
		// Повтором стала ничья копия — про неё больше не спрашиваем «чей чек?».
		b.dropReceiptAsk(chat.String(), same.VictimWa)
		return ""
	}
	// Вопрос про сумму/клиента этого чека больше не нужен — он не идёт в сбор.
	b.dropReceiptAsk(chat.String(), checkWa)
	if b.groupSilent(chat) {
		return ""
	}
	// Автоповтор отвечает цитатой на сам чек (вопрос задан давно); в ответе на
	// вопрос бота — обычным сообщением, как и остальные ответы там.
	send := func(text string) string {
		if senderJID == "" {
			return b.sendTextReturnID(chat, text)
		}
		return b.sendReply(chat, text, checkWa, senderJID)
	}
	when := same.TxDate.Format("02.01.2006 15:04")
	lead := "Это"
	if reread {
		lead = "Перечитал чек: это"
	}
	if same.Tentative {
		id := send(fmt.Sprintf(
			"🤔 %s, похоже, тот же чек, что уже засчитан: %s, %.0f ₽, чек от %s. Второй раз пока не считаю. "+
				"Если это ДРУГОЙ платёж — ответьте на это сообщение «другой», и я засчитаю его отдельно.",
			lead, same.Client, same.Amount, when))
		if id == "" {
			b.parkUnasked(ctx, checkWa)
		}
		b.registerSameCheckAsk(id, sameCheckAsk{checkWa: checkWa})
		return id
	}
	extra := ""
	if same.Strong && same.OtherClient && same.ThisClient != "" && nameKeyBot(same.ThisClient) != nameKeyBot(same.Client) {
		extra = fmt.Sprintf(" В первый раз он записан на «%s», этот — на «%s»: если клиент «%s», ответьте на это сообщение «да», и я поправлю.",
			same.Client, same.ThisClient, same.ThisClient)
	}
	id := send(fmt.Sprintf("⚠️ %s тот же чек, что уже присылали в эту группу: %s, %.0f ₽, чек от %s. Второй раз не учитываю в сумме сбора.%s",
		lead, same.Client, same.Amount, when, extra))
	if extra != "" {
		b.registerDupFixAsk(id, dupFixAsk{checkWa: same.KeptWa, name: same.ThisClient})
	}
	return id
}

// reportScreenReplaced — полный чек checkWa заменил в учёте экран того же
// перевода без времени и номера (same.VictimWa): говорим об этом и даём путь
// назад — ответ «другой» вернёт экран отдельным платежом. Возвращает id
// сообщения бота.
func (b *Bot) reportScreenReplaced(ctx context.Context, chat types.JID, checkWa, senderJID string, same db.SameCheck) string {
	// Экран больше не в сборе — про него не спрашиваем «чей чек?».
	b.dropReceiptAsk(chat.String(), same.VictimWa)
	if b.groupSilent(chat) {
		return ""
	}
	who := ""
	if same.KeptNamed && same.Client != "" {
		who = same.Client + ", "
	}
	when := ""
	if !same.TxDate.IsZero() {
		when = " (операция " + same.TxDate.Format("02.01.2006 15:04") + ")"
	}
	text := fmt.Sprintf("🤔 Похоже, это полный чек того же перевода, что и экран «Перевод выполнен» без даты и номера: %s%.0f ₽. "+
		"Считаю перевод один раз — по этому чеку%s. Если экран — ДРУГОЙ платёж, ответьте на это сообщение «другой», и я засчитаю его отдельно.",
		who, same.Amount, when)
	var id string
	if senderJID == "" || checkWa == "" {
		id = b.sendTextReturnID(chat, text)
	} else {
		id = b.sendReply(chat, text, checkWa, senderJID)
	}
	if id == "" {
		b.parkUnasked(ctx, same.VictimWa)
	}
	b.registerSameCheckAsk(id, sameCheckAsk{checkWa: same.VictimWa})
	return id
}

// parkUnasked — вопрос «тот же или другой?» про чек waID (повтор по догадке
// «похоже, тот же перевод») в группу не ушёл: догадку молча не держим — чек
// ждёт в «НЕ вошло», спросим в следующем цикле (askParkedSameChecks).
func (b *Bot) parkUnasked(ctx context.Context, waID string) {
	if b.db == nil {
		return
	}
	if err := b.db.ParkHeldSameCheck(ctx, waID); err != nil {
		fmt.Println("Не отложил чек «тот же перевод?» после неотправленного вопроса:", err)
	}
}

// parseSameCheckAnswer — ответ на «похоже, это тот же чек»: different — это
// другой платёж (засчитать отдельно).
func parseSameCheckAnswer(text string) (resolved, different bool) {
	t := strings.ReplaceAll(strings.ToLower(strings.TrimSpace(text)), "ё", "е")
	toks := strings.FieldsFunc(t, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
	if len(toks) == 0 {
		return false, false
	}
	phrase := " " + strings.Join(toks, " ") + " "
	has := func(ps ...string) bool {
		for _, p := range ps {
			if strings.Contains(phrase, " "+p+" ") {
				return true
			}
		}
		return false
	}
	switch {
	// «не тот же», «не повтор» — раньше, чем «тот же»/«повтор».
	case has("не тот", "не та", "не то", "не повтор", "не дубль", "другой", "другая", "другое", "другие", "разные",
		"разный", "новый", "новая", "отдельный", "отдельно", "отдельная", "еще один", "еще одна", "второй платеж"):
		return true, true
	case has("тот же", "та же", "то же", "тот самый", "повтор", "дубль", "дубликат", "он же", "одно и то же", "один и тот же"):
		return true, false
	case toks[0] == "нет" || toks[0] == "неа":
		return true, true // «нет» на «похоже, тот же чек» — не тот же
	case toks[0] == "да" || toks[0] == "ага" || toks[0] == "угу" || toks[0] == "верно":
		return true, false
	}
	return false, false
}

// sameCheckWords — служебные слова ответа «тот же/другой» (остаток — ФИО).
var sameCheckWords = map[string]bool{
	"это": true, "этот": true, "другой": true, "другая": true, "другое": true, "чек": true, "платеж": true,
	"перевод": true, "нет": true, "неа": true, "не": true, "тот": true, "та": true, "же": true, "а": true,
	"новый": true, "отдельный": true, "отдельно": true, "да": true, "он": true, "она": true, "клиент": true,
	"клиента": true, "за": true, "на": true, "от": true,
}

// withoutSameCheckWords — ответ без служебных слов («Это другой чек, Исаева
// Марьям» → «Исаева Марьям»); "" — клиента в ответе нет.
func withoutSameCheckWords(text string) string {
	var out []string
	for _, w := range strings.FieldsFunc(text, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '-' && r != '.' }) {
		k := strings.ReplaceAll(strings.ToLower(strings.Trim(w, ".-")), "ё", "е")
		if k == "" || sameCheckWords[k] {
			continue
		}
		out = append(out, w)
	}
	return strings.Join(out, " ")
}

// applySameCheckAnswer — ответ свайпом на «похоже, это тот же чек».
func (b *Bot) applySameCheckAnswer(ctx context.Context, chat types.JID, askID string, a sameCheckAsk, text string) bool {
	resolved, different := parseSameCheckAnswer(text)
	if !resolved {
		return false
	}
	b.clarify.mu.Lock()
	delete(b.clarify.sameAsks, askID)
	b.clarify.mu.Unlock()
	b.noteAnswer(ctx, "receipt", a.checkWa)
	if !different {
		b.sendText(chat, "Понял, это повтор — второй раз не считаю.")
		return true
	}
	// Каким чеком он был учтён (экран — полным чеком: копиям экрана в других
	// группах тогда поставили время операции с полного чека).
	orig, oerr := b.db.DuplicateOriginalOf(ctx, a.checkWa)
	found, amount, client, needName, err := b.db.UndoReceiptDuplicate(ctx, a.checkWa)
	if err != nil || !found {
		if err != nil {
			fmt.Println("Возврат чека из повторов не удался:", err)
		}
		b.sendText(chat, "Этот чек уже не найден (удалён или исключён) — ничего не менял.")
		return true
	}
	if oerr == nil && orig.Found {
		// Экран снова отдельный перевод — его копии в других группах снова с ним.
		if err := b.db.MoveScreenCopiesBack(ctx, a.checkWa, orig.TxDate); err != nil {
			fmt.Println("Возврат времени копиям экрана не удался:", err)
		}
	}
	if needName {
		// «Это другой чек, Исаева Марьям» — клиента назвали в том же ответе.
		if rest := withoutSameCheckWords(text); rest != "" {
			if handled, _ := b.applyReceiptAnswer(ctx, chat, a.checkWa, rest); handled {
				return true
			}
		}
		b.askAboutReceipt(ctx, chat, a.checkWa, fmt.Sprintf("Понял, это отдельный платёж на %.0f ₽. Чей он? Напишите ФИО клиента — тогда засчитаю.", amount), false, true)
		return true
	}
	b.sendText(chat, fmt.Sprintf("Понял, это отдельный платёж — засчитал: %s, %.0f ₽.", client, amount))
	return true
}

// answerForRepeatedCheck — ответ на «чей чек?»/«какая сумма?» пришёл про чек,
// который за это время стал повтором (пришла его подписанная копия, автоповтор
// дочитал копию). Ответ человека не теряем:
//   - назван ДРУГОЙ клиент, чем у оставшегося в учёте, и общего номера операции
//     нет — разных клиентов не склеиваем: чек снова отдельный платёж;
//   - оставшийся в учёте ещё ничей — ответ относится к нему;
//   - иначе — говорим, что этот чек уже учтён (и на кого).
func (b *Bot) answerForRepeatedCheck(ctx context.Context, chat types.JID, waID, text, canonical string, cid *int) (handled, done bool) {
	o, err := b.db.DuplicateOriginalOf(ctx, waID)
	if err != nil || !o.Found {
		return false, false
	}
	// Имя, совместимое с клиентом оставшегося чека («Исаев» или «Харон» при
	// «Исаев Харон»), — тот же клиент: всё остальное уже совпало (работник,
	// сумма, перевод), второй платёж молча не заводим.
	sameClient := (cid != nil && o.KeptContactID != nil && *cid == *o.KeptContactID) ||
		sameClientMoney(o.KeptClient, canonical) || clientNamePartMoney(o.KeptClient, canonical)
	switch {
	case canonical != "" && o.KeptNamed && !o.Strong && !sameClient:
		if ok, _, _, _, err := b.db.UndoReceiptDuplicate(ctx, waID); err != nil || !ok {
			return false, false
		}
		fmt.Printf("Чек %s: назван другой клиент (%s), чем у такого же чека (%s) — считаю отдельным платежом\n", waID, canonical, o.KeptClient)
		return b.applyReceiptAnswer(ctx, chat, waID, text)
	case canonical != "" && !o.KeptNamed && o.KeptWa != waID:
		b.dropReceiptAsk(chat.String(), o.KeptWa)
		return b.applyReceiptAnswer(ctx, chat, o.KeptWa, text)
	}
	b.noteAnswer(ctx, "receipt", waID)
	who := o.KeptClient
	if !o.KeptNamed {
		who = "без клиента (ждёт ответа «чей чек?»)"
	}
	extra, fix := "", false
	switch {
	case o.Strong && canonical != "" && o.KeptNamed && !sameClient:
		// Тот же номер операции — платёж точно один; спорный только клиент.
		extra, fix = fmt.Sprintf(" Если клиент «%s» — ответьте на это сообщение «да», и я поправлю.", canonical), true
	case !o.Strong:
		// Номера нет — совпало по тому, что напечатано/названо: путь назад.
		extra = " Если это ДРУГОЙ платёж — ответьте на это сообщение «другой», и я засчитаю его отдельно."
	}
	id := b.sendTextReturnID(chat, fmt.Sprintf("Этот чек уже учтён: тот же чек — %s, %.0f ₽, чек от %s. Второй раз не считаю.%s",
		who, o.Amount, o.TxDate.Format("02.01.2006 15:04"), extra))
	if fix {
		b.registerDupFixAsk(id, dupFixAsk{checkWa: o.KeptWa, name: canonical})
	} else if extra != "" {
		b.registerSameCheckAsk(id, sameCheckAsk{checkWa: waID})
	}
	return true, true
}

// crossClientAsk — «этот чек уже прислан в другой группе как otherClient, а
// здесь подписан thisClient — чей он?».
type crossClientAsk struct {
	thisWa, otherWa         string // копия чека в этой группе и в другой
	otherGroup              string // JID другой группы
	thisClient, otherClient string
	group, sender           string    // где спросили и кого (кто прислал этот чек)
	at                      time.Time // когда спросили
}

func (b *Bot) registerCrossClientAsk(botMsgID string, a crossClientAsk) {
	if botMsgID == "" || a.thisWa == "" {
		return
	}
	b.clarify.mu.Lock()
	defer b.clarify.mu.Unlock()
	if len(b.clarify.crossAsks) >= clarifyMapCap {
		for k := range b.clarify.crossAsks {
			delete(b.clarify.crossAsks, k)
			break
		}
	}
	b.clarify.crossAsks[botMsgID] = a
}

// askCrossGroupClient — чек, подписанный здесь клиентом client, с тем же
// номером операции уже засчитан в ДРУГОЙ группе на другого клиента: один из
// работников ошибся. Платёж один (отчёт склеивает копии), но клиент чека — ФИО
// рядом с ним, а рядом с ним два разных ФИО: молча по первой подписи не решаем,
// спрашиваем в этой группе.
func (b *Bot) askCrossGroupClient(ctx context.Context, chat types.JID, rawID int, waMsgID, senderJID, client string, cid *int, docNumber, authCode string, amount float64, txDate time.Time) {
	if waMsgID == "" || client == "" || b.groupSilent(chat) {
		return
	}
	copies, err := b.db.NamedCopiesInOtherGroups(ctx, chat.String(), rawID, docNumber, authCode, amount, txDate, cid)
	if err != nil {
		fmt.Println("Проверка чека в других группах не удалась:", err)
		return
	}
	for _, o := range copies {
		if sameClientMoney(o.Client, client) || clientNamePartMoney(o.Client, client) {
			// Тот же клиент, записанный иначе («Харон» и «Исаев Харон»), — у чека
			// один номер операции, спорить не о чем.
			continue
		}
		where := o.GroupJID
		if j, err := types.ParseJID(o.GroupJID); err == nil {
			where = b.groupLabel(ctx, j)
		}
		id := b.sendReply(chat, fmt.Sprintf(
			"🤔 Этот чек уже прислан в %s как %s, а здесь подписан %s. Чей он — %s или %s? "+
				"Ответьте на это сообщение ФИО клиента — поправлю чек в обеих группах.",
			where, o.Client, client, o.Client, client), waMsgID, senderJID)
		fmt.Printf("Чек %s: тот же чек в %s записан на %s, здесь — на %s: спросил, чей он\n", waMsgID, o.GroupJID, o.Client, client)
		if id == "" {
			return
		}
		// Вопрос — только к тому, кто прислал чек: в «последний вопрос группы»
		// (ответ без свайпа) его не ставим — он отнял бы ответ без свайпа у
		// открытых «чей чек?» / «новый или тот же?» других работников. ФИО без
		// свайпа от него разбирает crossCandidate.
		b.registerCrossClientAsk(id, crossClientAsk{
			thisWa: waMsgID, otherWa: o.WaMessage, otherGroup: o.GroupJID,
			thisClient: client, otherClient: o.Client,
			group: chat.String(), sender: senderJID, at: time.Now(),
		})
		// Вопрос переживает перезапуск (ответ свайпом найдёт его в базе).
		if err := b.db.SetCrossAsk(ctx, waMsgID, id); err != nil {
			fmt.Println("Не запомнил вопрос «чей он?» по чеку:", err)
		}
		return
	}
}

// crossKeepWords — «оставь как есть»: ответ не меняет ни одну копию.
var crossKeepWords = []string{"оставь", "оставьте", "как было", "как есть", "не трогай"}

// applyCrossClientAnswer — ответ на «этот чек уже прислан в другой группе как
// X — чей он?»: названный клиент становится клиентом ОБЕИХ копий чека, а
// текст «ФИО сумма», который отчёт считал одним платежом с этим чеком, — тоже
// (иначе текст со старым ФИО остался бы вторым платежом). swipe — ответили
// свайпом на вопрос; без свайпа принимаем только одно из двух названных ФИО
// (или «оставь»): другое имя — скорее, подпись к следующему чеку.
func (b *Bot) applyCrossClientAnswer(ctx context.Context, chat types.JID, askID string, a crossClientAsk, text string, swipe bool) bool {
	done := func() {
		b.clarify.mu.Lock()
		delete(b.clarify.crossAsks, askID)
		for k, c := range b.clarify.crossCands {
			if c.askID == askID {
				delete(b.clarify.crossCands, k)
			}
		}
		b.clarify.mu.Unlock()
		if err := b.db.ClearCrossAsk(ctx, askID); err != nil {
			fmt.Println("Не снял вопрос «чей он?» по чеку:", err)
		}
	}
	low := cleanAnswer(text)
	for _, w := range crossKeepWords {
		if strings.Contains(low, w) {
			done()
			b.sendText(chat, "Ок, оставил как было.")
			return true
		}
	}
	rest := withoutSameCheckWords(text)
	if rest == "" {
		return false
	}
	is := func(name string) bool {
		return name != "" && (sameClientMoney(rest, name) || clientNamePartMoney(rest, name))
	}
	name := ""
	switch this, other := is(a.thisClient), is(a.otherClient); {
	case this && other:
		if !swipe {
			return false
		}
		b.sendText(chat, fmt.Sprintf("Не понял, чей чек. Ответьте на вопрос одним ФИО: %s или %s.", a.otherClient, a.thisClient))
		return true
	case this:
		name = a.thisClient
	case other:
		name = a.otherClient
	default:
		if !swipe {
			return false
		}
		// Назвали третьего клиента — ему и принадлежит чек.
		c, ok := b.confirmClientName(ctx, rest)
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
	copies := []struct{ wa, group string }{{a.thisWa, chat.String()}, {a.otherWa, a.otherGroup}}
	// Тексты, которые отчёт сейчас считает одним платежом с этим чеком, — до
	// поправки (после неё пара по клиенту уже не найдётся).
	var was []string
	groups := map[string]bool{}
	for _, cp := range copies {
		if cp.wa != "" {
			was = append(was, cp.wa)
			groups[cp.group] = true
		}
	}
	// This validated reply names the client of the identified physical
	// receipt. Preserve previously established source-text links through the
	// resolution; no automatic path gets this explicit-resolution authority.
	texts := b.crossPairedTexts(ctx, was, groups, canonical, cid, true)
	fixed := 0
	for _, cp := range copies {
		if cp.wa == "" {
			continue
		}
		found, _, err := b.db.ReattributeReceiptByMessageFrom(ctx, cp.wa, canonical, cid, 0)
		if err != nil {
			fmt.Println("Поправка клиента чека не удалась:", err)
			continue
		}
		if !found {
			continue
		}
		fixed++
		if g, err := types.ParseJID(cp.group); err == nil {
			b.reresolveWatchByCheck(ctx, g, cp.wa, canonical)
		}
	}
	done()
	if fixed == 0 {
		b.sendText(chat, "Этот чек уже не найден (удалён или исключён) — ничего не менял.")
		return true
	}
	moved := b.moveCrossPairedTexts(ctx, chat, texts, canonical, cid)
	where := a.otherGroup
	if j, err := types.ParseJID(a.otherGroup); err == nil {
		where = b.groupLabel(ctx, j)
	}
	fmt.Printf("Чек %s: клиент %s — поправил и копию в %s\n", a.thisWa, canonical, a.otherGroup)
	b.sendText(chat, fmt.Sprintf("Поправил: чек на клиента %s — и здесь, и в %s.%s", canonical, where, moved))
	return true
}

// weakSameMode — как поступать в группе chat со слабым совпадением «похоже,
// тот же перевод» (общего номера нет, у одного из чеков время операции не
// прочиталось): держать повтором и спросить «тот же или другой?»; в группе на
// молчании — отложить в «НЕ вошло» и спросить, когда молчание снимут
// (askParkedSameChecks), а не угадывать молча. Вопросы по чекам выключены
// совсем (ASK_UNNAMED_RECEIPTS=0) — повтором без вопроса, как раньше (виден в
// «НЕ вошло» как повтор).
func (b *Bot) weakSameMode(chat types.JID) db.WeakSame {
	if askReceiptsEnabled() && b.groupSilent(chat) {
		return db.WeakPark
	}
	return db.WeakHold
}

// dedupeNamedReceipt — чек rid только что назвали (ФИО следующим сообщением,
// свайпом, «ФИО сумма» рядом): тот же перевод мог уже войти в сбор другим чеком
// того же клиента — экраном «Перевод выполнен» без даты или полным чеком.
// Второй раз не считаем; это догадка — спрашиваем «тот же или другой?» (группа
// на молчании — чек ждёт в «НЕ вошло»). Возвращает, что нашлось.
func (b *Bot) dedupeNamedReceipt(ctx context.Context, chat types.JID, rid int) db.SameCheck {
	if rid <= 0 || b.db == nil {
		return db.SameCheck{}
	}
	same, err := b.db.DedupeCompletedReceiptWeak(ctx, rid, false, b.weakSameMode(chat))
	if err != nil {
		fmt.Println("Проверка «тот же перевод» после ФИО не удалась:", err)
		return db.SameCheck{}
	}
	if same.Found {
		waID, senderJID, _, _ := b.db.ReceiptReplyRef(ctx, rid)
		b.reportSameCheck(ctx, chat, waID, senderJID, same, false)
	}
	return same
}

// reportReleasedHolds — чек checkWa записали на другого клиента, а эти чеки
// считались тем же переводом только потому, что клиент и сумма совпадали:
// разных клиентов не склеиваем — они снова отдельные платежи. Говорим об этом.
func (b *Bot) reportReleasedHolds(chat types.JID, checkWa, senderJID, newClient string, rel []db.ReleasedHold) {
	if len(rel) == 0 || b.groupSilent(chat) {
		return
	}
	for _, r := range rel {
		b.dropReceiptAsk(chat.String(), r.WaMessageID)
		text := fmt.Sprintf("Чек на %.0f ₽ (%s) я считал тем же переводом, что этот чек. Теперь этот чек — %s, поэтому тот снова считаю "+
			"отдельным платежом: %s, %.0f ₽. Если тот чек тоже %s — ответьте на него ФИО, и я сверю.",
			r.Amount, r.Client, newClient, r.Client, r.Amount, newClient)
		if senderJID == "" || checkWa == "" {
			b.sendText(chat, text)
		} else {
			b.sendReply(chat, text, checkWa, senderJID)
		}
	}
}

// heldScreen — свёрнутый экран, признанный тем же переводом, что засчитанный
// полный чек (бот сказал «Похоже, это тот же перевод…»). Голое ФИО от того же
// работника следом может быть и про экран, и именем для его следующего чека.
type heldScreen struct {
	screenWa   string
	keptClient string
	amount     float64
	txDate     time.Time
	at         time.Time
}

// heldNameAsk — «ФИО — это клиент экрана выше или имя для следующего чека?».
type heldNameAsk struct {
	screenWa   string
	name       string // клиент из ФИО
	keptClient string
	nameRawID  int // сообщение с ФИО (оно же ждёт чек в очереди имён)
	senderJID  string
	amount     float64
}

func heldKey(groupJID, senderJID string) string { return groupJID + "|" + senderJID }

// rememberHeldScreen — экран признан тем же переводом, что полный чек, вопрос
// задан.
func (b *Bot) rememberHeldScreen(groupJID, senderJID string, h heldScreen) {
	if senderJID == "" || h.screenWa == "" {
		return
	}
	h.at = time.Now()
	b.clarify.mu.Lock()
	defer b.clarify.mu.Unlock()
	if len(b.clarify.heldScreens) >= clarifyMapCap {
		for k := range b.clarify.heldScreens {
			delete(b.clarify.heldScreens, k)
			break
		}
	}
	b.clarify.heldScreens[heldKey(groupJID, senderJID)] = h
}

// takeHeldScreen — экран этого работника, о котором голое ФИО сейчас может
// быть ответом: вопрос свежий, на него не ответили и чеков после экрана работник
// не присылал. Берётся один раз.
func (b *Bot) takeHeldScreen(ctx context.Context, groupJID, senderJID string) (heldScreen, bool) {
	k := heldKey(groupJID, senderJID)
	b.clarify.mu.Lock()
	h, ok := b.clarify.heldScreens[k]
	delete(b.clarify.heldScreens, k)
	b.clarify.mu.Unlock()
	if !ok || time.Since(h.at) > contextAnswerWindow || b.db == nil {
		return heldScreen{}, false
	}
	pending, err := b.db.HeldScreenPending(ctx, h.screenWa)
	if err != nil || !pending {
		return heldScreen{}, false
	}
	return h, true
}

// nameAfterHeldScreen — голое ФИО (без суммы и свайпа) от работника, чей
// свёрнутый экран бот только что признал тем же переводом, что полный чек, а
// ждущего чека рядом нет. Тот же клиент, что у полного чека (и короче:
// «Исаев», «Харон»), — это про экран: он уже учтён. ДРУГОЙ клиент — непонятно:
// это клиент экрана (тогда это другой платёж) или имя для следующего чека
// (так работники и пишут — ФИО, потом чек). Не угадываем: имя ждёт следующий
// чек, как обычно, а бот спрашивает, не про экран ли оно («да» — экран
// засчитывается этому клиенту). Возвращает true, если сообщение обработано.
func (b *Bot) nameAfterHeldScreen(ctx context.Context, msg *events.Message, name, canonical string, cid *int, rawID int) bool {
	chat := msg.Info.Chat
	sender := msg.Info.Sender.String()
	if b.groupSilent(chat) {
		return false
	}
	h, ok := b.takeHeldScreen(ctx, chat.String(), sender)
	if !ok {
		return false
	}
	if sameClientMoney(h.keptClient, canonical) || clientNamePartMoney(h.keptClient, canonical) {
		handled, _ := b.answerForRepeatedCheck(ctx, chat, h.screenWa, canonical, canonical, cid)
		if handled {
			_ = b.db.MarkMessageParsed(ctx, rawID)
		}
		return handled
	}
	b.enqueuePendingName(ctx, msg, name, 0, rawID)
	_ = b.db.MarkMessageParsed(ctx, rawID)
	id := b.sendReply(chat, fmt.Sprintf(
		"🤔 «%s» — это клиент экрана на %.0f ₽ выше? Я считаю тот экран тем же переводом, что чек %s (%s). "+
			"Если экран — ДРУГОЙ платёж, клиента %s, — ответьте на это сообщение «да», и я засчитаю его отдельно. "+
			"Если «%s» — для следующего чека, просто пришлите чек.",
		canonical, h.amount, h.keptClient, h.txDate.Format("02.01.2006 15:04"), canonical, canonical), msg.Info.ID, sender)
	if id == "" {
		return true
	}
	b.clarify.mu.Lock()
	if len(b.clarify.heldNameAsks) >= clarifyMapCap {
		for k := range b.clarify.heldNameAsks {
			delete(b.clarify.heldNameAsks, k)
			break
		}
	}
	b.clarify.heldNameAsks[id] = heldNameAsk{screenWa: h.screenWa, name: canonical, keptClient: h.keptClient,
		nameRawID: rawID, senderJID: sender, amount: h.amount}
	b.clarify.mu.Unlock()
	fmt.Printf("ФИО %q после экрана того же перевода (%s): жду чек и спросил, не про экран ли оно\n", canonical, h.screenWa)
	return true
}

// parseYesNo — «да»/«нет» на вопрос бота (resolved=false — не понятно).
func parseYesNo(text string) (resolved, yes bool) {
	t := strings.ReplaceAll(strings.ToLower(strings.TrimSpace(text)), "ё", "е")
	toks := strings.FieldsFunc(t, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
	if len(toks) > 1 && toks[0] == "это" {
		toks = toks[1:] // «это он», «это не он»
	}
	if len(toks) == 0 {
		return false, false
	}
	switch toks[0] {
	case "да", "ага", "угу", "верно", "точно", "он", "она", "его", "ее", "экран":
		return true, true
	case "нет", "неа", "не", "следующий", "следующего", "следующему", "для":
		return true, false
	}
	if isConfirmReply(t) {
		return true, true
	}
	return false, false
}

// applyHeldNameAnswer — ответ на «ФИО — это клиент экрана выше?»: «да» — экран
// засчитывается этому клиенту отдельным платежом (а имя больше не ждёт
// следующий чек); «нет» — экран остаётся тем же переводом, имя ждёт чек.
func (b *Bot) applyHeldNameAnswer(ctx context.Context, chat types.JID, askID string, a heldNameAsk, text string) bool {
	resolved, yes := parseYesNo(text)
	if !resolved {
		return false
	}
	b.clarify.mu.Lock()
	delete(b.clarify.heldNameAsks, askID)
	b.clarify.mu.Unlock()
	if !yes {
		b.sendText(chat, fmt.Sprintf("Понял: экран на %.0f ₽ — тот же перевод (%s), «%s» жду для следующего чека.", a.amount, a.keptClient, a.name))
		return true
	}
	found, _, _, _, err := b.db.UndoReceiptDuplicate(ctx, a.screenWa)
	if err != nil || !found {
		if err != nil {
			fmt.Println("Возврат экрана из повторов не удался:", err)
		}
		b.sendText(chat, "Этот экран уже не найден или уже учтён отдельно — ничего не менял.")
		return true
	}
	// Имя ушло на экран — следующему чеку его не отдаём.
	if ps, err := b.db.PendingNamesOf(ctx, chat.String(), a.senderJID, time.Now().Add(-b.pairWindow)); err == nil {
		for _, p := range ps {
			if p.RawID == a.nameRawID {
				_ = b.db.ConsumePendingNameByID(ctx, p.ID)
				break
			}
		}
	}
	if handled, _ := b.applyReceiptAnswer(ctx, chat, a.screenWa, a.name); handled {
		return true
	}
	b.askAboutReceipt(ctx, chat, a.screenWa, fmt.Sprintf("Понял, экран на %.0f ₽ — отдельный платёж. Чей он? Напишите ФИО клиента — тогда засчитаю.", a.amount), false, true)
	return true
}

// askParkedSameChecks — чеки группы, отложенные в «НЕ вошло» догадкой «похоже,
// тот же перевод», пока группа молчала (db.WeakPark): теперь писать можно —
// говорим, что считаем перевод один раз, и ждём «другой». Чек, с которым
// совпало, уже не в сборе на того же клиента — догадки нет, чек идёт в сбор.
// Возвращает, сколько вопросов задано.
func (b *Bot) askParkedSameChecks(ctx context.Context, jid types.JID, limit int) int {
	if limit <= 0 || b.db == nil {
		return 0
	}
	items, err := b.db.ParkedSameChecks(ctx, jid.String(), limit)
	if err != nil {
		fmt.Println("Отложенные «тот же перевод?» чеки: ошибка выборки:", err)
		return 0
	}
	asked := 0
	for _, it := range items {
		if !it.KeptOK {
			if err := b.db.UnparkSameCheck(ctx, it.ID); err != nil {
				fmt.Println("Возврат отложенного чека в сбор не удался:", err)
			}
			continue
		}
		id := b.sendReply(jid, fmt.Sprintf(
			"🤔 Пока я молчал в группе: этот чек на %.0f ₽ (%s), похоже, тот же перевод, что чек %s от %s, — считаю его один раз. "+
				"Если это ДРУГОЙ платёж — ответьте на это сообщение «другой», и я засчитаю его отдельно.",
			it.Amount, it.Client, it.KeptClient, it.KeptTxDate.Format("02.01.2006 15:04")), it.WaMessageID, it.SenderJID)
		if id == "" {
			continue // не ушло — спросим в следующий раз, чек ждёт в «НЕ вошло»
		}
		if ok, err := b.db.HoldParkedSameCheck(ctx, it.ID); err != nil || !ok {
			if err != nil {
				fmt.Println("Отложенный чек не стал повтором:", err)
			}
			continue
		}
		b.registerSameCheckAsk(id, sameCheckAsk{checkWa: it.WaMessageID})
		asked++
	}
	return asked
}
