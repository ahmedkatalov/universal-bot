// Package ocr распознаёт текст на фото чеков через Yandex Vision OCR API
// (актуальный эндпоинт https://ai.api.cloud.yandex.net/ocr/v1/recognizeText).
//
// Как получить доступ (см. подробную инструкцию в README.md):
//  1. Завести аккаунт на https://console.yandex.cloud
//  2. Создать платёжный аккаунт (привязать карту)
//  3. Создать каталог (folder) и сервисный аккаунт с ролью ai.vision.user
//  4. Получить API-ключ сервисного аккаунта
//     (область действия yc.ai.foundationModels.execute)
//  5. Прописать YANDEX_OCR_API_KEY и YANDEX_FOLDER_ID в переменные окружения бота
package ocr

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

const visionURL = "https://ai.api.cloud.yandex.net/ocr/v1/recognizeText"

type Client struct {
	apiKey   string
	folderID string
	http     *http.Client
}

func NewClient(apiKey, folderID string) *Client {
	return &Client{
		apiKey:   apiKey,
		folderID: folderID,
		http:     &http.Client{Timeout: 30 * time.Second},
	}
}

type recognizeRequest struct {
	MimeType      string   `json:"mimeType"`
	LanguageCodes []string `json:"languageCodes"`
	Model         string   `json:"model"`
	Content       string   `json:"content"` // base64 изображения
}

// detectMimeType определяет формат по магическим байтам: фото из WhatsApp —
// JPEG, а страницы PDF после pdftoppm — PNG.
func detectMimeType(data []byte) string {
	if len(data) >= 8 && bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n")) {
		return "PNG"
	}
	return "JPEG"
}

// ExtractText отправляет изображение (JPEG/PNG байты) в Yandex Vision OCR и
// возвращает распознанный текст одной строкой (с переносами строк как в оригинале).
func (c *Client) ExtractText(ctx context.Context, imageBytes []byte) (string, error) {
	if c.apiKey == "" || c.folderID == "" {
		return "", fmt.Errorf("OCR не настроен: не заданы YANDEX_OCR_API_KEY / YANDEX_FOLDER_ID")
	}

	payload, err := json.Marshal(recognizeRequest{
		MimeType:      detectMimeType(imageBytes),
		LanguageCodes: []string{"*"},
		Model:         "page",
		Content:       base64.StdEncoding.EncodeToString(imageBytes),
	})
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, visionURL, bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Api-Key "+c.apiKey)
	req.Header.Set("x-folder-id", c.folderID)

	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("Yandex Vision вернул %d: %s", resp.StatusCode, string(body))
	}

	return parseVisionResponse(body)
}

// Структура ответа recognizeText (упрощённая): полный распознанный текст
// приходит готовым в result.textAnnotation.fullText.
type visionResponse struct {
	Result struct {
		TextAnnotation struct {
			FullText string `json:"fullText"`
		} `json:"textAnnotation"`
	} `json:"result"`
}

func parseVisionResponse(body []byte) (string, error) {
	var resp visionResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return "", fmt.Errorf("не удалось разобрать ответ Vision: %w", err)
	}
	if resp.Result.TextAnnotation.FullText == "" {
		return "", fmt.Errorf("Vision не вернул текст (пустой fullText): %s", truncate(string(body), 300))
	}
	return resp.Result.TextAnnotation.FullText, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
