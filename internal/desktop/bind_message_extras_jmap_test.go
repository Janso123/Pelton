package desktop

import (
	"reflect"
	"testing"
)

// Colour sync opened an IMAP connection for every account, which a JMAP
// account has no host for. newIMAPClient stays nil, so any IMAP attempt fails.
func TestPushColorKeywordJMAPSetsKeywordsThroughTheAdapter(t *testing.T) {
	a, db, ctx := moveTestApp(t)
	_, _, id, fake := jmapMoveAccount(t, a, db, ctx)

	a.pushColorKeyword(id, 3)

	want := []fakeKeywords{{
		remoteID: "E1",
		add:      []string{"$Label3"},
		remove:   []string{"$Label1", "$Label2", "$Label4", "$Label5", "$Label6", "$Label7", "$Label8"},
	}}
	if !reflect.DeepEqual(fake.keywords, want) {
		t.Errorf("keywords = %+v, want %+v", fake.keywords, want)
	}
}

// Clearing a colour only removes the label keywords.
func TestPushColorKeywordJMAPClearRemovesEveryLabel(t *testing.T) {
	a, db, ctx := moveTestApp(t)
	_, _, id, fake := jmapMoveAccount(t, a, db, ctx)

	a.pushColorKeyword(id, 0)

	if len(fake.keywords) != 1 || len(fake.keywords[0].add) != 0 || len(fake.keywords[0].remove) != len(colorKeywords) {
		t.Errorf("keywords = %+v, want one call removing all %d labels", fake.keywords, len(colorKeywords))
	}
}
