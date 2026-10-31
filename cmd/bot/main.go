package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

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
	checkAI := flag.Bool("check-ai", false, "проверить ИИ без подключения к WhatsApp и БД")
	flag.Parse()
	if *checkAI {
		a, err := ai.NewFromEnv()
		if err != nil {
			log.Fatal(err)
		}
		if a == nil {
			log.Fatal("задайте ODIROUTER_API_KEY в .env/окружении")
		}
		log.Println("Проверка ИИ:", a.Description())
		checkCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
		defer cancel()
		if err := a.CheckConnection(checkCtx); err != nil {
			log.Fatal("Проверка ИИ не пройдена: ", err)
		}
		fmt.Println("ИИ готов: JSON, изображения и вызов инструмента проверены.")
		return
	}

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

	assistant, err := ai.NewFromEnv()
	if err != nil {
		log.Fatalf("неверные настройки ИИ: %v", err)
	}
	if assistant != nil {
		log.Println("Личный ассистент включён:", assistant.Description())

		// Самодиагностика на старте: реально ли отвечают модель и «зрение».
		// Частая причина «бот тупит / не распознаёт чеки» — неверный id модели,
		// протухший ключ или нулевой баланс ИИ-сервиса: тогда чтение чеков молча
		// падает на слабый OCR. Пусть это видно в логах сразу, не блокируя старт.
		go func(a *ai.Assistant) {
			ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
			if err := a.Ping(ctx); err != nil {
				log.Printf("⚠️ ПРОВЕРКА МОДЕЛИ: ИИ не отвечает: %v — проверь модель, ключ и баланс сервиса", err)
			} else {
				log.Println("Проверка модели: основная модель отвечает ✔")
			}
			cancel()
			ctx, cancel = context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			if err := a.PingVision(ctx); err != nil {
				log.Printf("⚠️ ПРОВЕРКА ЗРЕНИЯ: модель чтения чеков НЕ отвечает: %v — чеки будут распознаваться ПЛОХО (падение на OCR). Проверь OPENROUTER_VISION_MODEL / OPENROUTER_MODEL", err)
			} else {
				log.Println("Проверка зрения: модель чтения чеков (фото) отвечает ✔")
			}
		}(assistant)
	} else {
		log.Println("ODIROUTER_API_KEY/OPENROUTER_API_KEY не задан — бот не будет отвечать в личных сообщениях")
	}

	// Интеграция с программой рассрочек (cmf): сверка чеков с внесёнными
	// платежами. Выключена, если CMF_API_URL/CMF_EMAIL/CMF_PASSWORD не заданы.
	cmfClient := cmf.NewFromEnv()
	if cmfClient != nil {
		log.Println("Сверка с программой рассрочек (cmf) включена — проверяю связь...")
		// Реальная проверка: вход + пробный поиск. Раньше «включена» значило лишь
		// «переменные заданы», а на деле каждый запрос мог падать (неверный адрес/
		// пароль) — и это всплывало только как «ошибка поиска» по каждому чеку.
		go func(c *cmf.Client) {
			pctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			defer cancel()
			if err := c.Ping(pctx); err != nil {
				log.Printf("⚠️ ПРОВЕРКА CMF: программа НЕ отвечает как надо — %s. Детали: %v", cmf.Human(err), err)
			} else {
				log.Println("Проверка CMF: вход и поиск клиентов работают ✔")
			}
		}(cmfClient)
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
