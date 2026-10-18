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
	defer b.clarify.mu.Unlock()
	if len(b.clarify.sameAsks) >= clarifyMapCap {
		for k := range b.clarify.sameAsks {
			delete(b.clarify.sameAsks, k)
			break
		}
	}
	b.clarify.sameAsks[botMsgID] = a
}

// reportSameCheck — сказать в группе, что чек checkWa (или его ничья копия)
// оказался повтором уже учтённого. reread — нашёл автоповтор распознавания.
// Возвращает id сообщения бота.
func (b *Bot) reportSameCheck(ctx context.Context, chat types.JID, checkWa, senderJID string, same db.SameCheck, reread bool) string {
	if !same.Found {
		return ""
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
	found, amount, client, needName, err := b.db.UndoReceiptDuplicate(ctx, a.checkWa)
	if err != nil || !found {
		if err != nil {
			fmt.Println("Возврат чека из повторов не удался:", err)
		}
		b.sendText(chat, "Этот чек уже не найден (удалён или исключён) — ничего не менял.")
		return true
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
	sameClient := (cid != nil && o.KeptContactID != nil && *cid == *o.KeptContactID) || sameClientMoney(o.KeptClient, canonical)
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
		b.registerCrossClientAsk(id, crossClientAsk{
			thisWa: waMsgID, otherWa: o.WaMessage, otherGroup: o.GroupJID,
			thisClient: client, otherClient: o.Client,
		})
		return
	}
}

// crossKeepWords — «оставь как есть»: ответ не меняет ни одну копию.
var crossKeepWords = []string{"оставь", "оставьте", "как было", "как есть", "не трогай"}

// applyCrossClientAnswer — ответ на «этот чек уже прислан в другой группе как
// X — чей он?»: названный клиент становится клиентом ОБЕИХ копий чека.
func (b *Bot) applyCrossClientAnswer(ctx context.Context, chat types.JID, askID string, a crossClientAsk, text string) bool {
	done := func() {
		b.clarify.mu.Lock()
		delete(b.clarify.crossAsks, askID)
		b.clarify.mu.Unlock()
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
	is := func(name string) bool { return sameClientMoney(rest, name) || clientNamePartMoney(rest, name) }
	name := ""
	switch this, other := is(a.thisClient), is(a.otherClient); {
	case this && other:
		b.sendText(chat, fmt.Sprintf("Не понял, чей чек. Ответьте на вопрос одним ФИО: %s или %s.", a.otherClient, a.thisClient))
		return true
	case this:
		name = a.thisClient
	case other:
		name = a.otherClient
	default:
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
	fixed := 0
	for _, cp := range []struct{ wa, group string }{{a.thisWa, chat.String()}, {a.otherWa, a.otherGroup}} {
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
	where := a.otherGroup
	if j, err := types.ParseJID(a.otherGroup); err == nil {
		where = b.groupLabel(ctx, j)
	}
	fmt.Printf("Чек %s: клиент %s — поправил и копию в %s\n", a.thisWa, canonical, a.otherGroup)
	b.sendText(chat, fmt.Sprintf("Поправил: чек на клиента %s — и здесь, и в %s.", canonical, where))
	return true
}
