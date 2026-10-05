package imap

import (
	"testing"

	goimap "github.com/emersion/go-imap/v2"
)

// an envelope name with a comma has to come out quoted, or the stored list
// reads back as two recipients when it is split for reply-all.
func TestFormatAddressesQuotesNameWithComma(t *testing.T) {
	got := formatAddresses([]goimap.Address{
		{Name: "Doe, John", Mailbox: "j", Host: "example.com"},
		{Mailbox: "me", Host: "example.com"},
	})
	if want := `"Doe, John" <j@example.com>, me@example.com`; got != want {
		t.Fatalf("formatAddresses = %q, want %q", got, want)
	}
}
