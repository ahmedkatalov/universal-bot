package bot

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"go.mau.fi/whatsmeow/types"
)

func pdfPageCount(ctx context.Context, data []byte) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "pdfinfo", "-")
	cmd.Stdin = bytes.NewReader(data)
	output, err := cmd.Output()
	if err != nil {
		return 0, fmt.Errorf("не удалось проверить страницы PDF")
	}
	for _, line := range strings.Split(string(output), "\n") {
		if strings.HasPrefix(line, "Pages:") {
			n, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "Pages:")))
			if err == nil && n > 0 {
				return n, nil
			}
		}
	}
	return 0, fmt.Errorf("не удалось определить число страниц PDF")
}

// One receipt per message is the current ledger contract. Never present a
// first-page vision result as a complete reading of a multi-page attachment.
func (b *Bot) holdUnverifiedPDF(ctx context.Context, chat types.JID, sender, waID string, rawID int, data []byte, ext string, quiet bool) bool {
	if ext != ".pdf" || len(data) == 0 {
		return false
	}
	pages, err := pdfPageCount(ctx, data)
	if err == nil && pages == 1 {
		return false
	}
	reason := "pdf_pages_unknown"
	text := "Не смог проверить страницы PDF. Оставил файл на проверке, сумму автоматически не засчитываю. Пришлите чек отдельным фото или одностраничным PDF."
	if pages > 1 {
		reason = "pdf_multiple_pages"
		text = fmt.Sprintf("В PDF %d страниц. Чтобы не посчитать только первую страницу или смешать чеки, оставил файл на проверке. Пришлите каждый чек отдельным фото или одностраничным PDF.", pages)
	}
	changed, saveErr := b.db.HoldMediaForReview(ctx, rawID, reason)
	if saveErr != nil {
		fmt.Println("PDF: не удалось сохранить причину проверки:", saveErr)
		return true
	}
	if changed && !quiet && !b.groupSilent(chat) {
		b.sendReply(chat, "📎 "+text, waID, sender)
	}
	return true
}
