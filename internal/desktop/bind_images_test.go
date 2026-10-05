package desktop

import (
	"testing"

	"github.com/peltonapp/Pelton/internal/storage"
)

func TestMessageRemoteKey(t *testing.T) {
	// a present Message-ID is used verbatim (lowercased/trimmed) so the allow
	// survives re-sync, expunge and a changed local row id.
	got := messageRemoteKey(&storage.Message{ID: 7, MessageID: "  <ABC@Example.com>  "})
	if got != "<abc@example.com>" {
		t.Errorf("messageRemoteKey with Message-ID = %q, want %q", got, "<abc@example.com>")
	}
	// without a Message-ID it falls back to the local row id.
	if got := messageRemoteKey(&storage.Message{ID: 42}); got != "local:42" {
		t.Errorf("messageRemoteKey fallback = %q, want %q", got, "local:42")
	}
}

func TestEmailDomain(t *testing.T) {
	tests := []struct{ addr, want string }{
		{"me@example.com", "example.com"},
		{"first@second@example.com", "example.com"},
		{"@example.com", "example.com"},
		{"me@", ""},
		{"example.com", ""},
		{"", ""},
	}
	for _, tt := range tests {
		if got := emailDomain(tt.addr); got != tt.want {
			t.Errorf("emailDomain(%q) = %q, want %q", tt.addr, got, tt.want)
		}
	}
}

// newTrustTestApp returns an app with one account and folder, and a helper that
// stores a message from the given from field.
func newTrustTestApp(t *testing.T) (*App, func(from string) int64) {
	t.Helper()
	a := newAccountTestApp(t)
	accountID, err := a.store.CreateAccount(a.ctx, &storage.Account{Email: "me@example.com"})
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	folderID, err := a.store.CreateFolder(a.ctx, &storage.Folder{AccountID: accountID, Name: "INBOX", IMAPPath: "INBOX"})
	if err != nil {
		t.Fatalf("create folder: %v", err)
	}
	var uid uint32
	add := func(from string) int64 {
		uid++
		id, err := a.store.InsertMessage(a.ctx, &storage.Message{
			AccountID: accountID, FolderID: folderID, UID: uid, FromAddress: from,
		})
		if err != nil {
			t.Fatalf("insert message: %v", err)
		}
		return id
	}
	return a, add
}

// A JMAP stub stores the bare address and the full message "Name <addr>", so
// trust given on one form must hold for the other. Otherwise the next message
// from a trusted sender opens with its images blocked.
func TestRemoteTrustMatchesEitherAddressForm(t *testing.T) {
	a, add := newTrustTestApp(t)
	full := add("InPost <Info@Paczkomaty.pl>")
	other := add("Twitch <no-reply@twitch.tv>")

	if err := a.TrustSenderImages(full); err != nil {
		t.Fatalf("TrustSenderImages: %v", err)
	}
	if err := a.AllowDomainImages(other); err != nil {
		t.Fatalf("AllowDomainImages: %v", err)
	}
	if got := a.remoteSenders(); len(got) != 1 || got[0] != "info@paczkomaty.pl" {
		t.Errorf("trusted senders %q, want the bare address", got)
	}
	if got := a.remoteDomains(); len(got) != 1 || got[0] != "twitch.tv" {
		t.Errorf("trusted domains %q, want twitch.tv", got)
	}
	for _, from := range []string{
		"info@paczkomaty.pl", "InPost <info@paczkomaty.pl>", "Paczkomaty <INFO@paczkomaty.pl>",
		"purchase-noreply@twitch.tv", "Twitch <no-reply@twitch.tv>",
	} {
		if !a.remoteAutoAllow(from) {
			t.Errorf("remoteAutoAllow(%q) = false, want true", from)
		}
	}
	if a.remoteAutoAllow("someone@paczkomaty.pl.evil.com") {
		t.Error("a lookalike domain must not be trusted")
	}
}

// Lists saved before the fix hold the whole from field ("name <addr>") and
// domains cut from it ("twitch.tv>"). They are read as bare values, so the
// user's earlier choices keep working and can still be removed.
func TestRemoteTrustReadsLegacyEntries(t *testing.T) {
	a, add := newTrustTestApp(t)
	add("Kowalski Żaneta <zaneta.kowalski@bank.example>")
	if err := a.store.SetJSON(a.ctx, settingRemoteSenders, []string{"kowalski żaneta <zaneta.kowalski@bank.example>"}); err != nil {
		t.Fatal(err)
	}
	if err := a.store.SetJSON(a.ctx, settingRemoteDomains, []string{"twitch.tv>"}); err != nil {
		t.Fatal(err)
	}

	if !a.remoteAutoAllow("zaneta.kowalski@bank.example") {
		t.Error("a legacy sender entry must match the bare address")
	}
	if !a.remoteAutoAllow("no-reply@twitch.tv") {
		t.Error("a legacy domain entry must match a bare address")
	}

	entries, err := a.ListImageAllowlist()
	if err != nil {
		t.Fatalf("ListImageAllowlist: %v", err)
	}
	if len(entries) != 2 || entries[0].Value != "zaneta.kowalski@bank.example" || entries[1].Value != "twitch.tv" {
		t.Fatalf("allowlist %+v, want bare values", entries)
	}
	if entries[0].ExampleMessageID == 0 {
		t.Error("the sender entry should find its example message stored as \"Name <addr>\"")
	}

	if err := a.RemoveImageAllow("domain", "twitch.tv"); err != nil {
		t.Fatalf("RemoveImageAllow: %v", err)
	}
	if a.remoteAutoAllow("no-reply@twitch.tv") {
		t.Error("removing the listed value must drop the legacy entry")
	}
}

// A From naming several senders cannot be trusted: matching any one of them
// would let the others' images in. A display name that looks like a trusted
// address is just a name; the address in brackets is what counts.
func TestRemoteTrustRefusesSpoofedAndMultiAddressFrom(t *testing.T) {
	a, add := newTrustTestApp(t)
	trusted := add("Trusted <trusted@bank.example>")
	if err := a.TrustSenderImages(trusted); err != nil {
		t.Fatalf("TrustSenderImages: %v", err)
	}
	for _, from := range []string{
		"Evil <evil@spam.example>, Trusted <trusted@bank.example>",
		"Trusted <trusted@bank.example>, Evil <evil@spam.example>",
		"trusted@bank.example <evil@spam.example>",
		"evil@spam.example, trusted@bank.example",
	} {
		if a.remoteAutoAllow(from) {
			t.Errorf("remoteAutoAllow(%q) = true, want false", from)
		}
	}
	// a name that is the address itself is the common, harmless case.
	if !a.remoteAutoAllow("trusted@bank.example <trusted@bank.example>") {
		t.Error("a From whose name repeats its address should still match")
	}

	multi := add("A <a@one.example>, B <b@two.example>")
	if err := a.TrustSenderImages(multi); err != nil {
		t.Fatalf("TrustSenderImages: %v", err)
	}
	if err := a.AllowDomainImages(multi); err != nil {
		t.Fatalf("AllowDomainImages: %v", err)
	}
	if got := a.remoteSenders(); len(got) != 1 {
		t.Errorf("trusted senders %q, want a multi-address From left out", got)
	}
	if got := a.remoteDomains(); len(got) != 0 {
		t.Errorf("trusted domains %q, want none from a multi-address From", got)
	}
}
