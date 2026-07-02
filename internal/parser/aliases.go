package parser

import "strings"

// AliasMap сопоставляет вариант написания имени с каноничным именем в базе.
// Заполняется из таблицы contacts.aliases при старте бота (см. internal/db).
// Здесь — дефолтный набор на основе твоих реальных сообщений, чтобы бот
// сразу узнавал "Пияна"/"Пиян" и "Хадижат"/"Хадижа" как одного человека.
type AliasMap struct {
	toCanonical map[string]string // нормализованный алиас -> каноничное имя
}

func NewAliasMap() *AliasMap {
	am := &AliasMap{toCanonical: make(map[string]string)}
	defaults := map[string][]string{
		"Пияна":   {"пияна", "пиян"},
		"Хадижат": {"хадижат", "хадижа"},
		"Наличка": {"наличка", "нал"},
		"Ахмед":   {"ахмед"},
		"Милана":  {"милана"},
		"Яхита":   {"яхита"},
		"Нажуд":   {"нажуд"},
		"Сафаи":   {"сафаи"},
	}
	for canonical, variants := range defaults {
		for _, v := range variants {
			am.toCanonical[normalize(v)] = canonical
		}
	}
	return am
}

// Add регистрирует новый алиас (например, добавленный владельцем через команду боту).
func (am *AliasMap) Add(alias, canonical string) {
	am.toCanonical[normalize(alias)] = canonical
}

// Resolve возвращает каноничное имя. Если алиас неизвестен — возвращает исходную
// строку как есть (с большой буквы), чтобы новое имя не потерялось молча.
func (am *AliasMap) Resolve(rawName string) string {
	if canonical, ok := am.toCanonical[normalize(rawName)]; ok {
		return canonical
	}
	return strings.TrimSpace(rawName)
}

// ResolveName — умное сопоставление для ФИО с банковских чеков вида
// "Милана Нажудовна К." или "Ахмед Нажудович К": сначала пробует точное
// совпадение всей строки, затем каждое слово ФИО по отдельности (банки
// печатают имя + отчество + первую букву фамилии, а в алиасах у нас
// обычно только имя). Возвращает (каноничное имя, true), если нашлось
// РОВНО одно совпадение; при нуле или нескольких разных кандидатах —
// (исходная строка, false), чтобы спорный чек ушёл на ручную проверку,
// а не записался не на того человека.
func (am *AliasMap) ResolveName(rawName string) (string, bool) {
	if canonical, ok := am.toCanonical[normalize(rawName)]; ok {
		return canonical, true
	}

	found := ""
	for _, word := range strings.Fields(rawName) {
		word = strings.Trim(word, ".,")
		if len([]rune(word)) < 3 {
			continue // инициалы ("К.") и предлоги не сравниваем
		}
		canonical, ok := am.toCanonical[normalize(word)]
		if !ok {
			continue
		}
		if found != "" && found != canonical {
			return strings.TrimSpace(rawName), false // двусмысленно — на ручную проверку
		}
		found = canonical
	}
	if found != "" {
		return found, true
	}
	return strings.TrimSpace(rawName), false
}

func normalize(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}
