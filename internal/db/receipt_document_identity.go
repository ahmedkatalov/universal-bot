package db

import "whatsapp-bot/internal/ledger"

// Keep live/retry duplicate checks consistent with ledger.NormDoc without
// rewriting the original OCR evidence stored in the receipt.
var receiptIdentityCTE = `WITH receipt_identity AS (
	SELECT br.*, ` + receiptDocumentSQL("br.doc_number") + ` AS document_key,
	 ` + receiptDocumentSQL("br.auth_code") + ` AS authorization_key
	FROM bank_receipts br
) `

// PostgreSQL's upper() under the C locale only folds ASCII. Fold Cyrillic
// explicitly before replacing its Latin lookalikes, matching Go in every DB.
// value is an internal SQL expression, never supplied by a user.
func receiptDocumentSQL(value string) string {
	return `regexp_replace(translate(upper(translate(COALESCE(` + value + `,''), 'абвгдеёжзийклмнопрстуфхцчшщъыьэюя', 'АБВГДЕЁЖЗИЙКЛМНОПРСТУФХЦЧШЩЪЫЬЭЮЯ')), 'АВСЕЁНКМОРТХУ', 'ABCEEHKMOPTXY'), '[^0-9A-ZА-Я]', '', 'g')`
}

func receiptDocumentKey(raw string) string { return ledger.NormDoc(raw) }
