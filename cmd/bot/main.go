package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"whatsapp-bot/internal/ai"
	"whatsapp-bot/internal/bot"
	"whatsapp-bot/internal/db"
	"whatsapp-bot/internal/ocr"
	"whatsapp-bot/internal/parser"
)

func mustEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		log.Fatalf("переменная окружения %s обязательна", key)
	}
	return v
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// parseGroupJIDs разбирает список JID групп через запятую. Пустая строка
// означает "без ограничений" — бот учитывает все группы, в которых состоит
// его номер.
func parseGroupJIDs(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var out []string
	for _, p := range strings.Split(raw, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func main() {
	ctx := context.Background()

	pgConn := mustEnv("DATABASE_URL") // например: postgres://user:pass@localhost:5432/finance
	// WHATSAPP_GROUP_JIDS — необязательный список групп через запятую
	// (120363...1@g.us,120363...2@g.us). Не задан — бот учитывает ВСЕ группы,
	// в которых состоит номер. WHATSAPP_GROUP_JID оставлен для обратной
	// совместимости со старыми .env (один JID).
	groupJIDs := parseGroupJIDs(envOr("WHATSAPP_GROUP_JIDS", os.Getenv("WHATSAPP_GROUP_JID")))
	sessionPath := envOr("SESSION_DB_PATH", "./data/session.db")
	fontDir := envOr("FONT_DIR", "./assets/fonts")
	reportDir := envOr("REPORT_DIR", "./data/reports")

	os.MkdirAll("./data", 0o755)
	os.MkdirAll(reportDir, 0o755)

	database, err := db.Connect(ctx, pgConn)
	if err != nil {
		log.Fatalf("не удалось подключиться к БД: %v", err)
	}
	defer database.Close()

	aliasMap := parser.NewAliasMap()
	if extra, err := database.LoadAliases(ctx); err == nil {
		for alias, canonical := range extra {
			aliasMap.Add(alias, canonical)
		}
	} else {
		log.Printf("предупреждение: не удалось загрузить алиасы из БД: %v", err)
	}

	ocrClient, err := ocr.NewFromEnv()
	if err != nil {
		log.Fatalf("не удалось настроить OCR: %v", err)
	}

	// Личный ассистент идёт через OpenRouter (OpenAI-совместимый API).
	// OPENAI_API_KEY/OPENAI_MODEL поддержаны как синонимы для удобства,
	// если переменные уже заведены в таком виде в другом окружении.
	var assistant *ai.Assistant
	apiKey := envOr("OPENROUTER_API_KEY", os.Getenv("OPENAI_API_KEY"))
	if apiKey != "" {
		model := envOr("OPENROUTER_MODEL", os.Getenv("OPENAI_MODEL"))
		baseURL := envOr("OPENROUTER_BASE_URL", "")
		assistant = ai.New(apiKey, model, baseURL)
		log.Println("Личный ассистент (OpenRouter) включён — отвечаю на сообщения в личку номеру бота")
	} else {
		log.Println("OPENROUTER_API_KEY не задан — бот не будет отвечать в личных сообщениях")
	}

	b, err := bot.New(ctx, sessionPath, database, aliasMap, ocrClient, assistant, groupJIDs, fontDir, reportDir)
	if err != nil {
		log.Fatalf("не удалось создать бота: %v", err)
	}

	if err := b.Connect(ctx); err != nil {
		log.Fatalf("не удалось подключиться к WhatsApp: %v", err)
	}
	defer b.Disconnect()

	if len(groupJIDs) == 0 {
		log.Println("Бот запущен. Учитываю ВСЕ группы, в которых состоит номер")
	} else {
		log.Println("Бот запущен. Учитываю группы:", strings.Join(groupJIDs, ", "))
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh
	log.Println("Остановка бота...")
}
