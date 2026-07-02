package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

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

func main() {
	ctx := context.Background()

	pgConn := mustEnv("DATABASE_URL")         // например: postgres://user:pass@localhost:5432/finance
	groupJID := mustEnv("WHATSAPP_GROUP_JID") // например: 120363000000000000@g.us
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

	b, err := bot.New(ctx, sessionPath, database, aliasMap, ocrClient, groupJID, fontDir, reportDir)
	if err != nil {
		log.Fatalf("не удалось создать бота: %v", err)
	}

	if err := b.Connect(ctx); err != nil {
		log.Fatalf("не удалось подключиться к WhatsApp: %v", err)
	}
	defer b.Disconnect()

	log.Println("Бот запущен. Ждём сообщения из группы", groupJID)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh
	log.Println("Остановка бота...")
}
