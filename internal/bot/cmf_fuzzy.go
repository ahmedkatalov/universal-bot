// Человечное сопоставление имени из чека с клиентом программы рассрочек: прощаем
// опечатки в буквах и склонения («Каталова»→«Каталов», «Котолов»→«Каталов»),
// разный порядок слов и латинские двойники букв. Логика: берём широкий пул
// кандидатов из программы (поиск по словам и их основам), затем ЛОКАЛЬНО
// сравниваем по буквам (префикс + расстояние редактирования) и решаем, уверенное
// это совпадение или надо переспросить.
package bot

import (
	"context"
	"sort"
	"strings"
	"unicode"

	"whatsapp-bot/internal/cmf"
)

// cmfMatchKind — качество сопоставления имени с клиентом программы.
type cmfMatchKind int

const (
	cmfNoMatch cmfMatchKind = iota // в программе не нашли
	cmfExact                       // прямое совпадение в программе
	cmfStrong                      // уверенное нечёткое (одна явная кандидатура) — считаем совпадением
	cmfWeak                        // слабое/неоднозначное — лучше переспросить/проверить вручную
)

// latinToCyr — похожие латинские буквы → кириллица (OCR и раскладка их путают,
// из-за чего «Kаталов» с латинской K не находился).
var latinToCyr = map[rune]rune{
	'a': 'а', 'e': 'е', 'o': 'о', 'c': 'с', 'p': 'р', 'x': 'х', 'y': 'у',
	'k': 'к', 'm': 'м', 't': 'т', 'h': 'н', 'b': 'в', 'n': 'п', 'u': 'и',
}

// normCyr нормализует имя для сравнения: нижний регистр, ё→е, латинские
// двойники→кириллица, составные «и+˘»→й и «е+¨»→е (текст в разложенной форме
// Unicode), только буквы, пробелы схлопнуты.
func normCyr(s string) string {
	out := make([]rune, 0, len(s))
	prevSpace := true
	for _, r := range strings.ToLower(s) {
		switch r {
		case '\u0306': // комбинирующая краткая: и+˘ = й
			if n := len(out); n > 0 && out[n-1] == 'и' {
				out[n-1] = 'й'
			}
			continue
		case '\u0308': // комбинирующее двоеточие: е+¨ = ё → е
			continue
		}
		if r == 'ё' {
			r = 'е'
		}
		if c, ok := latinToCyr[r]; ok {
			r = c
		}
		if unicode.IsLetter(r) {
			out = append(out, r)
			prevSpace = false
		} else if !prevSpace {
			out = append(out, ' ')
			prevSpace = true
		}
	}
	return strings.TrimSpace(string(out))
}

// normWords — значимые нормализованные слова имени (от 3 букв: инициалы и «оглы»
// и т.п. для сопоставления бесполезны).
func normWords(s string) []string {
	var out []string
	for _, w := range strings.Fields(normCyr(s)) {
		if len([]rune(w)) >= 3 {
			out = append(out, w)
		}
	}
	return out
}

// levenshtein — расстояние редактирования по рунам (число опечаток).
func levenshtein(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	la, lb := len(ra), len(rb)
	if la == 0 {
		return lb
	}
	if lb == 0 {
		return la
	}
	prev := make([]int, lb+1)
	for j := 0; j <= lb; j++ {
		prev[j] = j
	}
	for i := 1; i <= la; i++ {
		cur := make([]int, lb+1)
		cur[0] = i
		for j := 1; j <= lb; j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min3(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return prev[lb]
}

func min3(a, b, c int) int {
	if b < a {
		a = b
	}
	if c < a {
		a = c
	}
	return a
}

// caseEndings — окончания падежей/рода, которыми одно и то же имя отличается в
// тексте («чек Ахмеда», «Каталова», «Нажудовичу»). Только они делают слово-«хвост»
// тем же именем: «Ахмед»+«ов» = «Ахмедов» — это уже ДРУГАЯ фамилия, а
// «Магомед»+«али» — другое имя.
var caseEndings = map[string]bool{
	"а": true, "я": true, "у": true, "ю": true, "е": true, "ы": true, "и": true,
	"ом": true, "ем": true, "ым": true, "им": true, "ой": true, "ей": true, "ою": true, "ею": true,
	"ого": true, "его": true, "ому": true, "ему": true, "ую": true,
}

// wordSimilar — похожи ли два НОРМАЛИЗОВАННЫХ слова: склонение («каталов»/
// «каталова») или опечатка в букве («котолов»). Если одно слово — начало другого,
// решает ТОЛЬКО окончание (падеж — да, «ов/ев/али/бек» — нет), без допуска на
// опечатку: иначе «Ахмед» совпал бы с «Ахмедов».
func wordSimilar(a, b string) bool {
	if a == b {
		return true
	}
	ra, rb := []rune(a), []rune(b)
	if len(ra) < 3 || len(rb) < 3 {
		return false
	}
	short, long := ra, rb
	if len(rb) < len(ra) {
		short, long = rb, ra
	}
	if string(long[:len(short)]) == string(short) {
		return caseEndings[string(long[len(short):])]
	}
	tol := 1
	if len(long) >= 6 {
		tol = 2
	}
	return levenshtein(a, b) <= tol
}

// scoreCandidate — сколько слов запроса нашли похожее слово в имени кандидата
// (каждое слово кандидата используется один раз).
func scoreCandidate(queryWords, candWords []string) int {
	used := make([]bool, len(candWords))
	score := 0
	for _, q := range queryWords {
		for i, c := range candWords {
			if used[i] {
				continue
			}
			if wordSimilar(q, c) {
				used[i] = true
				score++
				break
			}
		}
	}
	return score
}

// cmfQueryVariants — что отправить в поиск программы по каждому слову: само слово
// и его основы (без 1–2 последних букв), чтобы серверный поиск-подстрока нашёл
// клиента даже при склонении/опечатке в хвосте («каталова»→основа «катал»).
func cmfQueryVariants(words []string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(s string) {
		if len([]rune(s)) >= 3 && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	for _, w := range words {
		r := []rune(w)
		add(w)
		if len(r) >= 5 {
			add(string(r[:len(r)-1]))
		}
		if len(r) >= 6 {
			add(string(r[:len(r)-2]))
		}
	}
	return out
}

// cmfFuzzyByWords ищет клиента, прощая опечатки и склонения. Собирает пул
// кандидатов из программы (по словам и основам), затем локально ранжирует по
// числу совпавших по буквам слов. strong=true — ровно один кандидат совпал по
// ВСЕМ значимым словам запроса (уверенно считаем, что это он).
func (b *Bot) cmfFuzzyByWords(ctx context.Context, name string) (clients []cmf.ClientInfo, strong bool) {
	if b.cmf == nil {
		return nil, false
	}
	qWords := normWords(name)
	if len(qWords) == 0 {
		return nil, false
	}

	byID := map[string]cmf.ClientInfo{}
	queries := cmfQueryVariants(qWords)
	const maxQueries = 6 // меньше запросов на имя: не забиваем программу (в сверке десятки имён)
	for i, q := range queries {
		if i >= maxQueries {
			break
		}
		found, err := b.cmf.LookupClients(ctx, q)
		if err != nil {
			continue
		}
		for _, c := range found {
			if c.ID != "" {
				byID[c.ID] = c
			}
		}
	}
	if len(byID) == 0 {
		return nil, false
	}

	type scored struct {
		c cmf.ClientInfo
		s int
	}
	var all []scored
	best := 0
	for _, c := range byID {
		s := scoreCandidate(qWords, normWords(c.FullName))
		if s == 0 {
			continue
		}
		all = append(all, scored{c, s})
		if s > best {
			best = s
		}
	}
	if best == 0 {
		return nil, false
	}
	var top []cmf.ClientInfo
	for _, sc := range all {
		if sc.s == best {
			top = append(top, sc.c)
		}
	}
	sort.Slice(top, func(i, j int) bool {
		if top[i].FullName != top[j].FullName {
			return top[i].FullName < top[j].FullName
		}
		return top[i].ID < top[j].ID
	})
	// Уверенно — когда совпали ВСЕ значимые слова запроса и кандидат ровно один.
	strong = len(top) == 1 && best == len(qWords)
	return top, strong
}

// cmfLookupWithTypos ищет клиента с допуском на опечатки/склонения и сообщает
// качество совпадения (exact/strong — можно считать совпадением; weak — лучше
// переспросить; noMatch — не нашли).
func (b *Bot) cmfLookupWithTypos(ctx context.Context, name string) ([]cmf.ClientInfo, cmfMatchKind, error) {
	direct, err := b.cmf.LookupClients(ctx, name)
	if err != nil {
		return nil, cmfNoMatch, err
	}
	if len(direct) == 1 {
		return direct, cmfExact, nil
	}
	if len(direct) > 1 {
		return direct, cmfWeak, nil // несколько прямых — неоднозначно, спросим
	}
	cands, strong := b.cmfFuzzyByWords(ctx, name)
	switch {
	case len(cands) == 0:
		return nil, cmfNoMatch, nil
	case strong:
		return cands, cmfStrong, nil
	default:
		return cands, cmfWeak, nil
	}
}
