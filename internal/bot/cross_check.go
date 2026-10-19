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
	"strings"
	"time"

	"go.mau.fi/whatsmeow/types"
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
	if !c.Found || !c.Named || c.Asked || (c.DocNumber == "" && c.AuthCode == "") || c.SenderJID == "" || c.SenderJID == "bot-forward" {
		return
	}
	b.askCrossGroupClient(ctx, chat, c.RawID, c.WaID, c.SenderJID, c.Client, c.ContactID, c.DocNumber, c.AuthCode, c.Amount, c.TxDate)
}

// crossAskFromDB — вопрос «чей он?» askID, которого нет в памяти (бот
// перезапускался): чек, по которому спросили, и его копия в другой группе,
// записанная на другого клиента.
func (b *Bot) crossAskFromDB(ctx context.Context, askID string) (crossClientAsk, bool) {
	c, err := b.db.CrossAskCheck(ctx, askID)
	if err != nil || !c.Found {
		return crossClientAsk{}, false
	}
	a := crossClientAsk{thisWa: c.WaID, thisClient: c.Client}
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
	c, err := b.db.CrossCheckOf(ctx, was[0], 0)
	if err != nil || !c.Found {
		return nil
	}
	book, err := b.db.LoadBook(ctx, c.TxDate.Add(-48*time.Hour), c.TxDate.Add(48*time.Hour), "")
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
