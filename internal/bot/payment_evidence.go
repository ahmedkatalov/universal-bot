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

// loadPayIndex — книга движка вокруг найденных записей: recs[i] — моменты
// i-й записи (дата операции, присылка), вокруг каждого — запас по три дня.
// Все моменты ОДНОЙ записи (и близкие моменты разных записей) — в одной
// книге: чек с датой операции месяц назад и текст, присланный вместе с ним
// сегодня, иначе попали бы в разные книги и не объединились в один платёж.
// Далёкие друг от друга периоды грузятся по отдельности (без месяцев между
// ними), но собираются в одну книгу.
func (b *Bot) loadPayIndex(ctx context.Context, recs [][]time.Time) *payIndex {
	x := &payIndex{of: map[string]*ledger.Payment{}, fwd: map[int]bool{}, twins: map[*ledger.Payment][]*ledger.Text{}}
	type point struct {
		t   time.Time
		rec int
	}
	var pts []point
	for i, ts := range recs {
		for _, t := range ts {
			if !t.IsZero() {
				pts = append(pts, point{t, i})
			}
		}
	}
	sort.Slice(pts, func(i, j int) bool { return pts[i].t.Before(pts[j].t) })
	// Записи с близкими моментами — одна книга (моменты одной записи — тоже).
	const near, pad = 14 * 24 * time.Hour, 3 * 24 * time.Hour
	parent := make([]int, len(recs))
	for i := range parent {
		parent[i] = i
	}
	var find func(int) int
	find = func(i int) int {
		if parent[i] != i {
			parent[i] = find(parent[i])
		}
		return parent[i]
	}
	for k := 1; k < len(pts); k++ {
		if pts[k].t.Sub(pts[k-1].t) <= near {
			parent[find(pts[k].rec)] = find(pts[k-1].rec)
		}
	}
	var roots []int
	at := map[int][]time.Time{} // книга → её моменты по возрастанию
	for _, p := range pts {
		r := find(p.rec)
		if _, ok := at[r]; !ok {
			roots = append(roots, r)
		}
		at[r] = append(at[r], p.t)
	}
	for _, r := range roots {
		ts := at[r]
		var spans [][2]time.Time
		for i := 0; i < len(ts); {
			j := i
			for j+1 < len(ts) && ts[j+1].Sub(ts[j]) <= near {
				j++
			}
			spans = append(spans, [2]time.Time{ts[i].Add(-pad), ts[j].Add(pad)})
			i = j + 1
		}
		if book, err := b.db.BookSpans(ctx, spans); err == nil {
			x.add(book)
		}
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

// opTimes — моменты каждой найденной записи: дата операции и присылка (для
// загрузки книги вокруг них).
func opTimes(ops []db.OperationRef) [][]time.Time {
	var out [][]time.Time
	for _, op := range ops {
		out = append(out, []time.Time{op.TxDate, op.Received})
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
	keptByCheck   bool     // исключён только текст, платёж остался в сборе по чеку
	keptGroups    []string // другие группы, где чек этого платежа остался в сборе
	extra         bool     // исключён лишний из одинаковых текстов (extraTextCopy)
	extraPaired   bool     // такой же текст остаётся в сборе вместе с чеком
	extraLeft     int      // таких же текстов — отдельных платежей — осталось
}

// note — пояснение к ответу: что исключено вместе с записью и что осталось.
// groupName — название группы по JID (пусто — неизвестна).
func (r exclRes) note(op db.OperationRef, groupName func(string) string) string {
	var out string
	if r.extra {
		out += " Это был лишний повтор такого же текста."
		if r.extraPaired {
			out += " Такой же текст остаётся в сборе вместе с чеком (один платёж)."
		}
		if r.extraLeft > 0 {
			out += fmt.Sprintf(" Таких же текстов отдельными платежами в сборе ещё %d — если и они лишние, уточни у владельца.", r.extraLeft)
		}
	}
	if r.keptByCheck {
		out += " Исключена только эта текстовая запись: сам платёж остаётся в сборе по чеку."
	}
	if r.checks > 0 {
		if op.Kind == "receipt" {
			out += fmt.Sprintf(" Вместе с ним исключил копию этого чека (%d) — это тот же платёж.", r.checks)
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
	if len(r.keptGroups) > 0 {
		var names []string
		for _, g := range r.keptGroups {
			if n := groupName(g); n != "" {
				names = append(names, "«"+n+"»")
			}
		}
		where := "в другой группе"
		if len(names) == len(r.keptGroups) {
			where = "в группе " + strings.Join(names, ", ")
		}
		if op.Kind == "receipt" {
			out += " Этот же чек прислан и " + where + " — там он остаётся в сборе: исключил только здесь, другую группу не трогал."
		} else {
			out += " Сам платёж остаётся в сборе по чеку " + where + ": исключил только здесь, другую группу не трогал."
		}
		out += " Если его нужно убрать и там — уточни у владельца (тогда исключи ещё раз с group)."
	}
	return out
}

// excludePayment исключает запись e.op и остальные записи её платежа ровно
// так, как их объединил движок подсчёта: чек (его копии) и его текст-пару.
// Отдельный такой же текст — другой платёж, его не трогаем. onlyRecord —
// владелец назвал именно эту запись (kind/sender): если это текст про чек,
// исключаем только текст, платёж остаётся по чеку.
//
// scope — группа, в которой искали запись (группа разговора или названная
// владельцем; пусто — все группы). Чек того же платежа, присланный в ДРУГУЮ
// группу (вручную, копия ботом, поправленная владельцем копия), там и
// остаётся: «не считай его здесь» не стирает платёж из группы, которую
// владелец не называл, — он узнаёт об этом из ответа и решает сам. Тексты про
// платёж, который остался в другой группе, тоже остаются (кроме текстов этой
// группы); если же чеков платежа не осталось нигде — исключаются и его тексты
// в других группах (это описание того же чека).
func (b *Bot) excludePayment(ctx context.Context, e opEntry, x *payIndex, onlyRecord bool, scope string) (exclRes, error) {
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
	if scope == "" && op.Kind == "receipt" && x.fwd[op.ID] {
		scope = op.GroupJID // копия, пересланная ботом, — «не считай здесь»
	}
	here := func(g string) bool { return scope == "" || g == scope }
	dropped := map[int]bool{}
	if op.Kind == "receipt" {
		dropped[op.ID] = true
	}
	for _, c := range x.checks(p) {
		if !dropped[c.ID] && here(c.Group) {
			if b.db.SetOperationIgnored(ctx, "receipt", c.ID, true) == nil {
				r.checks++
			}
			dropped[c.ID] = true
		}
	}
	// Что из чеков платежа остаётся в сборе: копия, пересланная ботом, идёт
	// вслед за своим исходным чеком.
	orig := map[string]*ledger.Check{}
	for _, c := range p.Checks {
		if !x.fwd[c.ID] && c.WaID != "" {
			orig[c.WaID] = c
		}
	}
	keptIn := map[string]bool{}
	for _, c := range p.Checks {
		if dropped[c.ID] {
			continue
		}
		if x.fwd[c.ID] {
			if o := orig[c.FwdOrigin]; o == nil || dropped[o.ID] {
				continue
			}
			if here(c.Group) { // копия здесь, а исходный остался в другой группе
				if b.db.SetOperationIgnored(ctx, "receipt", c.ID, true) == nil {
					r.checks++
				}
				dropped[c.ID] = true
				continue
			}
		}
		if !keptIn[c.Group] {
			keptIn[c.Group] = true
			r.keptGroups = append(r.keptGroups, c.Group)
		}
	}
	for _, t := range x.texts(p) {
		if op.Kind == "transaction" && t.ID == op.ID {
			continue
		}
		if len(r.keptGroups) > 0 && !here(t.Group) {
			continue // текст про платёж, который остаётся в своей группе
		}
		if b.db.SetOperationIgnored(ctx, "transaction", t.ID, true) == nil {
			r.texts++
		}
	}
	return r, nil
}

// extraTextCopy — «исключи лишний»: все найденные строки — одинаковые тексты
// (тот же клиент и сумма, та же дата, прислал тот же сотрудник в ту же
// группу), и хотя бы один из них движок считает отдельным платежом. Владельцу
// нечем их различить (ни дата, ни группа, ни кто прислал, ни чек/текст), а сбор
// после исключения любого из них один и тот же, — вопрос «какой?» остался бы
// без ответа. Лишний — отдельный платёж, последний присланный такой текст.
func extraTextCopy(entries []opEntry) (int, bool) {
	if len(entries) < 2 {
		return -1, false
	}
	f := entries[0].op
	pick := -1
	for i, e := range entries {
		op := e.op
		if op.Kind != "transaction" || e.pay == nil || op.Cash != f.Cash || math.Abs(op.Amount-f.Amount) >= 1 ||
			!strings.EqualFold(strings.TrimSpace(op.Name), strings.TrimSpace(f.Name)) || op.GroupJID != f.GroupJID ||
			!sameSenderOf(op, f) || op.TxDate.Format("2006-01-02") != f.TxDate.Format("2006-01-02") {
			return -1, false
		}
		if len(e.pay.Checks) > 0 {
			continue
		}
		if pick < 0 || op.Received.After(entries[pick].op.Received) ||
			(op.Received.Equal(entries[pick].op.Received) && op.ID > entries[pick].op.ID) {
			pick = i
		}
	}
	return pick, pick >= 0
}

// sameSenderOf — одну ли запись прислал один и тот же сотрудник (как их
// различает уточнение sender): по номеру, а без номера — по имени.
func sameSenderOf(a, b db.OperationRef) bool {
	if a.Phone != "" && b.Phone != "" {
		return onlyDigits(a.Phone) == onlyDigits(b.Phone)
	}
	return strings.EqualFold(strings.TrimSpace(a.Sender), strings.TrimSpace(b.Sender))
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
