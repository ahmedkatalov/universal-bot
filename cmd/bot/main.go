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
	"whatsapp-bot/internal/cmf"
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
	botName := envOr("BOT_NAME", "Джарвис") // обращение в группах: "Джарвис скинь отчет"
	// OWNER_NUMBERS — необязательный список номеров через запятую
	// (79370000000,79280000000): только они могут писать боту в личку
	// (ассистент, дозагрузка чеков). Пусто — личка открыта всем.
	ownerNumbers := parseGroupJIDs(os.Getenv("OWNER_NUMBERS"))
	// REPORT_ADMINS — кому доступна отчётность (суммы, сборы, отчёты,
	// корректировки). Остальным бот вежливо отказывает по этим вопросам,
	// но общается и ищет конкретные чеки. Пусто — доступно всем.
	reportAdmins := parseGroupJIDs(envOr("REPORT_ADMINS", "89287836800,89899171578"))
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

	// Повторно сопоставляем чеки, зависшие на ручной проверке: логика
	// алиасов могла улучшиться (например, теперь понимаем полные ФИО
	// "Милана Нажудовна К."), или владелец добавил новые алиасы в БД —
	// такие чеки автоматически возвращаются в общий учёт.
	if unresolved, err := database.UnresolvedReceipts(ctx); err != nil {
		log.Printf("предупреждение: не удалось загрузить чеки на проверке: %v", err)
	} else {
		fixed := 0
		for _, r := range unresolved {
			canonical, matched := aliasMap.ResolveName(r.RecipientRaw)
			if !matched {
				continue
			}
			contactID, err := database.GetOrCreateContact(ctx, canonical)
			if err != nil {
				log.Printf("предупреждение: контакт для чека %d: %v", r.ID, err)
				continue
			}
			if err := database.AssignReceiptContact(ctx, r.ID, contactID); err != nil {
				log.Printf("предупреждение: привязка чека %d: %v", r.ID, err)
				continue
			}
			fixed++
		}
		if fixed > 0 {
			log.Printf("Повторное сопоставление: %d чек(ов) с ручной проверки возвращены в учёт", fixed)
		}
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
		visionModel := os.Getenv("OPENROUTER_VISION_MODEL") // пусто => тем же, что и мозг
		baseURL := envOr("OPENROUTER_BASE_URL", "")
		assistant = ai.New(apiKey, model, visionModel, baseURL)
		log.Println("Личный ассистент (OpenRouter) включён — отвечаю на сообщения в личку номеру бота")
	} else {
		log.Println("OPENROUTER_API_KEY не задан — бот не будет отвечать в личных сообщениях")
	}

	// Интеграция с программой рассрочек (cmf): сверка чеков с внесёнными
	// платежами. Выключена, если CMF_API_URL/CMF_EMAIL/CMF_PASSWORD не заданы.
	cmfClient := cmf.NewFromEnv()
	if cmfClient != nil {
		log.Println("Сверка с программой рассрочек (cmf) включена")
	} else {
		log.Println("CMF_API_URL/CMF_EMAIL/CMF_PASSWORD не заданы — сверка с программой выключена")
	}

	b, err := bot.New(ctx, sessionPath, database, aliasMap, ocrClient, assistant, cmfClient, groupJIDs, ownerNumbers, reportAdmins, botName, fontDir, reportDir)
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
