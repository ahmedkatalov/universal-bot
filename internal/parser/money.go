package parser

import (
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Числа-суммы в тексте. Одно и то же правило для чеков (OCR) и для сообщений
// работников: пробел склеивает цифры ТОЛЬКО как разделитель тысяч («15 000»,
// «1 500 000,00»), а разные числа через пробел («15000 18.10») и через перенос
// строки («15000\n20000») остаются разными числами.

func isASCIIDigit(b byte) bool { return b >= '0' && b <= '9' }

// isGroupSep — пробелы, которыми печатают группы тысяч: обычный, неразрывный,
// узкий неразрывный и тонкий (так их печатают банковские приложения и PDF).
func isGroupSep(r rune) bool {
	return r == ' ' || r == '\u00a0' || r == '\u202f' || r == '\u2009'
}

// moneyNumberSpans — границы [start,end) всех чисел-сумм строки:
//
//	«15 000», «1 500 000,50» — группы тысяч через пробел: первая 1–3 цифры,
//	  дальше ровно по 3 (и за группой не идёт ещё цифра), копейки — 1–2 цифры;
//	«35.000», «10000.00», «1.234,56», «18.10.2026» — цифры с точками/запятыми.
func moneyNumberSpans(s string) [][2]int {
	var out [][2]int
	for i := 0; i < len(s); {
		if !isASCIIDigit(s[i]) {
			i++
			continue
		}
		start, end := i, i
		for end < len(s) && isASCIIDigit(s[end]) {
			end++
		}
		grouped := false
		if end-start <= 3 {
			for {
				r, size := utf8.DecodeRuneInString(s[end:])
				if size == 0 || !isGroupSep(r) {
					break
				}
				k := end + size
				if k+3 > len(s) || !isASCIIDigit(s[k]) || !isASCIIDigit(s[k+1]) || !isASCIIDigit(s[k+2]) ||
					(k+3 < len(s) && isASCIIDigit(s[k+3])) {
					break
				}
				end, grouped = k+3, true
			}
		}
		if grouped {
			if end+1 < len(s) && (s[end] == ',' || s[end] == '.') && isASCIIDigit(s[end+1]) {
				k := end + 1
				for k < len(s) && isASCIIDigit(s[k]) {
					k++
				}
				if k-end-1 <= 2 {
					end = k // копейки: «15 000,00»
				}
			}
		} else {
			for end+1 < len(s) && (s[end] == '.' || s[end] == ',') && isASCIIDigit(s[end+1]) {
				end++
				for end < len(s) && isASCIIDigit(s[end]) {
					end++
				}
			}
		}
		out = append(out, [2]int{start, end})
		i = end
	}
	return out
}

// MoneyNumberSpans — границы [start,end) чисел-сумм текста (см. moneyNumberSpans):
// для тех, кто сам решает, какое число — сумма (подсказка ИИ).
func MoneyNumberSpans(s string) [][2]int { return moneyNumberSpans(s) }

// firstMoneyNumber — первое число-сумма строки («» — чисел нет).
func firstMoneyNumber(s string) string {
	if sp := moneyNumberSpans(s); len(sp) > 0 {
		return s[sp[0][0]:sp[0][1]]
	}
	return ""
}

// amountSuffixRe — суффикс тысяч/миллионов сразу после числа: «22т», «5к»,
// «25 тыщ», «3 млн», «2 ляма», а также с рублями: «15тр», «15 т.р.»,
// «15 тыс.руб», «15 тыс. р.», «15к руб». После суффикса — конец строки,
// пробел или знак, за которым НЕ идёт буква: «22т.», «170т✅» — тысячи,
// а «22 т.е.» и «15 тренировок» — нет.
var amountSuffixRe = regexp.MustCompile(`(?i)^(\s*)(кк|к|k|тыщ[а-яё]*|тыс[а-яё]*|т|млн|лям[а-яё]*|косар[а-яё]*)(?:\.?\s?(?:р|руб[а-яё]*|₽))?\.?(?:$|\s|[^\p{L}\d\s](?:[^\p{L}]|$))`)

// dateTokenRe — «18.10», «18.10.26», «18.10.2026»: дата, а не сумма.
var dateTokenRe = regexp.MustCompile(`^(\d{1,2})\.(\d{1,2})(?:\.(\d{2}|\d{4}))?$`)

// nonAmountWords — слово перед числом, после которого число — номер, а не деньги.
var nonAmountWords = map[string]bool{
	"договор": true, "договора": true, "договору": true, "дог": true, "тел": true, "телефон": true,
	"счет": true, "счёт": true, "счета": true, "кв": true, "квартира": true, "дом": true, "код": true,
}

// amountHit — число-сумма в тексте ([start,end) — где оно стоит).
type amountHit struct {
	v          float64
	start, end int
	// real — похоже на деньги: с суффиксом тысяч или ≥ 100, не телефон и не номер
	// договора. Мелкие числа («за 2 месяца») берём, только если других нет.
	real bool
}

// amountHits — все суммы текста по порядку, без дат («18.10») и времени («12:30»).
func amountHits(text string) []amountHit {
	var out []amountHit
	for _, sp := range moneyNumberSpans(text) {
		tok := text[sp[0]:sp[1]]
		base := moneyValue(tok)
		if m := amountSuffixRe.FindStringSubmatch(text[sp[1]:]); m != nil {
			mult := strings.ToLower(m[2])
			// «Отдал 15000 к Адаму» — «к» тут предлог, а не тысячи.
			single := mult == "к" || mult == "k" || mult == "т"
			if !(single && m[1] != "" && base >= 1000) && base > 0 {
				switch {
				case mult == "кк" || strings.HasPrefix(mult, "млн") || strings.HasPrefix(mult, "лям"):
					out = append(out, amountHit{v: base * 1_000_000, start: sp[0], end: sp[1], real: true})
				default: // к, т, тыс, тыщ, косарь
					out = append(out, amountHit{v: base * 1000, start: sp[0], end: sp[1], real: true})
				}
				continue
			}
		}
		if isDateToken(tok) || isClockToken(text, sp[0], sp[1]) {
			continue
		}
		if base <= 0 {
			continue
		}
		digits := 0
		for k := 0; k < len(tok); k++ {
			if isASCIIDigit(tok[k]) {
				digits++
			}
		}
		out = append(out, amountHit{v: base, start: sp[0], end: sp[1], real: base >= 100 && digits < 10 && !numberMarkerBefore(text, sp[0])})
	}
	return out
}

func isDateToken(tok string) bool {
	m := dateTokenRe.FindStringSubmatch(tok)
	if m == nil {
		return false
	}
	d, _ := strconv.Atoi(m[1])
	mon, _ := strconv.Atoi(m[2])
	return d >= 1 && d <= 31 && mon >= 1 && mon <= 12
}

// isClockToken — «12:30», «23:40:15»: часы/минуты, а не сумма.
func isClockToken(text string, start, end int) bool {
	if end-start > 2 {
		return false
	}
	if end+1 < len(text) && text[end] == ':' && isASCIIDigit(text[end+1]) {
		return true
	}
	return start >= 2 && text[start-1] == ':' && isASCIIDigit(text[start-2])
}

// numberMarkerBefore — перед числом «№», «#», «+7» (телефон) или слово «договор/
// тел/счёт/кв…»: это номер, а не сумма. «15000 + 5000» — суммы.
func numberMarkerBefore(text string, start int) bool {
	if start > 0 && text[start-1] == '+' && (text[start] == '7' || text[start] == '8') {
		return true
	}
	before := strings.TrimRightFunc(text[:start], func(r rune) bool { return unicode.IsSpace(r) || r == ':' || r == '\u00a0' })
	if strings.HasSuffix(before, "№") || strings.HasSuffix(before, "#") {
		return true
	}
	k := len(before)
	for k > 0 {
		r, size := utf8.DecodeLastRuneInString(before[:k])
		if !unicode.IsLetter(r) {
			break
		}
		k -= size
	}
	return nonAmountWords[strings.ToLower(before[k:])]
}

// ExtractSingleAmount — сумма из ОТВЕТА работника, если она в нём одна. Две
// разные суммы («Исаев Харон 15000 20000») — неясно, какая по чеку: вернём
// 0 и ambiguous=true, чтобы бот переспросил, а не угадывал. Дата и время рядом
// («15000 18.10», «в 12:30») суммой не считаются; одно и то же число дважды
// («15000 (15т)») — одна сумма.
func ExtractSingleAmount(text string) (amount float64, ambiguous bool) {
	var real []float64
	for _, h := range amountHits(text) {
		if !h.real {
			continue
		}
		dup := false
		for _, v := range real {
			if v == h.v {
				dup = true
			}
		}
		if !dup {
			real = append(real, h.v)
		}
	}
	switch len(real) {
	case 0:
		return ExtractAmount(text), false
	case 1:
		return real[0], false
	default:
		return 0, true
	}
}

// moneyWords — слова-«хвосты» суммы: «т.р», «тыс», «руб», «к»… Это не часть ФИО
// («Исаев Харон 15 т.р.» — клиент «Исаев Харон», а не «Исаев Харон т.р»).
var moneyWords = map[string]bool{
	"т": true, "тр": true, "р": true, "к": true, "кк": true, "k": true, "₽": true,
	"руб": true, "рубль": true, "рубля": true, "рублей": true, "рублями": true,
	"тыс": true, "тысяч": true, "тысяча": true, "тысячи": true, "тысячу": true,
	"тыщ": true, "тыщи": true, "тыщу": true, "млн": true, "лям": true, "ляма": true, "лямов": true,
	"косарь": true, "косаря": true, "косарей": true,
}

// IsMoneyWord — слово целиком — хвост суммы («т.р.», «тыс.руб», «руб.», «₽»).
// afterNumber — перед ним стояло число: только тогда однобуквенные «т», «к»,
// «р» считаем хвостом суммы, а не инициалом («Исаев К.»).
func IsMoneyWord(word string, afterNumber bool) bool {
	w := strings.ToLower(strings.TrimFunc(word, func(r rune) bool { return !unicode.IsLetter(r) && r != '₽' }))
	if w == "" {
		return false
	}
	parts := strings.FieldsFunc(w, func(r rune) bool { return r == '.' || r == ',' })
	if len(parts) == 0 {
		return false
	}
	for _, p := range parts {
		if !moneyWords[p] {
			return false
		}
	}
	if afterNumber || len(parts) > 1 {
		return true // «т.р», «тыс.руб» — не имя, даже без числа перед ним
	}
	// Без числа рядом — только явные слова в нижнем регистре («руб», «тыс»):
	// одна буква или слово с заглавной может быть инициалом/именем.
	first, _ := utf8.DecodeRuneInString(strings.TrimLeftFunc(word, func(r rune) bool { return !unicode.IsLetter(r) }))
	return utf8.RuneCountInString(w) >= 2 && !unicode.IsUpper(first)
}

// SeparateAmountsSideBySide — в строке две РАЗНЫЕ суммы стоят рядом через
// пробел («Исаев Харон 15000 20000», «40000 5000»). Раньше такие числа
// склеивались в одну огромную сумму; теперь это неясная строка: какая из сумм
// платёж (или это два платежа), решает человек/ИИ, а не разбор.
func SeparateAmountsSideBySide(line string) bool {
	hits := amountHits(line)
	for k := 0; k+1 < len(hits); k++ {
		a, b := hits[k], hits[k+1]
		if !a.real || !b.real || a.v == b.v {
			continue
		}
		gap := line[a.end:b.start]
		if gap != "" && strings.TrimFunc(gap, isGroupSep) == "" {
			return true
		}
	}
	return false
}

// moneyValue — значение одного числа-суммы (см. moneyNumberSpans):
//
//	«15 000,00» -> 15000, «35.000» -> 35000 (точка + РОВНО 3 цифры — тысячи),
//	«10000.00» -> 10000, «1.234,56» -> 1234.56, «844,03» -> 844.03.
func moneyValue(tok string) float64 {
	cleaned := strings.Map(func(r rune) rune {
		if isGroupSep(r) {
			return -1
		}
		return r
	}, strings.TrimSpace(tok))
	if cleaned == "" {
		return 0
	}
	// Если и точка и запятая: точка — тысячи, запятая — десятичные ("1.234,56")
	if strings.Contains(cleaned, ".") && strings.Contains(cleaned, ",") {
		cleaned = strings.ReplaceAll(cleaned, ".", "")
		cleaned = strings.ReplaceAll(cleaned, ",", ".")
	} else if thousandsSepRe.MatchString(cleaned) {
		// Разделители тысяч: одна или несколько групп ровно по 3 цифры
		// ("35.000", "1,500", "2.500.000", "1,234,567") — убираем их.
		cleaned = strings.ReplaceAll(cleaned, ".", "")
		cleaned = strings.ReplaceAll(cleaned, ",", "")
	} else {
		// Единственный разделитель с 1-2 цифрами после — десятичная часть
		// ("844,03", "10000.00").
		cleaned = strings.ReplaceAll(cleaned, ",", ".")
	}
	val, err := strconv.ParseFloat(cleaned, 64)
	if err != nil {
		return 0
	}
	return val
}

var thousandsSepRe = regexp.MustCompile(`^\d{1,3}([.,]\d{3})+$`)
