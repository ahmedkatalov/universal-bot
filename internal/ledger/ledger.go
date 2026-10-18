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
	Printed   string // получатель, как напечатан на чеке (пусто — не прочитан)
	Payer     string // плательщик, как напечатан на чеке
	Fixed     bool   // сумму чека поправили вручную («не 15000 а 13000»)
	// TimeGuessed — время операции на чеке не прочитано (стоит время присылки
	// или полдень): сравнивать по нему нельзя.
	TimeGuessed bool
	Collector   string
	Ignored     bool // исключён владельцем / не прошёл (отклонён)
	Duplicate   bool // повтор уже присланного чека (решено при получении)
	Deleted     bool // сообщение удалено в WhatsApp
	Created     time.Time

	nd string // NormDoc(Doc), считается в Build
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
	// Conflicts — экземпляры того же чека (тот же номер документа), подписанные
	// другим клиентом: платёж один, но чей он — надо уточнить.
	Conflicts []*Check
	// ChatAt — дата для отчёта «по дате в чате»: самая ранняя улика платежа,
	// присланная в том же месяце, что и операция (иначе — присылка чека).
	ChatAt time.Time
	groups map[string]bool
	mark   int // служебная метка при подборе пар
}

// chatTime — дата «по дате в чате» (для книг, собранных не через Build, — присылка).
func (p *Payment) chatTime() time.Time {
	if p.ChatAt.IsZero() {
		return p.PostedAt
	}
	return p.ChatAt
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
	ReasonSplit     Reason = "текст-итог платежа частями (чеки уже посчитаны)"
	ReasonCopy      Reason = "копия чека, исходный не в сборе"
)

// Excluded — улика, которая в сбор не вошла (и почему).
type Excluded struct {
	Reason Reason
	Check  *Check
	Text   *Text
	Into   *Payment   // для текста-дубля: к какому платежу он относится
	Parts  []*Payment // для текста-итога платежа частями: все его посчитанные чеки
	// Missing — для текста-итога: части платежа, которые сейчас не в сборе
	// (ждут «чей чек?», исключены или удалены).
	Missing []*Check
	// Of — для непроверенного чека: первый непроверенный экземпляр того же
	// чека (присланного ещё и в другую группу); в «не вошло» считается один раз.
	Of *Check
}

// Root — экземпляр, по которому непроверенный чек считают один раз.
func (e Excluded) Root() *Check {
	if e.Of != nil {
		return e.Of
	}
	return e.Check
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

// NameCovers — все слова короткого имени part есть в полном имени full
// («Ахмед» — в «Магомедов Ахмед», «Исаев» — в «Исаев Харон»). Бот подставляет
// сравнение слов с падежами и опечатками.
var NameCovers = func(part, full string) bool {
	pw, fw := strings.Fields(ClientKey(part)), strings.Fields(ClientKey(full))
	if len(pw) == 0 {
		return false
	}
	have := map[string]bool{}
	for _, w := range fw {
		have[w] = true
	}
	for _, w := range pw {
		if !have[w] {
			return false
		}
	}
	return true
}

// nameMemo — сравнения имён в одной сборке книги: одни и те же клиенты
// сравниваются тысячи раз, а у улик с клиентом из справочника имя одно.
type nameMemo struct {
	same   map[[2]int]bool
	covers map[[2]int]bool
}

func newNameMemo() *nameMemo {
	return &nameMemo{same: map[[2]int]bool{}, covers: map[[2]int]bool{}}
}

// sameClient — один ли клиент у двух улик.
func (nm *nameMemo) sameClient(aID int, a string, bID int, b string) bool {
	if aID != 0 && aID == bID {
		return true
	}
	if strings.TrimSpace(a) == "" || strings.TrimSpace(b) == "" {
		return false
	}
	if aID == 0 || bID == 0 {
		return SameName(a, b)
	}
	k := [2]int{aID, bID}
	if v, ok := nm.same[k]; ok {
		return v
	}
	v := SameName(a, b)
	nm.same[k] = v
	return v
}

// nameCovers — неполное имя одной улики входит в полное имя другой (в любую сторону).
func (nm *nameMemo) nameCovers(aID int, a string, bID int, b string) bool {
	if aID == 0 || bID == 0 {
		return NameCovers(a, b) || NameCovers(b, a)
	}
	k := [2]int{aID, bID}
	if v, ok := nm.covers[k]; ok {
		return v
	}
	v := NameCovers(a, b) || NameCovers(b, a)
	nm.covers[k] = v
	return v
}

// samePrinted — одно ли напечатанное имя (получатель/плательщик на чеке).
func samePrinted(a, b string) bool {
	return ClientKey(a) == ClientKey(b) || SameName(a, b)
}

// longDoc — номер документа достаточно длинный, чтобы совпадение значило «тот
// же чек» даже при разной прочитанной сумме.
func longDoc(nd string) bool { return len([]rune(nd)) >= 8 }

func sameLongDoc(a, b *Check) bool { return longDoc(a.nd) && a.nd == b.nd }

// sameCheck — c и m — один и тот же чек (присланный дважды / в двух группах).
// named — оба чека с названным клиентом (иначе клиента сравнить нельзя, и без
// номера документа нужны совпадающие напечатанные получатель и время).
func sameCheck(c, m *Check, named bool, nm *nameMemo) bool {
	if absDur(c.OpDate.Sub(m.OpDate)) > 36*time.Hour {
		return false
	}
	dc, dm := c.nd, m.nd
	if dc != "" && dm != "" {
		if dc != dm {
			return false
		}
		return longDoc(dc) || sameMoney(c.Amount, m.Amount)
	}
	if !sameMoney(c.Amount, m.Amount) || absDur(c.OpDate.Sub(m.OpDate)) > 2*time.Minute {
		return false
	}
	if c.Printed != "" && m.Printed != "" && !samePrinted(c.Printed, m.Printed) {
		return false // разные получатели на чеках — разные переводы
	}
	if c.Payer != "" && m.Payer != "" && !samePrinted(c.Payer, m.Payer) {
		return false // разные плательщики — разные переводы
	}
	if a, b := NormDoc(c.Auth), NormDoc(m.Auth); a != "" && b != "" && a != b {
		return false
	}
	// Время напечатано у обоих и совпадает до минуты/секунды — сильный признак.
	exact := !c.TimeGuessed && !m.TimeGuessed && c.OpDate.Equal(m.OpDate) && c.Printed != "" && m.Printed != ""
	if named {
		if nm.sameClient(c.ContactID, c.Client, m.ContactID, m.Client) {
			return true
		}
		// «Магомедов Ахмед» в одной группе, «Ахмед» в другой — тот же чек, если
		// всё напечатанное совпало.
		return exact && nm.nameCovers(c.ContactID, c.Client, m.ContactID, m.Client)
	}
	return exact
}

// twinGap — насколько текст далёк от чеков платежа: по времени присылки чека
// или по времени операции на нём (чек могут прислать через выходные).
func twinGap(p *Payment, t *Text) time.Duration {
	best := time.Duration(math.MaxInt64)
	for _, c := range p.Checks {
		if g := absDur(c.PostedAt.Sub(t.PostedAt)); g < best {
			best = g
		}
		if !c.OpDate.IsZero() && !c.TimeGuessed {
			if g := absDur(c.OpDate.Sub(t.PostedAt)); g < best {
				best = g
			}
		}
	}
	return best
}

// textLead — текст пишут, когда деньги пришли; текст, написанный задолго ДО
// операции на чеке, — про другой платёж (вчерашний перевод без чека не
// «съедает» сегодняшний чек).
const textLead = 3 * time.Hour

// textBeforePayment — текст написан раньше операции на чеке больше чем на textLead.
// Время операции позже присылки самого чека — ошибка чтения (или другой
// часовой пояс), по нему не судим.
func textBeforePayment(p *Payment, t *Text) bool {
	for _, c := range p.Checks {
		if c.Review || !timeReliable(c) {
			continue
		}
		if c.OpDate.After(t.PostedAt.Add(textLead)) {
			return true
		}
	}
	return false
}

// timeReliable — по времени операции на чеке можно судить: оно прочитано (не
// подставлено), не позже присылки самого чека (иначе — ошибка чтения или другой
// часовой пояс) и не ровно 12:00:00 (так ставится время, когда на чеке только
// дата).
func timeReliable(c *Check) bool {
	op := c.OpDate
	if c.TimeGuessed || op.IsZero() || op.After(c.PostedAt.Add(10*time.Minute)) {
		return false
	}
	return !(op.Hour() == 12 && op.Minute() == 0 && op.Second() == 0 && op.Nanosecond() == 0)
}

// gapBucket — близость текста к чеку крупно: рядом (2 часа), в тот же день,
// дальше. Сначала решает близость, потом — полное ли имя.
func gapBucket(g time.Duration) int {
	switch {
	case g <= shortNameWindow:
		return 0
	case g <= 24*time.Hour:
		return 1
	}
	return 2
}

// shortNameWindow — текст с неполным именем («Ахмед 10000») относим к чеку
// только рядом по времени и от того же человека или в той же группе.
const shortNameWindow = 2 * time.Hour

// splitWindow — текст-итог платежа частями и его чеки присылают рядом.
const splitWindow = 3 * time.Hour

// textMatch — подходит ли клиент текста к чеку платежа: 0 — тот же клиент,
// 1 — неполное имя (слова одного есть в другом) от того же человека или в той
// же группе (только если short), -1 — нет.
func textMatch(p *Payment, t *Text, short bool, nm *nameMemo) int {
	kind := -1
	for _, c := range p.Checks {
		if c.Review || c.ContactID == 0 || strings.TrimSpace(c.Client) == "" {
			continue // у непроверенного чека клиент не назван (там напечатанный получатель)
		}
		if nm.sameClient(c.ContactID, c.Client, t.ContactID, t.Client) {
			return 0
		}
		if !short || kind == 1 || strings.TrimSpace(t.Client) == "" {
			continue
		}
		near := (t.Phone != "" && t.Phone == c.Phone) || t.Group == c.Group
		if near && nm.nameCovers(t.ContactID, t.Client, c.ContactID, c.Client) {
			kind = 1
		}
	}
	return kind
}

// Build собирает книгу платежей из чеков и текстов. Правила:
//
//  1. Удалённое в WhatsApp, исключённое владельцем и повтор уже присланного
//     чека в сбор не идут (видны в «не вошло» с причиной).
//  2. Копия чека, пересланная ботом в другую группу, — тот же платёж, что
//     исходный чек (своего платежа не даёт).
//  3. Чек, у которого не ясен клиент или сумма, ждёт проверки (не в сборе).
//     Если тот же чек уже посчитан в другой группе — он часть того платежа;
//     несколько непроверенных экземпляров одного чека — один в «не вошло».
//  4. Один и тот же чек, присланный вручную в две разные группы (тот же номер
//     документа — даже если сумму в одной группе поправили; без номера — тот
//     же клиент, сумма, минута операции, получатель и плательщик), — один платёж.
//  5. Наличка-повтор, ожидающая ответа «новый или тот же?», не в сборе.
//  6. Текстовый перевод про чек (тот же клиент и сумма, присланы в пределах
//     двух суток от присылки чека или от операции на нём) — тот же платёж:
//     считается чек, текст подтверждает его. Пары подбираются по близости во
//     времени (ближайшие — первыми). В одной группе один чек — один текст: два
//     одинаковых текста при одном чеке — два платежа; тот же текст в другой
//     группе — тот же платёж. Неполное имя («Ахмед 10000») — к чеку
//     «Магомедов Ахмед» той же суммы, только от того же человека или в той же
//     группе и в пределах двух часов.
//  7. Текст-итог платежа частями («Исаев 100000» и два чека по 50000 того же
//     клиента в той же группе рядом по времени) — чеки уже посчитаны.
//  8. Наличка — всегда отдельный платёж (чеком не подтверждается).
func Build(checks []Check, texts []Text) *Book {
	b := &Book{}
	nm := newNameMemo()
	cs := make([]*Check, len(checks))
	for i := range checks {
		cs[i] = &checks[i]
		cs[i].nd = NormDoc(cs[i].Doc)
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
	var copies, live, review []*Check
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
			review = append(review, c)
		default:
			live = append(live, c)
		}
	}

	attach := func(p *Payment, c *Check) {
		p.Checks = append(p.Checks, c)
		p.groups[c.Group] = true
		paymentOf[c] = p
	}

	// 4: один чек в разных группах — один платёж.
	mains := newCheckIndex()
	for _, c := range live {
		var into *Payment
		for _, m := range mains.near(c) {
			p := paymentOf[m]
			if p.groups[c.Group] && (c.nd == "" || c.nd != m.nd) {
				continue // без номера в одной группе — могут быть два перевода частями
			}
			if sameCheck(c, m, true, nm) {
				into = p
				break
			}
		}
		if into != nil {
			attach(into, c)
			if !sameMoney(into.Amount, c.Amount) && c.Fixed && !into.Main().Fixed {
				into.Amount = c.Amount // сумму исправили в этой группе — верна она
			}
			if m := into.Main(); !nm.sameClient(c.ContactID, c.Client, m.ContactID, m.Client) &&
				!nm.nameCovers(c.ContactID, c.Client, m.ContactID, m.Client) {
				into.Conflicts = append(into.Conflicts, c)
			}
			continue
		}
		p := &Payment{
			Key: "c" + itoa(c.ID), Client: c.Client, ClientID: c.ContactID, Amount: c.Amount,
			OpDate: c.OpDate, PostedAt: c.PostedAt, groups: map[string]bool{},
		}
		attach(p, c)
		mains.add(c)
		b.Payments = append(b.Payments, p)
	}

	// 3: непроверенный чек, уже посчитанный в другой группе, — часть того
	// платежа; непроверенные экземпляры одного чека — один в «не вошло».
	// Без номера документа в одной группе это могут быть два перевода частями —
	// там решает проверка повтора при получении.
	counted := newCheckIndex()
	for _, p := range b.Payments {
		for _, m := range p.Checks {
			counted.add(m)
		}
	}
	pending := newCheckIndex()
	reviewRoot := map[*Check]*Check{}
	for _, c := range review {
		var into *Payment
		for _, m := range counted.near(c) {
			if (m.Group != c.Group || sameLongDoc(c, m)) && sameCheck(c, m, false, nm) {
				into = paymentOf[m]
				break
			}
		}
		if into != nil {
			attach(into, c)
			continue
		}
		e := Excluded{Reason: ReasonReview, Check: c}
		for _, o := range pending.near(c) {
			if (o.Group != c.Group || sameLongDoc(c, o)) && sameCheck(c, o, false, nm) {
				e.Of = o
				break
			}
		}
		if e.Of == nil {
			pending.add(c)
		}
		reviewRoot[c] = e.Root()
		b.Excluded = append(b.Excluded, e)
	}

	// 2: копии — к платежу исходного чека (или туда же, где исходный отброшен).
	for _, c := range copies {
		orig := byWa[c.FwdOrigin]
		if p := paymentOf[orig]; p != nil {
			attach(p, c)
			continue
		}
		if root := reviewRoot[orig]; root != nil {
			// Исходный ждёт «чей чек?» — копия ждёт вместе с ним (один раз).
			b.Excluded = append(b.Excluded, Excluded{Reason: ReasonReview, Check: c, Of: root})
			continue
		}
		b.Excluded = append(b.Excluded, Excluded{Reason: ReasonCopy, Check: c})
	}

	// 5–8: тексты.
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
	var transfers []*Text
	for _, t := range ts {
		switch {
		case t.Amount <= 0:
		case t.Deleted:
			b.Excluded = append(b.Excluded, Excluded{Reason: ReasonDeleted, Text: t})
		case t.Ignored:
			// старая версия исправленного сообщения или исключённый платёж
		case t.Held:
			b.Excluded = append(b.Excluded, Excluded{Reason: ReasonHeld, Text: t})
		default:
			transfers = append(transfers, t)
		}
	}

	// 6: пары «текст — чек» по близости (ближайшие — первыми).
	type cand struct {
		t    *Text
		p    *Payment
		kind int
		gap  time.Duration
	}
	// Платежи с чеками по сумме и дню (присылки и операции): текст ищет пару
	// только среди чеков той же суммы за соседние дни.
	byAmountDay := map[[2]int64][]*Payment{}
	for _, p := range checkPays {
		k := amountKey(p.Amount)
		days := map[int64]bool{}
		for _, c := range p.Checks {
			days[dayKey(c.PostedAt)] = true
			if !c.OpDate.IsZero() {
				days[dayKey(c.OpDate)] = true
			}
		}
		for d := range days {
			byAmountDay[[2]int64{k, d}] = append(byAmountDay[[2]int64{k, d}], p)
		}
	}
	var cands []cand
	mark := 0
	for _, t := range transfers {
		if t.Cash {
			continue
		}
		k, d0 := amountKey(t.Amount), dayKey(t.PostedAt)
		mark++
		for k2 := k - 1; k2 <= k+1; k2++ {
			for d := d0 - 3; d <= d0+3; d++ {
				for _, p := range byAmountDay[[2]int64{k2, d}] {
					if p.mark == mark || !sameMoney(p.Amount, t.Amount) {
						continue
					}
					p.mark = mark
					gap := twinGap(p, t)
					if gap >= TwinWindow || textBeforePayment(p, t) {
						continue
					}
					kind := textMatch(p, t, gap <= shortNameWindow, nm)
					if kind < 0 {
						continue
					}
					cands = append(cands, cand{t, p, kind, gap})
				}
			}
		}
	}
	sort.SliceStable(cands, func(i, j int) bool {
		a, c := cands[i], cands[j]
		if ba, bc := gapBucket(a.gap), gapBucket(c.gap); ba != bc {
			return ba < bc
		}
		if a.kind != c.kind {
			return a.kind < c.kind
		}
		if a.gap != c.gap {
			return a.gap < c.gap
		}
		if a.t.ID != c.t.ID {
			return a.t.ID < c.t.ID
		}
		return a.p.Key < c.p.Key
	})
	twinOf := map[*Text]*Payment{}
	textIn := map[*Payment]map[string]bool{}
	for _, c := range cands {
		if twinOf[c.t] != nil || textIn[c.p][c.t.Group] {
			continue
		}
		twinOf[c.t] = c.p
		if textIn[c.p] == nil {
			textIn[c.p] = map[string]bool{}
		}
		textIn[c.p][c.t.Group] = true
	}

	// 7: текст-итог платежа частями. Сначала — итоги рядом со своими чеками
	// (в той же группе), потом — тот же итог в другой группе (или итог только
	// в группе без чеков): результат не зависит от того, где написали раньше.
	splitOf := map[*Text]split{}
	var splits []*Text
	byTime := newTimeIndex(checkPays)
	extra := uncountedChecks(b.Excluded)
	record := func(t *Text, sp split) {
		splitOf[t] = sp
		splits = append(splits, t)
		for _, p := range sp.parts {
			if textIn[p] == nil {
				textIn[p] = map[string]bool{}
			}
			textIn[p][t.Group] = true
		}
	}
	for _, anyGroup := range []bool{false, true} {
		for _, t := range transfers {
			if t.Cash || twinOf[t] != nil || len(splitOf[t].parts) > 0 {
				continue
			}
			if anyGroup {
				if sp, ok := sameSplitElsewhere(t, splits, splitOf, textIn, nm); ok {
					record(t, sp)
					continue
				}
			}
			if sp := splitParts(t, byTime.around(t.PostedAt, splitWindow), extra, textIn, nm, anyGroup); len(sp.parts) > 0 {
				record(t, sp)
			}
		}
	}

	for _, t := range transfers {
		if p := twinOf[t]; p != nil {
			p.Texts = append(p.Texts, t)
			p.groups[t.Group] = true
			b.Excluded = append(b.Excluded, Excluded{Reason: ReasonTwin, Text: t, Into: p})
			continue
		}
		if sp := splitOf[t]; len(sp.parts) > 0 {
			sp.parts[0].Texts = append(sp.parts[0].Texts, t)
			for _, p := range sp.parts {
				p.groups[t.Group] = true
			}
			b.Excluded = append(b.Excluded, Excluded{Reason: ReasonSplit, Text: t, Into: sp.parts[0], Parts: sp.parts, Missing: sp.missing})
			continue
		}
		b.Payments = append(b.Payments, &Payment{
			Key: "t" + itoa(t.ID), Client: t.Client, ClientID: t.ContactID, Amount: t.Amount, Cash: t.Cash,
			OpDate: t.OpDate, PostedAt: t.PostedAt, Texts: []*Text{t}, groups: map[string]bool{t.Group: true},
		})
	}
	for _, p := range b.Payments {
		p.ChatAt = chatAt(p)
	}
	return b
}

// chatAt — когда платёж впервые появился в чате: самая ранняя улика (чек или
// текст про него), присланная в месяце операции; если таких нет — присылка
// чека (тогда это «старый чек» для месяца присылки).
func chatAt(p *Payment) time.Time {
	m := p.Main()
	if m == nil {
		return p.PostedAt
	}
	inMonth := func(t time.Time) bool { return t.Year() == p.OpDate.Year() && t.Month() == p.OpDate.Month() }
	var best time.Time
	consider := func(t time.Time) {
		if inMonth(t) && (best.IsZero() || t.Before(best)) {
			best = t
		}
	}
	for _, c := range p.Checks {
		consider(c.PostedAt)
	}
	for _, t := range p.Texts {
		consider(t.PostedAt)
	}
	if best.IsZero() {
		return p.PostedAt
	}
	return best
}

// split — части платежа, итог которого написан текстом: посчитанные платежи
// и чеки, которые сейчас не в сборе.
type split struct {
	parts   []*Payment
	missing []*Check
}

// waiting — непосчитанный экземпляр чека и его корень (один чек в нескольких
// группах — один корень).
type waiting struct {
	inst, root *Check
	reason     Reason
}

// uncountedChecks — чеки, которые могут быть частью платежа, но сейчас не в
// сборе: ждут «чей чек?», исключены владельцем, удалены в WhatsApp.
func uncountedChecks(ex []Excluded) []waiting {
	var out []waiting
	for _, e := range ex {
		if e.Check == nil || e.Check.Amount <= 0 {
			continue
		}
		switch e.Reason {
		case ReasonReview, ReasonIgnored, ReasonDeleted:
			out = append(out, waiting{inst: e.Check, root: e.Root(), reason: e.Reason})
		}
	}
	return out
}

func checkGap(c *Check, t *Text) time.Duration {
	g := absDur(c.PostedAt.Sub(t.PostedAt))
	if !c.OpDate.IsZero() && !c.TimeGuessed {
		if g2 := absDur(c.OpDate.Sub(t.PostedAt)); g2 < g {
			g = g2
		}
	}
	return g
}

// sameSplitElsewhere — тот же итог (клиент и сумма), уже узнанный в другой
// группе: этот текст — про те же части.
func sameSplitElsewhere(t *Text, splits []*Text, splitOf map[*Text]split, textIn map[*Payment]map[string]bool, nm *nameMemo) (split, bool) {
	for _, o := range splits {
		if o.Group == t.Group || !sameMoney(o.Amount, t.Amount) || absDur(o.PostedAt.Sub(t.PostedAt)) >= TwinWindow ||
			!nm.sameClient(o.ContactID, o.Client, t.ContactID, t.Client) {
			continue
		}
		sp := splitOf[o]
		free := true
		for _, p := range sp.parts {
			if textIn[p][t.Group] {
				free = false
			}
		}
		if free {
			return sp, true
		}
	}
	return split{}, false
}

// splitParts — 2–3 чека того же клиента без своего текста в этой группе, рядом
// по времени, сумма которых равна сумме текста (платёж частями из-за лимита
// банка). Части ищутся в группе текста (anyGroup — в любой группе: итог
// написали там, где чеков нет). Хотя бы одна часть посчитана; остальные могут
// ждать «чей чек?», быть исключены или удалены — но только от того же
// работника и не от другого плательщика: тогда текст не считается сверх своих
// чеков. Из подходящих наборов — с меньшим числом непосчитанных частей, меньший
// и самый близкий по времени.
func splitParts(t *Text, pays []*Payment, extra []waiting, textIn map[*Payment]map[string]bool, nm *nameMemo, anyGroup bool) split {
	var cand []*Payment
	for _, p := range pays {
		if textIn[p][t.Group] || p.Amount >= t.Amount || textBeforePayment(p, t) {
			continue
		}
		gap := twinGap(p, t)
		if gap > splitWindow || textMatch(p, t, gap <= shortNameWindow, nm) < 0 {
			continue
		}
		inGroup := anyGroup
		for _, c := range p.Checks {
			if c.Group == t.Group {
				inGroup = true
			}
		}
		if inGroup {
			cand = append(cand, p)
		}
	}
	if len(cand) == 0 {
		return split{}
	}
	phones := map[string]bool{}
	payers := map[string]bool{}
	if t.Phone != "" {
		phones[t.Phone] = true
	}
	for _, p := range cand {
		for _, c := range p.Checks {
			if c.Phone != "" {
				phones[c.Phone] = true
			}
			if c.Payer != "" {
				payers[ClientKey(c.Payer)] = true
			}
		}
	}
	var miss []*Check
	seenRoot := map[*Check]bool{}
	for _, w := range extra {
		c := w.inst
		if seenRoot[w.root] || (!anyGroup && c.Group != t.Group) || c.Amount >= t.Amount || checkGap(c, t) > splitWindow {
			continue
		}
		// Часть платежа присылает тот же работник; удалённый чек — только тот,
		// кто написал итог.
		if c.Phone == "" || !phones[c.Phone] || (w.reason == ReasonDeleted && c.Phone != t.Phone) {
			continue
		}
		if c.Payer != "" && len(payers) > 0 && !payers[ClientKey(c.Payer)] {
			continue // другой плательщик — другой перевод
		}
		// Чек без названного клиента может быть частью; названный — только тот же клиент.
		if c.ContactID != 0 && c.Confirmed && !nm.sameClient(c.ContactID, c.Client, t.ContactID, t.Client) {
			continue
		}
		seenRoot[w.root] = true
		miss = append(miss, w.root)
	}
	if len(cand)+len(miss) < 2 {
		return split{}
	}
	sort.SliceStable(cand, func(i, j int) bool { return twinGap(cand[i], t) < twinGap(cand[j], t) })
	if len(cand) > 10 {
		cand = cand[:10]
	}
	sort.SliceStable(miss, func(i, j int) bool { return checkGap(miss[i], t) < checkGap(miss[j], t) })
	if len(miss) > 6 {
		miss = miss[:6]
	}
	type item struct {
		p   *Payment
		c   *Check
		amt float64
		gap time.Duration
	}
	var items []item
	for _, p := range cand {
		items = append(items, item{p: p, amt: p.Amount, gap: twinGap(p, t)})
	}
	for _, c := range miss {
		items = append(items, item{c: c, amt: c.Amount, gap: checkGap(c, t)})
	}
	var best []item
	var bestMiss int
	var bestGap time.Duration
	consider := func(set []item) {
		var sum float64
		var worst time.Duration
		counted, missing := 0, 0
		for _, it := range set {
			sum += it.amt
			if it.gap > worst {
				worst = it.gap
			}
			if it.p != nil {
				counted++
			} else {
				missing++
			}
		}
		if counted == 0 || !sameMoney(sum, t.Amount) {
			return
		}
		better := best == nil || missing < bestMiss ||
			(missing == bestMiss && (len(set) < len(best) || (len(set) == len(best) && worst < bestGap)))
		if better {
			best, bestMiss, bestGap = append([]item(nil), set...), missing, worst
		}
	}
	for i := range items {
		for j := i + 1; j < len(items); j++ {
			consider([]item{items[i], items[j]})
			for k := j + 1; k < len(items); k++ {
				consider([]item{items[i], items[j], items[k]})
			}
		}
	}
	var out split
	for _, it := range best {
		if it.p != nil {
			out.parts = append(out.parts, it.p)
		} else {
			out.missing = append(out.missing, it.c)
		}
	}
	sort.SliceStable(out.parts, func(i, j int) bool { return out.parts[i].PostedAt.Before(out.parts[j].PostedAt) })
	return out
}

func amountKey(a float64) int64 { return int64(math.Round(a)) }

// checkIndex — поиск «того же чека» без перебора всех чеков: по номеру
// документа и по сумме (в порядке добавления).
type checkIndex struct {
	byDoc map[string][]*Check
	byAmt map[[2]int64][]*Check // сумма и день операции
}

func newCheckIndex() *checkIndex {
	return &checkIndex{byDoc: map[string][]*Check{}, byAmt: map[[2]int64][]*Check{}}
}

func (x *checkIndex) add(c *Check) {
	if c.nd != "" {
		x.byDoc[c.nd] = append(x.byDoc[c.nd], c)
	}
	k := [2]int64{amountKey(c.Amount), dayKey(c.OpDate)}
	x.byAmt[k] = append(x.byAmt[k], c)
}

// near — чеки с тем же номером документа, затем с той же суммой и датой
// операции в пределах двух дней (дальше «тот же чек» не ищем).
func (x *checkIndex) near(c *Check) []*Check {
	var out []*Check
	seen := map[*Check]bool{}
	push := func(cs []*Check) {
		for _, m := range cs {
			if !seen[m] {
				seen[m] = true
				out = append(out, m)
			}
		}
	}
	if c.nd != "" {
		push(x.byDoc[c.nd])
	}
	k, d0 := amountKey(c.Amount), dayKey(c.OpDate)
	for k2 := k - 1; k2 <= k+1; k2++ {
		for d := d0 - 2; d <= d0+2; d++ {
			push(x.byAmt[[2]int64{k2, d}])
		}
	}
	return out
}

func dayKey(t time.Time) int64 { return t.Unix() / 86400 }

// timeIndex — платежи с чеками по времени присылки и операции.
type timeIndex struct {
	at []time.Time
	ps []*Payment
}

func newTimeIndex(pays []*Payment) *timeIndex {
	type ev struct {
		at time.Time
		p  *Payment
	}
	var evs []ev
	for _, p := range pays {
		for _, c := range p.Checks {
			evs = append(evs, ev{c.PostedAt, p})
			if !c.OpDate.IsZero() {
				evs = append(evs, ev{c.OpDate, p})
			}
		}
	}
	sort.SliceStable(evs, func(i, j int) bool { return evs[i].at.Before(evs[j].at) })
	x := &timeIndex{}
	for _, e := range evs {
		x.at = append(x.at, e.at)
		x.ps = append(x.ps, e.p)
	}
	return x
}

// around — платежи, у которых чек прислан или проведён в пределах d от t.
func (x *timeIndex) around(t time.Time, d time.Duration) []*Payment {
	lo := sort.Search(len(x.at), func(i int) bool { return !x.at[i].Before(t.Add(-d)) })
	var out []*Payment
	seen := map[*Payment]bool{}
	for i := lo; i < len(x.at) && !x.at[i].After(t.Add(d)); i++ {
		if !seen[x.ps[i]] {
			seen[x.ps[i]] = true
			out = append(out, x.ps[i])
		}
	}
	return out
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
			if !f.inPeriod(p.chatTime()) || f.oldOperation(p) {
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
		if p.InGroups(f.Groups) && f.inPeriod(p.chatTime()) && f.oldOperation(p) {
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
