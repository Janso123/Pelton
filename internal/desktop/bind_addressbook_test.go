package desktop

import (
	"slices"
	"testing"

	"github.com/peltonapp/Pelton/internal/storage"
)

// learningTestApp is an app whose Sent folder holds one message to
// friend@x.org and whose inbox holds mail from a stranger, a trusted
// image sender, a VIP and a mailing list.
func learningTestApp(t *testing.T) *App {
	t.Helper()
	a := newRoleTestApp(t)
	sent := makeFolder(t, a, storage.Folder{Name: "Sent", IMAPPath: "Sent", Attributes: []string{`\Sent`}})
	inbox := storage.Folder{AccountID: sent.AccountID, Name: "INBOX", IMAPPath: "INBOX"}
	if _, err := a.store.CreateFolder(a.ctx, &inbox); err != nil {
		t.Fatal(err)
	}
	msgs := []storage.Message{
		{FolderID: sent.ID, FromAddress: "a@example.com", ToAddresses: "Friend <friend@x.org>"},
		{FolderID: inbox.ID, FromAddress: "Stranger <stranger@x.org>"},
		{FolderID: inbox.ID, FromAddress: "Shop <news@x.org>", ListUnsubscribe: "<mailto:leave@x.org>"},
		{FolderID: inbox.ID, FromAddress: "Boss <boss@x.org>"},
		{FolderID: inbox.ID, FromAddress: "noreply@x.org"},
	}
	for i, m := range msgs {
		m.AccountID, m.UID, m.Subject, m.BodyComplete = sent.AccountID, uint32(i+1), "s", true
		if _, err := a.store.InsertMessage(a.ctx, &m); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.store.SetJSON(a.ctx, settingRemoteSenders, []string{"Shop <News@X.org>"}); err != nil {
		t.Fatal(err)
	}
	if err := a.store.SetJSON(a.ctx, settingVIPSenders, []string{"boss@x.org"}); err != nil {
		t.Fatal(err)
	}
	return a
}

func suggestedEmails(t *testing.T, a *App) []string {
	t.Helper()
	got, err := a.SearchAddresses("x.org", 10)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, 0, len(got))
	for _, e := range got {
		out = append(out, e.Email)
	}
	slices.Sort(out)
	return out
}

// Each learning level decides what the learned book offers: every level keeps
// what the one below it offers, and none offers the noreply sender. Lowering
// the level only hides rows, so raising it again brings them back.
func TestSearchAddressesPerLearningLevel(t *testing.T) {
	a := learningTestApp(t)
	tests := []struct {
		level string
		want  []string
	}{
		{learnAll, []string{"boss@x.org", "friend@x.org", "news@x.org", "stranger@x.org"}},
		{learnTrusted, []string{"boss@x.org", "friend@x.org", "news@x.org"}},
		{learnSent, []string{"friend@x.org"}},
		{learnOff, []string{}},
		{learnAll, []string{"boss@x.org", "friend@x.org", "news@x.org", "stranger@x.org"}},
	}
	for _, tt := range tests {
		if err := a.store.Set(a.ctx, settingAddressLearning, tt.level); err != nil {
			t.Fatal(err)
		}
		a.harvestAddressBook()
		if got := suggestedEmails(t, a); !slices.Equal(got, tt.want) {
			t.Errorf("%s: suggested %q, want %q", tt.level, got, tt.want)
		}
	}
}

// Off learns nothing from cached mail, and sent learns only from Sent.
func TestHarvestAddressBookRespectsLevel(t *testing.T) {
	for _, tt := range []struct {
		level string
		want  int
	}{{learnOff, 0}, {learnSent, 1}} {
		a := learningTestApp(t)
		if err := a.store.Set(a.ctx, settingAddressLearning, tt.level); err != nil {
			t.Fatal(err)
		}
		a.harvestAddressBook()
		book, err := a.ListAddresses()
		if err != nil {
			t.Fatal(err)
		}
		if len(book) != tt.want {
			t.Errorf("%s: book %+v, want %d entries", tt.level, book, tt.want)
		}
	}
}

// The level replaced an on/off switch: someone who had switched learning off
// keeps it off, everyone else starts at sent.
func TestAddressLearningFromLegacySetting(t *testing.T) {
	tests := []struct {
		name   string
		legacy string
		level  string
		want   string
	}{
		{"never set", "", "", learnSent},
		{"was on", "true", "", learnSent},
		{"was off", "false", "", learnOff},
		{"new value wins", "false", learnAll, learnAll},
		{"unknown value", "", "sometimes", learnSent},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := newRoleTestApp(t)
			if tt.legacy != "" {
				if err := a.store.Set(a.ctx, settingHarvestAddressesLegacy, tt.legacy); err != nil {
					t.Fatal(err)
				}
			}
			if tt.level != "" {
				if err := a.store.Set(a.ctx, settingAddressLearning, tt.level); err != nil {
					t.Fatal(err)
				}
			}
			if got := a.addressLearning(); got != tt.want {
				t.Errorf("addressLearning() = %q, want %q", got, tt.want)
			}
			prefs, err := a.GetUIPrefs()
			if err != nil {
				t.Fatal(err)
			}
			if prefs.AddressLearning != tt.want {
				t.Errorf("prefs.AddressLearning = %q, want %q", prefs.AddressLearning, tt.want)
			}
		})
	}
}
