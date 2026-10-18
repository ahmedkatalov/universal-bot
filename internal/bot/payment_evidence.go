package bot

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"whatsapp-bot/internal/db"
	"whatsapp-bot/internal/ledger"
)

// Ручные корректировки (исключить запись, поправить сумму) решают, какие
// записи — один платёж, ровно так же, как движок подсчёта (ledger.Build):
// чек, его копии и ОДИН текст про него. Своё угадывание пары «по контакту»
// расходилось с отчётами: текст с опечаткой в фамилии оставался в сборе после
// исключения чека, а «исключи лишний» стирал вместе с чеком и второй,
// отдельный платёж.

// evKey — ключ записи: чек или текстовый платёж.
func evKey(kind string, id int) string {
	switch kind {
	case "tx", "transaction":
		return "t" + strconv.Itoa(id)
	}
	return "c" + strconv.Itoa(id)
}

// payIndex — платежи книги движка по записям, из которых они сложены.
type payIndex struct {
	of    map[string]*ledger.Payment         // evKey → платёж, в который вошла запись
	fwd   map[int]bool                       // чеки — копии, пересланные ботом (повторяют исходный сами)
	twins map[*ledger.Payment][]*ledger.Text // тексты, отнесённые к платежу (Excluded.Into), если их нет в Texts
}

// loadPayIndex — книга движка вокруг моментов at (дата операции / присылки
// записей, с запасом по три дня). Далёкие друг от друга моменты — отдельными
// книгами, чтобы не грузить месяцы ради двух записей.
func (b *Bot) loadPayIndex(ctx context.Context, at []time.Time) *payIndex {
	x := &payIndex{of: map[string]*ledger.Payment{}, fwd: map[int]bool{}, twins: map[*ledger.Payment][]*ledger.Text{}}
	var ts []time.Time
	for _, t := range at {
		if !t.IsZero() {
			ts = append(ts, t)
		}
	}
	sort.Slice(ts, func(i, j int) bool { return ts[i].Before(ts[j]) })
	const pad = 3 * 24 * time.Hour
	for i := 0; i < len(ts); {
		j := i
		for j+1 < len(ts) && ts[j+1].Sub(ts[j]) <= 14*24*time.Hour {
			j++
		}
		if book, err := b.db.Book(ctx, ts[i].Add(-pad), ts[j].Add(pad), ""); err == nil {
			x.add(book)
		}
		i = j + 1
	}
	return x
}

func (x *payIndex) add(book *ledger.Book) {
	set := func(k string, p *ledger.Payment) {
		if _, ok := x.of[k]; !ok {
			x.of[k] = p
		}
	}
	for _, p := range book.Payments {
		for _, c := range p.Checks {
			set(evKey("receipt", c.ID), p)
			if c.FwdOrigin != "" {
				x.fwd[c.ID] = true
			}
		}
		for _, t := range p.Texts {
			set(evKey("transaction", t.ID), p)
		}
	}
	for _, e := range book.Excluded {
		if e.Into == nil || e.Text == nil {
			continue
		}
		in := false
		for _, t := range e.Into.Texts {
			if t.ID == e.Text.ID {
				in = true
				break
			}
		}
		if !in {
			x.twins[e.Into] = append(x.twins[e.Into], e.Text)
		}
		set(evKey("transaction", e.Text.ID), e.Into)
	}
}

// payment — платёж, в который движок включил запись (nil — запись не в сборе:
// ждёт проверки, придержана, уже исключена…).
func (x *payIndex) payment(kind string, id int) *ledger.Payment {
	return x.of[evKey(kind, id)]
}

// checks — чеки платежа, кроме копий, пересланных ботом: копия повторяет
// исходный чек сама (исключение, сумма).
func (x *payIndex) checks(p *ledger.Payment) []*ledger.Check {
	var out []*ledger.Check
	for _, c := range p.Checks {
		if !x.fwd[c.ID] {
			out = append(out, c)
		}
	}
	return out
}

// texts — тексты про этот платёж (для чека — его текст-пара).
func (x *payIndex) texts(p *ledger.Payment) []*ledger.Text {
	return append(append([]*ledger.Text(nil), p.Texts...), x.twins[p]...)
}

// ---- correct_operation: исключение платежа целиком ----

// opEntry — строка списка корректировок: один платёж (по движку) и его
// главная найденная запись.
type opEntry struct {
	op  db.OperationRef
	pay *ledger.Payment // nil — запись не в сборе
}

// opTimes — моменты найденных записей (для загрузки книги вокруг них).
func opTimes(ops []db.OperationRef) []time.Time {
	var out []time.Time
	for _, op := range ops {
		out = append(out, op.TxDate, op.Received)
	}
	return out
}

// opRank — какую запись платежа показывать: чек, затем копию чека, затем текст.
func opRank(op db.OperationRef, x *payIndex) int {
	switch {
	case op.Kind == "receipt" && !x.fwd[op.ID]:
		return 2
	case op.Kind == "receipt":
		return 1
	}
	return 0
}

// groupOpsByPayment — найденные записи одного платежа (чек и его текст-пара,
// копии чека) — одна строка; текст, который движок считает отдельным
// платежом, — своя строка.
func groupOpsByPayment(ops []db.OperationRef, x *payIndex) []opEntry {
	var out []opEntry
	for _, op := range ops {
		p := x.payment(op.Kind, op.ID)
		merged := false
		if p != nil {
			for i := range out {
				if out[i].pay == p {
					if opRank(op, x) > opRank(out[i].op, x) {
						out[i].op = op
					}
					merged = true
					break
				}
			}
		}
		if !merged {
			out = append(out, opEntry{op: op, pay: p})
		}
	}
	return out
}

// opKindMatches — подходит ли запись под уточнение «чек / текст / наличка».
func opKindMatches(kind string, op db.OperationRef) bool {
	switch opKind(kind) {
	case "check":
		return op.Kind == "receipt"
	case "text":
		return op.Kind == "transaction"
	case "cash":
		return op.Kind == "transaction" && op.Cash
	}
	return true
}

// opKind — уточнение «что именно» (check / text / cash; пусто — не уточнено).
func opKind(kind string) string {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "check", "receipt", "чек":
		return "check"
	case "text", "transaction", "текст":
		return "text"
	case "cash", "наличка":
		return "cash"
	}
	return ""
}

// opSenderMatches — прислал ли запись этот сотрудник («текст Ислама»): по
// имени (с падежом), номеру или «у кого деньги».
func opSenderMatches(q string, op db.OperationRef) bool {
	if d := onlyDigits(q); len(d) >= 5 {
		return op.Phone != "" && strings.Contains(onlyDigits(op.Phone), d)
	}
	if len(normWords(q)) == 0 {
		return true
	}
	return (op.Sender != "" && corrNameMatches(q, op.Sender)) || (op.Collector != "" && corrNameMatches(q, op.Collector))
}

// pairNote — пояснение к строке списка: из чего сложен этот платёж.
func (e opEntry) pairNote(x *payIndex) string {
	if e.pay == nil || x == nil {
		return ""
	}
	switch {
	case e.op.Kind == "receipt":
		ts := x.texts(e.pay)
		if len(ts) == 0 {
			return ""
		}
		var who []string
		for _, t := range ts {
			if t.Sender != "" {
				who = append(who, "от "+t.Sender)
			}
		}
		if len(who) > 0 {
			return " — вместе с текстом " + strings.Join(who, ", ") + " (один платёж)"
		}
		return " — вместе с текстом про этот чек (один платёж)"
	case len(e.pay.Checks) > 0:
		return " — текст про чек (один платёж с чеком)"
	}
	return ""
}

// exclRes — что ещё исключено вместе с записью.
type exclRes struct {
	texts, checks int
	keptByCheck   bool // исключён только текст, платёж остался в сборе по чеку
}

func (r exclRes) note(op db.OperationRef) string {
	var out string
	if r.keptByCheck {
		out += " Исключена только эта текстовая запись: сам платёж остаётся в сборе по чеку."
	}
	if r.checks > 0 {
		if op.Kind == "receipt" {
			out += fmt.Sprintf(" Вместе с ним исключил копию этого чека в другой группе (%d) — это тот же платёж.", r.checks)
		} else {
			out += fmt.Sprintf(" Это тот же платёж, что и чек, — исключил и чек (%d).", r.checks)
		}
	}
	if r.texts > 0 {
		if op.Kind == "receipt" {
			out += fmt.Sprintf(" Вместе с чеком исключил его текстовую запись (%d) — это был тот же платёж.", r.texts)
		} else {
			out += fmt.Sprintf(" Вместе с ним исключил текстовую запись того же платежа (%d).", r.texts)
		}
	}
	return out
}

// excludePayment исключает запись e.op и остальные записи её платежа ровно
// так, как их объединил движок подсчёта: чек (и его копии, присланные в другие
// группы вручную) и его текст-пару. Отдельный такой же текст — другой платёж,
// его не трогаем. onlyRecord — владелец назвал именно эту запись (kind/sender):
// если это текст про чек, исключаем только текст, платёж остаётся по чеку.
// Копия чека, пересланная ботом, исключается только в своей группе («не считай
// здесь»): исходный чек и тексты других групп остаются.
func (b *Bot) excludePayment(ctx context.Context, e opEntry, x *payIndex, onlyRecord bool) (exclRes, error) {
	var r exclRes
	op := e.op
	if err := b.db.SetOperationIgnored(ctx, op.Kind, op.ID, true); err != nil {
		return r, err
	}
	p := e.pay
	if p == nil || x == nil {
		return r, nil
	}
	if op.Kind == "transaction" && onlyRecord && len(p.Checks) > 0 {
		r.keptByCheck = true
		return r, nil
	}
	checks := x.checks(p)
	fwdCopy := op.Kind == "receipt" && x.fwd[op.ID] && len(checks) > 0
	if !fwdCopy {
		for _, c := range checks {
			if op.Kind == "receipt" && c.ID == op.ID {
				continue
			}
			if b.db.SetOperationIgnored(ctx, "receipt", c.ID, true) == nil {
				r.checks++
			}
		}
	}
	for _, t := range x.texts(p) {
		if op.Kind == "transaction" && t.ID == op.ID {
			continue
		}
		if fwdCopy && t.Group != op.GroupJID {
			continue
		}
		if b.db.SetOperationIgnored(ctx, "transaction", t.ID, true) == nil {
			r.texts++
		}
	}
	return r, nil
}

// collapseTextTwins (для возврата исключённого) убирает из списка текстовую
// запись — копию чека из того же списка (тот же клиент и сумма, ±2 дня,
// перевод): чек вернётся, а копия не нужна. Как в движке подсчёта, у чека не
// больше одной такой копии (ближайшая по времени); другие такие же тексты —
// отдельные платежи и остаются в списке.
func collapseTextTwins(ops []db.OperationRef) []db.OperationRef {
	hidden := map[int]bool{}
	for _, r := range ops {
		if r.Kind != "receipt" {
			continue
		}
		best, bestGap := -1, time.Duration(0)
		for j, op := range ops {
			if hidden[j] || op.Kind != "transaction" || op.Cash || math.Abs(r.Amount-op.Amount) >= 1 ||
				!ledger.SameName(r.Name, op.Name) {
				continue
			}
			d := op.Received.Sub(r.Received)
			if d < 0 {
				d = -d
			}
			if d <= 48*time.Hour && (best < 0 || d < bestGap) {
				best, bestGap = j, d
			}
		}
		if best >= 0 {
			hidden[best] = true
		}
	}
	out := ops[:0:0]
	for i, op := range ops {
		if !hidden[i] {
			out = append(out, op)
		}
	}
	return out
}
