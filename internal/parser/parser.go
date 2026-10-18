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
	"unicode"
)

// Transaction — одна извлечённая денежная операция.
type Transaction struct {
	RawName string  // имя ровно как написано в сообщении
	Amount  float64 // сумма в рублях
	Note    string  // "аванс", "премия", "долг" и т.п., если распознано
	CardTo  string  // "втб", "сбер" и т.п., если указано, куда скинули
	Line    string  // строка сообщения, из которой взят платёж
	// IsCash — в строке (или в заголовке блока) явно сказано, что это наличка:
	// «нал», «наличка», «у Дени» (кто забрал). Collector — кто забрал.
	IsCash    bool
	Collector string
}

// cashWordRe — слово налички отдельным словом.
var cashWordRe = regexp.MustCompile(`(?i)(^|[\s,.:;—-])(нал|наличка|наличкой|наличку|наличные|наличными|наличных|налом|кэш|кеш|cash)([\s,.:;!—-]|$)`)

// collectorTailRe — «… у Дени», «… Мансур взял», «… отдал Адаму» в конце строки.
var collectorTailRe = regexp.MustCompile(`(?:\s+у\s+([А-ЯЁ][а-яё]+)|\s+([А-ЯЁ][а-яё]+)\s+(?:взял|взяла|забрал|забрала)|\s+(?:отдал|отдала|передал|передала)\s+([А-ЯЁ][а-яё]+))\s*[.!]?\s*$`)

// pureAmountRe — строка — только сумма («27000», «27 000р», «27к»).
var pureAmountRe = regexp.MustCompile(`(?i)^\d[\d\s.,]*\s*(?:тыс[а-яё]*|тыщ[а-яё]*|т|к|k)?\s*(?:₽|руб[а-яё]*|р\.?)?\s*$`)

// stripCashMarkers — убрать из строки пометки налички и «у кого»: «Магомедов
// Ахмед 15000 нал» → «Магомедов Ахмед 15000», наличка. onlyCash — строка
// состояла только из пометки («Наличка:» — заголовок блока).
func stripCashMarkers(line string) (rest string, cash bool, collector string, onlyCash bool) {
	rest = line
	if m := collectorTailRe.FindStringSubmatchIndex(rest); m != nil && regexp.MustCompile(`\d`).MatchString(rest[:m[0]]) {
		for k := 2; k+1 < len(m); k += 2 {
			if m[k] >= 0 {
				collector = rest[m[k]:m[k+1]]
			}
		}
		rest, cash = strings.TrimSpace(rest[:m[0]]), true
	}
	for cashWordRe.MatchString(rest) {
		rest = cashWordRe.ReplaceAllString(rest, "$1$3")
		cash = true
	}
	rest = strings.TrimSpace(strings.Trim(strings.TrimSpace(rest), ",.:;—-"))
	rest = strings.Join(strings.Fields(rest), " ")
	return rest, cash, collector, cash && rest == ""
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
	thousandWordRe = regexp.MustCompile(`(?i)^(\d+[.,]?\d*)\s*(тыс[а-яё]*|тыщ[а-яё]*|т|к|k)(?:[^а-яёa-zA-Z]|$)`)

	// Ключевые пометки внутри строки
	noteKeywords = []string{"аванс", "премия", "долг", "перекур", "штраф", "бонус"}

	// Куда скинули: "на карту втб", "карта втб", "адлан карта"
	cardRe = regexp.MustCompile(`(?i)(втб|сбер(банк)?|альфа|тинькофф|т-банк|райффайзен|озон|яндекс)`)

	// "Кофейня 235364р", "Аренда 40тысяч", "Такси 8050" -> имя и сумма в одной строке.
	// Имя - 1-3 слова без цифр в начале строки, затем число.
	inlineNameAmountRe = regexp.MustCompile(`(?i)^([А-ЯЁа-яёA-Za-z][А-ЯЁа-яёA-Za-z\-]*(?:\s+[А-ЯЁа-яёA-Za-z\-]+){0,2})\s+(\d[\d\s.,]*(?:\s*(?:тыс[а-яё]*|тыщ[а-яё]*|т|к|k))?)\s*(р|руб|₽)?\.?\s*(аванс|премия|долг)?\s*$`)

	// «15000 Магомедов Ахмед» — сумма ПЕРЕД именем (имя с заглавной буквы).
	amountNameRe = regexp.MustCompile(`^(\d[\d\s.,]*(?:\s*(?:тыс[а-яё]*|тыщ[а-яё]*|т|к|k))?)\s*(?:р|руб|₽)?\.?\s+([А-ЯЁ][а-яё\-]+(?:\s+[А-ЯЁ][а-яё\-]+){0,2})\s*$`)
)

// ParseMessage разбирает один текст сообщения (может содержать несколько блоков "имя + суммы").
func ParseMessage(text string) ParseResult {
	var res ParseResult

	// Нормализуем переносы строк и убираем пустые строки по краям
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")

	currentName := ""
	blockCash := false // заголовок «Наличка:» — дальше по блоку наличка
	nonEmpty := 0
	for _, raw := range lines {
		if strings.TrimSpace(raw) != "" {
			nonEmpty++
		}
	}
	seen := 0
	for _, raw := range lines {
		orig := strings.TrimSpace(raw)
		if orig == "" {
			continue // пустая строка — просто разделитель блоков, не сбрасываем currentName,
			// т.к. в реальных сообщениях люди не всегда аккуратно отбивают блоки
		}
		seen++
		line, lineCash, collector, onlyCash := stripCashMarkers(orig)
		if onlyCash {
			// «Магомедов Ахмед 15000\nналичка» — пометка к единственному платежу.
			if seen == nonEmpty && len(res.Transactions) == 1 {
				res.Transactions[0].IsCash = true
			}
			if currentName == "" {
				// Заголовок блока «Нал» — суммы под ним — наличка без имени клиента.
				currentName = strings.Trim(orig, " :.,—-")
			}
			blockCash = true
			continue
		}
		start := len(res.Transactions)
		lineDone := func() {
			for k := start; k < len(res.Transactions); k++ {
				res.Transactions[k].Line = orig
				if lineCash || blockCash {
					res.Transactions[k].IsCash = true
				}
				if collector != "" {
					res.Transactions[k].Collector = collector
				}
			}
		}
		prevName := currentName
		parseLine(&res, line, &currentName, lineCash || blockCash)
		if currentName != prevName {
			blockCash = lineCash // новый блок — новое имя: наличка только если сказано
		}
		lineDone()
	}

	return res
}

// parseLine — разбор одной строки (уже без пометок налички).
func parseLine(res *ParseResult, line string, currentName *string, cash bool) {
	{ //nolint:gocritic // тело бывшего цикла

		// Формат "Имя Сумма" в одной строке (типично для списков расходов):
		// проверяем раньше остальных, т.к. такая строка начинается с буквы,
		// а не с цифры, и её легко спутать с заголовком блока (looksLikeName).
		if name, amount, note, card, ok := tryParseInlineNameAmount(line); ok {
			res.Transactions = append(res.Transactions, Transaction{
				RawName: name,
				Amount:  amount,
				Note:    note,
				CardTo:  card,
				Line:    line,
			})
			return
		}
		if m := amountNameRe.FindStringSubmatch(line); m != nil {
			if amount := ExtractAmount(m[1]); amount > 0 {
				res.Transactions = append(res.Transactions, Transaction{
					RawName: strings.TrimSpace(m[2]), Amount: amount, CardTo: extractCard(strings.ToLower(line)), Line: line,
				})
				return
			}
		}

		amount, note, card, isAmountLine := tryParseAmountLine(line)

		if isAmountLine {
			if *currentName == "" {
				if cash && pureAmountRe.MatchString(strings.TrimSpace(line)) {
					// «27000 наличка» без имени — наличка без клиента (заглушка).
					res.Transactions = append(res.Transactions, Transaction{RawName: "Наличка", Amount: amount, Note: note, CardTo: card, Line: line})
					return
				}
				res.Unparsed = append(res.Unparsed, line+" (сумма без имени)")
				return
			}
			res.Transactions = append(res.Transactions, Transaction{
				RawName: *currentName,
				Amount:  amount,
				Note:    note,
				CardTo:  card,
				Line:    line,
			})
			return
		}

		// Не похоже на сумму -> считаем, что это имя нового блока
		if looksLikeName(line) {
			*currentName = line
			return
		}

		// Иначе — свободный текст (типа "Скинул 15к на карту втб" без явного имени сверху)
		if amount, note, card, ok := tryParseFreeformLine(line); ok {
			name := *currentName
			if name == "" {
				name = "(не указано)"
			}
			res.Transactions = append(res.Transactions, Transaction{
				RawName: name,
				Amount:  amount,
				Note:    note,
				CardTo:  card,
				Line:    line,
			})
			return
		}

		res.Unparsed = append(res.Unparsed, line)
	}
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

	lowerNum := strings.ToLower(strings.ReplaceAll(numPart, " ", ""))
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
var reCash = regexp.MustCompile(`(?i)(налич|кэш|кэшем|нал[иь]ком|живыми|на\s+руки|из\s+рук|(^|[^\p{L}])нал([^\p{L}]|$)|(^|[^\p{L}])cash([^\p{L}]|$)|(^|[^\p{L}])офис|(^|[^\p{L}])у\s+\p{L})`)

// IsCash сообщает, помечен ли текст как наличная оплата.
func IsCash(text string) bool {
	return reCash.MatchString(text)
}

// reCollector — маркеры «кто забрал/собрал наличку».
var reCollector = regexp.MustCompile(`(?i)(взял|забрал|отдал|собрал|принёс|принес)`)

// LooksMessyPayment сообщает, что сообщение о платеже «грязного» формата, где
// построчный детерминированный парсер ненадёжен (сумма и имя на разных строках,
// наличка/ответственный отдельной строкой, «сумма без имени») — такое лучше
// целиком отдать ИИ, чтобы он правильно связал ФИО, сумму, наличку и кто собрал.
func LooksMessyPayment(text string, res ParseResult) bool {
	// наличка, ответственный, или сокращённая сумма (170т, 5к) — детерминированный
	// парсер такое путает (например «170т» читает как 170), отдаём ИИ.
	if IsCash(text) || reCollector.MatchString(text) || reShorthand.MatchString(text) {
		return true
	}
	for _, u := range res.Unparsed {
		if strings.Contains(u, "сумма без имени") {
			return true
		}
		for _, r := range u {
			if r >= '0' && r <= '9' {
				return true
			}
		}
	}
	return false
}

// reShorthand — сокращённая сумма: «22т», «5к», «25 тыщ», «3 млн», «полляма».
// После суффикса допускаем конец строки, пробел или знак препинания/эмодзи,
// за которым НЕ идёт буква: «22т.», «170т✅» — тысячи, а «22 т.е.» — нет
// (иначе «т.е.» превращало любое число в тысячи).
var reShorthand = regexp.MustCompile(`(?i)(\d[\d\s.,]*)\s*(кк|к|тыщ[а-яё]*|тыс[а-яё]*|т|млн|лям[а-яё]*|косар[а-яё]*)(?:$|\s|[^\p{L}\d\s](?:[^\p{L}]|$))`)

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

// correctionRe — «не 5000 а 6000», «не 5 а 6 тысяч»: поправка суммы, а не платёж.
var correctionRe = regexp.MustCompile(`(?i)(?:^|[^\p{L}])не\s+(\d[\d\s.,]*\s*(?:тыс[а-яё]*|тыщ[а-яё]*|т|к)?)\s*(?:₽|руб[а-яё]*|р\.?)?\s*,?\s*а\s+(\d[\d\s.,]*\s*(?:тыс[а-яё]*|тыщ[а-яё]*|т|к)?)`)

// Correction — поправка суммы: «не 5000 а 6000», «Магомедов не 5000 а 6000»,
// «правильно 6000». Name — ФИО/фамилия, если её назвали; Old=0 — старую сумму
// не назвали (такую поправку применяем только свайпом на платёж).
type Correction struct {
	Name     string
	Old, New float64
}

// correctionFillers — слова, допустимые в поправке рядом с «не X а Y».
var correctionFillers = map[string]bool{
	"исправь": true, "исправьте": true, "поправь": true, "поправьте": true, "сумма": true, "ошибся": true,
	"ошиблась": true, "ошибка": true, "опечатка": true, "извините": true, "извиняюсь": true, "там": true,
	"у": true, "по": true, "чек": true, "платёж": true, "платеж": true, "это": true, "для": true, "за": true,
	"нет": true, "ой": true, "сорри": true, "пардон": true, "вот": true, "тут": true,
	"нал": true, "наличка": true, "наличные": true, "наличными": true, "налом": true, "перевод": true,
	"переводом": true, "итого": true, "сегодня": true, "ок": true, "да": true, "давай": true, "короче": true,
	"слушай": true, "блин": true, "ладно": true, "так": true, "точнее": true, "вернее": true, "всё": true,
	"все": true, "же": true, "а": true, "и": true, "уже": true, "сорян": true,
}

// lowerSurnameRe — фамилия, написанная с маленькой буквы («магомедов»).
var lowerSurnameRe = regexp.MustCompile(`(?:ов|ев|ёв|ин|ын|ова|ева|ёва|ина|ына|ову|еву|ину|ым|ой|ий|ская|ский|ян|енко|аев|иев|аева|иева|аеву|иеву|ому|ему)$`)

// thousandSuffixRe — у суммы есть «т/к/тыс/тыщ».
var thousandSuffixRe = regexp.MustCompile(`(?i)\d\s*(?:т|к|тыс[а-яё]*|тыщ[а-яё]*)\s*$`)

// correctionTailRe — «₽», «р.», «руб» после новой суммы.
var correctionTailRe = regexp.MustCompile(`(?i)^[\s.,!)₽]*(?:р|руб[а-яё]*|рублей)?[\s.,!)]*$`)

// correctionSetRe — «правильно 6000», «исправь на 6000», «правильная сумма 6000».
var correctionSetRe = regexp.MustCompile(`(?i)^\s*(?:правильно|правильная сумма|исправь(?:те)? на|исправить на|поправь(?:те)? на|сумма должна быть)\s*[:\-–—]?\s*(\d[\d\s.,]*\s*(?:т|к|тыс[а-яё]*|тыщ[а-яё]*)?)\s*(?:₽|р\.?|руб[а-яё]*)?\s*[.!]?\s*$`)

// ParseCorrection — сообщение целиком — поправка суммы (а не разговор, где
// встретилось «не 5000 а 6000», и не платёж «принёс не 10000 а 15000»).
func ParseCorrection(text string) (Correction, bool) {
	t := strings.TrimSpace(text)
	if t == "" || strings.Contains(t, "\n") || len([]rune(t)) > 70 || strings.Contains(t, "?") || planWordsRe.MatchString(t) {
		return Correction{}, false
	}
	if m := correctionSetRe.FindStringSubmatch(t); m != nil {
		if n := ExtractAmount(m[1]); n > 0 {
			return Correction{New: n}, true
		}
		return Correction{}, false
	}
	loc := correctionRe.FindStringSubmatchIndex(t)
	if loc == nil {
		return Correction{}, false
	}
	oText, nText := t[loc[2]:loc[3]], t[loc[4]:loc[5]]
	o, n := ExtractAmount(oText), ExtractAmount(nText)
	// «не 5 а 6 тысяч» — «тысяч» относится к обеим суммам.
	if n >= 1000 && !thousandSuffixRe.MatchString(oText) && thousandSuffixRe.MatchString(nText) {
		if v, err := strconv.ParseFloat(strings.TrimSpace(cleanNumber(oText)), 64); err == nil && v > 0 && v < 1000 {
			o = v * 1000
		}
	}
	if o <= 0 || n <= 0 || o == n {
		return Correction{}, false
	}
	if !correctionTailRe.MatchString(t[loc[1]:]) {
		return Correction{}, false
	}
	var name []string
	for _, w := range strings.FieldsFunc(t[:loc[0]+1], func(r rune) bool { return !unicode.IsLetter(r) && r != '-' }) {
		lw := strings.ToLower(w)
		switch {
		case lw == "не":
			// «не» — начало самой поправки (оно попало в префикс из-за границы).
		case correctionFillers[lw]:
		case len([]rune(w)) >= 3 && unicode.IsUpper([]rune(w)[0]):
			name = append(name, w)
		case len([]rune(w)) >= 5 && lowerSurnameRe.MatchString(lw):
			name = append(name, w) // фамилия с маленькой буквы
		default:
			return Correction{}, false // посторонние слова — это разговор, а не поправка
		}
	}
	return Correction{Name: strings.Join(name, " "), Old: o, New: n}, true
}

// ParseAmountCorrection — поправка суммы (см. ParseCorrection): old=0 для «правильно 6000».
func ParseAmountCorrection(text string) (oldAmount, newAmount float64, ok bool) {
	c, ok := ParseCorrection(text)
	return c.Old, c.New, ok
}

// planOrQuestion — строка про план/вопрос/контакт, а не про совершённый платёж
// («Завтра принесет 15000», «Сколько будет 5000?», «Позвони 9287836800»).
var planWordsRe = regexp.MustCompile(`(?i)(^|[^\p{L}])(завтра|послезавтра|принесет|принесёт|принесу|отдаст|скинет|переведет|переведёт|будет|может|надо|нужно|должен|должна|сколько|позвони|позвоните|звони|телефон|номер|квартир|кв|дом|тел)([^\p{L}]|$)`)

// LooksLikeNonPayment — для запасного разбора без ИИ: строка явно не платёж.
func LooksLikeNonPayment(tr Transaction) bool {
	line := tr.Line
	if strings.Contains(line, "?") || planWordsRe.MatchString(line) {
		return true
	}
	// «не 5000 а 6000» в строке — сумма неоднозначна: без ИИ не записываем.
	if correctionRe.MatchString(line) {
		return true
	}
	// Телефон, номер договора/карты — не сумма: 9+ цифр подряд.
	digits := 0
	for _, r := range line {
		if r >= '0' && r <= '9' {
			digits++
			if digits >= 9 {
				return true
			}
		} else if r != ' ' {
			digits = 0
		}
	}
	return tr.Amount < 100 || tr.Amount >= 50_000_000
}
