package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ScheduledJob — задание планировщика (напоминание/сообщение).
type ScheduledJob struct {
	ID          int
	Kind        string // once | daily | weekly
	TargetKind  string // me | here | group | person (для показа)
	Target      string // JID получателя
	TargetLabel string
	Message     string
	NextRunAt   time.Time
	AtHour      int
	AtMinute    int
	Weekday     int // 0=вс..6=сб (для weekly), иначе -1
	Active      bool
	LastRunAt   *time.Time
}

// nextDailyRun — ближайшее время hour:minute СТРОГО после now (сегодня или завтра).
func nextDailyRun(now time.Time, hour, minute int) time.Time {
	y, m, d := now.Date()
	cand := time.Date(y, m, d, hour, minute, 0, 0, now.Location())
	if !cand.After(now) {
		cand = cand.AddDate(0, 0, 1)
	}
	return cand
}

// nextWeeklyRun — ближайшее weekday (0=вс..6=сб) в hour:minute СТРОГО после now.
func nextWeeklyRun(now time.Time, weekday, hour, minute int) time.Time {
	y, m, d := now.Date()
	cand := time.Date(y, m, d, hour, minute, 0, 0, now.Location())
	daysAhead := (weekday - int(cand.Weekday()) + 7) % 7
	cand = cand.AddDate(0, 0, daysAhead)
	if !cand.After(now) {
		cand = cand.AddDate(0, 0, 7)
	}
	return cand
}

// firstRun вычисляет первый срок запуска для нового задания.
func firstRun(now time.Time, kind string, atHour, atMinute, weekday int, onceAt time.Time) (time.Time, error) {
	switch kind {
	case "once":
		if !onceAt.After(now) {
			return time.Time{}, fmt.Errorf("время разового напоминания уже прошло")
		}
		return onceAt, nil
	case "daily":
		return nextDailyRun(now, atHour, atMinute), nil
	case "weekly":
		return nextWeeklyRun(now, weekday, atHour, atMinute), nil
	default:
		return time.Time{}, fmt.Errorf("неизвестный тип задания %q", kind)
	}
}

// CreateScheduledJob создаёт задание и считает его первый срок запуска.
func (d *DB) CreateScheduledJob(ctx context.Context, createdBy, kind, targetKind, target, targetLabel, message string, atHour, atMinute, weekday int, onceAt time.Time) (ScheduledJob, error) {
	now := time.Now()
	next, err := firstRun(now, kind, atHour, atMinute, weekday, onceAt)
	if err != nil {
		return ScheduledJob{}, err
	}
	var id int
	if err := d.pool.QueryRow(ctx, `
		INSERT INTO scheduled_jobs (created_by, kind, target_kind, target, target_label, message, next_run_at, at_hour, at_minute, weekday)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
		RETURNING id
	`, createdBy, kind, targetKind, target, targetLabel, message, next, atHour, atMinute, weekday).Scan(&id); err != nil {
		return ScheduledJob{}, err
	}
	return ScheduledJob{ID: id, Kind: kind, TargetKind: targetKind, Target: target, TargetLabel: targetLabel,
		Message: message, NextRunAt: next, AtHour: atHour, AtMinute: atMinute, Weekday: weekday, Active: true}, nil
}

// ClaimDueJobs атомарно забирает задания, которым пора сработать (next_run_at <=
// now), и СРАЗУ сдвигает их срок (для повторяющихся — на следующий; разовые
// деактивирует). Сдвиг происходит в той же транзакции ДО возврата, поэтому после
// перезапуска одно и то же задание не уходит дважды. Возвращает, что отправить.
func (d *DB) ClaimDueJobs(ctx context.Context, now time.Time) ([]ScheduledJob, error) {
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	rows, err := tx.Query(ctx, `
		SELECT id, kind, target_kind, target, target_label, message, next_run_at, at_hour, at_minute, weekday
		FROM scheduled_jobs
		WHERE active = true AND next_run_at <= $1::timestamptz
		ORDER BY next_run_at ASC
		FOR UPDATE SKIP LOCKED
		LIMIT 50
	`, now)
	if err != nil {
		return nil, err
	}
	var jobs []ScheduledJob
	for rows.Next() {
		var j ScheduledJob
		if err := rows.Scan(&j.ID, &j.Kind, &j.TargetKind, &j.Target, &j.TargetLabel, &j.Message,
			&j.NextRunAt, &j.AtHour, &j.AtMinute, &j.Weekday); err != nil {
			rows.Close()
			return nil, err
		}
		jobs = append(jobs, j)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	for i := range jobs {
		j := &jobs[i]
		switch j.Kind {
		case "daily":
			next := nextDailyRun(now, j.AtHour, j.AtMinute)
			_, err = tx.Exec(ctx, `UPDATE scheduled_jobs SET next_run_at=$2, last_run_at=$3 WHERE id=$1`, j.ID, next, now)
		case "weekly":
			next := nextWeeklyRun(now, j.Weekday, j.AtHour, j.AtMinute)
			_, err = tx.Exec(ctx, `UPDATE scheduled_jobs SET next_run_at=$2, last_run_at=$3 WHERE id=$1`, j.ID, next, now)
		default: // once или неизвестный — выполняем один раз и деактивируем
			_, err = tx.Exec(ctx, `UPDATE scheduled_jobs SET active=false, last_run_at=$2 WHERE id=$1`, j.ID, now)
		}
		if err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return jobs, nil
}

// ListScheduledJobs возвращает активные задания (все — владелец один).
func (d *DB) ListScheduledJobs(ctx context.Context, limit int) ([]ScheduledJob, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := d.pool.Query(ctx, `
		SELECT id, kind, target_kind, target, target_label, message, next_run_at, at_hour, at_minute, weekday, last_run_at
		FROM scheduled_jobs
		WHERE active = true
		ORDER BY next_run_at ASC
		LIMIT $1
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ScheduledJob
	for rows.Next() {
		var j ScheduledJob
		j.Active = true
		if err := rows.Scan(&j.ID, &j.Kind, &j.TargetKind, &j.Target, &j.TargetLabel, &j.Message,
			&j.NextRunAt, &j.AtHour, &j.AtMinute, &j.Weekday, &j.LastRunAt); err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// CancelScheduledJob деактивирует задание по id. found=false, если такого нет.
func (d *DB) CancelScheduledJob(ctx context.Context, id int) (label string, found bool, err error) {
	err = d.pool.QueryRow(ctx, `
		UPDATE scheduled_jobs SET active = false
		WHERE id = $1 AND active = true
		RETURNING COALESCE(NULLIF(target_label,''), message)
	`, id).Scan(&label)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return label, true, nil
}
