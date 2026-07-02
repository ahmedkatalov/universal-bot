// Package ocr распознаёт текст на фото чеков через Yandex Vision OCR API.
//
// Как получить доступ (см. подробную инструкцию в README.md):
//  1. Завести аккаунт на https://cloud.yandex.ru
//  2. Создать платёжный аккаунт (привязать карту)
//  3. Создать каталог (folder) и сервисный аккаунт с ролью ai.vision.user
//  4. Получить API-ключ сервисного аккаунта
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
	"strings"
	"time"
)

const visionURL = "https://vision.api.cloud.yandex.net/vision/v1/batchAnalyze"

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

type analyzeRequest struct {
	FolderID string       `json:"folderId"`
	Analyze  []analyzeJob `json:"analyzeSpecs"`
}

type analyzeJob struct {
	Content  string       `json:"content"` // base64 изображения
	Features []featureReq `json:"features"`
}

type featureReq struct {
	Type       string         `json:"type"`
	TextConfig map[string]any `json:"textDetectionConfig,omitempty"`
}

// ExtractText отправляет изображение (JPEG/PNG байты) в Yandex Vision и
// возвращает распознанный текст одной строкой (с переносами строк как в оригинале).
func (c *Client) ExtractText(ctx context.Context, imageBytes []byte) (string, error) {
	if c.apiKey == "" || c.folderID == "" {
		return "", fmt.Errorf("OCR не настроен: не заданы YANDEX_OCR_API_KEY / YANDEX_FOLDER_ID")
	}

	reqBody := analyzeRequest{
		FolderID: c.folderID,
		Analyze: []analyzeJob{
			{
				Content: base64.StdEncoding.EncodeToString(imageBytes),
				Features: []featureReq{
					{
						Type: "TEXT_DETECTION",
						TextConfig: map[string]any{
							"languageCodes": []string{"ru", "en"},
						},
					},
				},
			},
		},
	}

	payload, err := json.Marshal(reqBody)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, visionURL, bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Api-Key "+c.apiKey)

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

// Структуры ответа Yandex Vision (упрощённые, только то, что нужно для извлечения текста).
type visionResponse struct {
	Results []struct {
		Results []struct {
			TextDetection struct {
				Pages []struct {
					Blocks []struct {
						Lines []struct {
							Words []struct {
								Text string `json:"text"`
							} `json:"words"`
						} `json:"lines"`
					} `json:"blocks"`
				} `json:"pages"`
			} `json:"textDetection"`
		} `json:"results"`
	} `json:"results"`
}

func parseVisionResponse(body []byte) (string, error) {
	var resp visionResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return "", fmt.Errorf("не удалось разобрать ответ Vision: %w", err)
	}

	var sb strings.Builder
	for _, r := range resp.Results {
		for _, rr := range r.Results {
			for _, page := range rr.TextDetection.Pages {
				for _, block := range page.Blocks {
					for _, line := range block.Lines {
						var words []string
						for _, w := range line.Words {
							words = append(words, w.Text)
						}
						sb.WriteString(strings.Join(words, " "))
						sb.WriteString("\n")
					}
				}
			}
		}
	}
	return sb.String(), nil
}
