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
	// TimeDefaulted — на чеке напечатана только ДАТА операции, без времени:
	// время в TxTime (полдень) придумано. Дата годится для отчёта, а сравнивать
	// по времени (текст написан до операции, тот же ли это перевод) нельзя —
	// такой чек хранится с угаданным временем (tx_time_guessed).
	TimeDefaulted bool
}

// TxTimeGuessed — время операции не напечатано на чеке: его нет вовсе (берут
// время сообщения) или есть лишь дата (полдень).
func (rd ReceiptData) TxTimeGuessed() bool {
	return !rd.HasTxTime || rd.TimeDefaulted
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
	"Зенит":             regexp.MustCompile(`(?i)банк\s*зенит|(^|\W)зенит(\W|$)`),
	"Абсолют":           regexp.MustCompile(`(?i)абсолют\s*банк`),
	"Новиком":           regexp.MustCompile(`(?i)новикомбанк`),
	"Металлинвест":      regexp.MustCompile(`(?i)металлинвест`),
	"Локо-Банк":         regexp.MustCompile(`(?i)локо.?банк`),
	"СМП Банк":          regexp.MustCompile(`(?i)(^|\W)смп\s*банк|(^|\W)смп(\W|$)`),
	"ДОМ.РФ":            regexp.MustCompile(`(?i)дом\.рф|дом\s*рф`),
	"РНКБ":              regexp.MustCompile(`(?i)(^|\W)рнкб(\W|$)|rncb`),
	"Кубань Кредит":     regexp.MustCompile(`(?i)кубань\s*кредит`),
	"Центр-инвест":      regexp.MustCompile(`(?i)центр.?инвест`),
	"Экспобанк":         regexp.MustCompile(`(?i)экспобанк|expobank`),
	"АТБ":               regexp.MustCompile(`(?i)азиатско.?тихоокеанск|(^|\W)атб(\W|$)`),
	"УБРиР":             regexp.MustCompile(`(?i)убрир|уральский\s*банк\s*реконструкц`),
	"Синара":            regexp.MustCompile(`(?i)банк\s*синара|(^|\W)скб.?банк`),
	"Авангард":          regexp.MustCompile(`(?i)банк\s*авангард`),
	"ВБРР":              regexp.MustCompile(`(?i)(^|\W)вбрр(\W|$)|всероссийский\s*банк\s*развития`),
	"Транскапиталбанк":  regexp.MustCompile(`(?i)транскапитал|(^|\W)ткб(\W|$)`),
	"БКС Банк":          regexp.MustCompile(`(?i)бкс\s*банк|(^|\W)бкс(\W|$)`),
	"Финам":             regexp.MustCompile(`(?i)финам\s*банк`),
	"Держава":           regexp.MustCompile(`(?i)банк\s*держава`),
	"Солидарность":      regexp.MustCompile(`(?i)банк\s*солидарность`),
	"Генбанк":           regexp.MustCompile(`(?i)генбанк|genbank`),
	"Банк Приморье":     regexp.MustCompile(`(?i)банк\s*приморье`),
	"Кошелев-Банк":      regexp.MustCompile(`(?i)кошелев.?банк`),
	"Ланта-Банк":        regexp.MustCompile(`(?i)ланта.?банк`),
	"Интеза":            regexp.MustCompile(`(?i)банк\s*интеза|intesa`),
	"Меткомбанк":        regexp.MustCompile(`(?i)меткомбанк`),
	"Примсоцбанк":       regexp.MustCompile(`(?i)примсоцбанк`),
	"Севергазбанк":      regexp.MustCompile(`(?i)севергазбанк`),
	"Челябинвестбанк":   regexp.MustCompile(`(?i)челябинвест`),
	"Челиндбанк":        regexp.MustCompile(`(?i)челиндбанк`),
	"Дальневосточный":   regexp.MustCompile(`(?i)дальневосточн`),
	"Кредит Урал Банк":  regexp.MustCompile(`(?i)кредит\s*урал`),
	"Тольяттихимбанк":   regexp.MustCompile(`(?i)тольяттихим`),
	"Инвестторгбанк":    regexp.MustCompile(`(?i)инвестторгбанк`),
	"НС Банк":           regexp.MustCompile(`(?i)(^|\W)нс\s*банк`),
	"Таврический":       regexp.MustCompile(`(?i)таврический`),
	"Кузнецкбизнесбанк": regexp.MustCompile(`(?i)кузнецкбизнес`),
	"СДМ-Банк":          regexp.MustCompile(`(?i)сдм.?банк`),
	"Газэнергобанк":     regexp.MustCompile(`(?i)газэнергобанк`),
	"Запсибкомбанк":     regexp.MustCompile(`(?i)запсибкомбанк`),
	"Кредит Москва":     regexp.MustCompile(`(?i)банк\s*кредит\s*москва`),
	"Уралфинанс":        regexp.MustCompile(`(?i)уралфинанс`),
	"Хакасский":         regexp.MustCompile(`(?i)хакасский\s*муниципальн`),
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

// Дата/время операции на чеке — два реальных формата:
//
//	"26 июня 2026 22:58:35 (МСК)"   — день + название месяца (родительный падеж) + год + время
//	"27.06.2026 11:43:33" / "20.06.2026, 16:04" — числовой формат, с запятой перед временем или без,
//	с секундами или без.
var (
	// «18 октября 2026 14:05», «… 2026 в 14:05», «… 2026, 14:05», «… 2026 г. в
	// 14:05». Время необязательно: дата без времени читается как «только дата»
	// (dateOnlyIn) — полночь за время операции не выдаётся.
	namedDateTimeRe   = regexp.MustCompile(`(?i)(\d{1,2})\s+(января|февраля|марта|апреля|мая|июня|июля|августа|сентября|октября|ноября|декабря)\s+(\d{4})(?:\s*(?:года|г\.?))?(?:(?:\s*,\s*(?:в\s*)?|\s+(?:в\s*)?)(\d{1,2}):(\d{2})(?::(\d{2}))?)?`)
	numericDateTimeRe = regexp.MustCompile(`(\d{2})\.(\d{2})\.(\d{4}|\d{2})(?:,|\s+в)?\s+(\d{1,2}):(\d{2})(?::(\d{2}))?`)
	isoDateTimeRe     = regexp.MustCompile(`(\d{4})-(\d{2})-(\d{2})[ T](\d{1,2}):(\d{2})(?::(\d{2}))?`)
	shortMonthRe      = regexp.MustCompile(`(?i)(\d{1,2})\s+(янв|фев|мар|апр|мая|май|июн|июл|авг|сен|окт|ноя|дек)[а-я]*\.?,?\s*(\d{4})?(?:\s*г\.)?,?\s*(?:в\s*)?(\d{1,2}):(\d{2})(?::(\d{2}))?`)
	numericDateOnlyRe = regexp.MustCompile(`(\d{2})\.(\d{2})\.(\d{4})`)
	timeOnlyRe        = regexp.MustCompile(`(?:^|\s)(\d{1,2}):(\d{2})(?::(\d{2}))?(?:\s|$)`)

	shortMonths = map[string]time.Month{
		"янв": time.January, "фев": time.February, "мар": time.March, "апр": time.April, "мая": time.May, "май": time.May,
		"июн": time.June, "июл": time.July, "авг": time.August, "сен": time.September, "окт": time.October,
		"ноя": time.November, "дек": time.December,
	}

	monthGenitive = map[string]time.Month{
		"января": time.January, "февраля": time.February, "марта": time.March,
		"апреля": time.April, "мая": time.May, "июня": time.June,
		"июля": time.July, "августа": time.August, "сентября": time.September,
		"октября": time.October, "ноября": time.November, "декабря": time.December,
	}
)

// notOperationDateRe — даты на чеке, которые НЕ дата операции: когда квитанцию
// сформировали/распечатали/выдали (у PDF это обычно момент скачивания — утром
// следующего дня), дата рождения.
var notOperationDateRe = regexp.MustCompile(`(?i)формир|печат|выдач|выдан|рожд`)

// operationDateLabelRe — подпись именно даты/времени операции. «Дата
// списания/зачисления» сюда не входит: банк списывает позже (после полуночи,
// на следующий день), а в отчёт чек идёт по дате ОПЕРАЦИИ.
var operationDateLabelRe = regexp.MustCompile(`(?i)операци|перевод|платеж|платёж|оплат`)

// detectTxTime ищет дату/время ОПЕРАЦИИ построчно (по ней чек идёт в отчёт).
// «Дата формирования/печати» не годится ни в одном из проходов — ни сама
// строка, ни значение под такой подписью. Порядок: «дата операции» без
// времени + явная строка «Время операции»; «дата операции» с временем; «дата
// операции» без времени (время — рядом или полдень); любая строка «дата»
// («Дата списания» тоже) с временем; любая дата с временем; любая «дата» без
// времени; и лишь когда даты операции нет совсем — дата формирования/печати.
// defaulted — время не нашлось нигде, у даты стоит полдень.
func detectTxTime(lines []string) (t time.Time, ok, defaulted bool) {
	skip := make([]bool, len(lines))
	for i, l := range lines {
		if !notOperationDateRe.MatchString(l) {
			continue
		}
		skip[i] = true
		// «Дата формирования» без значения — значение строкой ниже.
		if j := dateValueBelow(lines, i); j >= 0 {
			skip[j] = true
		}
	}
	// valueLine — строка со значением подписи i: она сама или (если в ней нет
	// даты) следующая строка с датой («Дата операции\n17.10.2026 23:40»).
	valueLine := func(i int) string {
		if j := dateValueBelow(lines, i); j >= 0 && !skip[j] {
			return lines[j]
		}
		return lines[i]
	}
	isOpLabel := func(i int) bool {
		low := strings.ToLower(lines[i])
		return !skip[i] && (strings.Contains(low, "дата") || strings.Contains(low, "врем")) && operationDateLabelRe.MatchString(low)
	}
	// 1. «Дата операции: 17.10.2026» + «Время операции: 23:40» — пара явных
	// подписей операции важнее любой другой даты со временем («Дата
	// списания: 18.10.2026 00:15»).
	for i := range lines {
		if !isOpLabel(i) || !strings.Contains(strings.ToLower(lines[i]), "дата") {
			continue
		}
		if _, ok := parseDateTimeFromLine(valueLine(i)); ok {
			continue
		}
		if t, ok := dateWithLabelledTime(lines, skip, valueLine(i)); ok {
			return t, true, false
		}
	}
	// 2. «Дата (и время) операции/перевода/платежа» с временем.
	for i := range lines {
		if isOpLabel(i) {
			if t, ok := parseDateTimeFromLine(valueLine(i)); ok {
				return t, true, false
			}
		}
	}
	// 3. «Дата операции: 17.10.2026» без «Времени операции»: время рядом или полдень.
	for i := range lines {
		if isOpLabel(i) && strings.Contains(strings.ToLower(lines[i]), "дата") {
			if t, ok, noon := dateWithSeparateTime(lines, skip, i, valueLine(i)); ok {
				return t, true, noon
			}
		}
	}
	// 4. Любая строка «дата» с временем.
	for i, l := range lines {
		if !skip[i] && strings.Contains(strings.ToLower(l), "дата") {
			if t, ok := parseDateTimeFromLine(valueLine(i)); ok {
				return t, true, false
			}
		}
	}
	// 5. Любая дата с временем.
	for i, l := range lines {
		if skip[i] {
			continue
		}
		if t, ok := parseDateTimeFromLine(l); ok {
			return t, true, false
		}
	}
	// 6. «Дата: 30.09.2026» без времени (или время отдельной строкой): дата с
	// чека важнее даты присылки.
	for i, l := range lines {
		if !skip[i] && strings.Contains(strings.ToLower(l), "дата") {
			if t, ok, noon := dateWithSeparateTime(lines, skip, i, valueLine(i)); ok {
				return t, true, noon
			}
		}
	}
	// 6б. Дата словом месяца без подписи и без времени («Квитанция от 18 октября
	// 2026»): дата с чека важнее даты присылки; время — рядом или полдень.
	for i, l := range lines {
		if !skip[i] && namedDateTimeRe.MatchString(l) {
			if t, ok, noon := dateWithSeparateTime(lines, skip, i, l); ok {
				return t, true, noon
			}
		}
	}
	// 7. Даты операции на чеке нет вовсе — «Дата формирования» всё же ближе к
	// операции, чем время присылки (PDF скачали сразу, прислали позже). Дата
	// рождения/выдачи документа — никогда.
	for i, l := range lines {
		low := strings.ToLower(l)
		if !skip[i] || strings.Contains(low, "рожд") || strings.Contains(low, "выда") || !notOperationDateRe.MatchString(l) {
			continue
		}
		v := l
		if j := dateValueBelow(lines, i); j >= 0 {
			v = lines[j]
		}
		if t, ok := parseDateTimeFromLine(v); ok {
			return t, true, false
		}
	}
	return time.Time{}, false, false
}

// dateOnlyIn — дата «дд.мм.гггг» или «18 октября 2026» в строке.
func dateOnlyIn(s string) (time.Time, bool) {
	m := numericDateOnlyRe.FindStringSubmatch(s)
	if m == nil {
		if n := namedDateTimeRe.FindStringSubmatch(s); n != nil {
			day, _ := strconv.Atoi(n[1])
			year, _ := strconv.Atoi(n[3])
			if mon, ok := monthGenitive[strings.ToLower(n[2])]; ok && day >= 1 && day <= 31 {
				return time.Date(year, mon, day, 0, 0, 0, 0, time.Local), true
			}
		}
		return time.Time{}, false
	}
	day, _ := strconv.Atoi(m[1])
	mon, _ := strconv.Atoi(m[2])
	year, _ := strconv.Atoi(m[3])
	if mon < 1 || mon > 12 || day < 1 || day > 31 {
		return time.Time{}, false
	}
	return time.Date(year, time.Month(mon), day, 0, 0, 0, 0, time.Local), true
}

// timeOfDay — время «чч:мм[:сс]» (из timeOnlyRe) в день d.
func timeOfDay(d time.Time, tm []string) (time.Time, bool) {
	hour, _ := strconv.Atoi(tm[1])
	min, _ := strconv.Atoi(tm[2])
	sec := 0
	if tm[3] != "" {
		sec, _ = strconv.Atoi(tm[3])
	}
	if hour > 23 || min > 59 || sec > 59 {
		return time.Time{}, false
	}
	return time.Date(d.Year(), d.Month(), d.Day(), hour, min, sec, 0, time.Local), true
}

// otherDateIn — в строке своя дата, и это не день d: её время к d не относится
// («Дата списания: 18.10.2026 00:15» — не время операции 17.10).
func otherDateIn(line string, d time.Time) bool {
	if t, ok := dateOnlyIn(line); ok {
		return !(t.Year() == d.Year() && t.YearDay() == d.YearDay())
	}
	if t, ok := parseDateTimeFromLine(line); ok {
		return !(t.Year() == d.Year() && t.YearDay() == d.YearDay())
	}
	return false
}

// dateWithLabelledTime — дата из value и время из строки «Время операции/
// перевода» (значение в ней же или строкой ниже). Нет такой строки — false.
func dateWithLabelledTime(lines []string, skip []bool, value string) (time.Time, bool) {
	d, ok := dateOnlyIn(value)
	if !ok {
		return time.Time{}, false
	}
	for k, l := range lines {
		low := strings.ToLower(l)
		if skip[k] || !strings.Contains(low, "врем") || !operationDateLabelRe.MatchString(low) || otherDateIn(l, d) {
			continue
		}
		v := l
		if timeOnlyRe.FindStringSubmatch(v) == nil {
			if j := nextNonEmptyIndex(lines, k+1); j >= 0 && !skip[j] && !otherDateIn(lines[j], d) {
				v = lines[j]
			}
		}
		if tm := timeOnlyRe.FindStringSubmatch(v); tm != nil {
			if t, ok := timeOfDay(d, tm); ok {
				return t, true
			}
		}
	}
	return time.Time{}, false
}

// dateWithSeparateTime — дата из value (подпись в строке i) и время к ней:
// строка «Время операции/перевода» где угодно на чеке, иначе время в ближайших
// строках под датой, иначе время любой даты-со-временем ТОГО ЖЕ дня, иначе
// полдень (noon=true: время придумано). Строки формирования/печати (skip) и
// строки с ДРУГОЙ датой («Дата списания: 18.10.2026 00:15» под «Дата операции:
// 17.10.2026») не смотрим.
func dateWithSeparateTime(lines []string, skip []bool, i int, value string) (t time.Time, ok, noon bool) {
	d, ok := dateOnlyIn(value)
	if !ok {
		return time.Time{}, false, false
	}
	if t, ok := dateWithLabelledTime(lines, skip, value); ok {
		return t, true, false
	}
	for k := i; k < len(lines) && k <= i+3; k++ {
		if skip[k] || otherDateIn(lines[k], d) {
			continue
		}
		if tm := timeOnlyRe.FindStringSubmatch(lines[k]); tm != nil {
			if t, ok := timeOfDay(d, tm); ok {
				return t, true, false
			}
		}
	}
	for k, l := range lines {
		if skip[k] {
			continue
		}
		if t, ok := parseDateTimeFromLine(l); ok && t.Year() == d.Year() && t.YearDay() == d.YearDay() {
			return t, true, false
		}
	}
	return time.Date(d.Year(), d.Month(), d.Day(), 12, 0, 0, 0, time.Local), true, true
}

// lineHasDate — в строке есть дата (числом или словом месяца).
func lineHasDate(s string) bool {
	if _, ok := dateOnlyIn(s); ok {
		return true
	}
	if _, ok := parseDateTimeFromLine(s); ok {
		return true
	}
	return namedDateTimeRe.MatchString(s)
}

// dateValueBelow — у подписи i без даты («Дата операции») значение — следующая
// непустая строка с датой, которая сама не подпись. −1 — нет такой.
func dateValueBelow(lines []string, i int) int {
	if lineHasDate(lines[i]) {
		return -1
	}
	j := nextNonEmptyIndex(lines, i+1)
	if j < 0 || !lineHasDate(lines[j]) {
		return -1
	}
	low := strings.ToLower(lines[j])
	if strings.Contains(low, "дата") || strings.Contains(low, "врем") {
		return -1
	}
	return j
}

// nextNonEmptyIndex — номер следующей непустой строки (−1 — нет).
func nextNonEmptyIndex(lines []string, from int) int {
	for i := from; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) != "" {
			return i
		}
	}
	return -1
}

// parseDateTimeFromLine — дата С ВРЕМЕНЕМ в строке. «18 октября 2026» без
// времени сюда не годится (полночь — не время операции: два перевода одного
// клиента за день слились бы в один чек) — такую дату берёт dateOnlyIn.
func parseDateTimeFromLine(line string) (time.Time, bool) {
	if m := namedDateTimeRe.FindStringSubmatch(line); m != nil && m[4] != "" {
		day, _ := strconv.Atoi(m[1])
		month, ok := monthGenitive[strings.ToLower(m[2])]
		if !ok {
			return time.Time{}, false
		}
		year, _ := strconv.Atoi(m[3])
		hour, _ := strconv.Atoi(m[4])
		min, _ := strconv.Atoi(m[5])
		sec := 0
		if m[6] != "" {
			sec, _ = strconv.Atoi(m[6])
		}
		return time.Date(year, month, day, hour, min, sec, 0, time.Local), true
	}
	if m := isoDateTimeRe.FindStringSubmatch(line); m != nil {
		year, _ := strconv.Atoi(m[1])
		mon, _ := strconv.Atoi(m[2])
		day, _ := strconv.Atoi(m[3])
		hour, _ := strconv.Atoi(m[4])
		min, _ := strconv.Atoi(m[5])
		sec := 0
		if m[6] != "" {
			sec, _ = strconv.Atoi(m[6])
		}
		if mon >= 1 && mon <= 12 {
			return time.Date(year, time.Month(mon), day, hour, min, sec, 0, time.Local), true
		}
	}
	if m := shortMonthRe.FindStringSubmatch(line); m != nil {
		day, _ := strconv.Atoi(m[1])
		month := shortMonths[strings.ToLower(m[2])]
		hour, _ := strconv.Atoi(m[4])
		min, _ := strconv.Atoi(m[5])
		sec := 0
		if m[6] != "" {
			sec, _ = strconv.Atoi(m[6])
		}
		now := time.Now()
		year := now.Year()
		if m[3] != "" {
			year, _ = strconv.Atoi(m[3])
		}
		t := time.Date(year, month, day, hour, min, sec, 0, time.Local)
		if m[3] == "" && t.After(now.Add(24*time.Hour)) {
			t = t.AddDate(-1, 0, 0) // «28 декабря» в январе — прошлого года
		}
		return t, true
	}
	if m := numericDateTimeRe.FindStringSubmatch(line); m != nil {
		day, _ := strconv.Atoi(m[1])
		monthNum, _ := strconv.Atoi(m[2])
		if monthNum < 1 || monthNum > 12 {
			return time.Time{}, false
		}
		year, _ := strconv.Atoi(m[3])
		if year < 100 {
			year += 2000 // «16.10.26»
		}
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

	if t, ok, noon := detectTxTime(lines); ok {
		rd.TxTime = t
		rd.HasTxTime = true
		rd.TimeDefaulted = noon
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
			// Номер документа — не дата и не время («Квитанция» в шапке, а под
			// ней дата: дата номером документа не становится).
			if rule.field == "doc" && !plausibleDocNumber(value) {
				break
			}
			if value != "" && !isFieldLabel(value) {
				applyField(&rd, rule.field, value)
				filled[rule.field] = true
			}
			break
		}
	}

	// ЧИСТАЯ сумма перевода важнее того, что схватил построчный разбор. Построчно
	// побеждает ПЕРВАЯ подходящая строка, поэтому «Итого»/«Сумма списания»,
	// напечатанные ВЫШЕ (и иногда включающие комиссию), перебивали настоящую
	// «Сумма перевода» ниже. Явную сумму перевода/операции/платежа берём как
	// итоговую (комиссию НЕ вычитаем — банк берёт её сверх).
	if net, ok := netTransferAmount(lines); ok {
		rd.Amount = net
	} else if rd.Commission > 0 {
		// «Итого 15 150 / Сумма 15 000 / Комиссия 150» (Т-Банк, Озон): в сбор идёт
		// сумма ПЕРЕВОДА, без комиссии банка.
		if bare, ok := bareSumAmount(lines); ok && bare < rd.Amount {
			rd.Amount = bare
		} else if itogo, ok := labeledAmount(lines, itogoRe); ok && rd.Amount == itogo && itogo > rd.Commission {
			rd.Amount = itogo - rd.Commission
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

var (
	bareSumRe = regexp.MustCompile(`(?i)^сумма\s*:?\s*(.*)$`)
	itogoRe   = regexp.MustCompile(`(?i)^итого\s*:?\s*(.*)$`)
	docDateRe = regexp.MustCompile(`^\d{1,2}[./-]\d{1,2}[./-]\d{2,4}`)
)

// plausibleDocNumber — похоже на номер документа/операции: есть цифры (4+), и
// это не дата/время.
func plausibleDocNumber(v string) bool {
	v = strings.TrimSpace(v)
	if v == "" || docDateRe.MatchString(v) || strings.Count(v, ":") >= 1 && strings.ContainsAny(v, ".-") {
		return false
	}
	digits := 0
	for _, r := range v {
		if r >= '0' && r <= '9' {
			digits++
		}
	}
	return digits >= 4
}

// bareSumAmount — значение строки «Сумма» (без уточнения «перевода/комиссии/
// списания/прописью»).
func bareSumAmount(lines []string) (float64, bool) {
	return labeledAmount(lines, bareSumRe)
}

func labeledAmount(lines []string, re *regexp.Regexp) (float64, bool) {
	for i, raw := range lines {
		line := strings.TrimSpace(raw)
		m := re.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		value := strings.TrimSpace(m[1])
		if value == "" {
			value = nextNonEmptyLine(lines, i+1)
		}
		if value == "" || isFieldLabel(value) {
			continue
		}
		if v := ParseMoneyValue(value); v > 0 {
			return v, true
		}
	}
	return 0, false
}

// netAmountRules — лейблы ЧИСТОЙ суммы перевода (без комиссии), в порядке
// предпочтения. Отдельно от fieldRules, чтобы не трогать общий построчный разбор.
var netAmountRules = []*regexp.Regexp{
	regexp.MustCompile(`(?i)^сумма\s+перевода\s*:?\s*(.*)$`),
	regexp.MustCompile(`(?i)^сумма\s+операции\s*:?\s*(.*)$`),
	regexp.MustCompile(`(?i)^сумма\s+платежа\s*:?\s*(.*)$`),
	regexp.MustCompile(`(?i)^перевод\s+по\s+сбп\s*:?\s*(.*)$`),
}

// netTransferAmount ищет ЧИСТУЮ сумму перевода (по лейблу назначения), игнорируя
// «Итого»/«Сумма списания»/«Сумма прописью». Возвращает первое валидное значение.
func netTransferAmount(lines []string) (float64, bool) {
	for i, raw := range lines {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		for _, re := range netAmountRules {
			m := re.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			value := strings.TrimSpace(m[1])
			if value == "" {
				value = nextNonEmptyLine(lines, i+1)
			}
			if value == "" || isFieldLabel(value) {
				continue
			}
			if v := ParseMoneyValue(value); v > 0 {
				return v, true
			}
		}
	}
	return 0, false
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
//
// Берётся ПЕРВОЕ число строки. Пробел склеивает цифры только как разделитель
// тысяч («15 000», «1 500 000,00»): «15000 18.10» -> 15000, а не 1500018.10, и
// перенос строки числа не склеивает.
func ParseMoneyValue(s string) float64 {
	return moneyValue(firstMoneyNumber(s))
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
