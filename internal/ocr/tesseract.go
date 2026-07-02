// Бесплатная альтернатива Yandex Vision — локальный Tesseract OCR.
// Точность на кривых/тёмных фото чеков заметно хуже облачных сервисов,
// но не требует платной подписки. Требует установленного пакета tesseract-ocr
// с русским языковым модулем (tesseract-ocr-rus) на сервере/в контейнере.
package ocr

import (
	"context"
	"fmt"
	"os"
	"os/exec"
)

type TesseractClient struct{}

func NewTesseractClient() *TesseractClient {
	return &TesseractClient{}
}

// ExtractText сохраняет байты во временный файл и прогоняет через `tesseract`.
func (t *TesseractClient) ExtractText(ctx context.Context, imageBytes []byte) (string, error) {
	tmpImg, err := os.CreateTemp("", "receipt-*.jpg")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmpImg.Name())

	if _, err := tmpImg.Write(imageBytes); err != nil {
		tmpImg.Close()
		return "", err
	}
	tmpImg.Close()

	outBase := tmpImg.Name() + "_out"
	defer os.Remove(outBase + ".txt")

	cmd := exec.CommandContext(ctx, "tesseract", tmpImg.Name(), outBase, "-l", "rus+eng")
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("tesseract: %w (%s)", err, string(out))
	}

	data, err := os.ReadFile(outBase + ".txt")
	if err != nil {
		return "", err
	}
	return string(data), nil
}
