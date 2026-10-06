package desktop

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/peltonapp/Pelton/internal/credentials"
	"github.com/peltonapp/Pelton/internal/storage"
)

// newJMAPInboxFixture serves one JMAP inbox of n messages (m01 oldest, mNN
// newest) over httptest and returns the app, the account and the db. Mailbox
// discovery has not run yet.
func newJMAPInboxFixture(t *testing.T, n int) (*App, storage.Account, *storage.DB) {
	t.Helper()
	ctx, stopBackground := testContext(t)
	t.Cleanup(stopBackground)
	db, err := storage.Open(filepath.Join(t.TempDir(), "targets.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.RunMigrations(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	idx := map[string]int{}
	var ids []string
	for i := n; i >= 1; i-- {
		id := fmt.Sprintf("m%02d", i)
		idx[id] = i
		ids = append(ids, id)
	}
	mailbox := map[string]any{
		"id": "MbInbox", "name": "INBOX", "role": "inbox", "sortOrder": 0,
		"totalEmails": n, "unreadEmails": 0, "totalThreads": n, "unreadThreads": 0,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/.well-known/jmap"):
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, metricsSessionDoc(256, 128))
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/api"):
			body, _ := io.ReadAll(r.Body)
			calls := metricsParseCalls(t, body)
			c := calls[0]
			switch c.Name {
			case "Mailbox/get":
				writeMetricsMethod(w, "s1", c.ID, c.Name, map[string]any{
					"accountId": "A1", "state": "mb1", "list": []any{mailbox}, "notFound": []string{},
				})
			case "Email/changes":
				// A delta sends Email/changes with two back-referenced Email/get calls.
				emptyGet := map[string]any{"accountId": "A1", "state": "em1", "list": []any{}, "notFound": []string{}}
				responses := []any{[]any{c.Name, map[string]any{
					"accountId": "A1", "oldState": "em1", "newState": "em1",
					"hasMoreChanges": false, "created": []string{}, "updated": []string{}, "destroyed": []string{},
				}, c.ID}}
				for _, ref := range calls[1:] {
					responses = append(responses, []any{"Email/get", emptyGet, ref.ID})
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"methodResponses": responses, "sessionState": "s1"})
			case "Email/query":
				writeMetricsMethod(w, "s1", c.ID, c.Name, map[string]any{
					"accountId": "A1", "queryState": "q1", "canCalculateChanges": true,
					"position": 0, "ids": ids, "total": len(ids),
				})
			case "Email/get":
				bodyFetch := metricsEmailProps(c.Args)["blobId"]
				list := []map[string]any{}
				for _, id := range metricsStringIDs(c.Args["ids"]) {
					i, ok := idx[id]
					if !ok {
						continue
					}
					em := map[string]any{
						"id": id, "keywords": map[string]bool{},
						"mailboxIds": map[string]bool{"MbInbox": true},
						"receivedAt": fmt.Sprintf("2024-06-%02dT12:00:00Z", i),
					}
					if bodyFetch {
						em["blobId"] = "blob-" + id
					} else {
						em["subject"] = id
						em["preview"] = "preview-" + id
						em["from"] = []map[string]any{{"email": "a@ex.test"}}
						em["to"] = []map[string]any{{"email": "b@ex.test"}}
						em["size"] = 10
						em["hasAttachment"] = false
					}
					list = append(list, em)
				}
				writeMetricsMethod(w, "s1", c.ID, c.Name, map[string]any{
					"accountId": "A1", "state": "em1", "list": list, "notFound": []string{},
				})
			default:
				t.Errorf("unexpected JMAP method %q", c.Name)
			}
		case strings.Contains(r.URL.Path, "/download/"):
			w.Header().Set("Content-Type", "application/octet-stream")
			// Real messages carry their own Date; the body fetch rewrites the row
			// date from it, so the fixture must keep it consistent with the stub.
			day := 1
			if k := strings.LastIndex(r.URL.Path, "blob-m"); k >= 0 {
				fmt.Sscanf(r.URL.Path[k:], "blob-m%d", &day)
			}
			fmt.Fprintf(w, "Date: Sat, %02d Jun 2024 12:00:00 +0000\r\nSubject: t\r\n\r\nbody\r\n", day)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	accountID, err := db.CreateAccount(ctx, &storage.Account{
		Email: "user@example.com", Protocol: "jmap",
		JMAPSessionURL: srv.URL + "/.well-known/jmap", JMAPMailAccountID: "A1",
	})
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	if err := credentials.Store(accountID, credentials.Secret{
		Method: credentials.MethodPassword, Password: "secret",
	}); err != nil {
		t.Fatalf("store credentials: %v", err)
	}
	t.Cleanup(func() { _ = credentials.Delete(accountID) })
	account, err := db.GetAccount(ctx, accountID)
	if err != nil {
		t.Fatalf("get account: %v", err)
	}
	a := &App{ctx: ctx, store: db, log: slog.New(slog.DiscardHandler)}
	// The scheduler outlives a single sync pass, as the account worker's does.
	a.ensureAccountScheduler(ctx, account.ID)
	t.Cleanup(func() { a.stopAccountScheduler(account.ID) })
	return a, *account, db
}

// inboxBodyState returns the body_complete remote ids and the total stub count.
func inboxBodyState(t *testing.T, a *App, accountID int64) (complete map[string]bool, total int) {
	t.Helper()
	folders, err := a.store.ListFolders(a.ctx, accountID)
	if err != nil || len(folders) == 0 {
		t.Fatalf("folders: %v %v", folders, err)
	}
	msgs, err := a.store.ListMessages(a.ctx, folders[0].ID, 0)
	if err != nil {
		t.Fatalf("list messages: %v", err)
	}
	complete = map[string]bool{}
	for _, m := range msgs {
		if m.BodyComplete {
			complete[m.RemoteID] = true
		}
	}
	return complete, len(msgs)
}

// seedInboxStubs stores the inbox with n list-only stubs and no bodies, as an
// interrupted campaign leaves it: the stub list is complete, bodies are missing.
func seedInboxStubs(t *testing.T, a *App, accountID int64, n int) {
	t.Helper()
	folder := storage.Folder{AccountID: accountID, Name: "INBOX", IMAPPath: "INBOX", RemoteID: "MbInbox"}
	if _, err := a.store.CreateFolder(a.ctx, &folder); err != nil {
		t.Fatalf("create folder: %v", err)
	}
	for i := 1; i <= n; i++ {
		if _, err := a.store.UpsertMessageListMeta(a.ctx, &storage.Message{
			AccountID: accountID, FolderID: folder.ID, RemoteID: fmt.Sprintf("m%02d", i),
			Subject: fmt.Sprintf("m%02d", i), Date: time.Date(2024, 6, i, 12, 0, 0, 0, time.UTC),
		}); err != nil {
			t.Fatalf("seed stub: %v", err)
		}
	}
}

func TestJMAPRestartFillsBodiesOnlyForNewestX(t *testing.T) {
	a, account, db := newJMAPInboxFixture(t, 5)
	seedInboxStubs(t, a, account.ID, 5)
	if err := db.SetInt(a.ctx, settingSyncMessageLimit, 2); err != nil {
		t.Fatal(err)
	}
	// Reconcile has nothing new to fetch (every id has a row), so bodies must
	// come from the stored stubs. Run twice: a restart must not creep older.
	for pass := 1; pass <= 2; pass++ {
		if err := a.syncAccountOnceJMAP(a.ctx, account); err != nil {
			t.Fatalf("sync pass %d: %v", pass, err)
		}
		complete, total := inboxBodyState(t, a, account.ID)
		if total != 5 || len(complete) != 2 || !complete["m05"] || !complete["m04"] || complete["m03"] {
			t.Fatalf("pass %d: total=%d complete=%v, want exactly m05,m04", pass, total, complete)
		}
	}
}

func raiseLimitAndWait(t *testing.T, a *App, db *storage.DB, accountID int64, from, to, want int) {
	t.Helper()
	if err := db.SetInt(a.ctx, settingSyncMessageLimit, to); err != nil {
		t.Fatal(err)
	}
	a.enqueueSyncLimitDelta(from, to)
	deadline := time.Now().Add(10 * time.Second)
	for {
		complete, total := inboxBodyState(t, a, accountID)
		if total == 5 && len(complete) == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("limit %d -> %d: bodies %v of %d, want %d", from, to, complete, total, want)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestJMAPLimitRaiseFetchesRemainingBodies(t *testing.T) {
	for _, tc := range []struct {
		name     string
		to, want int
		missing  string
	}{
		{"finite 2 to 4", 4, 4, "m01"},
		{"all", 0, 5, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, account, db := newJMAPInboxFixture(t, 5)
			if err := db.SetInt(a.ctx, settingSyncMessageLimit, 2); err != nil {
				t.Fatal(err)
			}
			if err := a.syncAccountOnceJMAP(a.ctx, account); err != nil {
				t.Fatalf("first sync: %v", err)
			}
			if complete, _ := inboxBodyState(t, a, account.ID); len(complete) != 2 {
				t.Fatalf("after X=2: complete=%v, want 2", complete)
			}
			raiseLimitAndWait(t, a, db, account.ID, 2, tc.to, tc.want)
			complete, _ := inboxBodyState(t, a, account.ID)
			if tc.missing != "" && complete[tc.missing] {
				t.Fatalf("%s is outside the new window but got a body", tc.missing)
			}
			if tc.to == 4 && (!complete["m03"] || !complete["m02"]) {
				t.Fatalf("complete=%v, want m03 and m02 fetched", complete)
			}
		})
	}
}

// A JMAP folder can carry a floor (a capped SyncFolder sets one). Scroll
// backfill then widens stubs and must fetch bodies for the next page beyond
// the bodies already cached, on every scroll and not only the first.
func TestJMAPScrollBackfillFetchesNextPageOfBodies(t *testing.T) {
	a, account, db := newJMAPInboxFixture(t, 7)
	if err := db.SetInt(a.ctx, settingSyncMessageLimit, 2); err != nil {
		t.Fatal(err)
	}
	folder := storage.Folder{AccountID: account.ID, Name: "INBOX", IMAPPath: "INBOX", RemoteID: "MbInbox", SyncFloorID: "m06"}
	if _, err := db.CreateFolder(a.ctx, &folder); err != nil {
		t.Fatalf("create folder: %v", err)
	}
	for i := 7; i >= 1; i-- {
		m := &storage.Message{
			AccountID: account.ID, FolderID: folder.ID, RemoteID: fmt.Sprintf("m%02d", i),
			Subject: fmt.Sprintf("m%02d", i), Date: time.Date(2024, 6, i, 12, 0, 0, 0, time.UTC),
		}
		var err error
		if i >= 6 {
			m.BodyPlain = "body"
			_, err = db.InsertMessageWithAttachments(a.ctx, m, nil)
		} else {
			_, err = db.UpsertMessageListMeta(a.ctx, m)
		}
		if err != nil {
			t.Fatalf("seed m%02d: %v", i, err)
		}
	}
	pending, err := a.foldersWithOlder([]int64{folder.ID})
	if err != nil || len(pending) != 1 {
		t.Fatalf("foldersWithOlder = %v, %v; want the floored folder", pending, err)
	}
	for scroll, want := range [][]string{{"m07", "m06", "m05", "m04"}, {"m07", "m06", "m05", "m04", "m03", "m02"}} {
		if _, _, err := a.backfillAccount(account.ID, []storage.Folder{folder}); err != nil {
			t.Fatalf("backfill %d: %v", scroll+1, err)
		}
		complete, total := inboxBodyState(t, a, account.ID)
		if total != 7 || len(complete) != len(want) {
			t.Fatalf("scroll %d: total=%d complete=%v, want exactly %v", scroll+1, total, complete, want)
		}
		for _, id := range want {
			if !complete[id] {
				t.Fatalf("scroll %d: complete=%v, want exactly %v", scroll+1, complete, want)
			}
		}
	}
}
