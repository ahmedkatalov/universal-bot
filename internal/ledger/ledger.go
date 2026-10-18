// Package ledger — движок подсчёта сбора.
//
// Раньше каждый отчёт (сбор, по картам, по сотрудникам, журнал, по клиенту,
// «НЕ вошло») считал по-своему, отдельным SQL-запросом с копией правил
// («текст — дубль чека», «копия пересланного чека», «тот же чек в двух
// группах»). Копии правил расходились, и отчёты не сходились между собой.
//
// Теперь платежи собираются ОДИН раз из «улик» — чеков и текстовых платежей —
// по единым правилам (Build), и все отчёты строятся из получившейся книги.
// У каждого платежа видно, из чего он сложен, а у каждой отброшенной улики —
// почему она не в сборе.
package ledger

import (
	"math"
	"sort"
	"strings"
	"time"
	"unicode"
)

// Check — банковский чек (фото/PDF) из группы.
type Check struct {
	ID        int
	RawID     int
	WaID      string
	FwdOrigin string // для копии, пересланной ботом: id сообщения исходного чека
	Group     string
	Sender    string // кто прислал: имя (из памяти номеров / подписи / WhatsApp)
	Phone     string
	ContactID int
	Client    string // клиент (названный рядом с чеком) или напечатанный получатель
	Confirmed bool   // клиента назвали (подпись, имя рядом, ответ на «чей чек?»)
	Review    bool   // ждёт проверки (клиент/сумма не ясны)
	Amount    float64
	OpDate    time.Time // дата операции на чеке
	PostedAt  time.Time // когда прислали в группу
	Doc       string
	Auth      string
	Bank      string // банк чека (отправителя)
	CardBank  string // банк получателя (для разбивки по картам)
	CardOwner string // получатель на чеке (владелец карты)
	Collector string
	Ignored   bool // исключён владельцем / не прошёл (отклонён)
	Duplicate bool // повтор уже присланного чека (решено при получении)
	Deleted   bool // сообщение удалено в WhatsApp
	Created   time.Time
}

// Text — платёж, записанный из текста («Магомедов Ахмед 15000», наличка).
type Text struct {
	ID        int
	RawID     int
	WaID      string
	Group     string
	Sender    string
	Phone     string
	ContactID int
	Client    string
	Amount    float64
	Cash      bool
	Card      string
	Collector string
	Held      bool // наличка-повтор: ждёт ответа «новый или тот же?»
	Ignored   bool
	Deleted   bool
	OpDate    time.Time
	PostedAt  time.Time
}

// Payment — один реальный платёж: чек (возможно, с копиями в других группах)
// и/или текст про него.
type Payment struct {
	Key      string // устойчивый ключ: «c<id чека>» или «t<id текста>»
	Client   string
	ClientID int
	Amount   float64
	Cash     bool
	OpDate   time.Time
	PostedAt time.Time
	Checks   []*Check // главный — первый
	Texts    []*Text  // тексты про этот платёж (для чека — его «текст-дубль»)
	groups   map[string]bool
}

// Main — главный чек платежа (nil — платёж только из текста).
func (p *Payment) Main() *Check {
	if len(p.Checks) == 0 {
		return nil
	}
	return p.Checks[0]
}

// Groups — группы, где есть улики этого платежа (по порядку).
func (p *Payment) Groups() []string {
	var out []string
	for g := range p.groups {
		out = append(out, g)
	}
	sort.Strings(out)
	return out
}

// InGroups — есть ли улика платежа в одной из групп (пусто — все группы).
func (p *Payment) InGroups(gs map[string]bool) bool {
	if len(gs) == 0 {
		return true
	}
	for g := range p.groups {
		if gs[g] {
			return true
		}
	}
	return false
}

// HomeGroup — группа главной улики.
func (p *Payment) HomeGroup() string {
	if c := p.Main(); c != nil {
		return c.Group
	}
	if len(p.Texts) > 0 {
		return p.Texts[0].Group
	}
	return ""
}

// Sender — кто прислал (главная улика), Phone — его номер.
func (p *Payment) Sender() (name, phone string) {
	if c := p.Main(); c != nil {
		if c.Collector != "" {
			return c.Collector, c.Phone
		}
		return c.Sender, c.Phone
	}
	if len(p.Texts) > 0 {
		t := p.Texts[0]
		if t.Collector != "" {
			return t.Collector, t.Phone
		}
		return t.Sender, t.Phone
	}
	return "", ""
}

// Card — метка «куда» для разбивки по картам/способу.
func (p *Payment) Card() string {
	if c := p.Main(); c != nil {
		if c.Bank != "" {
			return c.Bank
		}
		return "банк не указан"
	}
	if len(p.Texts) > 0 && p.Texts[0].Card != "" {
		return p.Texts[0].Card
	}
	return "не указано"
}

// Reason — почему улика не в сборе.
type Reason string

const (
	ReasonDeleted   Reason = "удалён в WhatsApp"
	ReasonIgnored   Reason = "исключён из учёта"
	ReasonDuplicate Reason = "повтор уже присланного чека"
	ReasonReview    Reason = "ждёт клиента или суммы"
	ReasonNoAmount  Reason = "сумма не распознана"
	ReasonHeld      Reason = "придержан как возможный повтор налички"
	ReasonTwin      Reason = "текст про уже посчитанный чек"
	ReasonCopy      Reason = "копия чека, исходный не в сборе"
)

// Excluded — улика, которая в сбор не вошла (и почему).
type Excluded struct {
	Reason Reason
	Check  *Check
	Text   *Text
	Into   *Payment // для текста-дубля: к какому платежу он относится
}

// Amount, Client, Group, OpDate, PostedAt — поля улики.
func (e Excluded) Amount() float64 {
	if e.Check != nil {
		return e.Check.Amount
	}
	return e.Text.Amount
}

func (e Excluded) Client() string {
	if e.Check != nil {
		return e.Check.Client
	}
	return e.Text.Client
}

func (e Excluded) Group() string {
	if e.Check != nil {
		return e.Check.Group
	}
	return e.Text.Group
}

func (e Excluded) OpDate() time.Time {
	if e.Check != nil {
		return e.Check.OpDate
	}
	return e.Text.OpDate
}

func (e Excluded) PostedAt() time.Time {
	if e.Check != nil {
		return e.Check.PostedAt
	}
	return e.Text.PostedAt
}

// Book — книга платежей.
type Book struct {
	Payments []*Payment
	Excluded []Excluded
}

// TwinWindow — чек и текст про него присылают в пределах двух суток.
const TwinWindow = 48 * time.Hour

// ClientKey — ключ клиента: без регистра, ё=е, без знаков, слова по алфавиту
// («Ахмед Магомедов» = «магомедов ахмед»).
func ClientKey(s string) string {
	s = strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(s, "ё", "е"), "Ё", "Е"))
	f := strings.FieldsFunc(s, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
	sort.Strings(f)
	return strings.Join(f, " ")
}

// docLatin — кириллические буквы, неотличимые от латинских в номерах документов.
var docLatin = map[rune]rune{
	'А': 'A', 'В': 'B', 'С': 'C', 'Е': 'E', 'Ё': 'E', 'Н': 'H', 'К': 'K', 'М': 'M',
	'О': 'O', 'Р': 'P', 'Т': 'T', 'Х': 'X', 'У': 'Y',
}

// NormDoc — номер документа без пробелов/знаков, с латиницей вместо похожей кириллицы.
func NormDoc(s string) string {
	var out []rune
	for _, r := range strings.ToUpper(s) {
		if l, ok := docLatin[r]; ok {
			r = l
		}
		if (r >= '0' && r <= '9') || (r >= 'A' && r <= 'Z') || (r >= 'А' && r <= 'Я') {
			out = append(out, r)
		}
	}
	return string(out)
}

func sameMoney(a, b float64) bool { return math.Abs(a-b) < 0.5 }

func absDur(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

// SameName — одно ли имя клиента при разном написании (падеж, опечатка,
// порядок слов, инициалы). Бот подставляет своё сравнение имён; по умолчанию —
// совпадение слов без учёта регистра и порядка.
var SameName = func(a, b string) bool {
	ka, kb := ClientKey(a), ClientKey(b)
	return ka != "" && ka == kb
}

// sameClient — один ли клиент у двух улик.
func sameClient(aID int, a string, bID int, b string) bool {
	if aID != 0 && aID == bID {
		return true
	}
	return strings.TrimSpace(a) != "" && strings.TrimSpace(b) != "" && SameName(a, b)
}

// Build собирает книгу платежей из чеков и текстов. Правила:
//
//  1. Удалённое в WhatsApp, исключённое владельцем и повтор уже присланного
//     чека в сбор не идут (видны в «не вошло» с причиной).
//  2. Копия чека, пересланная ботом в другую группу, — тот же платёж, что
//     исходный чек (своего платежа не даёт).
//  3. Чек, у которого не ясен клиент или сумма, ждёт проверки (не в сборе).
//  4. Один и тот же чек, присланный вручную в две разные группы (тот же номер
//     документа, а без номера — тот же клиент, сумма и минута операции), —
//     один платёж.
//  5. Наличка-повтор, ожидающая ответа «новый или тот же?», не в сборе.
//  6. Текстовый перевод про чек (тот же клиент и сумма, присланы в пределах
//     двух суток) — тот же платёж: считается чек, текст подтверждает его.
//     Один чек — один текст: два одинаковых текста при одном чеке — два платежа.
//  7. Наличка — всегда отдельный платёж (чеком не подтверждается).
func Build(checks []Check, texts []Text) *Book {
	b := &Book{}
	cs := make([]*Check, len(checks))
	for i := range checks {
		cs[i] = &checks[i]
	}
	sort.SliceStable(cs, func(i, j int) bool {
		if !cs[i].PostedAt.Equal(cs[j].PostedAt) {
			return cs[i].PostedAt.Before(cs[j].PostedAt)
		}
		return cs[i].ID < cs[j].ID
	})
	byWa := map[string]*Check{}
	for _, c := range cs {
		if c.WaID != "" && c.FwdOrigin == "" {
			byWa[c.WaID] = c
		}
	}

	// 1–3: состояние каждого чека.
	paymentOf := map[*Check]*Payment{}
	var copies []*Check
	var live []*Check
	for _, c := range cs {
		switch {
		case c.Deleted:
			b.Excluded = append(b.Excluded, Excluded{Reason: ReasonDeleted, Check: c})
		case c.Ignored:
			b.Excluded = append(b.Excluded, Excluded{Reason: ReasonIgnored, Check: c})
		case c.FwdOrigin != "" && byWa[c.FwdOrigin] != nil:
			copies = append(copies, c) // решится вместе с исходным
		case c.Duplicate:
			b.Excluded = append(b.Excluded, Excluded{Reason: ReasonDuplicate, Check: c})
		case c.Amount <= 0:
			b.Excluded = append(b.Excluded, Excluded{Reason: ReasonNoAmount, Check: c})
		case c.Review || c.ContactID == 0:
			b.Excluded = append(b.Excluded, Excluded{Reason: ReasonReview, Check: c})
		default:
			live = append(live, c)
		}
	}

	// 4: один чек в разных группах — один платёж.
	for _, c := range live {
		var into *Payment
		for _, p := range b.Payments {
			m := p.Main()
			if m == nil || p.groups[c.Group] || !sameMoney(m.Amount, c.Amount) {
				continue
			}
			dc, dm := NormDoc(c.Doc), NormDoc(m.Doc)
			switch {
			case dc != "" && dm != "":
				if dc == dm && absDur(c.OpDate.Sub(m.OpDate)) <= 36*time.Hour {
					into = p
				}
			case sameClient(c.ContactID, c.Client, m.ContactID, m.Client) && absDur(c.OpDate.Sub(m.OpDate)) <= 2*time.Minute:
				into = p
			}
			if into != nil {
				break
			}
		}
		if into != nil {
			into.Checks = append(into.Checks, c)
			into.groups[c.Group] = true
			paymentOf[c] = into
			continue
		}
		p := &Payment{
			Key: "c" + itoa(c.ID), Client: c.Client, ClientID: c.ContactID, Amount: c.Amount,
			OpDate: c.OpDate, PostedAt: c.PostedAt, Checks: []*Check{c}, groups: map[string]bool{c.Group: true},
		}
		b.Payments = append(b.Payments, p)
		paymentOf[c] = p
	}

	// 2: копии — к платежу исходного чека (или туда же, где исходный отброшен).
	for _, c := range copies {
		orig := byWa[c.FwdOrigin]
		if p := paymentOf[orig]; p != nil {
			p.Checks = append(p.Checks, c)
			p.groups[c.Group] = true
			paymentOf[c] = p
			continue
		}
		b.Excluded = append(b.Excluded, Excluded{Reason: ReasonCopy, Check: c})
	}

	// 5–7: тексты.
	ts := make([]*Text, len(texts))
	for i := range texts {
		ts[i] = &texts[i]
	}
	sort.SliceStable(ts, func(i, j int) bool {
		if !ts[i].PostedAt.Equal(ts[j].PostedAt) {
			return ts[i].PostedAt.Before(ts[j].PostedAt)
		}
		return ts[i].ID < ts[j].ID
	})
	checkPays := append([]*Payment(nil), b.Payments...)
	hasText := map[*Payment]bool{}
	for _, t := range ts {
		switch {
		case t.Amount <= 0:
			continue
		case t.Deleted:
			b.Excluded = append(b.Excluded, Excluded{Reason: ReasonDeleted, Text: t})
			continue
		case t.Ignored:
			continue // старая версия исправленного сообщения или исключённый платёж
		case t.Held:
			b.Excluded = append(b.Excluded, Excluded{Reason: ReasonHeld, Text: t})
			continue
		}
		if !t.Cash {
			var best *Payment
			var bestGap time.Duration
			for _, p := range checkPays {
				if hasText[p] || !sameMoney(p.Amount, t.Amount) {
					continue
				}
				m := p.Main()
				if !sameClient(m.ContactID, m.Client, t.ContactID, t.Client) {
					continue
				}
				gap := absDur(m.PostedAt.Sub(t.PostedAt))
				if gap >= TwinWindow {
					continue
				}
				if best == nil || gap < bestGap {
					best, bestGap = p, gap
				}
			}
			if best != nil {
				best.Texts = append(best.Texts, t)
				best.groups[t.Group] = true
				hasText[best] = true
				b.Excluded = append(b.Excluded, Excluded{Reason: ReasonTwin, Text: t, Into: best})
				continue
			}
		}
		b.Payments = append(b.Payments, &Payment{
			Key: "t" + itoa(t.ID), Client: t.Client, ClientID: t.ContactID, Amount: t.Amount, Cash: t.Cash,
			OpDate: t.OpDate, PostedAt: t.PostedAt, Texts: []*Text{t}, groups: map[string]bool{t.Group: true},
		})
	}
	return b
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var d []byte
	for n > 0 {
		d = append([]byte{byte('0' + n%10)}, d...)
		n /= 10
	}
	if neg {
		d = append([]byte{'-'}, d...)
	}
	return string(d)
}

// Filter — какие платежи берём в отчёт.
type Filter struct {
	From, To time.Time       // период [From, To)
	ByChat   bool            // по дате присылки в чат (иначе — по дате операции)
	Groups   map[string]bool // пусто — все группы
	// Для режима по дате чата: чеки с датой операции вне месяца периода в
	// основную сумму не идут (их показывают отдельно как «старые чеки»).
	MonthStart, MonthEnd time.Time
}

func (f Filter) inPeriod(t time.Time) bool { return !t.Before(f.From) && t.Before(f.To) }

func (f Filter) oldOperation(p *Payment) bool {
	if !f.ByChat || p.Main() == nil || f.MonthStart.IsZero() {
		return false
	}
	return p.OpDate.Before(f.MonthStart) || !p.OpDate.Before(f.MonthEnd)
}

// Counted — платежи сбора за период.
func (b *Book) Counted(f Filter) []*Payment {
	var out []*Payment
	for _, p := range b.Payments {
		if !p.InGroups(f.Groups) {
			continue
		}
		if f.ByChat {
			if !f.inPeriod(p.PostedAt) || f.oldOperation(p) {
				continue
			}
		} else if !f.inPeriod(p.OpDate) {
			continue
		}
		out = append(out, p)
	}
	return out
}

// OldChecks — режим по дате чата: чеки, присланные в период, но с датой
// операции из другого месяца (в сумму не входят, показываются отдельно).
func (b *Book) OldChecks(f Filter) []*Payment {
	if !f.ByChat {
		return nil
	}
	var out []*Payment
	for _, p := range b.Payments {
		if p.InGroups(f.Groups) && f.inPeriod(p.PostedAt) && f.oldOperation(p) {
			out = append(out, p)
		}
	}
	return out
}

// Left — не вошедшие в сбор улики за период (по той же дате, что и сбор).
func (b *Book) Left(f Filter) []Excluded {
	var out []Excluded
	for _, e := range b.Excluded {
		if len(f.Groups) > 0 && !f.Groups[e.Group()] {
			continue
		}
		d := e.OpDate()
		if f.ByChat {
			d = e.PostedAt()
		}
		if !f.inPeriod(d) {
			continue
		}
		out = append(out, e)
	}
	return out
}
