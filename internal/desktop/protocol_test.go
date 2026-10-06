package desktop

import (
	"testing"

	"github.com/peltonapp/Pelton/internal/storage"
)

func TestProtocolForPicksTheAccountsDriver(t *testing.T) {
	a := &App{}
	if _, ok := a.protocolFor(storage.Account{Protocol: "imap"}).(imapProtocol); !ok {
		t.Fatal("an IMAP account did not get the IMAP driver")
	}
	if _, ok := a.protocolFor(storage.Account{}).(imapProtocol); !ok {
		t.Fatal("an account with no protocol did not get the IMAP driver")
	}
	if _, ok := a.protocolFor(storage.Account{Protocol: "jmap"}).(jmapProtocol); !ok {
		t.Fatal("a JMAP account did not get the JMAP driver")
	}
	for p, want := range map[string]bool{"imap": true, "jmap": true, "IMAP": true, "pop3": false, "": true} {
		if knownProtocol(p) != want {
			t.Errorf("knownProtocol(%q) = %v, want %v", p, !want, want)
		}
	}
}
