// Тот же чек (общий номер операции) в двух группах подписан разными клиентами:
// вопрос «чей он?» задаём, когда чек стал чьим любым путём (подпись, имя
// следом, свайп, ответ на «чей чек?»), ответ принимаем и без свайпа, и после
// перезапуска, а поправка клиента переносит вместе с чеком и текст «ФИО сумма»,
// который был с ним одним платежом.
package bot

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"

	"whatsapp-bot/internal/db"
)

// crossCheckNamed — чек сообщения waID (или, если waID пусто, сообщения rawID)
// только что стал чьим не при приёме: если тот же чек уже засчитан в другой
// группе на другого клиента — спрашиваем «чей он?», как и для подписанного чека.
func (b *Bot) crossCheckNamed(ctx context.Context, chat types.JID, waID string, rawID int) {
	c, err := b.db.CrossCheckOf(ctx, waID, rawID)
	if err != nil {
		fmt.Println("Проверка чека в других группах не удалась:", err)
		return
	}
	b.crossAskIfNeeded(ctx, chat, c)
}

func (b *Bot) crossAskIfNeeded(ctx context.Context, chat types.JID, c db.CrossCheck) {
	if !c.Found || !c.Named || c.Asked || (c.DocNumber == "" && c.AuthCode == "") || c.SenderJID == "" || c.SenderJID == "bot-forward" {
		return
	}
	b.askCrossGroupClient(ctx, chat, c.RawID, c.WaID, c.SenderJID, c.Client, c.ContactID, c.DocNumber, c.AuthCode, c.Amount, c.TxDate)
}

// afterCheckNamed — чек сообщения waID (или rawID) назвали после приёма (имя
// следом, свайп с ФИО, ФИО рядом, ответ из соседнего сообщения): при приёме у
// него был клиентом владелец карты, и ни экран того же перевода, ни его копия
// в другой группе с ним не сошлись. Теперь:
//   - тот же перевод того же клиента уже в сборе (dedupeNamedReceipt): экран
//     «Перевод выполнен» без времени и номера, засчитанный раньше, — повтор (в
//     учёте остаётся этот полный чек), или этот чек без даты — повтор
//     засчитанного полного; это догадка — спрашиваем «тот же или другой?»
//     (группа на молчании — ждёт в «НЕ вошло», спросим потом);
//   - тот же чек в другой группе на другого клиента — «чей он?» (если этот
//     чек остался в сборе).
//
// Возвращает, что нашла проверка «тот же перевод» (Found && VictimIsThis —
// этот чек в сбор не пошёл).
func (b *Bot) afterCheckNamed(ctx context.Context, chat types.JID, waID string, rawID int) db.SameCheck {
	c, err := b.db.CrossCheckOf(ctx, waID, rawID)
	if err != nil {
		fmt.Println("Проверка чека после привязки клиента не удалась:", err)
		return db.SameCheck{}
	}
	if !c.Found {
		return db.SameCheck{}
	}
	same := b.dedupeNamedReceipt(ctx, chat, c.ID)
	if same.Found && same.VictimIsThis {
		return same
	}
	b.crossAskIfNeeded(ctx, chat, c)
	return same
}

// crossAskFromDB — вопрос «чей он?» askID, которого нет в памяти (бот
// перезапускался): чек, по которому спросили, и его копия в другой группе,
// записанная на другого клиента.
func (b *Bot) crossAskFromDB(ctx context.Context, askID string) (crossClientAsk, bool) {
	c, err := b.db.CrossAskCheck(ctx, askID)
	if err != nil || !c.Found {
		return crossClientAsk{}, false
	}
	a := crossClientAsk{thisWa: c.WaID, thisClient: c.Client, group: c.GroupJID, sender: c.SenderJID}
	copies, err := b.db.NamedCopiesInOtherGroups(ctx, c.GroupJID, c.RawID, c.DocNumber, c.AuthCode, c.Amount, c.TxDate, c.ContactID)
	if err != nil {
		fmt.Println("Проверка чека в других группах не удалась:", err)
	}
	for _, o := range copies {
		if sameClientMoney(o.Client, c.Client) || clientNamePartMoney(o.Client, c.Client) {
			continue
		}
		a.otherWa, a.otherGroup, a.otherClient = o.WaMessage, o.GroupJID, o.Client
		break
	}
	return a, true
}

// crossPairedText — текст «ФИО сумма», который отчёт считает одним платежом с
// чеком.
type crossPairedText struct {
	id     int
	group  string
	client string
	amount float64
}

// crossPairedTexts — тексты в группах groups, которые отчёт сейчас считает
// одним платежом с чеком сообщений was (его копиями), а клиент в них — не
// canonical. Поправим клиента чека — их пара по клиенту пропадёт, и текст
// со старым ФИО стал бы вторым платежом.
func (b *Bot) crossPairedTexts(ctx context.Context, was []string, groups map[string]bool, canonical string, cid *int) []crossPairedText {
	if len(was) == 0 {
		return nil
	}
	// Окно отчёта — вокруг и даты операции, и времени присылки каждой копии:
	// старый чек присылают через неделю после операции, а текст «ФИО сумма»
	// пишут, когда его прислали.
	var from, to time.Time
	widen := func(t time.Time) {
		if t.IsZero() {
			return
		}
		if from.IsZero() || t.Before(from) {
			from = t
		}
		if to.IsZero() || t.After(to) {
			to = t
		}
	}
	for _, w := range was {
		c, err := b.db.CrossCheckOf(ctx, w, 0)
		if err != nil || !c.Found {
			continue
		}
		widen(c.TxDate)
		widen(c.ReceivedAt)
	}
	if from.IsZero() {
		return nil
	}
	book, err := b.db.LoadBook(ctx, from.Add(-48*time.Hour), to.Add(48*time.Hour), "")
	if err != nil {
		fmt.Println("Поиск текста про этот чек не удался:", err)
		return nil
	}
	isCopy := map[string]bool{}
	for _, w := range was {
		isCopy[w] = true
	}
	var out []crossPairedText
	seen := map[int]bool{}
	for _, p := range book.Payments {
		hit := false
		for _, ch := range p.Checks {
			if isCopy[ch.WaID] {
				hit = true
				break
			}
		}
		if !hit {
			continue
		}
		for _, t := range p.Texts {
			if t.Cash || seen[t.ID] || !groups[t.Group] || math.Abs(t.Amount-p.Amount) >= 0.5 {
				continue
			}
			if (cid != nil && t.ContactID == *cid) || sameClientMoney(t.Client, canonical) || clientNamePartMoney(t.Client, canonical) {
				continue // и после поправки останется парой чеку
			}
			seen[t.ID] = true
			out = append(out, crossPairedText{id: t.ID, group: t.Group, client: t.Client, amount: t.Amount})
		}
	}
	return out
}

// moveCrossPairedTexts — тексты про этот чек переписываем на canonical (это
// один платёж с чеком) и говорим об этом в ответе (возвращает дописку к нему).
func (b *Bot) moveCrossPairedTexts(ctx context.Context, chat types.JID, texts []crossPairedText, canonical string, cid *int) string {
	if cid == nil || len(texts) == 0 {
		return ""
	}
	var notes []string
	for _, t := range texts {
		if err := b.db.SetTxClient(ctx, t.id, *cid, canonical); err != nil {
			fmt.Println("Поправка клиента текста про чек не удалась:", err)
			continue
		}
		where := "здесь"
		if t.group != chat.String() {
			where = "в " + t.group
			if j, err := types.ParseJID(t.group); err == nil {
				where = "в " + b.groupLabel(ctx, j)
			}
		}
		fmt.Printf("Текст #%d (%s %.0f) — про тот же чек: клиент %s\n", t.id, t.client, t.amount, canonical)
		notes = append(notes, fmt.Sprintf("«%s %.0f» %s", t.client, t.amount, where))
	}
	if len(notes) == 0 {
		return ""
	}
	return fmt.Sprintf(" Запись %s — про этот же чек, её тоже записал на %s (один платёж).", strings.Join(notes, ", "), canonical)
}

// crossCandidate — после «Чей он?» тот, кого спросили, без свайпа написал одно
// из двух названных ФИО. Это может быть ответ на вопрос — а может быть имя
// перед следующим чеком (работник продолжает работу, следующий клиент — тот же):
// сразу не решаем. Имя ждёт в очереди «имя перед чеком», как всегда, а вопрос
// остаётся открытым, пока не станет ясно:
//   - следующий чек без подписи взял это имя — имя было для него;
//   - а следом за этим чеком назвали ДРУГОЕ ФИО (именем после чека или свайпом
//     на него) — значит, то ФИО было ответом на «Чей он?», а чек — нового;
//   - имя так и не досталось чеку (окно очереди прошло) — это был ответ.
type crossCandidate struct {
	askID  string
	group  string
	sender string
	text   string    // ответ для «Чей он?» (одно из двух ФИО)
	name   string    // каноническое ФИО
	rawID  int       // сообщение с ФИО (его запись в очереди имён)
	msgAt  time.Time // время этого сообщения
	at     time.Time
}

func crossCandKey(group, sender string) string { return group + "|" + sender }

type crossAskRef struct {
	id string
	a  crossClientAsk
}

// openCrossAsks — открытые (свежие) вопросы «Чей он?» группы к sender (или ко
// всем — для владельца), самые свежие первыми.
func (b *Bot) openCrossAsks(group, sender string, admin bool) []crossAskRef {
	b.clarify.mu.Lock()
	defer b.clarify.mu.Unlock()
	var out []crossAskRef
	for id, a := range b.clarify.crossAsks {
		if a.group != group || time.Since(a.at) > contextAnswerWindow || (!admin && (sender == "" || a.sender != sender)) {
			continue
		}
		out = append(out, crossAskRef{id, a})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].a.at.After(out[j].a.at) })
	return out
}

// crossAskByID — открытый вопрос «Чей он?» askID (из памяти или из базы).
func (b *Bot) crossAskByID(ctx context.Context, askID string) (crossClientAsk, bool) {
	b.clarify.mu.Lock()
	a, ok := b.clarify.crossAsks[askID]
	b.clarify.mu.Unlock()
	if ok {
		return a, true
	}
	return b.crossAskFromDB(ctx, askID)
}

// crossNamesOne — ответ text называет ровно одно из двух ФИО вопроса.
func crossNamesOne(a crossClientAsk, text string) bool {
	rest := withoutSameCheckWords(text)
	if rest == "" {
		return false
	}
	is := func(name string) bool {
		return name != "" && (sameClientMoney(rest, name) || clientNamePartMoney(rest, name))
	}
	return is(a.thisClient) != is(a.otherClient)
}

// crossKeepReply — «оставь», «как есть».
func crossKeepReply(text string) bool {
	low := cleanAnswer(text)
	for _, w := range crossKeepWords {
		if strings.Contains(low, w) {
			return true
		}
	}
	return false
}

// tryCrossAnswerNoSwipe — ответ без свайпа на «Чей он?», в котором нет
// сомнений: «оставь»/«как есть» от того, кого спросили; ФИО (одно из двух) от
// владельца или обращением к боту («Джарвис, Исаев Харон»). Голое ФИО
// работника — не здесь: оно может быть и именем перед следующим чеком (см.
// crossCandidate).
func (b *Bot) tryCrossAnswerNoSwipe(ctx context.Context, msg *events.Message, text string, addressed bool) bool {
	if b.db == nil || !shortPlainReply(text, 4) {
		return false
	}
	group, sender := msg.Info.Chat.String(), msg.Info.Sender.String()
	admin := b.isReportAdmin(msg.Info)
	asks := b.openCrossAsks(group, sender, admin)
	if len(asks) == 0 {
		return false
	}
	if !crossKeepReply(text) {
		if !admin && !addressed {
			return false
		}
		if !admin {
			// Ждёт чек без клиента от него же — ФИО для этого чека.
			if has, err := b.db.HasUnconfirmedReceiptFrom(ctx, group, sender, time.Now().Add(-b.pairWindow)); err != nil || has {
				return false
			}
		}
	}
	for _, x := range asks {
		if b.applyCrossClientAnswer(ctx, msg.Info.Chat, x.id, x.a, text, false) {
			return true
		}
	}
	return false
}

// noteCrossCandidate — ФИО name (без суммы) ушло в очередь «имя перед чеком», а
// отправителя только что спросили «Чей он?», и это одно из двух названных ФИО:
// запоминаем — может оказаться ответом (см. crossCandidate).
func (b *Bot) noteCrossCandidate(msg *events.Message, text, name string, rawID int) {
	group, sender := msg.Info.Chat.String(), msg.Info.Sender.String()
	for _, x := range b.openCrossAsks(group, sender, false) {
		answer := name
		if !crossNamesOne(x.a, name) {
			if !crossNamesOne(x.a, text) {
				continue
			}
			answer = text
		}
		b.clarify.mu.Lock()
		b.clarify.crossCands[crossCandKey(group, sender)] = crossCandidate{
			askID: x.id, group: group, sender: sender, text: answer, name: name,
			rawID: rawID, msgAt: msg.Info.Timestamp, at: time.Now(),
		}
		b.clarify.mu.Unlock()
		fmt.Printf("«%s» без свайпа после «Чей он?» (%s): ответ или имя перед следующим чеком — подождём\n", name, x.a.thisWa)
		return
	}
}

func (b *Bot) crossCandidateOf(group, sender string) (crossCandidate, bool) {
	b.clarify.mu.Lock()
	defer b.clarify.mu.Unlock()
	c, ok := b.clarify.crossCands[crossCandKey(group, sender)]
	return c, ok
}

func (b *Bot) dropCrossCandidate(group, sender string) {
	b.clarify.mu.Lock()
	delete(b.clarify.crossCands, crossCandKey(group, sender))
	b.clarify.mu.Unlock()
}

// crossCandidateResolved — чек ck взял из очереди ФИО кандидата cand, а теперь
// его назвали ДРУГИМ клиентом canonical (именем следом или свайпом): то ФИО было
// ответом на «Чей он?». Применяем его.
func (b *Bot) crossCandidateResolved(ctx context.Context, chat types.JID, cand crossCandidate, ck db.CrossCheck, canonical string, cid *int) bool {
	if !ck.Found || ck.SourceRawID != cand.rawID ||
		(cid != nil && ck.ContactID != nil && *cid == *ck.ContactID) || sameClientMoney(ck.Client, canonical) {
		return false
	}
	ca, open := b.crossAskByID(ctx, cand.askID)
	b.dropCrossCandidate(cand.group, cand.sender)
	if !open {
		return false
	}
	fmt.Printf("Чек %s: ФИО перед ним «%s» было ответом на «Чей он?», а чек — %s\n", ck.WaID, cand.name, canonical)
	return b.applyCrossClientAnswer(ctx, chat, cand.askID, ca, cand.text, false)
}

// crossCandidateNextName — отправитель написал ФИО canonical без свайпа, когда
// его последний чек взял из очереди ФИО-кандидата (см. crossCandidate): это ФИО
// — клиент того чека (имя после чека), а ФИО-кандидат — ответ на «Чей он?».
// true — чек перепривязан, вопрос «Чей он?» отвечен.
func (b *Bot) crossCandidateNextName(ctx context.Context, msg *events.Message, canonical string, cid *int, rawID int) bool {
	chat := msg.Info.Chat
	group, sender := chat.String(), msg.Info.Sender.String()
	cand, ok := b.crossCandidateOf(group, sender)
	if !ok || b.db == nil {
		return false
	}
	ck, err := b.db.LatestCheckFrom(ctx, group, sender, cand.msgAt.Add(-time.Minute))
	if err != nil || !ck.Found || ck.SourceRawID != cand.rawID || msg.Info.Timestamp.Sub(ck.ReceivedAt) > b.pairWindow {
		return false
	}
	if (cid != nil && ck.ContactID != nil && *cid == *ck.ContactID) || sameClientMoney(ck.Client, canonical) {
		return false
	}
	found, _, err := b.db.ReattributeReceiptByMessageFrom(ctx, ck.WaID, canonical, cid, rawID)
	if err != nil || !found {
		return false
	}
	if !b.reresolveWatchByCheck(ctx, chat, ck.WaID, canonical) {
		fmt.Printf("Чек переатрибутирован на клиента %q (ФИО после чека, а не перед ним)\n", canonical)
	}
	b.crossCandidateResolved(ctx, chat, cand, ck, canonical, cid)
	b.afterCheckNamed(ctx, chat, ck.WaID, 0)
	return true
}

// settleCrossCandidates — ФИО-кандидаты, которые за окно очереди так и не
// достались чеку: это был ответ на «Чей он?» (снимаем имя из очереди и
// применяем). Взятые чеком — были именем для него (вопрос остаётся открытым).
func (b *Bot) settleCrossCandidates(ctx context.Context) {
	if b.db == nil {
		return
	}
	b.clarify.mu.Lock()
	var due []crossCandidate
	for k, c := range b.clarify.crossCands {
		if time.Since(c.at) > b.pairWindow {
			due = append(due, c)
			delete(b.clarify.crossCands, k)
		}
	}
	b.clarify.mu.Unlock()
	for _, c := range due {
		id, ok, err := b.db.OpenPendingNameOf(ctx, c.rawID)
		if err != nil || !ok {
			continue
		}
		ca, open := b.crossAskByID(ctx, c.askID)
		if !open {
			continue
		}
		chat, err := types.ParseJID(c.group)
		if err != nil {
			continue
		}
		if err := b.db.ConsumePendingNameByID(ctx, id); err != nil {
			continue
		}
		fmt.Printf("«%s» после «Чей он?» так и не досталось чеку — это ответ на вопрос\n", c.name)
		b.applyCrossClientAnswer(ctx, chat, c.askID, ca, c.text, false)
	}
}
