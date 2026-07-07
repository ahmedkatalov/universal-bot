// Package parser (receipt.go) разбирает текст, распознанный OCR со скриншотов
// банковских приложений (Альфа-Банк, ВТБ, Сбер и т.д.) — в отличие от parser.go,
// который разбирает обычные текстовые сообщения "Имя + список сумм".
//
// Скриншоты разных банков оформлены по-разному, поэтому вместо одного жёсткого
// шаблона используется поиск по ключевым полям (Получатель/Сколько/Статус и т.п.)
// в произвольном порядке — так новый банк можно поддержать, просто дописав
// его лейблы в fieldLabels, без изменения логики разбора.
package parser

import (
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// ReceiptData — то, что удалось извлечь со скриншота банковского перевода.
type ReceiptData struct {
	Bank           string  // "Сбербанк", "Альфа-Банк", "ВТБ" — если распознано
	Recipient      string  // ФИО получателя (чья карта — куда пришли деньги)
	RecipientBank  string  // банк получателя, если указан отдельно
	RecipientPhone string  // телефон получателя, если указан
	Sender         string  // ФИО отправителя (клиент, который платил)
	SenderBank     string  // банк отправителя, если указан
	SenderAccount  string  // счёт/карта отправителя (последние цифры)
	Amount         float64 // сумма перевода
	Commission     float64 // комиссия, если указана
	DocNumber      string  // номер документа / операции
	AuthCode       string  // код авторизации
	Status         string  // "Выполнено", "Отклонено" и т.п.
	RawText        string  // исходный текст для отладки/ручной проверки

	// TxTime — дата и время операции, как указано на самом чеке (не время
	// получения сообщения в WhatsApp). Заполняется, только если на чеке
	// нашлась распознаваемая дата/время (HasTxTime = true) — иначе вызывающий
	// код должен использовать время получения сообщения как приближение.
	TxTime    time.Time
	HasTxTime bool
}

// Известные банки — по характерным словам на скриншоте.
// ВАЖНО: порядок проверки задаётся отдельным списком bankOrder, т.к. итерация
// по map в Go недетерминирована, а "сбер" входит в "сбербанк" и т.п.
var bankMarkers = map[string]*regexp.Regexp{
	// --- Федеральные / крупнейшие ---
	"Сбербанк":       regexp.MustCompile(`(?i)сбер(банк)?|sber(bank)?`),
	"ВТБ":            regexp.MustCompile(`(?i)(^|\W)втб(\W|$)|(^|\W)vtb(\W|$)`),
	"Газпромбанк":    regexp.MustCompile(`(?i)газпромбанк|(^|\W)гпб(\W|$)|gazprombank`),
	"Альфа-Банк":     regexp.MustCompile(`(?i)альфа.?банк|alfa.?bank`),
	"Россельхозбанк": regexp.MustCompile(`(?i)россельхоз|(^|\W)рсхб(\W|$)|(^|\W)rshb(\W|$)`),
	"Т-Банк":         regexp.MustCompile(`(?i)тинькофф|tinkoff|(^|[^\p{L}])т[-.\s]?банк|(^|[^\p{L}])t[-.\s]?bank`),
	"Открытие":       regexp.MustCompile(`(?i)банк\s*открытие|открытие\s*банк`),
	"Совкомбанк":     regexp.MustCompile(`(?i)совкомбанк|халва|sovcombank`),
	"Райффайзен":     regexp.MustCompile(`(?i)райффайзен|raiffeisen`),
	"Росбанк":        regexp.MustCompile(`(?i)росбанк|rosbank`),
	"ПСБ":            regexp.MustCompile(`(?i)промсвязь|(^|\W)псб(\W|$)|(^|\W)psb(\W|$)`),
	"Юникредит":      regexp.MustCompile(`(?i)юникредит|unicredit`),
	"Ак Барс":        regexp.MustCompile(`(?i)ак\s*барс|ak\s*bars`),
	"МКБ":            regexp.MustCompile(`(?i)московский\s*кредитн|(^|\W)мкб(\W|$)`),
	"Банк СПб":       regexp.MustCompile(`(?i)санкт.?петербург|(^|\W)бспб(\W|$)`),
	"Уралсиб":        regexp.MustCompile(`(?i)уралсиб|uralsib`),
	"Ситибанк":       regexp.MustCompile(`(?i)ситибанк|citibank`),
	// --- Розничные / цифровые / маркетплейсы ---
	"Озон Банк":        regexp.MustCompile(`(?i)озон\s*банк|ozon\s*bank|(^|\W)озон(\W|$)`),
	"Яндекс Банк":      regexp.MustCompile(`(?i)яндекс\s*банк|yandex\s*bank`),
	"Яндекс Пэй":       regexp.MustCompile(`(?i)яндекс\s*(пэй|pay)`),
	"ЮMoney":           regexp.MustCompile(`(?i)ю\W?money|юмани|yoomoney`),
	"QIWI":             regexp.MustCompile(`(?i)qiwi|киви\s*банк`),
	"Вайлдберриз Банк": regexp.MustCompile(`(?i)вайлдберриз|wildberries|(^|\W)вб\s*банк`),
	"Точка":            regexp.MustCompile(`(?i)точка\s*банк|банк\s*точка`),
	"Модульбанк":       regexp.MustCompile(`(?i)модуль\s*банк|модульбанк`),
	"МТС Банк":         regexp.MustCompile(`(?i)мтс\s*банк|mts\s*bank`),
	"Почта Банк":       regexp.MustCompile(`(?i)почта\s*банк|pochta`),
	"ОТП Банк":         regexp.MustCompile(`(?i)отп\s*банк|otp\s*bank`),
	"Хоум Кредит":      regexp.MustCompile(`(?i)хоум\s*кредит|home\s*credit`),
	"Хоум Банк":        regexp.MustCompile(`(?i)хоум\s*банк`),
	"Ренессанс":        regexp.MustCompile(`(?i)ренессанс`),
	"Русский Стандарт": regexp.MustCompile(`(?i)русский\s*стандарт|russian\s*standard`),
	"Драйв Клик":       regexp.MustCompile(`(?i)драйв\s*клик|drive\s*click`),
	"Кредит Европа":    regexp.MustCompile(`(?i)кредит\s*европа|credit\s*europe`),
	"Свой Банк":        regexp.MustCompile(`(?i)свой\s*банк`),
	// --- Средние / региональные ---
	"Зенит":               regexp.MustCompile(`(?i)банк\s*зенит|(^|\W)зенит(\W|$)`),
	"Абсолют":             regexp.MustCompile(`(?i)абсолют\s*банк`),
	"Новиком":             regexp.MustCompile(`(?i)новикомбанк`),
	"Металлинвест":        regexp.MustCompile(`(?i)металлинвест`),
	"Локо-Банк":           regexp.MustCompile(`(?i)локо.?банк`),
	"СМП Банк":            regexp.MustCompile(`(?i)(^|\W)смп\s*банк|(^|\W)смп(\W|$)`),
	"ДОМ.РФ":              regexp.MustCompile(`(?i)дом\.рф|дом\s*рф`),
	"РНКБ":                regexp.MustCompile(`(?i)(^|\W)рнкб(\W|$)|rncb`),
	"Кубань Кредит":       regexp.MustCompile(`(?i)кубань\s*кредит`),
	"Центр-инвест":        regexp.MustCompile(`(?i)центр.?инвест`),
	"Экспобанк":           regexp.MustCompile(`(?i)экспобанк|expobank`),
	"АТБ":                 regexp.MustCompile(`(?i)азиатско.?тихоокеанск|(^|\W)атб(\W|$)`),
	"УБРиР":               regexp.MustCompile(`(?i)убрир|уральский\s*банк\s*реконструкц`),
	"Синара":              regexp.MustCompile(`(?i)банк\s*синара|(^|\W)скб.?банк`),
	"Авангард":            regexp.MustCompile(`(?i)банк\s*авангард`),
	"ВБРР":                regexp.MustCompile(`(?i)(^|\W)вбрр(\W|$)|всероссийский\s*банк\s*развития`),
	"Транскапиталбанк":    regexp.MustCompile(`(?i)транскапитал|(^|\W)ткб(\W|$)`),
	"БКС Банк":            regexp.MustCompile(`(?i)бкс\s*банк|(^|\W)бкс(\W|$)`),
	"Финам":               regexp.MustCompile(`(?i)финам\s*банк`),
	"Держава":             regexp.MustCompile(`(?i)банк\s*держава`),
	"Солидарность":        regexp.MustCompile(`(?i)банк\s*солидарность`),
	"Генбанк":             regexp.MustCompile(`(?i)генбанк|genbank`),
	"Банк Приморье":       regexp.MustCompile(`(?i)банк\s*приморье`),
	"Кошелев-Банк":        regexp.MustCompile(`(?i)кошелев.?банк`),
	"Ланта-Банк":          regexp.MustCompile(`(?i)ланта.?банк`),
	"Интеза":              regexp.MustCompile(`(?i)банк\s*интеза|intesa`),
	"Меткомбанк":          regexp.MustCompile(`(?i)меткомбанк`),
	"Примсоцбанк":         regexp.MustCompile(`(?i)примсоцбанк`),
	"Севергазбанк":        regexp.MustCompile(`(?i)севергазбанк`),
	"Челябинвестбанк":     regexp.MustCompile(`(?i)челябинвест`),
	"Челиндбанк":          regexp.MustCompile(`(?i)челиндбанк`),
	"Дальневосточный":     regexp.MustCompile(`(?i)дальневосточн`),
	"Кредит Урал Банк":    regexp.MustCompile(`(?i)кредит\s*урал`),
	"Тольяттихимбанк":     regexp.MustCompile(`(?i)тольяттихим`),
	"Инвестторгбанк":      regexp.MustCompile(`(?i)инвестторгбанк`),
	"НС Банк":             regexp.MustCompile(`(?i)(^|\W)нс\s*банк`),
	"Таврический":         regexp.MustCompile(`(?i)таврический`),
	"Кузнецкбизнесбанк":   regexp.MustCompile(`(?i)кузнецкбизнес`),
	"СДМ-Банк":            regexp.MustCompile(`(?i)сдм.?банк`),
	"Газэнергобанк":       regexp.MustCompile(`(?i)газэнергобанк`),
	"Запсибкомбанк":       regexp.MustCompile(`(?i)запсибкомбанк`),
	"Кредит Москва":       regexp.MustCompile(`(?i)банк\s*кредит\s*москва`),
	"Уралфинанс":          regexp.MustCompile(`(?i)уралфинанс`),
	"Хакасский":           regexp.MustCompile(`(?i)хакасский\s*муниципальн`),
}

// Порядок проверки банков: более специфичные названия раньше, чтобы короткий
// маркер не перебивал составное название. Большая четвёрка (Сбер/Альфа/Т-Банк/
// ВТБ) и «озон» — в самом конце: их маркеры короткие/широкие.
var bankOrder = []string{
	// специфичные региональные и составные — первыми
	"Кредит Урал Банк", "Кредит Москва", "Кредит Европа", "Русский Стандарт",
	"Транскапиталбанк", "Металлинвест", "Инвестторгбанк", "Тольяттихимбанк",
	"Кузнецкбизнесбанк", "Челябинвестбанк", "Челиндбанк", "Севергазбанк",
	"Газэнергобанк", "Запсибкомбанк", "Примсоцбанк",
	"Хакасский", "Уралфинанс", "Кубань Кредит", "Дальневосточный",
	"Банк Приморье", "Кошелев-Банк", "Ланта-Банк", "Меткомбанк", "Генбанк",
	"Держава", "Солидарность", "Таврический", "СДМ-Банк", "НС Банк",
	"Центр-инвест", "Экспобанк", "Уралсиб", "Синара", "Авангард", "Интеза",
	"Россельхозбанк", "Газпромбанк", "ПСБ", "МКБ", "АТБ", "ВБРР", "БКС Банк",
	"СМП Банк", "УБРиР", "РНКБ", "ДОМ.РФ", "Банк СПб", "Ситибанк", "Финам",
	"Совкомбанк", "МТС Банк", "Росбанк", "Райффайзен", "Ак Барс", "Юникредит",
	"Хоум Кредит", "Хоум Банк", "Ренессанс", "Абсолют", "Зенит", "Свой Банк",
	"Новиком", "Локо-Банк", "Драйв Клик", "Модульбанк", "Точка",
	"Вайлдберриз Банк", "Яндекс Пэй", "Яндекс Банк", "ЮMoney", "QIWI",
	"Почта Банк", "ОТП Банк", "Открытие",
	// широкие/короткие маркеры — последними
	"Озон Банк", "Альфа-Банк", "Сбербанк", "Т-Банк", "ВТБ",
}

// Лейблы полей — берём значение с той же строки после лейбла или со следующей
// непустой строки. Порядок ВАЖЕН: специфичные лейблы идут раньше коротких,
// иначе "ФИО получателя перевода" совпадёт с коротким "получатель" и хвост
// "перевода" утечёт в значение. Первое успешное правило для поля побеждает.
type fieldRule struct {
	field string
	re    *regexp.Regexp
}

var fieldRules = []fieldRule{
	// --- Получатель (чья карта — куда пришли деньги) ---
	// Сбер СБП
	{"recipient", regexp.MustCompile(`(?i)^фио\s+получателя\s+перевода\s*:?\s*(.*)$`)},
	// Сбер перевод клиенту, Т-Банк, Газпромбанк и др.
	{"recipient", regexp.MustCompile(`(?i)^фио\s+получателя\s*:?\s*(.*)$`)},
	// Озон, Райффайзен и др.: "Получатель перевода"
	{"recipient", regexp.MustCompile(`(?i)^получатель\s+перевода\s*:?\s*(.*)$`)},
	// Альфа-Банк, Т-Банк: просто "Получатель"
	{"recipient", regexp.MustCompile(`(?i)^получатель\s*:?\s*(.*)$`)},
	// Почта Банк и др.: "Кому"
	{"recipient", regexp.MustCompile(`(?i)^кому\s*:?\s*(.*)$`)},

	// --- Отправитель (клиент, который платил) ---
	{"sender", regexp.MustCompile(`(?i)^фио\s+отправителя\s*:?\s*(.*)$`)},
	{"sender", regexp.MustCompile(`(?i)^отправитель\s*:?\s*(.*)$`)},
	{"sender", regexp.MustCompile(`(?i)^фио\s+плательщика\s*:?\s*(.*)$`)},
	{"sender", regexp.MustCompile(`(?i)^имя\s+плательщика\s*:?\s*(.*)$`)},
	{"sender", regexp.MustCompile(`(?i)^плательщик\s*:?\s*(.*)$`)},
	{"sender", regexp.MustCompile(`(?i)^от\s+кого\s*:?\s*(.*)$`)},

	// --- Комиссия: ПЕРЕД суммой, иначе "Сумма комиссии" совпадёт с "Сумма" ---
	{"commission", regexp.MustCompile(`(?i)^сумма\s+комиссии\s*:?\s*(.*)$`)},
	{"commission", regexp.MustCompile(`(?i)^комиссия\s*:?\s*(.*)$`)},

	// --- Сумма ---
	// Сбер, ВТБ, большинство банков
	{"amount", regexp.MustCompile(`(?i)^сумма\s+перевода\s*:?\s*(.*)$`)},
	{"amount", regexp.MustCompile(`(?i)^сумма\s+операции\s*:?\s*(.*)$`)},
	{"amount", regexp.MustCompile(`(?i)^сумма\s+платежа\s*:?\s*(.*)$`)},
	// Альфа-Банк
	{"amount", regexp.MustCompile(`(?i)^сколько\s*:?\s*(.*)$`)},
	// Т-Банк и др.
	{"amount", regexp.MustCompile(`(?i)^итого\s*:?\s*(.*)$`)},
	// Общий вариант — В САМОМ КОНЦЕ, чтобы не перехватывать специфичные
	{"amount", regexp.MustCompile(`(?i)^сумма\s*:?\s*(.*)$`)},

	// --- Служебные поля ---
	{"doc", regexp.MustCompile(`(?i)^номер\s+документа\s*:?\s*(.*)$`)},
	{"doc", regexp.MustCompile(`(?i)^номер\s+операции\s+в\s+сбп\s*:?\s*(.*)$`)},
	{"doc", regexp.MustCompile(`(?i)^номер\s+операции\s*:?\s*(.*)$`)},
	{"doc", regexp.MustCompile(`(?i)^идентификатор\s+операции(?:\s+в\s+сбп)?\s*:?\s*(.*)$`)},
	{"doc", regexp.MustCompile(`(?i)^id\s+операции(?:\s+в\s+сбп)?\s*:?\s*(.*)$`)},
	{"doc", regexp.MustCompile(`(?i)^номер\s+квитанции\s*:?\s*(.*)$`)},
	{"doc", regexp.MustCompile(`(?i)^квитанция\s*№?\s*(.*)$`)},
	{"auth", regexp.MustCompile(`(?i)^код\s+авторизации\s*:?\s*(.*)$`)},
	{"status", regexp.MustCompile(`(?i)^статус\s+операции\s*:?\s*(.*)$`)},
	{"status", regexp.MustCompile(`(?i)^статус\s*:?\s*(.*)$`)},
}

var moneyRe = regexp.MustCompile(`\d[\d\s.,]*\d|\d`)

// Дата/время операции на чеке — два реальных формата:
//
//	"26 июня 2026 22:58:35 (МСК)"   — день + название месяца (родительный падеж) + год + время
//	"27.06.2026 11:43:33" / "20.06.2026, 16:04" — числовой формат, с запятой перед временем или без,
//	с секундами или без.
var (
	namedDateTimeRe   = regexp.MustCompile(`(?i)(\d{1,2})\s+(января|февраля|марта|апреля|мая|июня|июля|августа|сентября|октября|ноября|декабря)\s+(\d{4})(?:\s+(\d{1,2}):(\d{2})(?::(\d{2}))?)?`)
	numericDateTimeRe = regexp.MustCompile(`(\d{2})\.(\d{2})\.(\d{4}),?\s+(\d{1,2}):(\d{2})(?::(\d{2}))?`)

	monthGenitive = map[string]time.Month{
		"января": time.January, "февраля": time.February, "марта": time.March,
		"апреля": time.April, "мая": time.May, "июня": time.June,
		"июля": time.July, "августа": time.August, "сентября": time.September,
		"октября": time.October, "ноября": time.November, "декабря": time.December,
	}
)

// detectTxTime ищет дату/время операции построчно. Строки с явным словом
// "дата" проверяются в первую очередь — это надёжнее, чем случайное первое
// совпадение шаблона где-то ещё на чеке.
func detectTxTime(lines []string) (time.Time, bool) {
	for _, l := range lines {
		if strings.Contains(strings.ToLower(l), "дата") {
			if t, ok := parseDateTimeFromLine(l); ok {
				return t, true
			}
		}
	}
	for _, l := range lines {
		if t, ok := parseDateTimeFromLine(l); ok {
			return t, true
		}
	}
	return time.Time{}, false
}

func parseDateTimeFromLine(line string) (time.Time, bool) {
	if m := namedDateTimeRe.FindStringSubmatch(line); m != nil {
		day, _ := strconv.Atoi(m[1])
		month, ok := monthGenitive[strings.ToLower(m[2])]
		if !ok {
			return time.Time{}, false
		}
		year, _ := strconv.Atoi(m[3])
		hour, min, sec := 0, 0, 0
		if m[4] != "" {
			hour, _ = strconv.Atoi(m[4])
			min, _ = strconv.Atoi(m[5])
		}
		if m[6] != "" {
			sec, _ = strconv.Atoi(m[6])
		}
		return time.Date(year, month, day, hour, min, sec, 0, time.Local), true
	}
	if m := numericDateTimeRe.FindStringSubmatch(line); m != nil {
		day, _ := strconv.Atoi(m[1])
		monthNum, _ := strconv.Atoi(m[2])
		if monthNum < 1 || monthNum > 12 {
			return time.Time{}, false
		}
		year, _ := strconv.Atoi(m[3])
		hour, _ := strconv.Atoi(m[4])
		min, _ := strconv.Atoi(m[5])
		sec := 0
		if m[6] != "" {
			sec, _ = strconv.Atoi(m[6])
		}
		return time.Date(year, time.Month(monthNum), day, hour, min, sec, 0, time.Local), true
	}
	return time.Time{}, false
}

// LooksLikeBankReceipt — быстрая проверка, стоит ли вообще пытаться
// разобрать текст как банковский чек (иначе он уйдёт в обычный ParseMessage).
func LooksLikeBankReceipt(text string) bool {
	lower := strings.ToLower(text)
	if strings.Contains(lower, "получател") || strings.Contains(lower, "исходящий перевод") ||
		strings.Contains(lower, "код авторизации") || strings.Contains(lower, "чек по операции") ||
		strings.Contains(lower, "сумма перевода") || strings.Contains(lower, "перевод по сбп") ||
		strings.Contains(lower, "перевод клиенту") {
		return true
	}
	for _, re := range bankMarkers {
		if re.MatchString(text) {
			return true
		}
	}
	return false
}

// ParseReceipt разбирает текст скриншота банковского перевода.
func ParseReceipt(text string) ReceiptData {
	rd := ReceiptData{RawText: text}

	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")

	if t, ok := detectTxTime(lines); ok {
		rd.TxTime = t
		rd.HasTxTime = true
	}

	// Банк ищем СНАЧАЛА в шапке чека (первые непустые строки): на чеке ВТБ
	// внизу есть поле "Банк получателя: Т-Банк", и поиск по всему тексту
	// определил бы банк неправильно. Если в шапке не нашли (логотип-картинка,
	// как у Т-Банка) — ищем по всему тексту (штампы внизу: "АО «ТБАНК»").
	rd.Bank = detectBank(headerLines(lines, 5))
	if rd.Bank == "" {
		rd.Bank = detectBank(text)
	}
	filled := map[string]bool{}
	for i := 0; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}

		for _, rule := range fieldRules {
			if filled[rule.field] {
				continue // первое успешное правило для поля побеждает
			}
			m := rule.re.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			value := strings.TrimSpace(m[1])
			if value == "" {
				value = nextNonEmptyLine(lines, i+1)
			}
			// Значение не должно само быть очередным лейблом (бывает при
			// нестандартной вёрстке скриншота, когда OCR склеивает строки).
			if value != "" && !isFieldLabel(value) {
				applyField(&rd, rule.field, value)
				filled[rule.field] = true
			}
			break
		}
	}

	// Формат СБП-перевода (ВТБ и другие банки часто пишут это фразой, а не текстом
	// с логотипом — логотип на скриншоте это картинка, OCR его не прочитает).
	// ФИО получателя обычно идёт следующей строкой, без явного лейбла "Получатель".
	if rd.Recipient == "" {
		for i, line := range lines {
			if strings.Contains(strings.ToLower(line), "исходящий перевод") {
				if rd.Bank == "" {
					rd.Bank = "ВТБ" // характерная формулировка именно приложения ВТБ
				}
				if name := nextNonEmptyLine(lines, i+1); looksLikeFIO(name) {
					rd.Recipient = name
				}
				break
			}
		}
	}

	// Фолбэк для суммы: у некоторых банков (РСХБ) сумма стоит крупно в шапке
	// БЕЗ лейбла ("18000 ₽" отдельной строкой). Берём наибольшую строку вида
	// "<число> ₽/руб/р." — наибольшую, чтобы не спутать с комиссией "0 ₽".
	if rd.Amount == 0 {
		standaloneAmountRe := regexp.MustCompile(`(?i)^(\d[\d\s.,]*)\s*(₽|руб\.?|р\.?)$`)
		for _, l := range lines {
			l = strings.TrimSpace(l)
			if m := standaloneAmountRe.FindStringSubmatch(l); m != nil {
				if v := ParseMoneyValue(m[1]); v > rd.Amount {
					rd.Amount = v
				}
			}
		}
	}

	return rd
}

func applyField(rd *ReceiptData, field, value string) {
	switch field {
	case "recipient":
		rd.Recipient = value
	case "sender":
		rd.Sender = value
	case "amount":
		rd.Amount = ParseMoneyValue(value)
	case "commission":
		rd.Commission = ParseMoneyValue(value)
	case "doc":
		rd.DocNumber = value
	case "auth":
		rd.AuthCode = value
	case "status":
		rd.Status = value
	}
}

// detectBank ищет маркер банка в заданном тексте в детерминированном порядке.
func detectBank(text string) string {
	for _, bank := range bankOrder {
		if bankMarkers[bank].MatchString(text) {
			return bank
		}
	}
	// Универсальный ловец: любой банк, которого нет в списке выше, но чьё
	// название оформлено одним словом на "…банк" (Мособлбанк, Тимербанк и т.п.).
	return genericBankName(text)
}

// reGenericBank ловит слово, оканчивающееся на "банк" (мин. 3 буквы до него),
// ограниченное не-буквами. Так распознаётся ЛЮБОЙ банк с названием-одним-словом,
// даже не перечисленный явно. \p{L} корректно работает с кириллицей в RE2.
var reGenericBank = regexp.MustCompile(`(?i)(?:^|[^\p{L}])([\p{L}-]{3,}банк)(?:[^\p{L}]|$)`)

// notBankWords — «…банк»-слова, которые банком НЕ являются (интернет-банк и т.п.).
var notBankWords = map[string]bool{
	"интернет-банк": true, "интернетбанк": true, "онлайн-банк": true, "онлайнбанк": true,
	"мобильныйбанк": true, "мобильный-банк": true, "смс-банк": true, "банк-клиент": true,
	"необанк": true, "мойбанк": true, "супербанк": true,
}

func genericBankName(text string) string {
	m := reGenericBank.FindStringSubmatch(text)
	if m == nil {
		return ""
	}
	name := strings.ToLower(strings.TrimSpace(m[1]))
	if notBankWords[name] {
		return ""
	}
	r := []rune(name)
	r[0] = unicode.ToUpper(r[0])
	return string(r)
}

// headerLines возвращает первые n непустых строк одной строкой — "шапку" чека.
func headerLines(lines []string, n int) string {
	var out []string
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		out = append(out, l)
		if len(out) >= n {
			break
		}
	}
	return strings.Join(out, "\n")
}

// isFieldLabel проверяет, не является ли строка сама очередным лейблом поля.
func isFieldLabel(s string) bool {
	for _, rule := range fieldRules {
		if m := rule.re.FindStringSubmatch(s); m != nil && strings.TrimSpace(m[1]) == "" {
			return true
		}
	}
	return false
}

// ParseMoneyValue разбирает денежную строку в любом из реальных форматов
// с банковских скриншотов и из подписей к ним:
//
//	"30 000 Р"     -> 30000    (пробел — разделитель тысяч)
//	"10000.00 ₽"   -> 10000    (точка + 2 цифры — десятичная часть)
//	"79 650,00 ₽"  -> 79650    (запятая + 2 цифры — десятичная часть)
//	"35.000"       -> 35000    (точка + РОВНО 3 цифры — разделитель тысяч!)
//	"20.000р"      -> 20000
//	"844,03"       -> 844.03
func ParseMoneyValue(s string) float64 {
	m := moneyRe.FindString(s)
	if m == "" {
		return 0
	}
	cleaned := strings.TrimSpace(m)
	cleaned = strings.ReplaceAll(cleaned, " ", "")
	cleaned = strings.ReplaceAll(cleaned, "\u00a0", "")

	// Если и точка и запятая: точка — тысячи, запятая — десятичные ("1.234,56")
	if strings.Contains(cleaned, ".") && strings.Contains(cleaned, ",") {
		cleaned = strings.ReplaceAll(cleaned, ".", "")
		cleaned = strings.ReplaceAll(cleaned, ",", ".")
	} else {
		// Единственный разделитель: если после него РОВНО 3 цифры и перед ним
		// нет других разделителей — это разделитель тысяч ("35.000", "1,500").
		// Если 1-2 цифры — десятичная часть ("844,03", "10000.00").
		thousandsRe := regexp.MustCompile(`^(\d{1,3})[.,](\d{3})$`)
		if tm := thousandsRe.FindStringSubmatch(cleaned); tm != nil {
			cleaned = tm[1] + tm[2]
		} else {
			cleaned = strings.ReplaceAll(cleaned, ",", ".")
		}
	}

	val, err := strconv.ParseFloat(cleaned, 64)
	if err != nil {
		return 0
	}
	return val
}

func nextNonEmptyLine(lines []string, from int) string {
	for i := from; i < len(lines); i++ {
		l := strings.TrimSpace(lines[i])
		if l != "" {
			return l
		}
	}
	return ""
}

// looksLikeFIO — грубая эвристика: 2-3 слова, без цифр, каждое с заглавной буквы.
func looksLikeFIO(s string) bool {
	if regexp.MustCompile(`\d`).MatchString(s) {
		return false
	}
	words := strings.Fields(s)
	if len(words) < 2 || len(words) > 4 {
		return false
	}
	for _, w := range words {
		r := []rune(w)
		if len(r) == 0 || !strings.ContainsRune("АБВГДЕЁЖЗИЙКЛМНОПРСТУФХЦЧШЩЭЮЯABCDEFGHIJKLMNOPQRSTUVWXYZ", r[0]) {
			return false
		}
	}
	return true
}
