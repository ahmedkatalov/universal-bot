package db

import (
	"context"
	"testing"
)

func TestReceiptDocumentSQLMatchesLedgerRegardlessOfLocale(t *testing.T) {
	d := testDB(t)
	for _, value := range []string{
		"а63042034027580i0b10070011881102", " A6304-2034 027580I0B10070011881102 ",
		"ёкл-нрстхуйцшщ12345", "Документ-аВcAbС-001", "", "---", "Auth-12345",
	} {
		var got string
		if err := d.pool.QueryRow(context.Background(), "SELECT "+receiptDocumentSQL("$1::text"), value).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if want := receiptDocumentKey(value); got != want {
			t.Errorf("SQL document key(%q)=%q, Go key=%q", value, got, want)
		}
	}
}
