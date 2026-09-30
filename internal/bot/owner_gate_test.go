package bot

import (
	"testing"

	"go.mau.fi/whatsmeow/types"
)

// TestSenderInSet — владелец распознаётся и когда WhatsApp адресует его через
// скрытый LID (реальный номер тогда в SenderAlt), и по обычному номеру, и с
// 8-префиксом; чужой номер не проходит.
func TestSenderInSet(t *testing.T) {
	set := map[string]bool{"79287836800": true}

	// LID в Sender, реальный номер в SenderAlt — узнаём по SenderAlt.
	lid := types.MessageInfo{MessageSource: types.MessageSource{
		Sender:    types.JID{User: "56560508211416", Server: "lid"},
		SenderAlt: types.JID{User: "79287836800", Server: types.DefaultUserServer},
	}}
	if !senderInSet(set, lid) {
		t.Error("владелец должен распознаться по SenderAlt при LID-адресации")
	}

	// Обычный номер в Sender.
	pn := types.MessageInfo{MessageSource: types.MessageSource{
		Sender: types.JID{User: "79287836800", Server: types.DefaultUserServer},
	}}
	if !senderInSet(set, pn) {
		t.Error("владелец должен распознаться по Sender")
	}

	// 8-префикс в наборе нормализуется к 7.
	set8 := map[string]bool{normalizePhone("89287836800"): true}
	if !senderInSet(set8, pn) {
		t.Error("8-префикс должен нормализоваться к 7")
	}

	// Чужой номер — не проходит.
	other := types.MessageInfo{MessageSource: types.MessageSource{
		Sender: types.JID{User: "70001112233", Server: types.DefaultUserServer},
	}}
	if senderInSet(set, other) {
		t.Error("чужой номер не должен проходить")
	}
}
