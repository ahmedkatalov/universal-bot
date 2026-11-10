package db

import (
	"strings"
	"unicode"
)

// Shared by manual recovery and reading coverage. A legacy parsed attachment
// with no financial row may have been silently classified as "other". Known
// cash, commands, holds, deleted sources and every existing ledger row stay out.
// File availability is deliberately checked by the caller, not this predicate.
const legacyParsedMediaCandidateSQL = `rm.has_media AND rm.parsed
 AND NOT COALESCE(rm.deleted,false) AND rm.handled_as IS NULL AND NOT rm.cash_hint
 AND COALESCE(rm.wa_group_jid,'') LIKE '%@g.us' AND COALESCE(rm.sender_jid,'') <> 'bot-forward'
 AND NOT EXISTS (SELECT 1 FROM bank_receipts legacy_br WHERE legacy_br.raw_message_id=rm.id)
 AND NOT EXISTS (SELECT 1 FROM transactions legacy_tx WHERE legacy_tx.raw_message_id=rm.id)`

// This conservative legacy selector only reopens a plain name, never a printed
// bank party embedded in OCR, an amount, or an instruction. Unicode matching in
// Go also works with PostgreSQL databases created with the C locale.
func legacyParsedMediaClientName(caption, body string) bool {
	return legacyPlainClientName(caption) || legacyPlainClientName(body)
}

func legacyPlainClientName(text string) bool {
	text = strings.TrimSpace(text)
	if len([]rune(text)) > 100 || strings.ContainsAny(text, "\n\r?@/0123456789") {
		return false
	}
	words := strings.Fields(strings.ToLower(text))
	if len(words) < 2 || len(words) > 4 {
		return false
	}
	hasSurname := false
	for _, word := range words {
		if len([]rune(word)) < 2 || strings.HasPrefix(word, "-") || strings.HasSuffix(word, "-") {
			return false
		}
		for _, r := range word {
			if !unicode.IsLetter(r) && r != '-' {
				return false
			}
		}
		switch word {
		case "фото", "фотография", "паспорт", "паспорта", "карта", "карты", "чек", "чеки", "чека", "нал", "наличка", "наличные", "наличными", "сумма", "перевод", "получатель", "отправитель", "банк", "сбербанк", "онлайн", "документ", "договор", "клиент", "клиента", "фио", "оплата", "оплатил", "оплатила", "сегодня", "вчера", "завтра", "добрый", "доброе", "привет", "спасибо", "здравствуйте", "покажи", "найди", "запиши", "проверь", "джарвис", "бот":
			return false
		}
		for _, suffix := range []string{"ов", "ев", "ёв", "ин", "ын", "ова", "ева", "ёва", "ина", "ына", "ский", "ская", "цкий", "цкая", "енко", "ян", "дзе", "швили"} {
			if len([]rune(word)) > len([]rune(suffix))+2 && strings.HasSuffix(word, suffix) {
				hasSurname = true
			}
		}
	}
	return hasSurname
}
