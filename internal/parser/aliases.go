package parser

import (
	"sort"
	"strings"
)

// AliasMap сопоставляет вариант написания имени с каноничным именем в базе.
// Заполняется из таблицы contacts.aliases при старте бота (см. internal/db).
// Здесь — дефолтный набор на основе твоих реальных сообщений, чтобы бот
// сразу узнавал "Пияна"/"Пиян" и "Хадижат"/"Хадижа" как одного человека.
type AliasMap struct {
	toCanonical map[string]string // нормализованный алиас -> каноничное имя
}

func NewAliasMap() *AliasMap {
	am := &AliasMap{toCanonical: make(map[string]string)}
	// ТОЛЬКО реальные варианты написания ОДНОГО человека (короткая форма/опечатка)
	// и служебная «наличка». БАРЕ-имена (Ахмед, Милана, Яхита, Нажуд, Сафаи) убраны
	// намеренно: они существовали лишь ради сопоставления по словам, которое сливало
	// РАЗНЫХ людей с одним именем в один контакт (любой «Ахмед …» → один «Ахмед»).
	defaults := map[string][]string{
		"Пияна":   {"пияна", "пиян"},
		"Хадижат": {"хадижат", "хадижа"},
		"Наличка": {"наличка", "нал"},
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

// ResolveName сопоставляет ФИО с канонимом ТОЛЬКО по точному совпадению всей
// строки (с нормализацией) или по алиасу, который владелец зарегистрировал сам.
// Сопоставления «по отдельным словам» здесь больше НЕТ: оно сливало разных людей
// с одинаковым именем в один контакт (любой «Ахмед Нажудович К.» → один «Ахмед»)
// — это была главная причина неверного учёта. Частичное/нечёткое сопоставление,
// где оно уместно, делает слой выше (ИИ по смыслу, сверка с программой). Если имя
// не распознано точно — возвращаем (исходная строка, false): пусть станет своим
// контактом, а владелец при необходимости объединит дубликаты инструментом слияния.
func (am *AliasMap) ResolveName(rawName string) (string, bool) {
	if canonical, ok := am.toCanonical[normalize(rawName)]; ok {
		return canonical, true
	}
	return strings.TrimSpace(rawName), false
}

// Canonicals возвращает отсортированный список всех каноничных имён —
// используется, например, чтобы подсказать ИИ-доразбору известных людей.
func (am *AliasMap) Canonicals() []string {
	seen := map[string]bool{}
	var out []string
	for _, canonical := range am.toCanonical {
		if !seen[canonical] {
			seen[canonical] = true
			out = append(out, canonical)
		}
	}
	sort.Strings(out)
	return out
}

func normalize(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}
