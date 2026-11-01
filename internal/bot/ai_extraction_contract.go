package bot

import (
	"encoding/json"
	"regexp"
	"sort"
	"strings"
	"time"

	"whatsapp-bot/internal/ledger"
)

var receiptPendingRe = regexp.MustCompile(`(?i)в\s+обработке|ожида|обрабатыва|не\s+заверш|pending|processing|in\s+progress|awaiting`)

// A successful HTTP response is not an extraction verdict. The required
// collection must actually exist: {} must never mean "there were no payments".
// Optional legacy payment fields (cash, collector, note, card, clarify) retain
// their defaults, while malformed payment rows invalidate the whole reading.
func decodePaymentExtraction(out string) ([]aiPayment, string, bool) {
	block := extractJSONBlock(out)
	var fields map[string]json.RawMessage
	if json.Unmarshal([]byte(block), &fields) != nil || fields == nil {
		return nil, "", false
	}
	raw := strings.TrimSpace(string(fields["payments"]))
	if !strings.HasPrefix(raw, "[") {
		return nil, "", false
	}
	var parsed struct {
		Payments []aiPayment `json:"payments"`
		Clarify  string      `json:"clarify"`
	}
	if json.Unmarshal([]byte(block), &parsed) != nil {
		return nil, "", false
	}
	for _, p := range parsed.Payments {
		if strings.TrimSpace(p.Name) == "" || !validExtractedMoney(p.Amount, false) {
			return nil, "", false
		}
	}
	return parsed.Payments, strings.TrimSpace(parsed.Clarify), true
}

func decodeReceiptExtraction(out string) (aiReceipt, bool) {
	block := extractJSONBlock(out)
	var fields map[string]json.RawMessage
	var rec aiReceipt
	if json.Unmarshal([]byte(block), &fields) != nil || fields == nil || json.Unmarshal([]byte(block), &rec) != nil {
		return aiReceipt{}, false
	}
	switch rec.Kind {
	case "other":
		// Non-financial classification does not carry usable receipt fields.
		return aiReceipt{Kind: "other"}, true
	case "cash":
		// A photo of banknotes need not reveal an amount.
	case "receipt":
		amount, exists := fields["amount"]
		if !exists || strings.TrimSpace(string(amount)) == "null" {
			return aiReceipt{}, false
		}
	default:
		return aiReceipt{}, false
	}
	if !validExtractedMoney(rec.Amount, true) || !validExtractedMoney(rec.Commission, true) {
		return aiReceipt{}, false
	}
	if strings.TrimSpace(rec.Datetime) != "" {
		if _, ok, _ := parseAIDatetime(rec.Datetime); !ok {
			return aiReceipt{}, false
		}
	}
	return rec, true
}

// verifiedReceipt uses a quorum of the REQUESTED readings. One available
// response out of three is useful evidence, but cannot confirm money alone.
// The caller retains/retries the original message when this returns false.
func verifiedReceipt(recs []aiReceipt, quorum int) (aiReceipt, bool) {
	counts := map[float64]int{}
	for _, r := range recs {
		counts[r.Amount]++
	}
	var winner float64
	won := false
	for amount, votes := range counts {
		if votes >= quorum {
			winner, won = amount, true
			break
		}
	}
	if !won {
		return aiReceipt{}, false
	}
	var agreed []aiReceipt
	for _, r := range recs {
		if r.Amount == winner {
			agreed = append(agreed, r)
		}
	}
	out := pickConsensusReceipt(agreed)
	out.Kind, out.Amount = "receipt", winner
	var ok bool
	out.Datetime, ok = verifiedReceiptDate(agreed, quorum)
	if !ok {
		return aiReceipt{}, false
	}
	out.Status, ok = verifiedReceiptStatus(agreed, quorum)
	if !ok {
		return aiReceipt{}, false
	}
	// A differing operation number or recipient may mean readings described
	// different transfers. Do not assemble a synthetic receipt from them.
	out.DocNumber, ok = verifiedReceiptField(agreed, quorum, func(r aiReceipt) string { return r.DocNumber }, ledger.NormDoc)
	if !ok {
		return aiReceipt{}, false
	}
	out.AuthCode, ok = verifiedReceiptField(agreed, quorum, func(r aiReceipt) string { return r.AuthCode }, receiptFieldKey)
	if !ok {
		return aiReceipt{}, false
	}
	out.Recipient, ok = verifiedReceiptField(agreed, quorum, func(r aiReceipt) string { return r.Recipient }, receiptFieldKey)
	if !ok {
		return aiReceipt{}, false
	}
	// Weak auxiliary fields are cleared instead of copying a lone reading.
	out.Sender, _ = verifiedReceiptField(agreed, quorum, func(r aiReceipt) string { return r.Sender }, receiptFieldKey)
	out.Bank, _ = verifiedReceiptField(agreed, quorum, func(r aiReceipt) string { return r.Bank }, receiptFieldKey)
	out.RecipientBank, _ = verifiedReceiptField(agreed, quorum, func(r aiReceipt) string { return r.RecipientBank }, receiptFieldKey)
	out.SenderBank, _ = verifiedReceiptField(agreed, quorum, func(r aiReceipt) string { return r.SenderBank }, receiptFieldKey)
	out.SenderAccount, _ = verifiedReceiptField(agreed, quorum, func(r aiReceipt) string { return r.SenderAccount }, receiptFieldKey)
	out.RecipientPhone, _ = verifiedReceiptField(agreed, quorum, func(r aiReceipt) string { return r.RecipientPhone }, receiptFieldKey)
	if out.Amount <= 0 && strings.TrimSpace(out.Recipient) == "" && strings.TrimSpace(out.DocNumber) == "" {
		return aiReceipt{}, false
	}
	return out, true
}

func receiptFieldKey(s string) string { return strings.ToLower(strings.Join(strings.Fields(s), " ")) }

func verifiedReceiptField(recs []aiReceipt, quorum int, field func(aiReceipt) string, key func(string) string) (string, bool) {
	values := map[string][]string{}
	for _, r := range recs {
		v := strings.TrimSpace(field(r))
		if v != "" {
			values[key(v)] = append(values[key(v)], v)
		}
	}
	if len(values) > 1 {
		return "", false
	}
	for _, vs := range values {
		if len(vs) >= quorum {
			sort.Strings(vs)
			return vs[0], true
		}
	}
	return "", true
}

func verifiedReceiptDate(recs []aiReceipt, quorum int) (string, bool) {
	days, full := map[string]int{}, map[string]int{}
	for _, r := range recs {
		if strings.TrimSpace(r.Datetime) == "" {
			continue
		}
		t, ok, dateOnly := parseAIDatetime(r.Datetime)
		if !ok {
			return "", false
		}
		t = t.In(time.Local)
		days[t.Format("2006-01-02")]++
		if !dateOnly {
			full[t.Format("2006-01-02 15:04:05")]++
		}
	}
	if len(days) > 1 || len(full) > 1 {
		return "", false
	}
	for date, votes := range days {
		if votes < quorum {
			return "", false
		}
		for exact, votes := range full {
			if votes >= quorum {
				return exact, true
			}
		}
		return date, true // the date is known; an unconfirmed time is not invented
	}
	return "", true
}

func verifiedReceiptStatus(recs []aiReceipt, quorum int) (string, bool) {
	categories := map[string][]string{}
	for _, r := range recs {
		status := strings.TrimSpace(r.Status)
		if status == "" {
			continue
		}
		key := receiptFieldKey(status)
		switch {
		case receiptDeclined(status):
			key = "declined"
		case receiptPendingRe.MatchString(status):
			return "", false
		case declineSuccessRe.MatchString(status):
			key = "success"
		}
		categories[key] = append(categories[key], status)
	}
	if len(categories) > 1 {
		return "", false
	}
	for key, values := range categories {
		if len(values) >= quorum {
			sort.Strings(values)
			return values[0], true
		}
		if key == "declined" {
			return "", false
		}
	}
	return "", true
}
