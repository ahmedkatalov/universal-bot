// «Тот же чек или другой?»: чек стал полным/чьим не при приёме (автоповтор
// распознавания, ответ суммой или ФИО) и оказался тем же чеком, что уже учтён в
// группе. Сообщаем об этом в группе; если уверенности нет — спрашиваем, и ответ
// «другой» возвращает чек в сбор отдельным платежом.
package bot

import (
	"context"
	"fmt"
	"strings"
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
