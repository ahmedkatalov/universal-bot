package db

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"
)

// testDB подключается к БД из TEST_DATABASE_URL и прогоняет миграции. Без этой
// переменной интеграционные тесты пропускаются (в CI без Postgres это норма).
// Пример: TEST_DATABASE_URL=postgres://financebot:pass@localhost:5433/finance go test ./internal/db/
func testDB(t *testing.T) *DB {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL не задан — пропускаю интеграционный тест БД")
	}
	d, err := Connect(context.Background(), url)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(func() { d.pool.Close() })
	return d
}

// uniqueScope даёт уникальные group/sender на прогон, чтобы тесты не мешали друг
// другу и реальным данным.
func uniqueScope(tag string) (string, string) {
	s := fmt.Sprintf("%s-%d", tag, time.Now().UnixNano())
	return "grp-" + s, "snd-" + s + "@x"
}

// TestPendingNameFIFO — главный сценарий владельца: имена приходят ПЕРЕД чеками,
// и чеки должны разобрать их в ТОМ ЖЕ порядке (name A, name B -> receipt A, B).
func TestPendingNameFIFO(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	grp, snd := uniqueScope("fifo")
	base := time.Now().Add(-3 * time.Minute)

	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	must(d.EnqueuePendingName(ctx, grp, snd, "Клиент A", 0, 0, base))
	must(d.EnqueuePendingName(ctx, grp, snd, "Клиент B", 0, 0, base.Add(time.Second)))

	since := time.Now().Add(-15 * time.Minute)
	n1, _, ok1, err := d.TakePendingName(ctx, grp, snd, since)
	must(err)
	n2, _, ok2, err := d.TakePendingName(ctx, grp, snd, since)
	must(err)
	_, _, ok3, err := d.TakePendingName(ctx, grp, snd, since)
	must(err)

	if !ok1 || n1 != "Клиент A" {
		t.Errorf("первым чеком должен уйти Клиент A, получили %q ok=%v", n1, ok1)
	}
	if !ok2 || n2 != "Клиент B" {
		t.Errorf("вторым чеком должен уйти Клиент B, получили %q ok=%v", n2, ok2)
	}
	if ok3 {
		t.Errorf("после двух заборов очередь должна опустеть")
	}
}

// TestPendingNameWindow — запись, ВСТАВЛЕННАЯ давно (created_at вне окна), не
// берётся; свежая — берётся. Окно считается по времени появления записи.
func TestPendingNameWindow(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	grp, snd := uniqueScope("window")

	// Явно вставляем запись с created_at 40 минут назад (вне окна 15 мин).
	old := time.Now().Add(-40 * time.Minute)
	if _, err := d.pool.Exec(ctx, `
		INSERT INTO pending_client_names (group_jid, sender_jid, name, amount, received_at, created_at)
		VALUES ($1,$2,$3,0,$4,$5)`, grp, snd, "Старое Имя", old, old); err != nil {
		t.Fatal(err)
	}
	since := time.Now().Add(-15 * time.Minute)
	if _, _, ok, _ := d.TakePendingName(ctx, grp, snd, since); ok {
		t.Errorf("запись, вставленная 40 мин назад (вне окна 15), не должна браться")
	}
	// Свежая запись берётся.
	if err := d.EnqueuePendingName(ctx, grp, snd, "Свежее Имя", 0, 0, time.Now().Add(-2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if n, _, ok, _ := d.TakePendingName(ctx, grp, snd, since); !ok || n != "Свежее Имя" {
		t.Errorf("свежая запись должна взяться, получили %q ok=%v", n, ok)
	}
}

// TestPendingNameBacklogEligible — регрессионный: имя с ОЧЕНЬ старым временем
// сообщения (офлайн-догрузка), но только что вставленное, ДОЛЖНО браться —
// окно по created_at, а не по received_at. Иначе после простоя ничего не парится.
func TestPendingNameBacklogEligible(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	grp, snd := uniqueScope("backlog")
	// received_at 30 мин назад (сообщение из офлайна), created_at = сейчас (вставка).
	if err := d.EnqueuePendingName(ctx, grp, snd, "Догруженное Имя", 0, 0, time.Now().Add(-30*time.Minute)); err != nil {
		t.Fatal(err)
	}
	since := time.Now().Add(-15 * time.Minute)
	if n, _, ok, _ := d.TakePendingName(ctx, grp, snd, since); !ok || n != "Догруженное Имя" {
		t.Errorf("догруженное имя (старое received_at, свежий created_at) должно браться, got %q ok=%v", n, ok)
	}
}

// TestPendingNameScope — очередь строго по group+sender: имя одного отправителя
// не должно «украсть» чеку другого сотрудника.
func TestPendingNameScope(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	grp, snd1 := uniqueScope("scope")
	snd2 := snd1 + "-other"
	now := time.Now().Add(-1 * time.Minute)
	since := time.Now().Add(-15 * time.Minute)

	if err := d.EnqueuePendingName(ctx, grp, snd1, "Имя Сотрудника1", 0, 0, now); err != nil {
		t.Fatal(err)
	}
	// Чек второго сотрудника не должен получить имя первого.
	if _, _, ok, _ := d.TakePendingName(ctx, grp, snd2, since); ok {
		t.Errorf("имя sender1 не должно уходить чеку sender2")
	}
	// Первый — получает.
	if n, _, ok, _ := d.TakePendingName(ctx, grp, snd1, since); !ok || n != "Имя Сотрудника1" {
		t.Errorf("sender1 должен получить своё имя, получили %q ok=%v", n, ok)
	}
}

// TestPendingNameTakeOnce — одно имя не отдаётся дважды (атомарный забор).
func TestPendingNameTakeOnce(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	grp, snd := uniqueScope("once")
	if err := d.EnqueuePendingName(ctx, grp, snd, "Единственный", 0, 0, time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	since := time.Now().Add(-15 * time.Minute)
	if _, _, ok, _ := d.TakePendingName(ctx, grp, snd, since); !ok {
		t.Fatalf("первый забор должен сработать")
	}
	if _, _, ok, _ := d.TakePendingName(ctx, grp, snd, since); ok {
		t.Errorf("второй забор того же имени не должен сработать (имя уже съедено)")
	}
}

// TestPendingNameWithAmount — для фото наличных берём только имя С СУММОЙ.
func TestPendingNameWithAmount(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	grp, snd := uniqueScope("amount")
	base := time.Now().Add(-2 * time.Minute)
	// Сначала имя БЕЗ суммы, потом с суммой — WithAmount должен пропустить первое.
	if err := d.EnqueuePendingName(ctx, grp, snd, "Без Суммы", 0, 0, base); err != nil {
		t.Fatal(err)
	}
	if err := d.EnqueuePendingName(ctx, grp, snd, "С Суммой", 22000, 0, base.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	since := time.Now().Add(-15 * time.Minute)
	n, amt, ok, _ := d.TakePendingNameWithAmount(ctx, grp, snd, since)
	if !ok || n != "С Суммой" || amt != 22000 {
		t.Errorf("WithAmount должен взять «С Суммой» 22000, получили %q %.0f ok=%v", n, amt, ok)
	}
}

// TestPendingNameSurvivesRestart — durable-очередь переживает «рестарт»: второе
// подключение к той же БД видит имена, положенные первым (нет in-memory потери).
func TestPendingNameSurvivesRestart(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	grp, snd := uniqueScope("restart")
	if err := d.EnqueuePendingName(ctx, grp, snd, "После Рестарта", 0, 0, time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	// «Рестарт»: новое подключение к той же БД.
	url := os.Getenv("TEST_DATABASE_URL")
	d2, err := Connect(ctx, url)
	if err != nil {
		t.Fatalf("повторный Connect: %v", err)
	}
	defer d2.pool.Close()
	since := time.Now().Add(-15 * time.Minute)
	if n, _, ok, _ := d2.TakePendingName(ctx, grp, snd, since); !ok || n != "После Рестарта" {
		t.Errorf("после рестарта имя должно сохраниться, получили %q ok=%v", n, ok)
	}
}
