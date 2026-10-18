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
	// splits — тексты-итоги платежа частями по id текста; partOf — итоги, в
	// которые входит чек (evKey чека: посчитанная часть, её копия или часть,
	// которой сейчас нет в сборе).
	splits map[int]*splitTotal
	partOf map[string][]*splitTotal
}

// splitTotal — текст-итог платежа частями («Магомедов Ахмед 15000» про чеки
// 10000 + 5000). Это ОДИН платёж с этими чеками: в сбор входят чеки, а текст
// сверх них не считается. Движок кладёт текст в платёж первой части (Into =
// Parts[0]), но текст — не пара одного этого чека: исключение итога касается
// всех частей, поправка суммы итога не переносится на чек, а поправка одной
// части — на итог.
type splitTotal struct {
	text    *ledger.Text
	parts   []*ledger.Payment // посчитанные части
	missing []*ledger.Check   // части, которых сейчас нет в сборе (ждут «чей чек?», исключены, удалены)
}

// sum — сумма всех частей (и посчитанных, и тех, что не в сборе).
func (s *splitTotal) sum() float64 {
	var v float64
	for _, p := range s.parts {
		v += p.Amount
	}
	for _, c := range s.missing {
		v += c.Amount
	}
	return v
}

// partsLine — «10 000 + 5 000» (части, которых нет в сборе, — с пометкой).
// Часть, в которой есть чек из changed, показана с новой суммой to.
func (s *splitTotal) partsLine(changed map[int]bool, to float64) string {
	var ps []string
	for _, p := range s.parts {
		v := p.Amount
		for _, c := range p.Checks {
			if changed[c.ID] {
				v = to
			}
		}
		ps = append(ps, formatMoney(v))
	}
	for _, c := range s.missing {
		v := c.Amount
		if changed[c.ID] {
			v = to
		}
		ps = append(ps, formatMoney(v)+" (пока не в сборе)")
	}
	return strings.Join(ps, " + ")
}

// label — «Магомедов Ахмед 15 000 ₽».
func (s *splitTotal) label() string {
	return strings.TrimSpace(s.text.Client+" "+formatMoney(s.text.Amount)) + " ₽"
}

// loadPayIndex — книга движка вокруг найденных записей: recs[i] — моменты
// i-й записи (дата операции, присылка), вокруг каждого — запас по три дня.
// Все моменты ОДНОЙ записи (и близкие моменты разных записей) — в одной
// книге: чек с датой операции месяц назад и текст, присланный вместе с ним
// сегодня, иначе попали бы в разные книги и не объединились в один платёж.
// Далёкие друг от друга периоды грузятся по отдельности (без месяцев между
// ними), но собираются в одну книгу.
func (b *Bot) loadPayIndex(ctx context.Context, recs [][]time.Time) *payIndex {
	x := &payIndex{of: map[string]*ledger.Payment{}, fwd: map[int]bool{}, twins: map[*ledger.Payment][]*ledger.Text{},
		splits: map[int]*splitTotal{}, partOf: map[string][]*splitTotal{}}
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
	// Тексты-итоги платежа частями — не пара первой части: свой учёт.
	for _, e := range book.Excluded {
		if e.Reason != ledger.ReasonSplit || e.Text == nil || len(e.Parts) == 0 {
			continue
		}
		if _, ok := x.splits[e.Text.ID]; ok || x.of[evKey("transaction", e.Text.ID)] != nil {
			continue // уже узнан в другой книге
		}
		st := &splitTotal{text: e.Text, parts: e.Parts, missing: e.Missing}
		x.splits[e.Text.ID] = st
		mark := func(id int) {
			k := evKey("receipt", id)
			for _, o := range x.partOf[k] {
				if o == st {
					return
				}
			}
			x.partOf[k] = append(x.partOf[k], st)
		}
		for _, p := range e.Parts {
			for _, c := range p.Checks {
				mark(c.ID)
			}
		}
		for _, c := range e.Missing {
			mark(c.ID)
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
			if x.splits[t.ID] == nil {
				set(evKey("transaction", t.ID), p)
			}
		}
	}
	for _, e := range book.Excluded {
		if e.Into == nil || e.Text == nil || e.Reason == ledger.ReasonSplit || x.splits[e.Text.ID] != nil {
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

// texts — тексты про этот платёж (для чека — его текст-пара). Текст-итог
// платежа частями сюда не входит: он про все части сразу (splitOf, totalsOf).
func (x *payIndex) texts(p *ledger.Payment) []*ledger.Text {
	var out []*ledger.Text
	for _, t := range p.Texts {
		if x.splits[t.ID] == nil {
			out = append(out, t)
		}
	}
	return append(out, x.twins[p]...)
}

// splitOf — текст-итог платежа частями (nil — запись не такой итог).
func (x *payIndex) splitOf(kind string, id int) *splitTotal {
	if x == nil || evKey(kind, id)[0] != 't' {
		return nil
	}
	return x.splits[id]
}

// totalsOf — тексты-итоги платежа частями, в которые входят эти чеки (по id).
func (x *payIndex) totalsOf(checkIDs ...int) []*splitTotal {
	if x == nil {
		return nil
	}
	var out []*splitTotal
	seen := map[*splitTotal]bool{}
	for _, id := range checkIDs {
		for _, st := range x.partOf[evKey("receipt", id)] {
			if !seen[st] {
				seen[st] = true
				out = append(out, st)
			}
		}
	}
	return out
}

// checkIDs — id всех чеков платежа (с копиями).
func checkIDs(p *ledger.Payment) []int {
	var out []int
	if p != nil {
		for _, c := range p.Checks {
			out = append(out, c.ID)
		}
	}
	return out
}

// ---- correct_operation: исключение платежа целиком ----

// opEntry — строка списка корректировок: один платёж (по движку) и его
// главная найденная запись.
type opEntry struct {
	op    db.OperationRef
	pay   *ledger.Payment // nil — запись не в сборе
	split *splitTotal     // запись — текст-итог платежа частями (тогда pay == nil)
}

// withChecks — платёж в сборе по чекам (текст — пара чека или итог его частей).
func (e opEntry) withChecks() bool {
	return e.split != nil || (e.pay != nil && len(e.pay.Checks) > 0)
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
		if st := x.splitOf(op.Kind, op.ID); st != nil {
			// Тот же итог в другой группе (те же части) — тот же платёж.
			merged := false
			for i := range out {
				if o := out[i].split; o != nil && o.parts[0] == st.parts[0] {
					merged = true
					break
				}
			}
			if !merged {
				out = append(out, opEntry{op: op, split: st})
			}
			continue
		}
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
	if e.split != nil {
		return " — итог платежа частями: чеки " + e.split.partsLine(nil, 0) + " ₽ (один платёж: в сборе чеки, текст сверх них не считается)"
	}
	if e.pay == nil || x == nil {
		return ""
	}
	switch {
	case e.op.Kind == "receipt":
		part := ""
		if sts := x.totalsOf(checkIDs(e.pay)...); len(sts) > 0 {
			part = " — часть платежа «" + sts[0].label() + "» (итог текстом)"
		}
		ts := x.texts(e.pay)
		if len(ts) == 0 {
			return part
		}
		var who []string
		for _, t := range ts {
			if t.Sender != "" {
				who = append(who, "от "+t.Sender)
			}
		}
		if len(who) > 0 {
			return " — вместе с текстом " + strings.Join(who, ", ") + " (один платёж)" + part
		}
		return " — вместе с текстом про этот чек (один платёж)" + part
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

	// Текст-итог платежа частями.
	split       *splitTotal // исключён итог
	splitOnly   bool        // исключён только текст-итог: чеки остаются в сборе
	splitParts  []float64   // исключённые части (суммы чеков, без копий)
	splitCopies int         // исключённые копии этих чеков
	splitLeft   []float64   // части без названного клиента, которых нет в сборе, — не тронуты
	// Исключён чек — часть платежа с итогом текстом.
	totalsDropped []*splitTotal // итоги, у которых в сборе не осталось других частей, — исключены тоже
	totalsKept    []*splitTotal // итоги, у которых остались другие части
}

// moneyList — «10 000 ₽ + 5 000 ₽».
func moneyList(vs []float64) string {
	var ps []string
	for _, v := range vs {
		ps = append(ps, formatMoney(v)+" ₽")
	}
	return strings.Join(ps, " + ")
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
	switch {
	case r.split != nil && r.splitOnly:
		out += " Исключена только эта текстовая запись — итог платежа частями: сами чеки " + r.split.partsLine(nil, 0) +
			" ₽ остаются в сборе, сбор не изменился."
	case r.split != nil:
		if len(r.splitParts) > 0 {
			out += " Это итог платежа частями — исключил весь платёж: чеки " + moneyList(r.splitParts) + "."
		} else {
			out += " Это итог платежа частями (чеки " + r.split.partsLine(nil, 0) + " ₽): его чеков в этой группе нет — исключил только этот текст."
		}
		if r.splitCopies > 0 {
			out += fmt.Sprintf(" Вместе с ними — копии этих чеков (%d).", r.splitCopies)
		}
		if len(r.splitLeft) > 0 {
			out += " Чек " + moneyList(r.splitLeft) + " без названного клиента (ждёт «чей чек?») не трогал — он и так не в сборе;" +
				" если он тоже часть этого платежа, исключи его отдельно."
		}
	case r.checks > 0:
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
		switch {
		case r.split != nil:
			out += " Эти чеки присланы и " + where + " — там они остаются в сборе: исключил только здесь, другую группу не трогал."
		case op.Kind == "receipt":
			out += " Этот же чек прислан и " + where + " — там он остаётся в сборе: исключил только здесь, другую группу не трогал."
		default:
			out += " Сам платёж остаётся в сборе по чеку " + where + ": исключил только здесь, другую группу не трогал."
		}
		out += " Если его нужно убрать и там — уточни у владельца (тогда исключи ещё раз с group)."
	}
	said := map[string]bool{}
	for _, st := range r.totalsDropped {
		if l := st.label(); !said[l] {
			said[l] = true
			out += " Итог текстом «" + l + "» исключил тоже: других его чеков в сборе не осталось — иначе он посчитался бы сам по себе."
		}
	}
	for _, st := range r.totalsKept {
		out += " Этот чек — часть платежа «" + st.label() + "» (итог текстом, чеки " + st.partsLine(nil, 0) +
			"): исключил только этот чек, остальные части остаются в сборе. Если нужно убрать весь платёж — исключи итог " +
			formatMoney(st.text.Amount) + " ₽."
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
//
// Текст-итог платежа частями («Магомедов Ахмед 15000» про чеки 10000 + 5000) —
// один платёж со всеми своими чеками: без kind исключается весь платёж (все
// части, их копии и тексты), с kind=text — только сам текст (чеки остаются, сбор
// не меняется). Чек-часть исключается один; итог исключается вместе с ним,
// только если других его частей в сборе не осталось (иначе итог посчитался бы
// сверх чеков).
func (b *Bot) excludePayment(ctx context.Context, e opEntry, x *payIndex, onlyRecord bool, scope string) (exclRes, error) {
	var r exclRes
	op := e.op
	if err := b.db.SetOperationIgnored(ctx, op.Kind, op.ID, true); err != nil {
		return r, err
	}
	if st := e.split; st != nil && x != nil {
		r.split = st
		if onlyRecord {
			r.splitOnly = true
			return r, nil
		}
		dropped, here := b.dropPayments(ctx, op, st.parts, x, scope, &r)
		// Части, которых сейчас нет в сборе (ждут «чей чек?»): чек, подписанный
		// тем же клиентом, — тот же платёж; чек без названного клиента может
		// оказаться чужим — его не трогаем и говорим об этом.
		for _, c := range st.missing {
			if c.Ignored || c.Deleted || dropped[c.ID] || !here(c.Group) {
				continue
			}
			if c.Confirmed && c.ContactID != 0 {
				if b.db.SetOperationIgnored(ctx, "receipt", c.ID, true) == nil {
					r.splitParts = append(r.splitParts, c.Amount)
				}
				continue
			}
			r.splitLeft = append(r.splitLeft, c.Amount)
		}
		return r, nil
	}
	p := e.pay
	if p == nil || x == nil {
		return r, nil
	}
	if op.Kind == "transaction" && onlyRecord && len(p.Checks) > 0 {
		r.keptByCheck = true
		return r, nil
	}
	b.dropPayments(ctx, op, []*ledger.Payment{p}, x, scope, &r)
	return r, nil
}

// dropPayments исключает (кроме самой записи op) записи платежей pays так,
// как их объединил движок: чеки и копии в области scope, тексты-пары и итоги
// платежа частями. Возвращает исключённые чеки и область.
func (b *Bot) dropPayments(ctx context.Context, op db.OperationRef, pays []*ledger.Payment, x *payIndex, scope string, r *exclRes) (map[int]bool, func(string) bool) {
	if scope == "" && op.Kind == "receipt" && x.fwd[op.ID] {
		scope = op.GroupJID // копия, пересланная ботом, — «не считай здесь»
	}
	here := func(g string) bool { return scope == "" || g == scope }
	ignore := func(kind string, id int) bool { return b.db.SetOperationIgnored(ctx, kind, id, true) == nil }
	dropped := map[int]bool{}
	if op.Kind == "receipt" {
		dropped[op.ID] = true
	}
	for _, p := range pays {
		hit := false
		for _, c := range x.checks(p) {
			switch {
			case dropped[c.ID]:
				hit = true
			case here(c.Group):
				if ignore("receipt", c.ID) {
					r.checks++
					hit = true
				}
				dropped[c.ID] = true
			}
		}
		if hit && r.split != nil {
			r.splitParts = append(r.splitParts, p.Amount)
		}
	}
	// Что из чеков платежа остаётся в сборе: копия, пересланная ботом, идёт
	// вслед за своим исходным чеком.
	orig := map[string]*ledger.Check{}
	for _, p := range pays {
		for _, c := range p.Checks {
			if !x.fwd[c.ID] && c.WaID != "" {
				orig[c.WaID] = c
			}
		}
	}
	keptIn := map[string]bool{}
	keptPay := map[string]bool{} // платежи, чек которых остаётся в сборе в другой группе
	for _, p := range pays {
		for _, c := range p.Checks {
			if dropped[c.ID] {
				continue
			}
			if x.fwd[c.ID] {
				if o := orig[c.FwdOrigin]; o == nil || dropped[o.ID] {
					continue
				}
				if here(c.Group) { // копия здесь, а исходный остался в другой группе
					if ignore("receipt", c.ID) {
						r.checks++
					}
					dropped[c.ID] = true
					continue
				}
			}
			keptPay[p.Key] = true
			if !keptIn[c.Group] {
				keptIn[c.Group] = true
				r.keptGroups = append(r.keptGroups, c.Group)
			}
		}
	}
	if r.split != nil {
		if n := r.checks - len(r.splitParts); n > 0 {
			r.splitCopies = n
		}
	}
	seen := map[int]bool{}
	if op.Kind == "transaction" {
		seen[op.ID] = true
	}
	dropText := func(t *ledger.Text) {
		if seen[t.ID] {
			return
		}
		seen[t.ID] = true
		if len(r.keptGroups) > 0 && !here(t.Group) {
			return // текст про платёж, который остаётся в своей группе
		}
		if ignore("transaction", t.ID) {
			r.texts++
		}
	}
	for _, p := range pays {
		for _, t := range x.texts(p) {
			dropText(t)
		}
	}
	// Итоги платежа частями, в которые входят исключённые платежи.
	gone := map[string]bool{} // части, которых больше нет в сборе ни в одной группе
	var ids []int
	for _, p := range pays {
		if !keptPay[p.Key] {
			gone[p.Key] = true
		}
		ids = append(ids, checkIDs(p)...)
	}
	kept := map[string]bool{}
	for _, st := range x.totalsOf(ids...) {
		if seen[st.text.ID] {
			continue
		}
		// Тот же итог в другой группе — про те же части (тот же платёж).
		if r.split != nil && st.parts[0] == r.split.parts[0] {
			dropText(st.text)
			continue
		}
		left := 0
		for _, q := range st.parts {
			if !gone[q.Key] {
				left++
			}
		}
		if left > 0 {
			if l := st.label(); !kept[l] {
				kept[l] = true
				r.totalsKept = append(r.totalsKept, st)
			}
			continue
		}
		// Других частей в сборе не осталось нигде — итог посчитался бы сам по
		// себе (как текст про чек, которого больше нет).
		seen[st.text.ID] = true
		if ignore("transaction", st.text.ID) {
			r.totalsDropped = append(r.totalsDropped, st)
		}
	}
	return dropped, here
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
		if op.Kind != "transaction" || (e.pay == nil && e.split == nil) || op.Cash != f.Cash || math.Abs(op.Amount-f.Amount) >= 1 ||
			!strings.EqualFold(strings.TrimSpace(op.Name), strings.TrimSpace(f.Name)) || op.GroupJID != f.GroupJID ||
			!sameSenderOf(op, f) || op.TxDate.Format("2006-01-02") != f.TxDate.Format("2006-01-02") {
			return -1, false
		}
		if e.withChecks() {
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
