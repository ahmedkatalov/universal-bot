// Package parser разбирает текстовые сообщения из WhatsApp-группы вида:
//
//	Имя
//	40000
//	5000
//
//	Другое имя
//	30500 аванс
//
// и извлекает структурированные транзакции (имя, сумма, заметка).
package parser

import (
	"regexp"
	"strconv"
	"strings"
)

// Transaction — одна извлечённая денежная операция.
type Transaction struct {
	RawName string  // имя ровно как написано в сообщении
	Amount  float64 // сумма в рублях
	Note    string  // "аванс", "премия", "долг" и т.п., если распознано
	CardTo  string  // "втб", "сбер" и т.п., если указано, куда скинули
}

// ParseResult — результат разбора одного сообщения.
type ParseResult struct {
	Transactions []Transaction
	Unparsed     []string // строки, которые не удалось разобрать (для ручной проверки)
}

var (
	numberRe = regexp.MustCompile(`^\d[\d\s.,]*`)

	// "5тысяч", "5 тысяч", "9тысяя" (опечатка), "40к", "40k" -> множитель 1000
	// Примечание: Go's \b не распознаёт кириллицу как "word"-символы, поэтому
	// граница задаётся явно через отрицание класса символов, а не через \b.
	thousandWordRe = regexp.MustCompile(`(?i)^(\d+[.,]?\d*)\s*(тыс[а-яё]*|к|k)(?:[^а-яёa-zA-Z]|$)`)

	// Ключевые пометки внутри строки
	noteKeywords = []string{"аванс", "премия", "долг", "перекур", "штраф", "бонус"}

	// Куда скинули: "на карту втб", "карта втб", "адлан карта"
	cardRe = regexp.MustCompile(`(?i)(втб|сбер(банк)?|альфа|тинькофф|т-банк|райффайзен|озон|яндекс)`)

	// "Кофейня 235364р", "Аренда 40тысяч", "Такси 8050" -> имя и сумма в одной строке.
	// Имя - 1-3 слова без цифр в начале строки, затем число.
	inlineNameAmountRe = regexp.MustCompile(`(?i)^([А-ЯЁа-яёA-Za-z][А-ЯЁа-яёA-Za-z\-]*(?:\s+[А-ЯЁа-яёA-Za-z\-]+){0,2})\s+(\d[\d\s.,]*(?:тыс[а-яё]*|к|k)?)\s*(р|руб|₽)?\.?\s*(аванс|премия|долг)?\s*$`)
)

// ParseMessage разбирает один текст сообщения (может содержать несколько блоков "имя + суммы").
func ParseMessage(text string) ParseResult {
	var res ParseResult

	// Нормализуем переносы строк и убираем пустые строки по краям
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")

	currentName := ""
	for _, raw := range lines {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue // пустая строка — просто разделитель блоков, не сбрасываем currentName,
			// т.к. в реальных сообщениях люди не всегда аккуратно отбивают блоки
		}

		// Формат "Имя Сумма" в одной строке (типично для списков расходов):
		// проверяем раньше остальных, т.к. такая строка начинается с буквы,
		// а не с цифры, и её легко спутать с заголовком блока (looksLikeName).
		if name, amount, note, card, ok := tryParseInlineNameAmount(line); ok {
			res.Transactions = append(res.Transactions, Transaction{
				RawName: name,
				Amount:  amount,
				Note:    note,
				CardTo:  card,
			})
			continue
		}

		amount, note, card, isAmountLine := tryParseAmountLine(line)

		if isAmountLine {
			if currentName == "" {
				res.Unparsed = append(res.Unparsed, line+" (сумма без имени)")
				continue
			}
			res.Transactions = append(res.Transactions, Transaction{
				RawName: currentName,
				Amount:  amount,
				Note:    note,
				CardTo:  card,
			})
			continue
		}

		// Не похоже на сумму -> считаем, что это имя нового блока
		if looksLikeName(line) {
			currentName = line
			continue
		}

		// Иначе — свободный текст (типа "Скинул 15к на карту втб" без явного имени сверху)
		if amount, note, card, ok := tryParseFreeformLine(line); ok {
			name := currentName
			if name == "" {
				name = "(не указано)"
			}
			res.Transactions = append(res.Transactions, Transaction{
				RawName: name,
				Amount:  amount,
				Note:    note,
				CardTo:  card,
			})
			continue
		}

		res.Unparsed = append(res.Unparsed, line)
	}

	return res
}

// tryParseAmountLine пытается распознать строку как "чистую" сумму, возможно с пометкой.
// Примеры: "40000", "40 000", "5тысяч", "30к", "3000 аванс", "20к премия"
func tryParseAmountLine(line string) (amount float64, note string, card string, ok bool) {
	lower := strings.ToLower(line)

	// Ищем денежный множитель "тысяч/к/k"
	if m := thousandWordRe.FindStringSubmatch(lower); m != nil {
		val, err := strconv.ParseFloat(strings.ReplaceAll(m[1], ",", "."), 64)
		if err == nil {
			amount = val * 1000
			ok = true
		}
	} else if m := numberRe.FindString(line); m != "" {
		val := ParseMoneyValue(m)
		if val > 0 {
			amount = val
			ok = true
		}
	}

	if !ok {
		return 0, "", "", false
	}

	note = extractNote(lower)
	card = extractCard(lower)
	return amount, note, card, true
}

// tryParseFreeformLine обрабатывает строки вида "Скинул 15к на карту втб",
// "9тысяя скинул на твою карту", "Закрыл договор 903 35.000" — сумма может быть
// не в начале строки и соседствовать с другими числами (номер договора и т.п.).
// Если чисел несколько — берём НАИБОЛЬШЕЕ: сумма практически всегда больше
// номера договора/квартиры/процента, а ошибка в меньшую сторону (взять номер
// договора вместо суммы) исказила бы отчёт незаметно.
func tryParseFreeformLine(line string) (amount float64, note string, card string, ok bool) {
	lower := strings.ToLower(line)

	if m := thousandWordRe.FindStringSubmatch(lower); m != nil {
		val, err := strconv.ParseFloat(strings.ReplaceAll(m[1], ",", "."), 64)
		if err == nil {
			amount = val * 1000
			ok = true
		}
	} else {
		// Ищем ВСЕ числа в строке и берём наибольшее осмысленное.
		// Пробел внутри числа НЕ допускаем: в свободном тексте "договор 903 35.000"
		// пробел разделяет разные числа, а не группы тысяч.
		anyNumRe := regexp.MustCompile(`\d[\d.,]*`)
		for _, m := range anyNumRe.FindAllString(line, -1) {
			val := ParseMoneyValue(m)
			if val >= 100 && val > amount { // отсекаем мелкие случайные числа
				amount = val
				ok = true
			}
		}
	}

	if !ok {
		return 0, "", "", false
	}

	note = extractNote(lower)
	card = extractCard(lower)
	return amount, note, card, true
}

// Слова-действия: если "имя" в строке формата "Имя Сумма" содержит такое слово,
// это не имя человека, а описание операции ("Закрыл просрочку 79.650") —
// строка должна относиться к текущему имени блока, а не создавать новое.
var actionWords = []string{"закрыл", "закрыла", "скинул", "скинула", "перевел", "перевёл", "оплатил", "оплатила", "взял", "взяла", "отдал", "отдала", "договор", "просрочк", "остаток"}

func containsActionWord(s string) bool {
	lower := strings.ToLower(s)
	for _, w := range actionWords {
		if strings.Contains(lower, w) {
			return true
		}
	}
	return false
}

// tryParseInlineNameAmount разбирает строки вида "Кофейня 235364р", "Аренда 40тысяч",
// "Такси 8050", "Повар 10400р" — где имя/статья и сумма находятся в одной строке.
func tryParseInlineNameAmount(line string) (name string, amount float64, note string, card string, ok bool) {
	m := inlineNameAmountRe.FindStringSubmatch(line)
	if m == nil {
		return "", 0, "", "", false
	}
	name = strings.TrimSpace(m[1])
	if containsActionWord(name) {
		return "", 0, "", "", false // это действие, не имя — пусть уйдёт в freeform
	}
	numPart := strings.TrimSpace(m[2])

	lowerNum := strings.ToLower(numPart)
	if tm := thousandWordRe.FindStringSubmatch(lowerNum + " "); tm != nil {
		val, err := strconv.ParseFloat(strings.ReplaceAll(tm[1], ",", "."), 64)
		if err != nil {
			return "", 0, "", "", false
		}
		amount = val * 1000
	} else {
		val := ParseMoneyValue(numPart)
		if val == 0 {
			return "", 0, "", "", false
		}
		amount = val
	}

	note = m[4]
	card = extractCard(strings.ToLower(line))
	return name, amount, note, card, true
}

func extractNote(lower string) string {
	for _, kw := range noteKeywords {
		if strings.Contains(lower, kw) {
			return kw
		}
	}
	return ""
}

// reCash — пометки, что платёж НАЛИЧНЫЙ: слова наличка/нал/кэш, «офис» (сдал в
// офис) или «у ‹имя›» (нал на руках у кого-то). \p{L}-границы корректно
// работают с кириллицей (в отличие от \b/\W в RE2). Используется, чтобы решить,
// считать ли текстовый платёж в сбор (чистое «ФИО+сумма» без пометки — дубль чека).
var reCash = regexp.MustCompile(`(?i)(налич|кэш|(^|[^\p{L}])нал([^\p{L}]|$)|(^|[^\p{L}])cash([^\p{L}]|$)|(^|[^\p{L}])офис|(^|[^\p{L}])у\s+\p{L})`)

// IsCash сообщает, помечен ли текст как наличная оплата.
func IsCash(text string) bool {
	return reCash.MatchString(text)
}

// reShorthand — сокращённая сумма: «22т», «5к», «25 тыщ», «3 млн», «полляма».
var reShorthand = regexp.MustCompile(`(?i)(\d[\d\s.,]*)\s*(кк|к|тыщ[а-яё]*|тыс[а-яё]*|т|млн|лям[а-яё]*|косар[а-яё]*)(?:[^\p{L}]|$)`)

// ExtractAmount вытаскивает денежную сумму из строки, понимая сокращения
// («22т»=22000, «5к»=5000, «3 млн»=3000000). Если сокращения нет — обычный
// разбор числа. 0, если суммы не нашлось.
func ExtractAmount(text string) float64 {
	if m := reShorthand.FindStringSubmatch(text); m != nil {
		base := ParseMoneyValue(m[1])
		switch mult := strings.ToLower(m[2]); {
		case mult == "кк" || strings.HasPrefix(mult, "млн") || strings.HasPrefix(mult, "лям"):
			return base * 1_000_000
		default: // к, т, тыс, тыщ, косарь
			return base * 1000
		}
	}
	return ParseMoneyValue(text)
}

func extractCard(lower string) string {
	if m := cardRe.FindString(lower); m != "" {
		return strings.ToLower(m)
	}
	if strings.Contains(lower, "карту") || strings.Contains(lower, "карта") {
		return "карта (банк не указан)"
	}
	if strings.Contains(lower, "нал") {
		return "наличные"
	}
	return ""
}

// looksLikeName — эвристика: строка без цифр, короткая (1-3 слова), не служебное слово.
func looksLikeName(line string) bool {
	if regexp.MustCompile(`\d`).MatchString(line) {
		return false
	}
	words := strings.Fields(line)
	if len(words) == 0 || len(words) > 4 {
		return false
	}
	// Первая буква обычно заглавная у имени
	r := []rune(line)
	if len(r) == 0 {
		return false
	}
	return true
}

func cleanNumber(s string) string {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, " ", "")
	s = strings.ReplaceAll(s, "\u00a0", "") // неразрывный пробел
	// если есть и точка и запятая - это разделитель тысяч, убираем запятые
	s = strings.ReplaceAll(s, ",", ".")
	return s
}
