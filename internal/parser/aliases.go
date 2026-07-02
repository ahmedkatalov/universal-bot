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

func normalize(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}
