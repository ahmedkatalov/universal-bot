// Извлечение текста из PDF-чеков. Банки (Сбер, ВТБ, РСХБ, Т-Банк) отдают
// чеки PDF-файлами с настоящим текстовым слоем — его вытаскивает pdftotext
// (poppler-utils, есть в Docker-образе). Если текстового слоя нет
// (отсканированный PDF), первая страница рендерится в PNG через pdftoppm
// и распознаётся обычным OCR.
package bot

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// extractPDFText возвращает текст из PDF-чека: сначала текстовый слой,
// при его отсутствии — OCR первой страницы.
func (b *Bot) extractPDFText(ctx context.Context, data []byte) (string, error) {
	tmpDir, err := os.MkdirTemp("", "receipt-pdf-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmpDir)

	pdfPath := filepath.Join(tmpDir, "receipt.pdf")
	if err := os.WriteFile(pdfPath, data, 0o644); err != nil {
		return "", err
	}

	// -layout сохраняет расположение строк, что важно для парсера полей чека.
	out, err := exec.CommandContext(ctx, "pdftotext", "-layout", pdfPath, "-").Output()
	if err == nil && strings.TrimSpace(string(out)) != "" {
		return string(out), nil
	}
	if err != nil {
		fmt.Println("pdftotext не отработал (возможно, скан без текстового слоя):", err)
	}

	// Фолбэк: первая страница -> PNG -> OCR.
	imgBytes, err := renderPDFFirstPage(ctx, data)
	if err != nil {
		return "", err
	}
	return b.ocr.ExtractText(ctx, imgBytes)
}

// renderPDFFirstPage рендерит первую страницу PDF в PNG (pdftoppm) —
// используется и для OCR сканов, и чтобы показать чек ИИ "глазами".
func renderPDFFirstPage(ctx context.Context, data []byte) ([]byte, error) {
	tmpDir, err := os.MkdirTemp("", "receipt-render-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmpDir)

	pdfPath := filepath.Join(tmpDir, "receipt.pdf")
	if err := os.WriteFile(pdfPath, data, 0o644); err != nil {
		return nil, err
	}
	pngBase := filepath.Join(tmpDir, "page")
	if err := exec.CommandContext(ctx, "pdftoppm", "-png", "-r", "200", "-f", "1", "-l", "1", pdfPath, pngBase).Run(); err != nil {
		return nil, fmt.Errorf("pdftoppm: %w", err)
	}
	// pdftoppm называет файл page-1.png (или page-01.png в старых версиях).
	matches, _ := filepath.Glob(pngBase + "*.png")
	if len(matches) == 0 {
		return nil, fmt.Errorf("pdftoppm не создал изображение")
	}
	return os.ReadFile(matches[0])
}

// isPDFDocument определяет, что документ-вложение — это PDF.
func isPDFDocument(mimetype, fileName string) bool {
	return strings.Contains(strings.ToLower(mimetype), "pdf") ||
		strings.HasSuffix(strings.ToLower(fileName), ".pdf")
}
