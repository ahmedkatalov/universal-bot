package bot

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"whatsapp-bot/internal/db"
)

// GroupSpoolStatus describes events not yet committed to PostgreSQL. Unknown
// files are kept for inspection, never discarded or reported as money.
type GroupSpoolStatus struct {
	Pending, Unreadable int
	Oldest              time.Time
}

func (b *Bot) LocalGroupSpoolStatus(from, to time.Time, groups []string) (GroupSpoolStatus, error) {
	b.inboxCaptureMu.Lock()
	defer b.inboxCaptureMu.Unlock()
	var result GroupSpoolStatus
	files, err := b.groupSpoolFiles()
	if err != nil {
		return result, err
	}
	for _, path := range files {
		e, err := readGroupSpool(path)
		if err != nil {
			result.Unreadable++
			continue
		}
		if !from.IsZero() && e.ReceivedAt.Before(from) {
			continue
		}
		if !to.IsZero() && !e.ReceivedAt.Before(to) {
			continue
		}
		if len(groups) > 0 {
			matches := false
			for _, g := range groups {
				if g == e.GroupJID {
					matches = true
					break
				}
			}
			if !matches {
				continue
			}
		}
		result.Pending++
		if result.Oldest.IsZero() || e.ReceivedAt.Before(result.Oldest) {
			result.Oldest = e.ReceivedAt
		}
	}
	return result, nil
}

func (b *Bot) groupSpoolFiles() ([]string, error) {
	if b.inboxSpoolDir == "" {
		return nil, nil
	}
	entries, err := os.ReadDir(b.inboxSpoolDir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var files []string
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		files = append(files, filepath.Join(b.inboxSpoolDir, entry.Name()))
	}
	sort.Strings(files)
	return files, nil
}

func readGroupSpool(path string) (db.GroupInboxEvent, error) {
	var e db.GroupInboxEvent
	data, err := os.ReadFile(path)
	if err != nil {
		return e, err
	}
	if err = json.Unmarshal(data, &e); err != nil {
		return e, err
	}
	if len(e.Key) != 64 || e.GroupJID == "" || e.WaID == "" || !json.Valid(e.Metadata) || len(e.Payload) == 0 {
		return e, fmt.Errorf("повреждённая запись журнала %s", filepath.Base(path))
	}
	hash := sha256.Sum256(append([]byte(e.GroupJID+"|"+e.WaID+"|"+e.Kind+"|"), e.Payload...))
	if hex.EncodeToString(hash[:]) != e.Key {
		return e, fmt.Errorf("контрольная сумма журнала %s не совпала", filepath.Base(path))
	}
	return e, nil
}

func syncSpoolDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

// Called under inboxCaptureMu. The filename is monotonically ordered even
// when the machine clock moves backwards or the process is restarted.
func (b *Bot) persistGroupSpool(e db.GroupInboxEvent) error {
	if err := os.MkdirAll(b.inboxSpoolDir, 0700); err != nil {
		return err
	}
	files, err := b.groupSpoolFiles()
	if err != nil {
		return err
	}
	for _, path := range files {
		name := filepath.Base(path)
		if strings.HasSuffix(name, "-"+e.Key+".json") {
			if existing, err := readGroupSpool(path); err == nil && existing.Key == e.Key {
				return nil
			}
		}
		if len(name) >= 20 {
			if seq, err := strconv.ParseInt(name[:20], 10, 64); err == nil && seq > b.inboxSpoolSeq {
				b.inboxSpoolSeq = seq
			}
		}
	}
	seq := time.Now().UnixNano()
	if seq <= b.inboxSpoolSeq {
		seq = b.inboxSpoolSeq + 1
	}
	b.inboxSpoolSeq = seq
	data, err := json.Marshal(e)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(b.inboxSpoolDir, ".writing-")
	if err != nil {
		return err
	}
	tmp := file.Name()
	defer os.Remove(tmp)
	if err := file.Chmod(0600); err != nil {
		file.Close()
		return err
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	path := filepath.Join(b.inboxSpoolDir, fmt.Sprintf("%020d-%s.json", seq, e.Key))
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	return syncSpoolDirectory(b.inboxSpoolDir)
}

// The local record is removed only AFTER PostgreSQL acknowledges commit.
// A timeout after commit is harmless: the retained record is deduplicated
// against the same event_key on the next attempt.
type capturedSpoolEvent struct {
	event    db.GroupInboxEvent
	inserted bool
}

func (b *Bot) flushGroupSpoolLocked(ctx context.Context) ([]capturedSpoolEvent, error) {
	var ready []capturedSpoolEvent
	files, err := b.groupSpoolFiles()
	if err != nil {
		return ready, err
	}
	for _, path := range files {
		e, err := readGroupSpool(path)
		if err != nil {
			fmt.Println("Журнал группы: повреждённая локальная запись оставлена для проверки:", err)
			continue
		}
		writeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		saved, inserted, err := b.db.CaptureGroupEvent(writeCtx, e)
		cancel()
		if err != nil {
			return ready, err
		}
		ready = append(ready, capturedSpoolEvent{saved, inserted})
		if err := os.Remove(path); err != nil {
			return ready, err
		}
		if err := syncSpoolDirectory(b.inboxSpoolDir); err != nil {
			return ready, err
		}
	}
	return ready, nil
}

func (b *Bot) dispatchCapturedGroupEvent(saved db.GroupInboxEvent, inserted bool) {
	if !inserted && saved.State != "pending" {
		return
	}
	if saved.Command {
		go b.runGroupEvent(saved.ID)
		return
	}
	b.enqueueChat(saved.GroupJID, func() { b.runGroupEvent(saved.ID) })
}
