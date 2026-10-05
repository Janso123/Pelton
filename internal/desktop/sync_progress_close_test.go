package desktop

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/peltonapp/Pelton/internal/credentials"
	pimap "github.com/peltonapp/Pelton/internal/imap"
	"github.com/peltonapp/Pelton/internal/storage"
)

// progressRecorder collects the progress events of one account and cancels the
// run when its first non-closing event arrives.
type progressRecorder struct {
	mu     sync.Mutex
	events []SyncProgressEvent
	cancel context.CancelFunc
}

func (r *progressRecorder) hook(accountID int64) func(SyncProgressEvent) {
	return func(e SyncProgressEvent) {
		if e.AccountID != accountID {
			return
		}
		r.mu.Lock()
		r.events = append(r.events, e)
		r.mu.Unlock()
		if e.Folder != "" && r.cancel != nil {
			r.cancel()
		}
	}
}

func (r *progressRecorder) snapshot() []SyncProgressEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]SyncProgressEvent(nil), r.events...)
}

func closings(events []SyncProgressEvent) int {
	n := 0
	for _, e := range events {
		if e.Folder == "" {
			n++
		}
	}
	return n
}

// blockingIMAP parks SELECT until gate closes, so a run is always mid-folder
// when the test cancels it.
type blockingIMAP struct {
	fakeIMAP
	gate chan struct{}
}

func (b *blockingIMAP) Select(mailbox string) (*pimap.Mailbox, error) {
	<-b.gate
	return &pimap.Mailbox{Name: mailbox}, nil
}

func TestIMAPInitialSyncCancelledRunClosesProgress(t *testing.T) {
	a := newJMAPSwitchTestApp(t)
	id, _ := seedSwitchAccount(t, a, "a@example.test", "imap")
	acc, err := a.store.GetAccount(a.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	gate := make(chan struct{})
	t.Cleanup(func() { close(gate) })
	a.newIMAPClient = func(pimap.Config) (mailClient, error) {
		return &blockingIMAP{gate: gate}, nil
	}
	ctx, cancel := context.WithCancel(a.ctx)
	defer cancel()
	rec := &progressRecorder{cancel: cancel}
	a.syncProgressEmitForTest = rec.hook(id)

	if err := a.syncIMAPInitialPass(ctx, *acc, nil); err == nil {
		t.Fatal("cancelled run returned nil")
	}
	ev := rec.snapshot()
	if len(ev) == 0 || ev[len(ev)-1].Folder != "" {
		t.Fatalf("last progress event %+v, want a closing one", ev)
	}
	if n := closings(ev); n != 1 {
		t.Fatalf("closing events = %d, want 1", n)
	}
}

// newMiniJMAPAccount seeds a JMAP account served by a one-mailbox server. With
// block set, every call after Mailbox/get hangs, so a run stays mid-folder;
// without it the mailbox is empty and the run completes.
func newMiniJMAPAccount(t *testing.T, a *App, block bool) *storage.Account {
	t.Helper()
	gate := make(chan struct{})
	session := metricsSessionDoc(256, 128)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/.well-known/jmap"):
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, session)
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/api"):
			body, _ := io.ReadAll(r.Body)
			calls := metricsParseCalls(t, body)
			switch {
			case calls[0].Name == "Mailbox/get":
				writeMetricsMethod(w, "s1", calls[0].ID, "Mailbox/get", map[string]any{
					"accountId": "A1", "state": "mb1", "notFound": []string{},
					"list": []map[string]any{{
						"id": "MbInbox", "name": "INBOX", "role": "inbox", "sortOrder": 0,
						"totalEmails": 0, "unreadEmails": 0, "totalThreads": 0, "unreadThreads": 0,
					}},
				})
			case block:
				select {
				case <-gate:
				case <-r.Context().Done():
				}
			case calls[0].Name == "Email/query":
				writeMetricsMethod(w, "s1", calls[0].ID, "Email/query", map[string]any{
					"accountId": "A1", "queryState": "q1", "canCalculateChanges": true,
					"position": 0, "ids": []string{}, "total": 0,
				})
			default:
				writeMetricsMethod(w, "s1", calls[0].ID, calls[0].Name, map[string]any{
					"accountId": "A1", "state": "em1", "list": []any{}, "notFound": []string{},
				})
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(gate) })

	id, err := a.store.CreateAccount(a.ctx, &storage.Account{
		Email: "j@example.test", Protocol: "jmap",
		JMAPSessionURL: srv.URL + "/.well-known/jmap", JMAPMailAccountID: "A1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := credentials.Store(id, credentials.Secret{Method: credentials.MethodPassword, Password: "secret"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = credentials.Delete(id) })
	acc, err := a.store.GetAccount(a.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.stopAccountScheduler(id) })
	return acc
}

func TestJMAPCancelledRunClosesProgress(t *testing.T) {
	a := newJMAPSwitchTestApp(t)
	acc := newMiniJMAPAccount(t, a, true)
	id := acc.ID

	ctx, cancel := context.WithCancel(a.ctx)
	defer cancel()
	rec := &progressRecorder{cancel: cancel}
	a.syncProgressEmitForTest = rec.hook(id)

	done := make(chan error, 1)
	go func() { done <- a.syncAccountOnceJMAP(ctx, *acc) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled run returned nil")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not stop after cancel")
	}
	ev := rec.snapshot()
	if len(ev) == 0 || ev[len(ev)-1].Folder != "" {
		t.Fatalf("last progress event %+v, want a closing one", ev)
	}
	if n := closings(ev); n != 1 {
		t.Fatalf("closing events = %d, want 1", n)
	}
}

func TestJMAPCompletedRunClosesProgressOnce(t *testing.T) {
	a := newJMAPSwitchTestApp(t)
	acc := newMiniJMAPAccount(t, a, false)
	rec := &progressRecorder{}
	a.syncProgressEmitForTest = rec.hook(acc.ID)

	if err := a.syncAccountOnceJMAP(a.ctx, *acc); err != nil {
		t.Fatalf("sync: %v", err)
	}
	ev := rec.snapshot()
	if len(ev) == 0 || ev[len(ev)-1].Folder != "" {
		t.Fatalf("last progress event %+v, want a closing one", ev)
	}
	if n := closings(ev); n != 1 {
		t.Fatalf("closing events = %d, want 1", n)
	}
}
