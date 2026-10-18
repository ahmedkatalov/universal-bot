// Сверка учёта с программой рассрочек «как человек»: по каждому клиенту берём
// ВСЮ картину (его чеки и наличку вокруг периода из всех групп, все его
// рассрочки и оплаты в программе) и объясняем каждый чек периода.
package bot

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"whatsapp-bot/internal/cmf"
	"whatsapp-bot/internal/db"
	"whatsapp-bot/internal/ledger"
)

// reconContextDays — сколько дней вокруг периода смотрим, чтобы оплаты соседних
// чеков (сентябрьский чек, внесённый 2 октября) не засчитали чужой чек.
const reconContextDays = 45

// reconRec — чек после склейки копий из разных групп.
type reconRec struct {
	db.ReconReceipt
	groups map[string]bool
}

// docLatin — кириллические буквы, неотличимые от латинских: на копиях одного чека
// распознавание пишет номер документа то так, то так.
var docLatin = map[rune]rune{
	'А': 'A', 'В': 'B', 'С': 'C', 'Е': 'E', 'Ё': 'E', 'Н': 'H', 'К': 'K', 'М': 'M',
	'О': 'O', 'Р': 'P', 'Т': 'T', 'Х': 'X', 'У': 'Y',
}

// normDoc — номер документа без пробелов/знаков (на копиях его печатают по-разному).
func normDoc(s string) string {
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

// sameCheckCopy — это копия того же чека из ДРУГОЙ группы? Та же сумма и либо
// тот же номер документа (и та же дата операции ±1 день — номера у разных банков
// повторяются), либо (номер есть не на обеих копиях) та же минута операции и то
// же лицо. У неподтверждённых копий имя — получатель с чека: две такие копии
// сравнимы между собой, а с подтверждённым клиентом — нет.
func sameCheckCopy(a, b db.ReconReceipt) bool {
	if !sameMoney(itemKop(a.Amount), itemKop(b.Amount)) {
		return false
	}
	da, dbn := normDoc(a.DocNumber), normDoc(b.DocNumber)
	if da != "" && dbn != "" {
		dd := dayNum(a.TxDate) - dayNum(b.TxDate)
		return da == dbn && dd >= -1 && dd <= 1
	}
	gap := a.TxDate.Sub(b.TxDate)
	if gap < 0 {
		gap = -gap
	}
	if gap > 2*time.Minute {
		return false
	}
	switch {
	case a.NeedsReview && b.NeedsReview:
		na, nb := strings.TrimSpace(a.Name), strings.TrimSpace(b.Name)
		return na == "" || nb == "" || sameClientName(na, nb) || shareNameWord(na, nb)
	case a.NeedsReview || b.NeedsReview:
		return true
	}
	return sameClientName(a.Name, b.Name)
}

// shareNameWord — есть ли у имён общее полное слово (варианты распознавания
// одного получателя: «Хадижат Имрановна С.» / «Хадижат С.»).
func shareNameWord(a, b string) bool {
	fa, _ := nameParts(a)
	fb, _ := nameParts(b)
	for _, x := range fa {
		for _, y := range fb {
			if nameWordSame(x, y) {
				return true
			}
		}
	}
	return false
}

func fwdOrigin(waID string) string {
	if i := strings.Index(waID, "-fwd-"); i >= 0 {
		return waID[:i]
	}
	return ""
}

// dedupeReconReceipts склеивает копии ОДНОГО чека, разосланного по группам:
// пересылку «<id>-fwd-<группа>» — с оригиналом (и между собой, если оригинала нет
// в выборке), остальное — по sameCheckCopy, но ТОЛЬКО между разными группами
// (внутри одной группы повторы уже помечены дублями, а два одинаковых чека в
// одной группе — это два разных платежа). Группа, куда переслан оригинал, сразу
// считается занятой его копией. Ответ не зависит от порядка строк.
func dedupeReconReceipts(rs []db.ReconReceipt) []reconRec {
	rs = append([]db.ReconReceipt(nil), rs...)
	sort.SliceStable(rs, func(a, b int) bool {
		if !rs[a].TxDate.Equal(rs[b].TxDate) {
			return rs[a].TxDate.Before(rs[b].TxDate)
		}
		return rs[a].ID < rs[b].ID
	})
	fwdTo := map[string][]string{}
	for _, r := range rs {
		if o := fwdOrigin(r.WaMessageID); o != "" {
			fwdTo[o] = append(fwdTo[o], r.GroupJID)
		}
	}
	own := func(r db.ReconReceipt) []string {
		g := []string{r.GroupJID}
		if r.WaMessageID != "" && fwdOrigin(r.WaMessageID) == "" {
			g = append(g, fwdTo[r.WaMessageID]...)
		}
		return g
	}

	var out []reconRec
	var members [][]db.ReconReceipt
	var spanFrom, spanTo []time.Time // даты чеков группы копий (быстрый отсев)
	byWa := map[string]int{}
	merge := func(k int, r db.ReconReceipt) {
		for _, g := range own(r) {
			out[k].groups[g] = true
		}
		members[k] = append(members[k], r)
		if r.TxDate.Before(spanFrom[k]) {
			spanFrom[k] = r.TxDate
		}
		if r.TxDate.After(spanTo[k]) {
			spanTo[k] = r.TxDate
		}
		if out[k].NeedsReview && !r.NeedsReview { // предпочитаем копию с подтверждённым клиентом
			g, doc := out[k].groups, out[k].DocNumber
			out[k].ReconReceipt = r
			out[k].groups = g
			if out[k].DocNumber == "" {
				out[k].DocNumber = doc
			}
		} else if out[k].DocNumber == "" {
			out[k].DocNumber = r.DocNumber
		}
	}
	findCopy := func(r db.ReconReceipt) int {
		rd := normDoc(r.DocNumber)
		rg := own(r)
	next:
		for k := range out {
			// Копии одного чека — в пределах суток друг от друга.
			if r.TxDate.Sub(spanTo[k]) > 50*time.Hour || spanFrom[k].Sub(r.TxDate) > 50*time.Hour {
				continue
			}
			for _, g := range rg {
				if out[k].groups[g] {
					continue next
				}
			}
			copyOf := false
			for _, m := range members[k] {
				if md := normDoc(m.DocNumber); rd != "" && md != "" && md != rd {
					continue next // у копий одного чека номер документа один
				}
				same := sameCheckCopy(m, r)
				if !same && !m.NeedsReview && !r.NeedsReview {
					continue next // два подтверждённых клиента не склеиваются через неподтверждённую копию
				}
				copyOf = copyOf || same
			}
			if copyOf {
				return k
			}
		}
		return -1
	}
	add := func(r db.ReconReceipt) int {
		if k := findCopy(r); k >= 0 {
			merge(k, r)
			return k
		}
		groups := map[string]bool{}
		for _, g := range own(r) {
			groups[g] = true
		}
		out = append(out, reconRec{ReconReceipt: r, groups: groups})
		members = append(members, []db.ReconReceipt{r})
		spanFrom, spanTo = append(spanFrom, r.TxDate), append(spanTo, r.TxDate)
		return len(out) - 1
	}
	var fwd []db.ReconReceipt
	for _, r := range rs {
		if fwdOrigin(r.WaMessageID) != "" {
			fwd = append(fwd, r)
			continue
		}
		k := add(r)
		if r.WaMessageID != "" {
			byWa[r.WaMessageID] = k
		}
	}
	for _, r := range fwd {
		orig := fwdOrigin(r.WaMessageID)
		if k, ok := byWa[orig]; ok {
			merge(k, r)
			continue
		}
		byWa[orig] = add(r) // оригинала нет — следующие пересылки того же оригинала склеятся сюда
	}
	return out
}

// sameClientName — одно ли это имя клиента (разное написание/порядок слов/
// склонение/отчество/опечатка, «Каталов А.» = «Каталов Ахмед», «Хаджи-Мурат» =
// «Хаджи Мурат» = «Хаджимурат»). Однословные имена — только точное совпадение,
// чтобы «Ахмед» не прилип ко всем Ахмедам; разные имена (Рустам/Руслан,
// Мадина/Марина, Магомед/Магомед-Расул) — разные.
// Движок подсчёта сравнивает клиентов текста и чека строже сверки
// (sameClientMoney): «Магомедову Ахмеду» и «Магомедов Ахмед» — один клиент,
// а «Омаров» и «Умаров» — разные.
func init() {
	ledger.SameName = sameNameMemo
	ledger.NameCovers = func(part, full string) bool { return nameCoversWith(part, full, nameWordMoney) }
}

// sameNameMemo — sameClientName с запоминанием: в сверке одни и те же имена
// сравниваются тысячи раз (каждый клиент со всеми чеками за полтора месяца).
var (
	sameNameMu    sync.Mutex
	sameNameCache = map[string]bool{}
)

func sameNameMemo(a, b string) bool {
	k := a + "\x00" + b
	sameNameMu.Lock()
	v, ok := sameNameCache[k]
	sameNameMu.Unlock()
	if ok {
		return v
	}
	r := sameClientMoney(a, b)
	sameNameMu.Lock()
	if len(sameNameCache) > 300000 {
		sameNameCache = map[string]bool{}
	}
	sameNameCache[k] = r
	sameNameMu.Unlock()
	return r
}

func sameClientName(a, b string) bool { return sameClientNameWith(a, b, nameWordSame) }

// sameClientMoney — то же, но строже, для решений о деньгах (текст и чек — один
// платёж, два чека — один, наличка — возможный повтор): опечатка допускается
// только внутри слова. Первая буква должна совпасть («Омаров» ≠ «Умаров»,
// «Османов» ≠ «Усманов»), согласную в длинном слове не заменяем
// («Магомедов» ≠ «Магометов»). Падежи, порядок слов, инициалы — как в сверке.
func sameClientMoney(a, b string) bool { return sameClientNameWith(a, b, nameWordMoney) }

// clientNamePartMoney — одно имя целиком входит в другое («Харон» и «Исаев
// Харон», «Исаев» и «Исаев Харон Магомедович»): все полные слова короткого есть
// в длинном (сравнение слов — как для денег), инициалы не спорят. Это не «тот же
// клиент» вообще (Харонов много), а «совместимое» имя — годится там, где всё
// остальное уже совпало (тот же работник, сумма, свёрнутый экран и полный чек).
func clientNamePartMoney(a, b string) bool {
	fa, ia := nameParts(a)
	fb, ib := nameParts(b)
	if len(fa) == 0 || len(fb) == 0 {
		return false
	}
	if len(fa) > len(fb) {
		fa, fb, ia, ib = fb, fa, ib, ia
	}
	score, ua, ub := matchNameWordsWith(fa, fb, nameWordMoney)
	if score < len(fa) {
		return false
	}
	return initialsAgree(ia, leftovers(fb, ub, ib)) && initialsAgree(ib, leftovers(fa, ua, ia))
}

func sameClientNameWith(a, b string, same func(a, b string) bool) bool {
	ha, hb := hasHyphen(a), hasHyphen(b)
	if ha == hb && sameClientNameOnce(a, b, same) {
		return true // дефис у обоих (или ни у кого) — можно сравнивать по частям
	}
	da, db := dehyphen(a), dehyphen(b)
	if (ha || hb) && sameClientNameOnce(da, db, same) {
		return true // составное имя с дефисом — одно слово
	}
	return joinedSame(da, db, same) || joinedSame(db, da, same)
}

func hasHyphen(s string) bool { return strings.ContainsAny(s, "-‐–—") }

// joinedSame — «Хаджи Мурат» (через пробел) против «Хаджимурат»: склеиваем
// соседние слова x, только если склейка есть среди слов y.
func joinedSame(x, y string, same func(a, b string) bool) bool {
	fy, _ := nameParts(y)
	ws := strings.Fields(x)
	for k := 0; k+1 < len(ws); k++ {
		a, b := normCyr(ws[k]), normCyr(ws[k+1])
		if len([]rune(a)) < 3 || len([]rune(b)) < 3 {
			continue
		}
		hit := false
		for _, w := range fy {
			if same(a+b, w) {
				hit = true
				break
			}
		}
		if !hit {
			continue
		}
		v := append(append(append([]string(nil), ws[:k]...), ws[k]+ws[k+1]), ws[k+2:]...)
		if sameClientNameOnce(strings.Join(v, " "), y, same) {
			return true
		}
	}
	return false
}

func dehyphen(s string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case '-', '‐', '–', '—':
			return -1
		}
		return r
	}, s)
}

func sameClientNameOnce(a, b string, same func(a, b string) bool) bool {
	na, nb := normCyr(a), normCyr(b)
	if na == "" || nb == "" {
		return false
	}
	if na == nb {
		return true
	}
	fa, ia := nameParts(a)
	fb, ib := nameParts(b)
	mn := len(fa)
	if len(fb) < mn {
		mn = len(fb)
	}
	switch {
	case mn == 0:
		return false
	case mn == 1:
		// «Каталов А.» — одно слово годится только с инициалами.
		if (len(fa) == 1 && len(ia) == 0) || (len(fb) == 1 && len(ib) == 0) {
			return false
		}
	}
	score, ua, ub := matchNameWordsWith(fa, fb, same)
	if score < mn {
		return false
	}
	return initialsAgree(ia, leftovers(fb, ub, ib)) && initialsAgree(ib, leftovers(fa, ua, ia))
}

// leftovers — первые буквы несовпавших слов и инициалы стороны.
func leftovers(full []string, used []bool, initials []rune) []rune {
	var out []rune
	for k, w := range full {
		if !used[k] {
			out = append(out, []rune(w)[0])
		}
	}
	return append(out, initials...)
}

// initialsAgree — инициалы одной стороны не противоречат остатку другой:
// каждому нужен свой остаток на ту же букву (если остатков нет — не с чем спорить).
func initialsAgree(initials, otherRest []rune) bool {
	rest := append([]rune(nil), otherRest...)
	for _, ch := range initials {
		if len(rest) == 0 {
			return true
		}
		found := -1
		for k, r := range rest {
			if r == ch {
				found = k
				break
			}
		}
		if found < 0 {
			return false
		}
		rest = append(rest[:found], rest[found+1:]...)
	}
	return true
}

// matchNameWords — жадно сопоставляет полные слова (каждое — не больше одного раза).
func matchNameWords(fa, fb []string) (score int, ua, ub []bool) {
	return matchNameWordsWith(fa, fb, nameWordSame)
}

func matchNameWordsWith(fa, fb []string, same func(a, b string) bool) (score int, ua, ub []bool) {
	ua, ub = make([]bool, len(fa)), make([]bool, len(fb))
	for x, w := range fa {
		for y, v := range fb {
			if !ub[y] && same(w, v) {
				ua[x], ub[y] = true, true
				score++
				break
			}
		}
	}
	return score, ua, ub
}

// nameParts — полные слова (от 3 букв, нормализованные) и инициалы имени.
// «А.Н.», «А. Н.» и «АН» (заглавными в обычно написанном имени) — два инициала.
func nameParts(raw string) (full []string, initials []rune) {
	toks := strings.FieldsFunc(raw, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsMark(r) })
	mixed := false
	for _, t := range toks {
		if len([]rune(t)) >= 3 && strings.ToUpper(t) != t {
			mixed = true
		}
	}
	for _, t := range toks {
		n := normCyr(t)
		r := []rune(n)
		switch {
		case len(r) == 0:
		case len(r) >= 3:
			full = append(full, n)
		case len(r) == 2 && mixed && strings.ToUpper(t) == t:
			initials = append(initials, r[0], r[1])
		default:
			initials = append(initials, r[0])
		}
	}
	return full, initials
}

// patronymicTails — окончания отчеств (в т.ч. в косвенных падежах).
var patronymicTails = []string{"вич", "вича", "вичу", "вичем", "виче", "вна", "вны", "вне", "вну", "вной",
	"ична", "ичны", "ичне", "ичну", "ичной", "оглы", "кызы", "улы"}

// isPatronymic — слово похоже на отчество («Ахмедовна», «Нажудовича»).
func isPatronymic(w string) bool {
	if len([]rune(w)) < 6 {
		return false
	}
	for _, t := range patronymicTails {
		if strings.HasSuffix(w, t) {
			return true
		}
	}
	return false
}

// nameCovers — всё, что есть в имени query, есть и в полном имени cand: каждое
// полное слово совпало (nameWordSame) и инициалы не противоречат. «Каталов А.»
// покрывается «Каталов Ахмед Нажудович»; «Магомед» — НЕ «Магомедова Патимат»;
// «Альмурзаева М.» — НЕ «Альмурзаева Разет».
func nameCovers(query, cand string) bool {
	return nameCoversWith(query, cand, nameWordSame)
}

// nameCoversStrict — то же, но слова совпадают только точно или как падежные
// формы (й/и не в счёт): без допуска на «опечатку» — Мусаева ≠ Мусиева,
// Дудаева ≠ Дадаева (это разные фамилии). Для выбора варианта по ответу.
func nameCoversStrict(query, cand string) bool {
	return nameCoversWith(query, cand, nameWordStrict)
}

// nameCoversWith — составное имя через дефис сравниваем по частям, только если
// дефис есть у обоих (или ни у кого): «Магомед» ≠ «Магомед-Расул». Написание
// через пробел («Хаджи Мурат») сравниваем и со слитным («Хаджимурат»).
func nameCoversWith(query, cand string, same func(a, b string) bool) bool {
	hq, hc := hasHyphen(query), hasHyphen(cand)
	if hq == hc && nameCoversOnce(query, cand, same) {
		return true
	}
	dq, dc := dehyphen(query), dehyphen(cand)
	if (hq || hc) && nameCoversOnce(dq, dc, same) {
		return true
	}
	// «Хаджи Мурат Магомедов» ↔ «Магомедов Хаджимурат»: склеиваем соседние слова
	// запроса, если склейка есть среди слов клиента.
	fc, _ := nameParts(dc)
	ws := strings.Fields(dq)
	for k := 0; k+1 < len(ws); k++ {
		a, b := normCyr(ws[k]), normCyr(ws[k+1])
		if len([]rune(a)) < 3 || len([]rune(b)) < 3 {
			continue
		}
		for _, w := range fc {
			if same(a+b, w) {
				v := append(append(append([]string(nil), ws[:k]...), ws[k]+ws[k+1]), ws[k+2:]...)
				if nameCoversOnce(strings.Join(v, " "), dc, same) {
					return true
				}
			}
		}
	}
	return false
}

func nameCoversOnce(query, cand string, same func(a, b string) bool) bool {
	fq, iq := nameParts(query)
	fc, ic := nameParts(cand)
	if len(fq) == 0 {
		return false
	}
	used := make([]bool, len(fc))
	for _, w := range fq {
		hit := false
		for k, v := range fc {
			if !used[k] && same(w, v) {
				used[k], hit = true, true
				break
			}
		}
		if !hit {
			return false
		}
	}
	return initialsAgree(iq, leftovers(fc, used, ic))
}

// nameWordStrict — слова совпадают точно или как падежные формы одной основы.
func nameWordStrict(a, b string) bool {
	if isPatronymic(a) != isPatronymic(b) {
		return false
	}
	a, b = foldYi(a), foldYi(b)
	if a == b {
		return true
	}
	ra, rb := []rune(a), []rune(b)
	if len(ra) > len(rb) {
		ra, rb = rb, ra
	}
	if len(ra) < 3 {
		return false
	}
	p := 0
	for p < len(ra) && ra[p] == rb[p] {
		p++
	}
	if p == len(ra) {
		tail := string(rb[p:])
		return caseEndings[tail] || tail == "ь"
	}
	ta, tb := string(ra[p:]), string(rb[p:])
	return p >= 3 && ((caseEndings[ta] && caseEndings[tb]) || (ta == "ь" && softEndings[tb]) || (tb == "ь" && softEndings[ta]))
}

func isVowelRu(r rune) bool { return strings.ContainsRune("аеёиоуыэюя", r) }

// softEndings — падежные окончания имён на «ь»: Шамиль → Шамиля/Шамилю/Шамилем.
var softEndings = map[string]bool{"я": true, "ю": true, "е": true, "ем": true, "и": true}

// foldYi — «й» внутри слова → «и» (Хусейн/Хусеин, Айшат/Аишат); в конце слова
// «й» не трогаем — это окончание.
func foldYi(w string) string {
	r := []rune(w)
	for k := 0; k < len(r)-1; k++ {
		if r[k] == 'й' {
			r[k] = 'и'
		}
	}
	return string(r)
}

// nameWordSame — одно ли это слово имени (нормализованные). Строже, чем поиск
// клиента в программе: склонение, пропущенная/лишняя буква, перепутанная гласная
// («Ахмед/Ахмад», «Каталов/Котолов») — да; другая согласная в коротком слове
// («Рустам/Руслан», «Мадина/Марина», «Ахмедов/Ахматов») — нет; короткие слова
// (Иса/Ира) — только точно или склонение.
func nameWordSame(a, b string) bool {
	if isPatronymic(a) != isPatronymic(b) {
		return false // «Магомедова» (фамилия) — не «Магомедовна» (отчество)
	}
	if nameWordSame0(a, b) {
		return true
	}
	if fa, fb := foldYi(a), foldYi(b); fa != a || fb != b {
		return nameWordSame0(fa, fb)
	}
	return false
}

// nameWordMoney — nameWordSame без опечатки в первой букве и без замены
// согласной в длинном слове.
func nameWordMoney(a, b string) bool {
	if isPatronymic(a) != isPatronymic(b) {
		return false
	}
	ra, rb := []rune(a), []rune(b)
	if len(ra) == 0 || len(rb) == 0 || ra[0] != rb[0] {
		return false
	}
	if nameWordSame0(a, b, true) {
		return true
	}
	if fa, fb := foldYi(a), foldYi(b); fa != a || fb != b {
		return nameWordSame0(fa, fb, true)
	}
	return false
}

func nameWordSame0(a, b string, strict ...bool) bool {
	money := len(strict) > 0 && strict[0]
	if a == b {
		return true
	}
	ra, rb := []rune(a), []rune(b)
	if len(ra) > len(rb) {
		ra, rb = rb, ra
	}
	if len(ra) < 3 {
		return false
	}
	p := 0
	for p < len(ra) && ra[p] == rb[p] {
		p++
	}
	if p == len(ra) { // одно — начало другого: решает только окончание
		tail := string(rb[p:])
		return caseEndings[tail] || tail == "ь"
	}
	if ta, tb := string(ra[p:]), string(rb[p:]); p >= 3 &&
		((caseEndings[ta] && caseEndings[tb]) || (ta == "ь" && softEndings[tb]) || (tb == "ь" && softEndings[ta])) {
		return true // два падежа одной основы: «Каталова» / «Каталову», «Шамиль» / «Шамиля»
	}
	if len(ra) < 5 {
		return false
	}
	if len(ra) == len(rb) {
		var diffs []int
		for k := range ra {
			if ra[k] != rb[k] {
				diffs = append(diffs, k)
			}
		}
		switch len(diffs) {
		case 1:
			k := diffs[0]
			return (isVowelRu(ra[k]) && isVowelRu(rb[k])) || (len(ra) >= 8 && !money)
		case 2:
			x, y := diffs[0], diffs[1]
			if y == x+1 && ra[x] == rb[y] && ra[y] == rb[x] {
				return true // переставили соседние буквы
			}
			if len(ra) < 6 {
				return false
			}
			return isVowelRu(ra[x]) && isVowelRu(rb[x]) && isVowelRu(ra[y]) && isVowelRu(rb[y])
		}
		return false
	}
	if len(rb) == len(ra)+1 { // пропущенная/лишняя буква внутри слова
		for k := 0; k < len(rb); k++ {
			if string(rb[:k])+string(rb[k+1:]) == string(ra) {
				return true
			}
		}
	}
	return false
}

type reconBucket struct {
	display string
	items   []reconItem // позиции периода (Report=true)
}

// cmfReconcile — живая сверка учёта за период [from, to) с программой.
func (b *Bot) cmfReconcile(ctx context.Context, from, to time.Time, groupJID string) (string, error) {
	return b.cmfReconcileFor(ctx, from, to, groupJID, "")
}

// cmfReconcileFor — сверка, при client != "" — только чеки этого клиента
// («внесён ли чек Каталова?» — без сверки всех клиентов за период).
func (b *Bot) cmfReconcileFor(ctx context.Context, from, to time.Time, groupJID, client string) (string, error) {
	// Сверка не дольше 2 минут: висящая программа не держит ответ владельцу вечно.
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	ctxFrom := from.AddDate(0, 0, -reconContextDays)
	ctxTo := to.AddDate(0, 0, reconContextDays)

	rawRecs, err := b.db.ReceiptsForReconcile(ctx, ctxFrom, ctxTo)
	if err != nil {
		return "", err
	}
	cash, err := b.db.CashForReconcile(ctx, ctxFrom, ctxTo)
	if err != nil {
		return "", err
	}
	recs := dedupeReconReceipts(rawRecs)

	inPeriod := func(t time.Time) bool { return !t.Before(from) && t.Before(to) }
	inGroup := func(groups map[string]bool) bool { return groupJID == "" || groups[groupJID] }

	periodLabel := from.Format("02.01.2006")
	if last := to.AddDate(0, 0, -1); last.Format("2006-01-02") != from.Format("2006-01-02") {
		periodLabel += " — " + last.Format("02.01.2006")
	}

	// Позиции ПЕРИОДА — по клиентам (нормализованное имя). Неподтверждённые чеки
	// (клиента не назвали — «имя» там владельца карты) в сверку не берём, считаем
	// отдельно: сначала надо понять, чей чек.
	order := []string{}
	buckets := map[string]*reconBucket{}
	addReported := func(name string, it reconItem) {
		key := normCyr(name)
		bk := buckets[key]
		if bk == nil {
			bk = &reconBucket{display: strings.TrimSpace(name)}
			buckets[key] = bk
			order = append(order, key)
		}
		bk.items = append(bk.items, it)
	}
	unconfirmed, unconfirmedSum := 0, 0.0
	nChecks, nCash := 0, 0
	// Чеки, про которые в группе ответили «нет в программе», никому не относим
	// (даже по памяти о плательщике) — показываем отдельно.
	var waIDs []string
	for _, r := range recs {
		if r.WaMessageID != "" {
			waIDs = append(waIDs, r.WaMessageID)
		}
	}
	watchStatus, _ := b.db.CmfWatchStatusByMessages(ctx, waIDs)
	var notInProgram []string
	for _, r := range recs {
		if !inPeriod(r.TxDate) || !inGroup(r.groups) {
			continue
		}
		if r.NeedsReview || strings.TrimSpace(r.Name) == "" {
			unconfirmed++
			unconfirmedSum += r.Amount
			continue
		}
		if watchStatus[r.WaMessageID] == "not_in_program" {
			nChecks++
			notInProgram = append(notInProgram, fmt.Sprintf("%s — чек %s · %s ₽", strings.TrimSpace(r.Name), fmtDay(r.TxDate), formatMoney(r.Amount)))
			continue
		}
		nChecks++
		addReported(r.Name, reconItem{ID: r.ID, Kind: "check", Amount: r.Amount, Date: r.TxDate, Report: true})
	}
	for _, c := range cash {
		if !inPeriod(c.TxDate) || (groupJID != "" && c.GroupJID != groupJID) {
			continue
		}
		nCash++
		addReported(c.Name, reconItem{ID: -c.ID, Kind: "cash", Amount: c.Amount, Date: c.TxDate, Report: true})
	}
	if c := strings.TrimSpace(client); c != "" {
		var keep []string
		for _, key := range order {
			d := buckets[key].display
			if sameClientName(d, c) || nameCovers(c, d) || nameCovers(d, c) {
				keep = append(keep, key)
			}
		}
		if len(keep) == 0 {
			return fmt.Sprintf("За %s чеков и налички клиента «%s» в учёте не нашёл.", periodLabel, c), nil
		}
		order, unconfirmed, unconfirmedSum, notInProgram = keep, 0, 0, nil
	}
	if len(order) == 0 && unconfirmed == 0 && len(notInProgram) == 0 {
		return "За " + periodLabel + " чеков и налички в учёте нет.", nil
	}
	// Поиск клиентов в программе — параллельно (по 6): по одному он шёл минутами.
	type lookupRes struct {
		clients []cmf.ClientInfo
		kind    cmfMatchKind
		err     error
	}
	lookups := make(map[string]lookupRes, len(order))
	{
		var mu sync.Mutex
		var wg sync.WaitGroup
		sem := make(chan struct{}, 6)
		for _, key := range order {
			wg.Add(1)
			go func(key string) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				cl, kind, err := b.cmfLookupWithTypos(ctx, buckets[key].display)
				mu.Lock()
				lookups[key] = lookupRes{cl, kind, err}
				mu.Unlock()
			}(key)
		}
		wg.Wait()
	}

	// Имя → клиент программы; один клиент под разными написаниями сводится по ID.
	type clientGroup struct {
		client cmf.ClientInfo
		names  []string
		items  []reconItem
	}
	var attention []string
	for _, l := range notInProgram {
		attention = append(attention, l+" — в группе ответили «нет в программе»")
	}
	cidOrder := []string{}
	groupsByCID := map[string]*clientGroup{}
	pinged := false
	for _, key := range order {
		bk := buckets[key]
		lr := lookups[key]
		clients, kind, err := lr.clients, lr.kind, lr.err
		if err == nil && kind != cmfExact && kind != cmfStrong {
			// Плательщик, за которого в группе уже ответили «это оплата за X».
			if pc, ok := b.payerFor(ctx, bk.display, clients); ok {
				clients, kind = []cmf.ClientInfo{pc}, cmfStrong
			}
		}
		switch {
		case err != nil:
			fmt.Printf("cmf: ошибка поиска клиента %q: %v\n", bk.display, err)
			if !pinged {
				pinged = true
				if perr := b.cmf.Ping(ctx); perr != nil {
					fmt.Printf("cmf: программа недоступна: %v\n", perr)
					var sum float64
					for _, k := range order {
						for _, it := range buckets[k].items {
							sum += it.Amount
						}
					}
					return fmt.Sprintf("⚠️ Сверку не сделал — %s. В учёте за %s позиций: %d на %s ₽; сверю, как только программа ответит.",
						cmf.Human(perr), periodLabel, nChecks+nCash, formatRub(sum)), nil
				}
			}
			attention = append(attention, fmt.Sprintf("%s — %s (%s)", bk.display, cmf.Human(err), itemsBrief(bk.items)))
			continue
		case kind == cmfNoMatch:
			attention = append(attention, fmt.Sprintf("%s — в программе не найден (%s)", bk.display, itemsBrief(bk.items)))
			continue
		case kind == cmfExact || kind == cmfStrong:
		case len(clients) == 1:
			attention = append(attention, fmt.Sprintf("%s — точного совпадения нет, похоже на «%s», уточни (%s)", bk.display, clients[0].FullName, itemsBrief(bk.items)))
			continue
		default:
			var names []string
			for _, c := range clients {
				names = append(names, c.FullName)
			}
			_, opts := cmfAskText(bk.display, 0, clients)
			names = names[:0]
			for _, c := range opts {
				names = append(names, c.FullName)
			}
			attention = append(attention, fmt.Sprintf("%s — точно такого клиента нет, похожие: %s — уточни (%s)", bk.display, strings.Join(names, ", "), itemsBrief(bk.items)))
			continue
		}
		c := clients[0]
		g := groupsByCID[c.ID]
		if g == nil {
			g = &clientGroup{client: c}
			groupsByCID[c.ID] = g
			cidOrder = append(cidOrder, c.ID)
		}
		g.names = append(g.names, bk.display)
		g.items = append(g.items, bk.items...)
	}

	// Проход 1: по каждому клиенту — его позиции (период + контекст) и оплаты из
	// программы по ВСЕМ его рассрочкам.
	type clientCase struct {
		g     *clientGroup
		items []reconItem
		pays  []cmf.Payment
	}
	// Оплаты клиентов из программы — параллельно (по 6).
	type payRes struct {
		pays []cmf.Payment
		err  error
	}
	payments := make(map[string]payRes, len(cidOrder))
	{
		var mu sync.Mutex
		var wg sync.WaitGroup
		sem := make(chan struct{}, 6)
		for _, cid := range cidOrder {
			wg.Add(1)
			go func(cid string) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				p, err := b.cmf.PaymentsBetween(ctx, cid, ctxFrom.AddDate(0, 0, -10), ctxTo.AddDate(0, 0, 20))
				mu.Lock()
				payments[cid] = payRes{p, err}
				mu.Unlock()
			}(cid)
		}
		wg.Wait()
	}
	var cases []clientCase
	for _, cid := range cidOrder {
		g := groupsByCID[cid]
		// Контекст: остальные чеки/наличка ЭТОГО клиента вокруг периода и из других
		// групп — они «забирают» свои оплаты, в ответ не выводятся.
		isHis := func(name string) bool {
			if sameNameMemo(name, g.client.FullName) {
				return true
			}
			for _, n := range g.names {
				if sameNameMemo(name, n) {
					return true
				}
			}
			return false
		}
		reported := map[int]bool{}
		for _, it := range g.items {
			reported[it.ID] = true
		}
		items := append([]reconItem(nil), g.items...)
		for _, r := range recs {
			if r.NeedsReview || reported[r.ID] || !isHis(r.Name) {
				continue
			}
			items = append(items, reconItem{ID: r.ID, Kind: "check", Amount: r.Amount, Date: r.TxDate})
		}
		for _, c := range cash {
			if reported[-c.ID] || !isHis(c.Name) {
				continue
			}
			items = append(items, reconItem{ID: -c.ID, Kind: "cash", Amount: c.Amount, Date: c.TxDate})
		}
		pays, perr := payments[cid].pays, payments[cid].err
		if perr != nil {
			fmt.Printf("cmf: ошибка платежей клиента %q: %v\n", g.client.FullName, perr)
			attention = append(attention, fmt.Sprintf("%s — не смог получить его оплаты: %s (%s)", g.client.FullName, cmf.Human(perr), itemsBrief(g.items)))
			continue
		}
		cases = append(cases, clientCase{g, items, pays})
	}

	// Единица сумм у программы одна — решаем её по ВСЕМ клиентам сразу.
	rubN, kopN := 0, 0
	for _, c := range cases {
		r, k := reconUnitTally(c.items, c.pays)
		rubN += r
		kopN += k
	}
	mode := unitsRubles
	if kopN > rubN {
		mode = unitsKopecks
	}

	// Проход 2: объясняем.
	var blocks []string
	okClients, entered, notEntered, toCheck := 0, 0, 0, 0
	lastDay := dayNum(to) - 1
	for _, c := range cases {
		mr := humanMatchFull(c.items, c.pays, mode)
		verdicts, leftover, kopecks := mr.Verdicts, mr.Leftover, mr.Kopecks
		pays := c.pays
		if len(mr.Net) > 0 { // частичные корректировки — показываем суммы после них
			pays = append([]cmf.Payment(nil), c.pays...)
			for j, a := range mr.Net {
				pays[j].Amount = a
			}
		}
		if mr.Partial {
			fmt.Printf("сверка: у клиента %q очень длинная история — перебор ограничен\n", c.g.client.FullName)
		}

		var idx []int
		for i, it := range c.items {
			if it.Report {
				idx = append(idx, i)
			}
		}
		sort.SliceStable(idx, func(a, b int) bool { return c.items[idx[a]].Date.Before(c.items[idx[b]].Date) })

		plain := true // всё внесено обычно, без оговорок — клиента достаточно посчитать
		var lines []string
		for _, i := range idx {
			v := verdicts[i]
			switch {
			case v.entered():
				entered++
			case v.needsCheck():
				toCheck++
			default:
				notEntered++
			}
			if !(v.Status == stEntered || v.Status == stCombined || v.Status == stSplit) || v.Note != "" {
				plain = false
			}
			lines = append(lines, "  "+describeVerdict(c.items, pays, verdicts, i, kopecks))
		}
		// Оплаты в программе без пары в учёте — относящиеся к периоду (в т.ч. «внесли
		// дважды»). Показываем ВСЕГДА: это тоже то, что бухгалтер должен увидеть.
		var extra []string
		for _, j := range leftover {
			p := pays[j]
			pd := effDate(p)
			if pd.IsZero() {
				continue
			}
			if d := dayNum(pd); d < dayNum(from)-3 || d > lastDay+20 {
				continue
			}
			extra = append(extra, fmt.Sprintf("%s ₽ от %s%s", formatMoney(payRub(p, kopecks)), pd.In(reconLoc).Format("02.01"), contractLabel(p)))
		}
		// Списания/сторно, которые не удалось отнести ни к одной записи.
		var stray []string
		for _, j := range mr.Stray {
			p := c.pays[j]
			pd := effDate(p)
			if !pd.IsZero() {
				if d := dayNum(pd); d < dayNum(from)-3 || d > lastDay+20 {
					continue
				}
			}
			stray = append(stray, fmt.Sprintf("%s ₽ от %s%s", formatMoney(payRub(p, kopecks)), dayOrDash(pd), contractLabel(p)))
		}
		if len(extra) > 0 || len(stray) > 0 {
			plain = false
		}
		if plain {
			okClients++
			continue
		}
		blk := c.g.client.FullName + ":\n" + strings.Join(lines, "\n")
		if len(extra) > 0 {
			blk += "\n  ↳ в программе есть оплата без чека в учёте: " + strings.Join(extra, "; ")
		}
		if len(stray) > 0 {
			blk += "\n  ↳ в программе есть списание, не понял к какой оплате — проверь: " + strings.Join(stray, "; ")
		}
		blocks = append(blocks, blk)
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "Сверка с программой за %s: чеков %d", periodLabel, nChecks)
	if nCash > 0 {
		fmt.Fprintf(&sb, ", наличных %d", nCash)
	}
	sb.WriteString(".\n")
	if len(blocks) > 0 {
		sb.WriteString("\n" + strings.Join(blocks, "\n\n") + "\n")
	}
	if len(attention) > 0 {
		fmt.Fprintf(&sb, "\n⚠️ Уточнить (%d):\n• %s\n", len(attention), strings.Join(attention, "\n• "))
	}
	if unconfirmed > 0 {
		fmt.Fprintf(&sb, "\nЕщё %d чек(ов) на %s ₽ без подтверждённого клиента — не знаю, чьи; ответь на «чей чек», тогда сверю.\n", unconfirmed, formatRub(unconfirmedSum))
	}
	if okClients > 0 {
		fmt.Fprintf(&sb, "\n✅ У остальных %d клиент(ов) всё внесено.", okClients)
	}
	fmt.Fprintf(&sb, "\nИтого: внесено %d, НЕ внесено %d", entered, notEntered)
	if toCheck > 0 {
		fmt.Fprintf(&sb, ", проверить %d", toCheck)
	}
	sb.WriteString(".")
	return sb.String(), nil
}

// payRub — сумма оплаты программы в рублях.
func payRub(p cmf.Payment, kopecks bool) float64 {
	if kopecks {
		return float64(p.Amount) / 100
	}
	return float64(p.Amount)
}

// contractLabel — « (дог. №142)», если программа дала номер договора.
func contractLabel(p cmf.Payment) string {
	if p.ContractNumber > 0 {
		return fmt.Sprintf(" (дог. №%d)", p.ContractNumber)
	}
	return ""
}

// formatMoney — сумма с копейками, если они есть: «15 000,50».
func formatMoney(v float64) string {
	k := int64(math.Round(v * 100))
	neg := k < 0
	if neg {
		k = -k
	}
	s := formatRub(float64(k / 100))
	if c := k % 100; c != 0 {
		s += fmt.Sprintf(",%02d", c)
	}
	if neg {
		s = "-" + s
	}
	return s
}

func itemWord(it reconItem) string {
	if it.Kind == "cash" {
		return "наличка"
	}
	return "чек"
}

// itemWordGen — «чека» / «налички» (после «как у», «вместе с …»).
func itemWordGen(it reconItem) string {
	if it.Kind == "cash" {
		return "налички"
	}
	return "чека"
}

// itemWordInstr — «чеком» / «наличкой» (после «вместе с»).
func itemWordInstr(it reconItem) string {
	if it.Kind == "cash" {
		return "наличкой"
	}
	return "чеком"
}

// itemsBrief — коротко позиции клиента для строки «уточнить».
func itemsBrief(items []reconItem) string {
	parts := make([]string, 0, len(items))
	for _, it := range items {
		parts = append(parts, fmt.Sprintf("%s %s · %s ₽", itemWord(it), fmtDay(it.Date), formatMoney(it.Amount)))
	}
	return strings.Join(parts, ", ")
}

// describeVerdict — строка-объяснение по позиции, как сказал бы бухгалтер.
// Дата оплаты — та, что в программе; если её нет — дата внесения.
func describeVerdict(items []reconItem, pays []cmf.Payment, vs []reconVerdict, i int, kopecks bool) string {
	it, v := items[i], vs[i]
	head := fmt.Sprintf("%s · %s ₽", it.Date.In(reconLoc).Format("02.01"), formatMoney(it.Amount))
	entered, notEntered := "внесён", "НЕ внесён"
	if it.Kind == "cash" {
		head += " наличка"
		entered, notEntered = "внесена", "НЕ внесена"
	}
	note := ""
	if v.Note != "" {
		note = " (" + v.Note + ")"
	}
	// Дата, по которой сошлось: для одной оплаты — из вердикта (дата оплаты или
	// дата внесения), иначе — дата оплаты, а если её нет — дата внесения.
	date := func(j int) time.Time {
		if !v.At.IsZero() && len(v.Pays) == 1 {
			return v.At
		}
		return effDate(pays[j])
	}
	day := func(j int) string { return dayOrDash(date(j)) }
	payAt := func(j int) string {
		if date(j).IsZero() {
			return "оплатой без даты" + contractLabel(pays[j])
		}
		return "оплатой " + day(j) + contractLabel(pays[j])
	}
	days := func(j int) int { return dayDelta(date(j), it.Date) }
	switch v.Status {
	case stEntered:
		return "✅ " + head + " — " + entered + " " + payAt(v.Pays[0]) + note
	case stCombined:
		var others []string
		for _, k := range v.With {
			others = append(others, itemWordInstr(items[k])+" "+fmtDay(items[k].Date))
		}
		p := pays[v.Pays[0]]
		return fmt.Sprintf("✅ %s — %s одной оплатой %s ₽ от %s%s вместе с %s%s", head, entered,
			formatMoney(payRub(p, kopecks)), day(v.Pays[0]), contractLabel(p), strings.Join(others, ", "), note)
	case stSplit:
		var parts []string
		labels := map[string]bool{}
		var cl []string
		for _, j := range v.Pays {
			parts = append(parts, fmt.Sprintf("%s ₽ (%s)", formatMoney(payRub(pays[j], kopecks)), day(j)))
			if l := contractLabel(pays[j]); l != "" && !labels[l] {
				labels[l] = true
				cl = append(cl, strings.TrimSuffix(strings.TrimPrefix(l, " ("), ")"))
			}
		}
		s := "✅ " + head + " — " + entered + " частями: " + strings.Join(parts, " + ")
		if len(cl) > 0 {
			s += " (" + strings.Join(cl, ", ") + ")"
		}
		return s + note
	case stLate:
		if v.Note != "" { // дата оплаты в программе неверная — показываем, что поправить
			return "✅ " + head + " — " + entered + " " + payAt(v.Pays[0]) + note
		}
		return fmt.Sprintf("✅ %s — %s поздно: %s (через %d дн.)", head, entered, payAt(v.Pays[0]), days(v.Pays[0]))
	case stDateCheck:
		return fmt.Sprintf("⚠️ %s — в программе оплата той же суммы от %s%s, на %d дн. РАНЬШЕ %s — проверь, за этот ли %s%s",
			head, day(v.Pays[0]), contractLabel(pays[v.Pays[0]]), -days(v.Pays[0]), itemWordGen(it), map[bool]string{true: "платёж", false: "чек"}[it.Kind == "cash"], note)
	case stSuspicious:
		p := pays[v.Pays[0]]
		return fmt.Sprintf("⚠️ %s — в программе оплата %s ₽ от %s%s: %s — проверь сумму", head,
			formatMoney(payRub(p, kopecks)), day(v.Pays[0]), contractLabel(p), v.Note)
	default:
		s := "❌ " + head + " — " + notEntered
		if k := v.SameAmountAs; k >= 0 {
			s += fmt.Sprintf(" (сумма как у %s %s — оплат такой суммы в программе меньше, чем чеков)", itemWordGen(items[k]), fmtDay(items[k].Date))
		}
		return s
	}
}
