package db

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"go.mau.fi/whatsmeow/types"
)

const senderIdentitySchema = `CREATE TABLE IF NOT EXISTS whatsapp_sender_aliases (
	alias_jid TEXT PRIMARY KEY,
	actor_jid TEXT NOT NULL,
	verified_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_whatsapp_sender_actor ON whatsapp_sender_aliases(actor_jid);`

// sender_phone was already populated only from a real telephone JID. Preserve
// those observations when upgrading an installation that predates this table.
const senderIdentityBackfillSQL = `INSERT INTO whatsapp_sender_aliases(alias_jid,actor_jid)
	SELECT sender_jid,MIN(split_part(sender_phone,'@',1)||'@s.whatsapp.net') FROM raw_messages
	WHERE sender_jid ~ '^[0-9]+@lid$' AND sender_phone ~ '^[0-9]+(@s[.]whatsapp[.]net)?$'
	  AND split_part(sender_phone,'@',1)<>split_part(sender_jid,'@',1)
	GROUP BY sender_jid HAVING COUNT(DISTINCT split_part(sender_phone,'@',1))=1
	ON CONFLICT(alias_jid) DO NOTHING`

type SenderIdentity struct{ ActorJID, Phone string }

var ErrSenderIdentityConflict = errors.New("конфликт подтверждённого отправителя WhatsApp")

func parsedSenderJID(value string) (types.JID, bool) {
	j, err := types.ParseJID(strings.TrimSpace(value))
	if err != nil || j.User == "" || (j.Server != types.DefaultUserServer && j.Server != types.HiddenUserServer) {
		return types.JID{}, false
	}
	for _, r := range j.User {
		if r < '0' || r > '9' {
			return types.JID{}, false
		}
	}
	return j.ToNonAD(), true
}

// RememberSenderIdentity is called only with the Sender/SenderAlt pair supplied
// by WhatsApp. Display names and coincidentally equal LID digits are not proof.
func (d *DB) RememberSenderIdentity(ctx context.Context, sender, alternate string) error {
	if strings.TrimSpace(alternate) == "" {
		return nil
	}
	a, aok := parsedSenderJID(sender)
	b, bok := parsedSenderJID(alternate)
	if !aok || !bok {
		return fmt.Errorf("неверная пара идентификаторов WhatsApp")
	}
	if a == b {
		return nil
	}
	var lid, pn types.JID
	if a.Server == types.HiddenUserServer && b.Server == types.DefaultUserServer {
		lid, pn = a, b
	} else if b.Server == types.HiddenUserServer && a.Server == types.DefaultUserServer {
		lid, pn = b, a
	} else {
		return fmt.Errorf("неоднозначная пара идентификаторов WhatsApp")
	}
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, lid.String()); err != nil {
		return err
	}
	var conflict bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM raw_messages WHERE `+senderNonADSQL("sender_jid")+`=$1 AND NULLIF(sender_phone,'') IS NOT NULL AND split_part(sender_phone,'@',1)<>$2)`, lid.String(), pn.User).Scan(&conflict); err != nil {
		return err
	}
	if conflict {
		return ErrSenderIdentityConflict
	}
	var actor string
	err = tx.QueryRow(ctx, `INSERT INTO whatsapp_sender_aliases(alias_jid,actor_jid) VALUES($1,$2)
		ON CONFLICT(alias_jid) DO UPDATE SET verified_at=now() WHERE whatsapp_sender_aliases.actor_jid=EXCLUDED.actor_jid RETURNING actor_jid`, lid.String(), pn.String()).Scan(&actor)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrSenderIdentityConflict
	}
	if err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE raw_messages SET sender_phone=$2 WHERE `+senderNonADSQL("sender_jid")+`=$1 AND NULLIF(sender_phone,'') IS NULL`, lid.String(), pn.String()); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (d *DB) ResolveSenderIdentity(ctx context.Context, sender string) (actor, phone string, err error) {
	j, ok := parsedSenderJID(sender)
	if !ok {
		return strings.TrimSpace(sender), "", nil
	}
	if j.Server == types.DefaultUserServer {
		return j.String(), j.User, nil
	}
	var mapped string
	err = d.pool.QueryRow(ctx, `SELECT actor_jid FROM whatsapp_sender_aliases WHERE alias_jid=$1`, j.String()).Scan(&mapped)
	if errors.Is(err, pgx.ErrNoRows) {
		return j.String(), "", nil
	}
	if err != nil {
		return "", "", err
	}
	pn, ok := parsedSenderJID(mapped)
	if !ok || pn.Server != types.DefaultUserServer {
		return "", "", fmt.Errorf("повреждена привязка отправителя WhatsApp")
	}
	return pn.String(), pn.User, nil
}

func (d *DB) SenderIdentityAliases(ctx context.Context, sender string) ([]string, error) {
	actor, _, err := d.ResolveSenderIdentity(ctx, sender)
	if err != nil {
		return nil, err
	}
	rows, err := d.pool.Query(ctx, `SELECT alias_jid FROM whatsapp_sender_aliases WHERE actor_jid=$1`, actor)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	seen := map[string]bool{actor: true}
	for rows.Next() {
		var alias string
		if err := rows.Scan(&alias); err != nil {
			return nil, err
		}
		seen[alias] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var out []string
	for alias := range seen {
		out = append(out, alias)
	}
	sort.Strings(out)
	return out, nil
}

func (d *DB) RawSenderIdentities(ctx context.Context, ids []int) (map[int]SenderIdentity, error) {
	out := map[int]SenderIdentity{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := d.pool.Query(ctx, `SELECT rm.id,COALESCE(rm.sender_jid,''),COALESCE(rm.sender_phone,''),COALESCE(a.actor_jid,'')
		FROM raw_messages rm LEFT JOIN whatsapp_sender_aliases a ON a.alias_jid=`+senderNonADSQL("rm.sender_jid")+` WHERE rm.id=ANY($1::int[])`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int
		var sender, phone, mapped string
		if err := rows.Scan(&id, &sender, &phone, &mapped); err != nil {
			return nil, err
		}
		identity, err := decodeRawSenderIdentity(sender, phone, mapped)
		if err != nil {
			return nil, err
		}
		out[id] = identity
	}
	return out, rows.Err()
}

func decodeRawSenderIdentity(sender, phone, mapped string) (SenderIdentity, error) {
	if phone != "" && !strings.Contains(phone, "@") {
		phone += "@" + types.DefaultUserServer
	}
	pn, phoneOK := parsedSenderJID(phone)
	phoneOK = phoneOK && pn.Server == types.DefaultUserServer
	if mapped != "" {
		known, ok := parsedSenderJID(mapped)
		if !ok || known.Server != types.DefaultUserServer {
			return SenderIdentity{}, fmt.Errorf("повреждена привязка отправителя WhatsApp")
		}
		if phoneOK && known != pn {
			return SenderIdentity{}, ErrSenderIdentityConflict
		}
		pn, phoneOK = known, true
	}
	if phoneOK {
		return SenderIdentity{ActorJID: pn.String(), Phone: pn.User}, nil
	}
	j, ok := parsedSenderJID(sender)
	if !ok {
		return SenderIdentity{ActorJID: sender}, nil
	}
	if j.Server == types.DefaultUserServer {
		return SenderIdentity{ActorJID: j.String(), Phone: j.User}, nil
	}
	return SenderIdentity{ActorJID: j.String()}, nil
}

func (d *DB) RawSenderIdentity(ctx context.Context, id int) (actor, phone string, err error) {
	all, err := d.RawSenderIdentities(ctx, []int{id})
	if err != nil {
		return "", "", err
	}
	value, ok := all[id]
	if !ok {
		return "", "", pgx.ErrNoRows
	}
	return value.ActorJID, value.Phone, nil
}

// senderIdentitySQL compares full observed JIDs; unknown LIDs never match by
// numeric resemblance or display name. Arguments are internal SQL fragments.
func senderIdentitySQL(column, param string) string {
	canonical := func(value string) string {
		value = senderNonADSQL(value)
		return "COALESCE((SELECT actor_jid FROM whatsapp_sender_aliases WHERE alias_jid=" + value + ")," + value + ")"
	}
	return "(" + canonical(column) + "=" + canonical(param) + ")"
}

func senderNonADSQL(value string) string {
	return "regexp_replace(" + value + ", '[.:][0-9]+(:[0-9]+)?@', '@')"
}
