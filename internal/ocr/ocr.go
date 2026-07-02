package ocr

import (
	"context"
	"fmt"
	"os"
)

// Extractor — общий интерфейс, чтобы бот не зависел от конкретного провайдера OCR.
type Extractor interface {
	ExtractText(ctx context.Context, imageBytes []byte) (string, error)
}

// NewFromEnv выбирает провайдера по переменной окружения OCR_PROVIDER
// ("yandex" или "tesseract"). По умолчанию — "tesseract" (бесплатный),
// чтобы бот работал из коробки без платной подписки.
func NewFromEnv() (Extractor, error) {
	provider := os.Getenv("OCR_PROVIDER")
	if provider == "" {
		provider = "tesseract"
	}

	switch provider {
	case "yandex":
		apiKey := os.Getenv("YANDEX_OCR_API_KEY")
		folderID := os.Getenv("YANDEX_FOLDER_ID")
		if apiKey == "" || folderID == "" {
			return nil, fmt.Errorf("OCR_PROVIDER=yandex, но не заданы YANDEX_OCR_API_KEY / YANDEX_FOLDER_ID")
		}
		return NewClient(apiKey, folderID), nil
	case "tesseract":
		return NewTesseractClient(), nil
	default:
		return nil, fmt.Errorf("неизвестный OCR_PROVIDER: %s (допустимо: yandex, tesseract)", provider)
	}
}
