package jmap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	gojmap "github.com/Janso123/go-jmap"

	"github.com/peltonapp/Pelton/internal/storage"
	psync "github.com/peltonapp/Pelton/internal/sync"
)

func TestKeywords(t *testing.T) {
	got := FlagsFromKeywords(map[string]bool{"$seen": true, "$flagged": true})
	want := storage.FlagSeen | storage.FlagFlagged
	if got != want {
		t.Fatalf("FlagsFromKeywords: got %v want %v", got, want)
	}
	kw := KeywordsFromFlags(want)
	if !kw["$seen"] || !kw["$flagged"] {
		t.Fatalf("KeywordsFromFlags: got %#v", kw)
	}
	if _, ok := kw["$deleted"]; ok {
		t.Fatal("KeywordsFromFlags must not set $deleted")
	}
}

func TestListMessagesEnvelopeProps(t *testing.T) {
	var getProps []string
	srv := newJMAPServer(t, sessionDocLimits(256, 128), func(w http.ResponseWriter, r *http.Request, calls []methodCall) {
		switch calls[0].Name {
		case "Email/query":
			writeMethod(w, "s1", calls[0].ID, "Email/query", map[string]any{
				"accountId": "A1", "queryState": "q1", "canCalculateChanges": true,
				"position": 0, "ids": []string{"E1"}, "total": 1,
			})
		case "Email/get":
			if props, ok := calls[0].Args["properties"].([]any); ok {
				getProps = make([]string, len(props))
				for i, p := range props {
					getProps[i] = fmt.Sprint(p)
				}
			}
			writeMethod(w, "s1", calls[0].ID, "Email/get", map[string]any{
				"accountId": "A1", "state": "st1",
				"list": []map[string]any{{
					"id": "E1", "keywords": map[string]bool{"$seen": true},
					"mailboxIds": map[string]bool{"M1": true},
					"subject":    "Hello", "preview": "hi there",
					"from":       []map[string]any{{"name": "Ada", "email": "ada@ex"}},
					"to":         []map[string]any{{"email": "bob@ex"}},
					"receivedAt": "2024-06-01T12:00:00Z",
					"size":       42, "hasAttachment": true,
				}},
				"notFound": []string{},
			})
		default:
			t.Fatalf("unexpected method %s", calls[0].Name)
		}
	})
	defer srv.Close()

	ad := newTestAdapter(t, srv, "A1")
	headers, _, _, err := ad.ListMessages(context.Background(), psync.RemoteMailbox{RemoteID: "M1"})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	propSet := map[string]bool{}
	for _, p := range getProps {
		propSet[p] = true
	}
	for _, want := range []string{"id", "keywords", "mailboxIds", "subject", "from", "to", "receivedAt", "size", "hasAttachment", "preview"} {
		if !propSet[want] {
			t.Errorf("Email/get missing property %q; got %v", want, getProps)
		}
	}
	if propSet["blobId"] {
		t.Fatal("list Email/get must not request blobId")
	}
	if len(headers) != 1 {
		t.Fatalf("headers: got %d", len(headers))
	}
	h := headers[0]
	if !h.HasListMeta || h.Subject != "Hello" || h.From != "ada@ex" || h.FromName != "Ada" {
		t.Fatalf("header envelope: %+v", h)
	}
	if h.To != "bob@ex" || h.Preview != "hi there" || h.Size != 42 || !h.HasAttachment {
		t.Fatalf("header meta: %+v", h)
	}
	if h.Flags != storage.FlagSeen {
		t.Fatalf("flags: %v", h.Flags)
	}
	if h.Date.IsZero() {
		t.Fatal("expected receivedAt date")
	}
}

func TestFetchDownloadsBlobsInParallel(t *testing.T) {
	var peak atomic.Int32
	var cur atomic.Int32
	raw := "Subject: hi\r\n\r\nhello\r\n"
	srv := newJMAPServer(t, sessionDocLimits(256, 128), func(w http.ResponseWriter, r *http.Request, calls []methodCall) {
		if calls[0].Name != "Email/get" {
			t.Fatalf("unexpected %s", calls[0].Name)
		}
		list := make([]map[string]any, 0, 5)
		for i := 1; i <= 5; i++ {
			id := fmt.Sprintf("e%d", i)
			list = append(list, map[string]any{
				"id": id, "blobId": fmt.Sprintf("B%d", i),
				"keywords": map[string]bool{},
			})
		}
		writeMethod(w, "s1", calls[0].ID, "Email/get", map[string]any{
			"accountId": "A1", "state": "s",
			"list":     list,
			"notFound": []string{},
		})
	})
	defer srv.Close()
	base := srv.Config.Handler
	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/download/") {
			n := cur.Add(1)
			for {
				p := peak.Load()
				if n <= p || peak.CompareAndSwap(p, n) {
					break
				}
			}
			time.Sleep(25 * time.Millisecond)
			cur.Add(-1)
			w.Header().Set("Content-Type", "application/octet-stream")
			io.WriteString(w, raw)
			return
		}
		base.ServeHTTP(w, r)
	})

	ad := newTestAdapter(t, srv, "A1")
	ad.BlobDownloadParallel = 3
	got, err := ad.Fetch(context.Background(), "M1", []string{"e1", "e2", "e3", "e4", "e5"})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(got) != 5 {
		t.Fatalf("got %d messages want 5", len(got))
	}
	if peak.Load() < 2 {
		t.Fatalf("peak=%d want >=2", peak.Load())
	}
}

func TestFetchCancelledMidDownload(t *testing.T) {
	raw := "Subject: hi\r\n\r\nhello\r\n"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := newJMAPServer(t, sessionDocLimits(256, 128), func(w http.ResponseWriter, r *http.Request, calls []methodCall) {
		list := make([]map[string]any, 0, 8)
		for i := 1; i <= 8; i++ {
			list = append(list, map[string]any{
				"id": fmt.Sprintf("e%d", i), "blobId": fmt.Sprintf("B%d", i),
				"keywords": map[string]bool{},
			})
		}
		writeMethod(w, "s1", calls[0].ID, "Email/get", map[string]any{
			"accountId": "A1", "state": "s", "list": list, "notFound": []string{},
		})
	})
	defer srv.Close()
	base := srv.Config.Handler
	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/download/") {
			cancel()
			w.Header().Set("Content-Type", "application/octet-stream")
			io.WriteString(w, raw)
			return
		}
		base.ServeHTTP(w, r)
	})

	ad := newTestAdapter(t, srv, "A1")
	ad.BlobDownloadParallel = 2
	ids := []string{"e1", "e2", "e3", "e4", "e5", "e6", "e7", "e8"}
	got, err := ad.Fetch(ctx, "M1", ids)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Fetch error = %v, want context.Canceled", err)
	}
	for i, f := range got {
		if f.RemoteID == "" || len(f.Raw) == 0 {
			t.Fatalf("entry %d is empty: %+v", i, f)
		}
	}
	if len(got) == len(ids) {
		t.Fatalf("got all %d messages despite cancellation", len(got))
	}
}

func TestFetchBlob(t *testing.T) {
	raw := "Subject: hi\r\n\r\nhello\r\n"
	srv := newJMAPServer(t, sessionDocLimits(256, 128), func(w http.ResponseWriter, r *http.Request, calls []methodCall) {
		if calls[0].Name != "Email/get" {
			t.Fatalf("unexpected %s", calls[0].Name)
		}
		writeMethod(w, "s1", calls[0].ID, "Email/get", map[string]any{
			"accountId": "A1", "state": "s",
			"list": []map[string]any{{
				"id": "E1", "blobId": "B1",
				"keywords": map[string]bool{"$seen": true},
			}},
			"notFound": []string{},
		})
	})
	defer srv.Close()
	srv.Config.Handler = wrapWithDownload(srv.Config.Handler, "B1", raw)

	ad := newTestAdapter(t, srv, "A1")
	got, err := ad.Fetch(context.Background(), "M1", []string{"E1"})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d messages", len(got))
	}
	if got[0].Subject != "hi" {
		t.Fatalf("Subject: %q", got[0].Subject)
	}
	if !strings.Contains(got[0].Text, "hello") {
		t.Fatalf("Text: %q", got[0].Text)
	}
	if got[0].LegacyUID != 0 {
		t.Fatalf("LegacyUID: %d", got[0].LegacyUID)
	}
	if got[0].Flags != storage.FlagSeen {
		t.Fatalf("Flags: %v", got[0].Flags)
	}
}

func TestFetchLegacyUIDZero(t *testing.T) {
	raw := "Subject: n\r\n\r\nbody\r\n"
	srv := newJMAPServer(t, sessionDocLimits(256, 128), func(w http.ResponseWriter, r *http.Request, calls []methodCall) {
		writeMethod(w, "s1", calls[0].ID, "Email/get", map[string]any{
			"accountId": "A1", "state": "s",
			"list": []map[string]any{{
				"id": "123", "blobId": "B1", "keywords": map[string]bool{},
			}},
			"notFound": []string{},
		})
	})
	defer srv.Close()
	srv.Config.Handler = wrapWithDownload(srv.Config.Handler, "B1", raw)

	ad := newTestAdapter(t, srv, "A1")
	got, err := ad.Fetch(context.Background(), "M1", []string{"123"})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if got[0].RemoteID != "123" || got[0].LegacyUID != 0 {
		t.Fatalf("got RemoteID=%q LegacyUID=%d", got[0].RemoteID, got[0].LegacyUID)
	}
}

func TestMovePatchesOnlySourceDest(t *testing.T) {
	var patch map[string]any
	srv := newJMAPServer(t, sessionDocLimits(256, 128), func(w http.ResponseWriter, r *http.Request, calls []methodCall) {
		if calls[0].Name != "Email/set" {
			t.Fatalf("unexpected %s", calls[0].Name)
		}
		upd, _ := calls[0].Args["update"].(map[string]any)
		patch, _ = upd["E1"].(map[string]any)
		writeMethod(w, "s1", calls[0].ID, "Email/set", map[string]any{
			"accountId": "A1", "newState": "s",
			"updated": map[string]any{"E1": nil},
		})
	})
	defer srv.Close()

	ad := newTestAdapter(t, srv, "A1")
	if err := ad.Move(context.Background(), "M1", []string{"E1"}, "M2"); err != nil {
		t.Fatalf("Move: %v", err)
	}
	if len(patch) != 2 {
		t.Fatalf("patch keys: %#v", patch)
	}
	// RFC 8621 only allows true in mailboxIds, so removal must be null.
	if v, ok := patch["mailboxIds/M1"]; !ok || v != nil {
		t.Fatalf("source patch: %#v, want null", v)
	}
	if patch["mailboxIds/M2"] != true {
		t.Fatalf("dest patch: %#v", patch["mailboxIds/M2"])
	}
}

func TestDeleteAndFlagsNoDeletedKeyword(t *testing.T) {
	var setBodies []map[string]any
	srv := newJMAPServer(t, sessionDocLimits(256, 128), func(w http.ResponseWriter, r *http.Request, calls []methodCall) {
		setBodies = append(setBodies, calls[0].Args)
		writeMethod(w, "s1", calls[0].ID, "Email/set", map[string]any{
			"accountId": "A1", "newState": "s",
			"updated":   map[string]any{"E1": nil},
			"destroyed": []string{"E1"},
		})
	})
	defer srv.Close()

	ad := newTestAdapter(t, srv, "A1")
	if err := ad.SetFlags(context.Background(), "M1", "E1", storage.FlagSeen); err != nil {
		t.Fatalf("SetFlags: %v", err)
	}
	if err := ad.Delete(context.Background(), "M1", []string{"E1"}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	raw, _ := json.Marshal(setBodies)
	if strings.Contains(string(raw), "$deleted") {
		t.Fatalf("must not emit $deleted: %s", raw)
	}
}

func TestChunkingMaxObjects(t *testing.T) {
	var getSizes, setSizes []int
	srv := newJMAPServer(t, sessionDocLimits(2, 2), func(w http.ResponseWriter, r *http.Request, calls []methodCall) {
		switch calls[0].Name {
		case "Email/get":
			ids := stringIDs(calls[0].Args["ids"])
			getSizes = append(getSizes, len(ids))
			list := make([]map[string]any, 0, len(ids))
			for _, id := range ids {
				list = append(list, map[string]any{
					"id": id, "blobId": "B1", "keywords": map[string]bool{},
					"mailboxIds": map[string]bool{"M1": true},
				})
			}
			writeMethod(w, "s1", calls[0].ID, "Email/get", map[string]any{
				"accountId": "A1", "state": "s", "list": list, "notFound": []string{},
			})
		case "Email/set":
			n := 0
			if upd, ok := calls[0].Args["update"].(map[string]any); ok {
				n += len(upd)
			}
			if dest, ok := calls[0].Args["destroy"].([]any); ok {
				n += len(dest)
			}
			setSizes = append(setSizes, n)
			writeMethod(w, "s1", calls[0].ID, "Email/set", map[string]any{
				"accountId": "A1", "newState": "s",
			})
		case "Email/query":
			writeMethod(w, "s1", calls[0].ID, "Email/query", map[string]any{
				"accountId": "A1", "queryState": "q", "canCalculateChanges": true,
				"position": 0, "ids": []string{"E1", "E2", "E3"}, "total": 3,
			})
		default:
			t.Fatalf("unexpected %s", calls[0].Name)
		}
	})
	defer srv.Close()
	srv.Config.Handler = wrapWithDownload(srv.Config.Handler, "B1", "Subject: x\r\n\r\ny\r\n")

	ad := newTestAdapter(t, srv, "A1")
	if _, _, _, err := ad.ListMessages(context.Background(), psync.RemoteMailbox{RemoteID: "M1"}); err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if _, err := ad.Fetch(context.Background(), "M1", []string{"E1", "E2", "E3"}); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if err := ad.Move(context.Background(), "M1", []string{"E1", "E2", "E3"}, "M2"); err != nil {
		t.Fatalf("Move: %v", err)
	}
	if err := ad.Delete(context.Background(), "M1", []string{"E1", "E2", "E3"}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	for _, n := range append(append([]int{}, getSizes...), setSizes...) {
		if n > 2 {
			t.Fatalf("chunk size %d exceeds maxObjects 2 (gets=%v sets=%v)", n, getSizes, setSizes)
		}
	}
	if len(getSizes) < 2 || len(setSizes) < 2 {
		t.Fatalf("expected chunking: gets=%v sets=%v", getSizes, setSizes)
	}
}

func TestSessionStaleDoesNotReplayMutation(t *testing.T) {
	var apiPosts, sessionGETs atomic.Int32
	state := "A"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/.well-known/jmap"):
			sessionGETs.Add(1)
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, sessionDocWithState(state, 256, 128))
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/api"):
			apiPosts.Add(1)
			body, _ := io.ReadAll(r.Body)
			calls := parseCalls(t, body)
			// First mutation response changes sessionState; result must still be returned.
			state = "B"
			writeMethod(w, "B", calls[0].ID, calls[0].Name, map[string]any{
				"accountId": "A1", "newState": "s",
				"updated": map[string]any{"E1": nil},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	ad := NewAdapter(c, "A1")
	if err := ad.SetFlags(context.Background(), "M1", "E1", storage.FlagSeen); err != nil {
		t.Fatalf("SetFlags: %v", err)
	}
	if apiPosts.Load() != 1 {
		t.Fatalf("api posts: got %d want 1 (no mutation replay)", apiPosts.Load())
	}
	// Next call must refresh session first.
	before := sessionGETs.Load()
	if err := ad.SetFlags(context.Background(), "M1", "E1", 0); err != nil {
		t.Fatalf("second SetFlags: %v", err)
	}
	if sessionGETs.Load() <= before {
		t.Fatal("expected RefreshSession before next call")
	}
	if apiPosts.Load() != 2 {
		t.Fatalf("api posts after second call: %d", apiPosts.Load())
	}
}

func TestDoReadRetriesOnce(t *testing.T) {
	var posts atomic.Int32
	failOnce := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/.well-known/jmap"):
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, sessionDocLimits(256, 128))
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/api"):
			posts.Add(1)
			if failOnce {
				failOnce = false
				hj, ok := w.(http.Hijacker)
				if !ok {
					t.Fatal("hijack unsupported")
				}
				conn, _, _ := hj.Hijack()
				conn.Close()
				return
			}
			calls := parseCalls(t, mustRead(t, r.Body))
			switch calls[0].Name {
			case "Email/query":
				writeMethod(w, "s1", calls[0].ID, "Email/query", map[string]any{
					"accountId": "A1", "queryState": "q", "canCalculateChanges": true,
					"position": 0, "ids": []string{}, "total": 0,
				})
			case "Email/get":
				writeMethod(w, "s1", calls[0].ID, "Email/get", map[string]any{
					"accountId": "A1", "state": "st-full", "list": []any{}, "notFound": []string{},
				})
			default:
				t.Fatalf("unexpected %s", calls[0].Name)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	ad := NewAdapter(newTestClient(t, srv), "A1")
	_, _, _, err := ad.ListMessages(context.Background(), psync.RemoteMailbox{RemoteID: "M1"})
	if err != nil {
		t.Fatalf("ListMessages: %v (posts=%d)", err, posts.Load())
	}
	if posts.Load() < 2 {
		t.Fatalf("expected read retry after transport failure, posts=%d", posts.Load())
	}
}

func TestDoMutationNoRetry(t *testing.T) {
	var posts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/.well-known/jmap"):
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, sessionDocLimits(256, 128))
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/api"):
			posts.Add(1)
			hj, ok := w.(http.Hijacker)
			if !ok {
				t.Fatal("hijack unsupported")
			}
			conn, _, _ := hj.Hijack()
			conn.Close()
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	ad := NewAdapter(newTestClient(t, srv), "A1")
	err := ad.SetFlags(context.Background(), "M1", "E1", storage.FlagSeen)
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, ErrIndeterminate) {
		t.Fatalf("want ErrIndeterminate, got %v", err)
	}
	if posts.Load() != 1 {
		t.Fatalf("posts: got %d want 1 (no mutation replay)", posts.Load())
	}
}

// Keyword names are lowercased and removals patch to null, matching how
// servers store them.
func TestSetKeywordsLowercasesAndClears(t *testing.T) {
	var patch map[string]any
	srv := newJMAPServer(t, sessionDocLimits(256, 128), func(w http.ResponseWriter, r *http.Request, calls []methodCall) {
		upd, _ := calls[0].Args["update"].(map[string]any)
		patch, _ = upd["E1"].(map[string]any)
		writeMethod(w, "s1", calls[0].ID, "Email/set", map[string]any{
			"accountId": "A1", "newState": "s", "updated": map[string]any{"E1": nil},
		})
	})
	defer srv.Close()

	ad := newTestAdapter(t, srv, "A1")
	if err := ad.SetKeywords(context.Background(), "E1", []string{"$Label3"}, []string{"$Label1", "$Label2"}); err != nil {
		t.Fatalf("SetKeywords: %v", err)
	}
	if patch["keywords/$label3"] != true {
		t.Fatalf("add patch: %#v", patch)
	}
	for _, k := range []string{"keywords/$label1", "keywords/$label2"} {
		if v, ok := patch[k]; !ok || v != nil {
			t.Fatalf("%s: %#v, want null", k, v)
		}
	}
}

// Clearing a keyword patches it to null: RFC 8621 only allows true as a
// keywords value, so a server may reject false.
func TestSetFlagsClearsKeywordsWithNull(t *testing.T) {
	var patch map[string]any
	srv := newJMAPServer(t, sessionDocLimits(256, 128), func(w http.ResponseWriter, r *http.Request, calls []methodCall) {
		upd, _ := calls[0].Args["update"].(map[string]any)
		patch, _ = upd["E1"].(map[string]any)
		writeMethod(w, "s1", calls[0].ID, "Email/set", map[string]any{
			"accountId": "A1", "newState": "s", "updated": map[string]any{"E1": nil},
		})
	})
	defer srv.Close()

	ad := newTestAdapter(t, srv, "A1")
	if err := ad.SetFlags(context.Background(), "M1", "E1", storage.FlagFlagged); err != nil {
		t.Fatalf("SetFlags: %v", err)
	}
	if v, ok := patch["keywords/$seen"]; !ok || v != nil {
		t.Fatalf("$seen patch: %#v, want null", v)
	}
	if patch["keywords/$flagged"] != true {
		t.Fatalf("$flagged patch: %#v, want true", patch["keywords/$flagged"])
	}
}

// A record the server rejects in notUpdated/notDestroyed fails the call, so
// the engine keeps the pending change instead of treating it as pushed. A
// notFound record is already gone and is not an error.
func TestSetRejectionsFailMutation(t *testing.T) {
	rejected := map[string]any{"E1": map[string]any{"type": "forbidden"}}
	gone := map[string]any{"E1": map[string]any{"type": "notFound"}}
	tests := []struct {
		name    string
		resp    map[string]any
		call    func(*Adapter) error
		wantErr bool
	}{
		{"flags rejected", map[string]any{"notUpdated": rejected}, func(a *Adapter) error {
			return a.SetFlags(context.Background(), "M1", "E1", storage.FlagSeen)
		}, true},
		{"flags on gone email", map[string]any{"notUpdated": gone}, func(a *Adapter) error {
			return a.SetFlags(context.Background(), "M1", "E1", storage.FlagSeen)
		}, false},
		{"move rejected", map[string]any{"notUpdated": rejected}, func(a *Adapter) error {
			return a.Move(context.Background(), "M1", []string{"E1"}, "M2")
		}, true},
		{"delete rejected", map[string]any{"notDestroyed": rejected}, func(a *Adapter) error {
			return a.Delete(context.Background(), "M1", []string{"E1"})
		}, true},
		{"delete gone email", map[string]any{"notDestroyed": gone}, func(a *Adapter) error {
			return a.Delete(context.Background(), "M1", []string{"E1"})
		}, false},
		{"rename mailbox rejected", map[string]any{"notUpdated": rejected}, func(a *Adapter) error {
			return a.RenameMailbox(context.Background(), "E1", "x")
		}, true},
		{"rename gone mailbox", map[string]any{"notUpdated": gone}, func(a *Adapter) error {
			return a.RenameMailbox(context.Background(), "E1", "x")
		}, true},
		{"move gone email", map[string]any{"notUpdated": gone}, func(a *Adapter) error {
			return a.Move(context.Background(), "M1", []string{"E1"}, "M2")
		}, false},
		{"delete mailbox rejected", map[string]any{"notDestroyed": rejected}, func(a *Adapter) error {
			return a.DeleteMailbox(context.Background(), "E1")
		}, true},
		{"delete gone mailbox", map[string]any{"notDestroyed": gone}, func(a *Adapter) error {
			return a.DeleteMailbox(context.Background(), "E1")
		}, false},
		{"null rejection", map[string]any{"notUpdated": map[string]any{"E1": nil}}, func(a *Adapter) error {
			return a.SetFlags(context.Background(), "M1", "E1", storage.FlagSeen)
		}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := newJMAPServer(t, sessionDocLimits(256, 128), func(w http.ResponseWriter, r *http.Request, calls []methodCall) {
				resp := map[string]any{"accountId": "A1", "newState": "s"}
				for k, v := range tc.resp {
					resp[k] = v
				}
				writeMethod(w, "s1", calls[0].ID, calls[0].Name, resp)
			})
			defer srv.Close()

			err := tc.call(newTestAdapter(t, srv, "A1"))
			if (err != nil) != tc.wantErr {
				t.Fatalf("err=%v, wantErr=%v", err, tc.wantErr)
			}
		})
	}
}

// A rejected record surfaces as its *gojmap.SetError, so callers can tell
// e.g. forbidden from overQuota.
func TestSetRejectionUnwrapsToSetError(t *testing.T) {
	srv := newJMAPServer(t, sessionDocLimits(256, 128), func(w http.ResponseWriter, r *http.Request, calls []methodCall) {
		writeMethod(w, "s1", calls[0].ID, "Email/set", map[string]any{
			"accountId": "A1", "newState": "s",
			"notUpdated": map[string]any{"E1": map[string]any{"type": "overQuota"}},
		})
	})
	defer srv.Close()

	err := newTestAdapter(t, srv, "A1").SetFlags(context.Background(), "M1", "E1", storage.FlagSeen)
	var se *gojmap.SetError
	if !errors.As(err, &se) || se.Type != string(gojmap.SetErrOverQuota) {
		t.Fatalf("err=%v, want *SetError overQuota", err)
	}
}

// A rejection in one Email/set batch stops the remaining batches, so the
// engine retries the whole change rather than reporting part of it as done.
func TestBatchedSetStopsAfterRejectedBatch(t *testing.T) {
	tests := []struct {
		name string
		call func(*Adapter) error
	}{
		{"move", func(a *Adapter) error {
			return a.Move(context.Background(), "M1", []string{"E1", "E2", "E3"}, "M2")
		}},
		{"delete", func(a *Adapter) error {
			return a.Delete(context.Background(), "M1", []string{"E1", "E2", "E3"})
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var sets int
			srv := newJMAPServer(t, sessionDocLimits(256, 2), func(w http.ResponseWriter, r *http.Request, calls []methodCall) {
				sets++
				writeMethod(w, "s1", calls[0].ID, "Email/set", map[string]any{
					"accountId": "A1", "newState": "s",
					"notUpdated":   map[string]any{"E1": map[string]any{"type": "forbidden"}},
					"notDestroyed": map[string]any{"E1": map[string]any{"type": "forbidden"}},
				})
			})
			defer srv.Close()

			if err := tc.call(newTestAdapter(t, srv, "A1")); err == nil {
				t.Fatal("expected the batch-1 rejection")
			}
			if sets != 1 {
				t.Fatalf("Email/set calls=%d, want 1 (batch 2 skipped)", sets)
			}
		})
	}
}

// Deleting a mailbox asks the server to remove its emails too; without
// onDestroyRemoveEmails RFC 8621 servers refuse any non-empty mailbox.
func TestDeleteMailboxRemovesEmails(t *testing.T) {
	var args map[string]any
	srv := newJMAPServer(t, sessionDocLimits(256, 128), func(w http.ResponseWriter, r *http.Request, calls []methodCall) {
		args = calls[0].Args
		writeMethod(w, "s1", calls[0].ID, "Mailbox/set", map[string]any{
			"accountId": "A1", "newState": "s", "destroyed": []string{"M1"},
		})
	})
	defer srv.Close()

	if err := newTestAdapter(t, srv, "A1").DeleteMailbox(context.Background(), "M1"); err != nil {
		t.Fatalf("DeleteMailbox: %v", err)
	}
	if args["onDestroyRemoveEmails"] != true {
		t.Fatalf("onDestroyRemoveEmails=%#v, want true", args["onDestroyRemoveEmails"])
	}
}

// A full list returns the Email state read before Email/query. An email that
// arrives while the list runs is then still in the next Email/changes, instead
// of being covered by a token taken after the query missed it.
func TestListMessagesFullStateReadBeforeQuery(t *testing.T) {
	var order []string
	srv := newJMAPServer(t, sessionDocLimits(256, 128), func(w http.ResponseWriter, r *http.Request, calls []methodCall) {
		switch calls[0].Name {
		case "Email/query":
			order = append(order, "query")
			writeMethod(w, "s1", calls[0].ID, "Email/query", map[string]any{
				"accountId": "A1", "queryState": "q1", "canCalculateChanges": true,
				"position": 0, "ids": []string{"E1"}, "total": 1,
			})
		case "Email/get":
			req := stringIDs(calls[0].Args["ids"])
			if len(req) == 0 {
				order = append(order, "state")
				writeMethod(w, "s1", calls[0].ID, "Email/get", map[string]any{
					"accountId": "A1", "state": "before", "list": []any{}, "notFound": []string{},
				})
				return
			}
			order = append(order, "get")
			// E2 arrived after the query; the server state now includes it.
			writeMethod(w, "s1", calls[0].ID, "Email/get", map[string]any{
				"accountId": "A1", "state": "after",
				"list":     []map[string]any{{"id": "E1", "keywords": map[string]bool{}, "mailboxIds": map[string]bool{"M1": true}}},
				"notFound": []string{},
			})
		default:
			t.Fatalf("unexpected method %s", calls[0].Name)
		}
	})
	defer srv.Close()

	ad := newTestAdapter(t, srv, "A1")
	_, state, _, err := ad.ListMessages(context.Background(), psync.RemoteMailbox{RemoteID: "M1"})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if fmt.Sprint(order) != "[state query get]" {
		t.Fatalf("call order %v, want state read before the query", order)
	}
	if state != "before" {
		t.Fatalf("state %q, want the pre-query state", state)
	}
}

// --- test helpers ---

type methodCall struct {
	Name string
	Args map[string]any
	ID   string
}

func newTestAdapter(t *testing.T, srv *httptest.Server, accountID string) *Adapter {
	t.Helper()
	return NewAdapter(newTestClient(t, srv), accountID)
}

func newTestClient(t *testing.T, srv *httptest.Server) *Client {
	t.Helper()
	jc := gojmap.NewClient(srv.URL+"/.well-known/jmap", gojmap.WithHTTPClient(srv.Client()), gojmap.WithBasic("u", "p"))
	if err := jc.Authenticate(context.Background()); err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	return NewClient(jc)
}

func newJMAPServer(t *testing.T, sessionJSON string, onAPI func(http.ResponseWriter, *http.Request, []methodCall)) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/.well-known/jmap"):
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, sessionJSON)
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/api"):
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatalf("read body: %v", err)
			}
			onAPI(w, r, parseCalls(t, body))
		default:
			http.NotFound(w, r)
		}
	}))
}

func wrapWithDownload(next http.Handler, blobID, raw string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/download/") && strings.Contains(r.URL.Path, blobID) {
			w.Header().Set("Content-Type", "application/octet-stream")
			io.WriteString(w, raw)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func parseCalls(t *testing.T, body []byte) []methodCall {
	t.Helper()
	var req struct {
		MethodCalls [][]json.RawMessage `json:"methodCalls"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("unmarshal request: %v\n%s", err, body)
	}
	out := make([]methodCall, 0, len(req.MethodCalls))
	for _, call := range req.MethodCalls {
		if len(call) != 3 {
			t.Fatalf("bad call: %s", call)
		}
		var name, id string
		var args map[string]any
		json.Unmarshal(call[0], &name)
		json.Unmarshal(call[1], &args)
		json.Unmarshal(call[2], &id)
		out = append(out, methodCall{Name: name, Args: args, ID: id})
	}
	return out
}

func writeMethod(w http.ResponseWriter, sessionState, callID, name string, args map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	_ = enc.Encode(map[string]any{
		"methodResponses": []any{[]any{name, args, callID}},
		"sessionState":    sessionState,
	})
}

func writeError(w http.ResponseWriter, sessionState, callID, typ string) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	_ = enc.Encode(map[string]any{
		"methodResponses": []any{[]any{"error", map[string]any{"type": typ}, callID}},
		"sessionState":    sessionState,
	})
}

func stringIDs(v any) []string {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, x := range arr {
		out = append(out, fmt.Sprint(x))
	}
	return out
}

func mustRead(t *testing.T, r io.Reader) []byte {
	t.Helper()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func sessionDocLimits(maxGet, maxSet int) string {
	return sessionDocWithState("s1", maxGet, maxSet)
}

func sessionDocWithState(state string, maxGet, maxSet int) string {
	return fmt.Sprintf(`{
  "capabilities": {
    "urn:ietf:params:jmap:core": {
      "maxSizeUpload": 50000000,
      "maxConcurrentUpload": 8,
      "maxSizeRequest": 10000000,
      "maxConcurrentRequest": 8,
      "maxCallsInRequest": 32,
      "maxObjectsInGet": %d,
      "maxObjectsInSet": %d,
      "collationAlgorithms": ["i;ascii-casemap"]
    },
    "urn:ietf:params:jmap:mail": {},
    "urn:ietf:params:jmap:submission": {}
  },
  "accounts": {
    "A1": {
      "name": "user@example.com",
      "isPersonal": true,
      "isReadOnly": false,
      "accountCapabilities": {
        "urn:ietf:params:jmap:mail": {},
        "urn:ietf:params:jmap:submission": {}
      }
    }
  },
  "primaryAccounts": {
    "urn:ietf:params:jmap:mail": "A1",
    "urn:ietf:params:jmap:submission": "A1"
  },
  "username": "user@example.com",
  "apiUrl": "/api/",
  "downloadUrl": "/download/{accountId}/{blobId}/{name}?accept={type}",
  "uploadUrl": "/upload/{accountId}/",
  "eventSourceUrl": "/eventsource/?types={types}&closeafter={closeafter}&ping={ping}",
  "state": %q
}`, maxGet, maxSet, state)
}

// A full list hands each Email/get chunk to onPage as soon as it is read, in
// Email/query order (newest first), so stubs can land before the whole
// mailbox is listed.
func TestListMessagesPagedFullCallsOnPagePerChunk(t *testing.T) {
	const total = 600
	ids := make([]string, total)
	for i := range ids {
		ids[i] = fmt.Sprintf("E%03d", i)
	}
	srv := newJMAPServer(t, sessionDocLimits(256, 128), func(w http.ResponseWriter, r *http.Request, calls []methodCall) {
		switch calls[0].Name {
		case "Email/query":
			writeMethod(w, "s1", calls[0].ID, "Email/query", map[string]any{
				"accountId": "A1", "queryState": "q1", "canCalculateChanges": true,
				"position": 0, "ids": ids, "total": total,
			})
		case "Email/get":
			req := stringIDs(calls[0].Args["ids"])
			list := make([]map[string]any, 0, len(req))
			for _, id := range req {
				list = append(list, map[string]any{
					"id": id, "keywords": map[string]bool{}, "mailboxIds": map[string]bool{"M1": true},
					"subject": "s-" + id,
				})
			}
			writeMethod(w, "s1", calls[0].ID, "Email/get", map[string]any{
				"accountId": "A1", "state": "st1", "list": list, "notFound": []string{},
			})
		default:
			t.Fatalf("unexpected method %s", calls[0].Name)
		}
	})
	defer srv.Close()

	ad := newTestAdapter(t, srv, "A1")
	var _ psync.PagedLister = ad
	var sizes []int
	var seen []string
	headers, state, _, err := ad.ListMessagesPaged(context.Background(), psync.RemoteMailbox{RemoteID: "M1"}, func(page []psync.Header) error {
		sizes = append(sizes, len(page))
		for _, h := range page {
			if !h.HasListMeta || h.Subject != "s-"+h.RemoteID {
				t.Errorf("page header without list meta: %+v", h)
			}
			seen = append(seen, h.RemoteID)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("ListMessagesPaged: %v", err)
	}
	if fmt.Sprint(sizes) != "[256 256 88]" {
		t.Fatalf("page sizes %v, want [256 256 88]", sizes)
	}
	if strings.Join(seen, ",") != strings.Join(ids, ",") {
		t.Fatal("pages not delivered in Email/query order")
	}
	if len(headers) != total || headers[0].RemoteID != "E000" || headers[total-1].RemoteID != "E599" {
		t.Fatalf("headers len=%d, want full snapshot in query order", len(headers))
	}
	if state != "st1" {
		t.Fatalf("state %q, want st1", state)
	}
}

// An onPage error stops the list after that chunk and is returned.
func TestListMessagesPagedStopsOnPageError(t *testing.T) {
	ids := []string{"E1", "E2", "E3"}
	var gets int
	srv := newJMAPServer(t, sessionDocLimits(2, 2), func(w http.ResponseWriter, r *http.Request, calls []methodCall) {
		switch calls[0].Name {
		case "Email/query":
			writeMethod(w, "s1", calls[0].ID, "Email/query", map[string]any{
				"accountId": "A1", "queryState": "q1", "canCalculateChanges": true,
				"position": 0, "ids": ids, "total": len(ids),
			})
		case "Email/get":
			req := stringIDs(calls[0].Args["ids"])
			if len(req) > 0 {
				gets++
			}
			list := make([]map[string]any, 0, len(req))
			for _, id := range req {
				list = append(list, map[string]any{"id": id, "keywords": map[string]bool{}, "mailboxIds": map[string]bool{"M1": true}})
			}
			writeMethod(w, "s1", calls[0].ID, "Email/get", map[string]any{
				"accountId": "A1", "state": "st1", "list": list, "notFound": []string{},
			})
		default:
			t.Fatalf("unexpected method %s", calls[0].Name)
		}
	})
	defer srv.Close()

	stop := errors.New("stop")
	ad := newTestAdapter(t, srv, "A1")
	_, _, _, err := ad.ListMessagesPaged(context.Background(), psync.RemoteMailbox{RemoteID: "M1"}, func([]psync.Header) error { return stop })
	if !errors.Is(err, stop) {
		t.Fatalf("err=%v, want onPage error", err)
	}
	if gets != 1 {
		t.Fatalf("Email/get calls=%d, want 1 (list stops after the failing page)", gets)
	}
}

func writeResponses(w http.ResponseWriter, sessionState string, responses ...[]any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"methodResponses": responses,
		"sessionState":    sessionState,
	})
}

// changesPage is one Email/changes answer, keyed by the sinceState it answers.
type changesPage struct {
	created, updated, destroyed []string
	newState                    string
	more                        bool
	cannotCalculate             bool
}

// changesServer answers Email/changes plus the two back-referenced Email/get
// calls in one request, and plain Email/get by ids. emails holds the server's
// current view; an id missing from it is reported in notFound.
func changesServer(t *testing.T, pages map[string]changesPage, emails map[string]map[string]any, methods *[]string) *httptest.Server {
	t.Helper()
	get := func(ids []string) map[string]any {
		list := []map[string]any{}
		notFound := []string{}
		for _, id := range ids {
			if em, ok := emails[id]; ok {
				list = append(list, em)
			} else {
				notFound = append(notFound, id)
			}
		}
		return map[string]any{"accountId": "A1", "state": "s", "list": list, "notFound": notFound}
	}
	return newJMAPServer(t, sessionDocLimits(256, 128), func(w http.ResponseWriter, _ *http.Request, calls []methodCall) {
		for _, c := range calls {
			*methods = append(*methods, c.Name)
		}
		switch calls[0].Name {
		case "Email/changes":
			since, _ := calls[0].Args["sinceState"].(string)
			p := pages[since]
			if p.cannotCalculate {
				writeError(w, "s1", calls[0].ID, "cannotCalculateChanges")
				return
			}
			if len(calls) != 3 {
				t.Fatalf("Email/changes request has %d calls, want 3 (changes + 2 back-referenced gets)", len(calls))
			}
			for i, path := range []string{"/created", "/updated"} {
				ref, _ := calls[i+1].Args["#ids"].(map[string]any)
				if calls[i+1].Name != "Email/get" || ref["path"] != path || ref["resultOf"] != calls[0].ID {
					t.Fatalf("call %d = %s %v, want Email/get #ids %s of %s", i+1, calls[i+1].Name, ref, path, calls[0].ID)
				}
			}
			writeResponses(w, "s1",
				[]any{"Email/changes", map[string]any{
					"accountId": "A1", "oldState": since, "newState": p.newState, "hasMoreChanges": p.more,
					"created": nonNil(p.created), "updated": nonNil(p.updated), "destroyed": nonNil(p.destroyed),
				}, calls[0].ID},
				[]any{"Email/get", get(p.created), calls[1].ID},
				[]any{"Email/get", get(p.updated), calls[2].ID},
			)
		case "Email/get":
			writeMethod(w, "s1", calls[0].ID, "Email/get", get(stringIDs(calls[0].Args["ids"])))
		default:
			t.Fatalf("unexpected method %s", calls[0].Name)
		}
	})
}

func nonNil(ids []string) []string {
	if ids == nil {
		return []string{}
	}
	return ids
}

func jmapEmail(id, mailbox string, keywords map[string]bool, received string) map[string]any {
	return map[string]any{"id": id, "keywords": keywords, "mailboxIds": map[string]bool{mailbox: true}, "receivedAt": received, "subject": "s-" + id}
}

func TestListChangesOneRequestPerPage(t *testing.T) {
	var methods []string
	srv := changesServer(t,
		map[string]changesPage{"st1": {created: []string{"E3"}, updated: []string{"E2"}, destroyed: []string{"E1"}, newState: "st2"}},
		map[string]map[string]any{
			"E3": jmapEmail("E3", "M1", map[string]bool{"$flagged": true}, "2026-10-04T10:00:00Z"),
			"E2": jmapEmail("E2", "M2", nil, "2026-10-03T10:00:00Z"), // moved to another mailbox
		}, &methods)
	defer srv.Close()
	ad := newTestAdapter(t, srv, "A1")

	d, err := ad.ListChanges(context.Background(), psync.RemoteMailbox{RemoteID: "M1", StateToken: "st1"}, nil)
	if err != nil {
		t.Fatalf("ListChanges: %v", err)
	}
	if len(d.Changed) != 1 || d.Changed[0].RemoteID != "E3" || d.Changed[0].Flags != storage.FlagFlagged || !d.Changed[0].HasListMeta {
		t.Fatalf("Changed = %+v", d.Changed)
	}
	if strings.Join(d.Removed, ",") != "E1,E2" {
		t.Fatalf("Removed = %v, want [E1 E2]", d.Removed)
	}
	if d.Cursor != "st2" || d.Unchanged || d.Members != nil {
		t.Fatalf("delta = %+v", d)
	}
	if ad.Requests() != 1 {
		t.Fatalf("requests = %d, want 1", ad.Requests())
	}
	for _, m := range methods {
		if m == "Email/query" {
			t.Fatal("a delta must not query the whole mailbox")
		}
	}
}

func TestListChangesFollowsHasMoreChanges(t *testing.T) {
	var methods []string
	srv := changesServer(t, map[string]changesPage{
		"st1": {created: []string{"E5"}, newState: "st2", more: true},
		"st2": {destroyed: []string{"E5"}, newState: "st3"},
	}, map[string]map[string]any{"E5": jmapEmail("E5", "M1", nil, "2026-10-04T10:00:00Z")}, &methods)
	defer srv.Close()
	ad := newTestAdapter(t, srv, "A1")

	d, err := ad.ListChanges(context.Background(), psync.RemoteMailbox{RemoteID: "M1", StateToken: "st1"}, nil)
	if err != nil {
		t.Fatalf("ListChanges: %v", err)
	}
	if len(d.Changed) != 0 || len(d.Removed) != 1 || d.Removed[0] != "E5" || d.Cursor != "st3" {
		t.Fatalf("delta = %+v, want E5 removed only, cursor st3", d)
	}
	if ad.Requests() != 2 {
		t.Fatalf("requests = %d, want 2", ad.Requests())
	}
}

func TestListChangesNotFoundIsRemoved(t *testing.T) {
	var methods []string
	srv := changesServer(t, map[string]changesPage{"st1": {created: []string{"E7"}, newState: "st2"}}, map[string]map[string]any{}, &methods)
	defer srv.Close()
	d, err := newTestAdapter(t, srv, "A1").ListChanges(context.Background(), psync.RemoteMailbox{RemoteID: "M1", StateToken: "st1"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Changed) != 0 || len(d.Removed) != 1 || d.Removed[0] != "E7" {
		t.Fatalf("delta = %+v, want E7 removed", d)
	}
}

func TestListChangesCannotCalculateNeedsFullList(t *testing.T) {
	var methods []string
	srv := changesServer(t, map[string]changesPage{"old": {cannotCalculate: true}}, nil, &methods)
	defer srv.Close()
	_, err := newTestAdapter(t, srv, "A1").ListChanges(context.Background(), psync.RemoteMailbox{RemoteID: "M1", StateToken: "old"}, nil)
	if !errors.Is(err, psync.ErrNeedFullList) {
		t.Fatalf("err = %v, want ErrNeedFullList", err)
	}
}

func TestListChangesWithoutTokenNeedsFullList(t *testing.T) {
	ad := NewAdapter(nil, "A1")
	if _, err := ad.ListChanges(context.Background(), psync.RemoteMailbox{RemoteID: "M1"}, nil); !errors.Is(err, psync.ErrNeedFullList) {
		t.Fatalf("err = %v, want ErrNeedFullList", err)
	}
}

func TestListChangesReportsEnsureIDs(t *testing.T) {
	var methods []string
	srv := changesServer(t, map[string]changesPage{"st1": {newState: "st1"}},
		map[string]map[string]any{"E2": jmapEmail("E2", "M1", map[string]bool{"$seen": true}, "2026-10-01T10:00:00Z")}, &methods)
	defer srv.Close()
	d, err := newTestAdapter(t, srv, "A1").ListChanges(context.Background(), psync.RemoteMailbox{RemoteID: "M1", StateToken: "st1"}, []string{"E2", "E9"})
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Changed) != 1 || d.Changed[0].RemoteID != "E2" || d.Changed[0].Flags != storage.FlagSeen {
		t.Fatalf("Changed = %+v, want E2 with its server flags", d.Changed)
	}
	if len(d.Removed) != 1 || d.Removed[0] != "E9" {
		t.Fatalf("Removed = %v, want [E9] (ensure id gone from the server)", d.Removed)
	}
	if d.Unchanged {
		t.Fatal("a delta with ensure ids is never Unchanged")
	}
}

func TestListChangesUnchanged(t *testing.T) {
	var methods []string
	srv := changesServer(t, map[string]changesPage{"st1": {newState: "st1"}}, nil, &methods)
	defer srv.Close()
	d, err := newTestAdapter(t, srv, "A1").ListChanges(context.Background(), psync.RemoteMailbox{RemoteID: "M1", StateToken: "st1"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !d.Unchanged || d.Cursor != "st1" {
		t.Fatalf("delta = %+v, want Unchanged at st1", d)
	}
}

// ListMessages always lists in full: the stored token is for ListChanges.
func TestListMessagesIgnoresStateToken(t *testing.T) {
	srv := newJMAPServer(t, sessionDocLimits(256, 128), func(w http.ResponseWriter, r *http.Request, calls []methodCall) {
		switch calls[0].Name {
		case "Email/query":
			writeMethod(w, "s1", calls[0].ID, "Email/query", map[string]any{
				"accountId": "A1", "queryState": "q1", "canCalculateChanges": true,
				"position": 0, "ids": []string{"E1"}, "total": 1,
			})
		case "Email/get":
			writeMethod(w, "s1", calls[0].ID, "Email/get", map[string]any{
				"accountId": "A1", "state": "st1",
				"list":     []map[string]any{jmapEmail("E1", "M1", nil, "2024-06-01T12:00:00Z")},
				"notFound": []string{},
			})
		default:
			t.Fatalf("unexpected method %s", calls[0].Name)
		}
	})
	defer srv.Close()

	ad := newTestAdapter(t, srv, "A1")
	headers, _, _, err := ad.ListMessages(context.Background(), psync.RemoteMailbox{RemoteID: "M1", StateToken: "st1"})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(headers) != 1 {
		t.Fatalf("headers = %d, want 1", len(headers))
	}
}
