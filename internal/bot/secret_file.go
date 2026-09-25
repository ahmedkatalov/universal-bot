// Выдача СЕКРЕТНОГО файла (например, «доступы к проектам») по секретному коду.
// Максимальная защита:
//   - работает ТОЛЬКО в личке (никогда в группе);
//   - ТОЛЬКО для номеров из белого списка (SECRET_FILE_RECIPIENTS);
//   - код сравнивается по SHA-256 в постоянное время; сам код нигде не хранится
//     и не логируется; сообщение с кодом НЕ попадает в БД (handleSecretFile
//     отрабатывает до сохранения);
//   - перед отправкой бот ПЕРЕСПРАШИВАЕТ подтверждение, файл уходит только на «да».
//
// Для чужих номеров функция НИЧЕГО не делает и НИЧЕГО не раскрывает — код у них
// просто «не сработает», как обычное сообщение.
package bot

import (
	"crypto/sha256"
	"crypto/subtle"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

// secretAskTTL — сколько ждём подтверждения «да» после ввода кода.
const secretAskTTL = 5 * time.Minute

// loadSecretFileConfig читает настройки секретного файла из окружения. Хранит
// только SHA-256 кода. Если код/файл/получатели не заданы — функция выключена.
func (b *Bot) loadSecretFileConfig() {
	b.secretAsk = make(map[string]time.Time)

	path := strings.TrimSpace(os.Getenv("SECRET_FILE_PATH"))
	code := strings.TrimSpace(os.Getenv("SECRET_FILE_CODE"))
	recips := strings.TrimSpace(os.Getenv("SECRET_FILE_RECIPIENTS"))
	if path == "" || code == "" || recips == "" {
		return // не настроено — фича выключена (безопасно по умолчанию)
	}

	b.secretRecipients = map[string]bool{}
	for _, n := range strings.Split(recips, ",") {
		if p := normalizePhone(n); p != "" {
			b.secretRecipients[p] = true
		}
	}
	if len(b.secretRecipients) == 0 {
		return
	}

	b.secretFilePath = path
	b.secretFileName = strings.TrimSpace(os.Getenv("SECRET_FILE_NAME"))
	if b.secretFileName == "" {
		b.secretFileName = filepath.Base(path)
	}
	b.secretCodeSHA = sha256.Sum256([]byte(code))
	b.secretHasCode = true
	// Лог без секретов: только факт настройки и число получателей.
	fmt.Printf("Секретный файл настроен: получателей %d, файл %q\n", len(b.secretRecipients), b.secretFileName)
}

// isSecretRecipient — отправитель среди тех, кому вообще разрешено получить файл.
// Сверяем и основной номер, и альтернативный (WhatsApp может подставлять LID).
func (b *Bot) isSecretRecipient(info types.MessageInfo) bool {
	if len(b.secretRecipients) == 0 {
		return false
	}
	return b.secretRecipients[normalizePhone(info.Sender.User)] ||
		b.secretRecipients[normalizePhone(info.SenderAlt.User)]
}

// handleSecretFile обрабатывает поток выдачи секретного файла в ЛИЧКЕ. Возвращает
// true, если сообщение относится к этому потоку (обрабатывать дальше не нужно, и
// оно НЕ должно логироваться/сохраняться). Для чужих номеров и не-кода — false.
func (b *Bot) handleSecretFile(msg *events.Message) bool {
	if !b.secretHasCode || msg.Info.IsGroup {
		return false
	}
	if !b.isSecretRecipient(msg.Info) {
		return false // не наш номер — ведём себя как обычно, файла «не существует»
	}
	text := strings.TrimSpace(extractText(msg.Message))
	if text == "" {
		return false
	}
	key := normalizePhone(msg.Info.Sender.User)
	chat := msg.Info.Chat

	// Ждём ли подтверждения от этого отправителя?
	b.secretAskMu.Lock()
	asked, pending := b.secretAsk[key]
	if pending && time.Since(asked) > secretAskTTL {
		delete(b.secretAsk, key)
		pending = false
	}
	b.secretAskMu.Unlock()

	if pending {
		lower := strings.ToLower(text)
		switch {
		case secretIsYes(lower):
			b.secretAskMu.Lock()
			delete(b.secretAsk, key)
			b.secretAskMu.Unlock()
			b.sendSecretFile(chat)
			return true
		case secretIsNo(lower):
			b.secretAskMu.Lock()
			delete(b.secretAsk, key)
			b.secretAskMu.Unlock()
			b.sendText(chat, "Понял, не отправляю. Если понадобится — пришлите код ещё раз.")
			return true
		default:
			// Непонятный ответ — переспросим, поток держим до истечения времени.
			b.sendText(chat, "Ответьте «да» — отправлю файл, или «нет» — отменю.")
			return true
		}
	}

	// Не в режиме подтверждения: пришёл ли секретный код?
	if b.matchesSecretCode(text) {
		b.secretAskMu.Lock()
		b.secretAsk[key] = time.Now()
		b.secretAskMu.Unlock()
		b.sendText(chat, "Вы точно хотите, чтобы я отправил вам PDF с доступами? Может, вы просто проверяете? "+
			"Ответьте «да» — отправлю, «нет» — отменю.")
		return true
	}
	return false // код не совпал — обычная обработка (ассистент и т.д.)
}

// matchesSecretCode сравнивает присланный текст с кодом по SHA-256 в постоянное
// время (без утечки по времени/длине и без хранения самого кода).
func (b *Bot) matchesSecretCode(text string) bool {
	got := sha256.Sum256([]byte(strings.TrimSpace(text)))
	return subtle.ConstantTimeCompare(got[:], b.secretCodeSHA[:]) == 1
}

// sendSecretFile отправляет секретный файл получателю (в личку).
func (b *Bot) sendSecretFile(chat types.JID) {
	if fi, err := os.Stat(b.secretFilePath); err != nil || fi.IsDir() {
		fmt.Println("Секретный файл недоступен на сервере:", b.secretFilePath)
		b.sendText(chat, "Не получилось найти файл на сервере — сообщите владельцу, что файл не на месте.")
		return
	}
	if err := b.sendDocument(chat, b.secretFilePath, b.secretFileName); err != nil {
		b.sendText(chat, "Не получилось отправить файл — попробуйте ещё раз чуть позже.")
		return
	}
	b.sendText(chat, "Готово, отправил файл. Пожалуйста, никому его не пересылайте.")
}

// firstWord — первое слово строки (до пробела/знака препинания).
func firstWord(lower string) string {
	lower = strings.TrimSpace(lower)
	if i := strings.IndexFunc(lower, func(r rune) bool {
		return r == ' ' || r == ',' || r == '.' || r == '!' || r == '?' || r == ';' || r == ':'
	}); i >= 0 {
		lower = lower[:i]
	}
	return lower
}

// secretIsYes / secretIsNo — распознавание подтверждения/отказа (короткие ответы).
func secretIsYes(lower string) bool {
	switch firstWord(lower) {
	case "да", "ага", "угу", "давай", "отправь", "отправляй", "скинь", "кидай",
		"конечно", "точно", "верно", "yes", "ок", "ok", "ес":
		return true
	}
	return false
}

func secretIsNo(lower string) bool {
	if fw := firstWord(lower); fw == "нет" || fw == "no" {
		return true
	}
	for _, n := range []string{"не надо", "не нужно", "не отправляй", "отмен", "стоп", "cancel"} {
		if strings.Contains(lower, n) {
			return true
		}
	}
	return false
}
