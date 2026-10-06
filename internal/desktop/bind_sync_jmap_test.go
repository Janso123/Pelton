package desktop

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/peltonapp/Pelton/internal/credentials"
	"github.com/peltonapp/Pelton/internal/desktop/syncsched"
	pjmap "github.com/peltonapp/Pelton/internal/jmap"
	"github.com/peltonapp/Pelton/internal/outbox"
	"github.com/peltonapp/Pelton/internal/storage"
	psync "github.com/peltonapp/Pelton/internal/sync"
	"github.com/peltonapp/Pelton/internal/sync/pool"
)

func TestAccountTransmitterJMAPSkipsSMTP(t *testing.T) {
	ctx, stopBackground := testContext(t)
	db, err := storage.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	t.Cleanup(stopBackground)
	if err := db.RunMigrations(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	var sessionGETs atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/.well-known/jmap") {
			sessionGETs.Add(1)
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{
  "capabilities": {
    "urn:ietf:params:jmap:core": {
      "maxSizeUpload": 50000000, "maxConcurrentUpload": 8,
      "maxSizeRequest": 10000000, "maxConcurrentRequest": 8,
      "maxCallsInRequest": 32, "maxObjectsInGet": 256, "maxObjectsInSet": 128,
      "collationAlgorithms": ["i;ascii-casemap"]
    },
    "urn:ietf:params:jmap:mail": {},
    "urn:ietf:params:jmap:submission": {}
  },
  "accounts": {
    "A1": {
      "name": "user@example.com", "isPersonal": true, "isReadOnly": false,
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
  "state": "s1"
}`)
			return
		}
		if strings.Contains(r.URL.Path, "/api") {
			t.Fatal("JMAP API must not be called when submit is stubbed")
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	accountID, err := db.CreateAccount(ctx, &storage.Account{
		Email:             "user@example.com",
		Protocol:          "jmap",
		JMAPSessionURL:    srv.URL + "/.well-known/jmap",
		JMAPMailAccountID: "A1",
		SMTPHost:          "smtp.should-not-dial.invalid",
		SMTPPort:          587,
	})
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	if err := credentials.Store(accountID, credentials.Secret{
		Method:   credentials.MethodPassword,
		Password: "secret",
	}); err != nil {
		t.Fatalf("store credentials: %v", err)
	}
	t.Cleanup(func() { _ = credentials.Delete(accountID) })

	if _, err := db.CreateFolder(ctx, &storage.Folder{
		AccountID:  accountID,
		Name:       "Sent",
		IMAPPath:   "MbSent",
		RemoteID:   "MbSent",
		Attributes: []string{`\Sent`},
	}); err != nil {
		t.Fatalf("create sent: %v", err)
	}

	var submitCalls atomic.Int32
	old := submit
	submit = func(ctx context.Context, client *pjmap.Client, mailAccountID, sentMailboxID string, raw []byte, accountEmail, from string, recipients []string) error {
		submitCalls.Add(1)
		if client == nil {
			t.Fatal("nil client")
		}
		if mailAccountID != "A1" || sentMailboxID != "MbSent" {
			t.Fatalf("ids: mail=%q sent=%q", mailAccountID, sentMailboxID)
		}
		if accountEmail != "user@example.com" || from != "user@example.com" {
			t.Fatalf("from/email: %q %q", accountEmail, from)
		}
		if string(raw) != "raw-body" || len(recipients) != 1 || recipients[0] != "a@b.c" {
			t.Fatalf("payload: raw=%q recipients=%v", raw, recipients)
		}
		return nil
	}
	t.Cleanup(func() { submit = old })

	if err := db.SetInt(ctx, settingSyncMaxParallel, 1); err != nil {
		t.Fatalf("set parallel: %v", err)
	}
	a := &App{ctx: ctx, store: db, log: slog.New(slog.DiscardHandler)}
	a.ensureAccountScheduler(ctx, accountID)
	t.Cleanup(func() { a.stopAccountScheduler(accountID) })
	rt := a.accountSync(accountID)
	// The only sync slot is busy. EmailSubmission must still send without
	// checking it out.
	if err := rt.pool.Acquire(ctx, pool.Background); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	t.Cleanup(func() { rt.pool.Release(pool.Background) })

	tx := &accountTransmitter{app: a}
	txCtx, txCancel := context.WithTimeout(ctx, time.Second)
	defer txCancel()
	err = tx.Transmit(txCtx, outbox.Message{
		AccountID:    accountID,
		EnvelopeFrom: "user@example.com",
		Recipients:   []string{"a@b.c"},
		Raw:          []byte("raw-body"),
	})
	if err != nil {
		t.Fatalf("Transmit: %v", err)
	}
	if submitCalls.Load() != 1 {
		t.Fatalf("submit calls: got %d want 1", submitCalls.Load())
	}
	if sessionGETs.Load() < 1 {
		t.Fatal("expected JMAP Authenticate against session URL")
	}
}

func TestJMAPAutoSyncSkippedWhenPushHealthy(t *testing.T) {
	a := &App{}
	jmap := storage.Account{ID: 7, Protocol: "jmap"}
	imap := storage.Account{ID: 8, Protocol: "imap"}
	local := storage.Account{ID: 9, Protocol: "jmap", Local: true}

	if !a.autoSyncAccount(jmap) {
		t.Fatal("jmap without a healthy push should auto-sync")
	}
	if !a.autoSyncAccount(imap) {
		t.Fatal("imap should auto-sync")
	}
	if a.autoSyncAccount(local) {
		t.Fatal("local account should not auto-sync")
	}

	a.setJMAPPushHealthy(jmap.ID, true)
	if a.autoSyncAccount(jmap) {
		t.Fatal("healthy jmap push should skip auto-sync")
	}
	if !a.autoSyncAccount(imap) {
		t.Fatal("imap auto-sync should not follow jmap push")
	}

	a.setJMAPPushHealthy(jmap.ID, false)
	if !a.autoSyncAccount(jmap) {
		t.Fatal("clearing jmap push should resume auto-sync")
	}
}

func TestJMAPStubJobsCoverAllFoldersBeforeBodies(t *testing.T) {
	folders := []storage.Folder{
		{ID: 2, Name: "Archive", IMAPPath: "Archive"},
		{ID: 1, Name: "INBOX", IMAPPath: "INBOX"},
		{ID: 3, Name: "Nope", IMAPPath: "Nope", SyncExcluded: true},
	}
	inbox := folders[1]

	var mu sync.Mutex
	var stubs []string
	bodies := map[string][]string{}
	releaseStubs := make(chan struct{})
	var stubStarts int

	s := syncsched.New(1)
	s.Start(context.Background(), pool.New(3), 0)
	t.Cleanup(s.Stop)

	done := make(chan error, 1)
	enqueueJMAPPhases(s, folders, 1,
		func(ctx context.Context, folder storage.Folder) ([]jmapListed, error) {
			mu.Lock()
			stubs = append(stubs, folder.IMAPPath)
			stubStarts++
			start := stubStarts
			mu.Unlock()
			if start < 2 {
				<-releaseStubs
			} else {
				close(releaseStubs)
			}
			if folder.ID == inbox.ID {
				return []jmapListed{
					{Folder: folder, RemoteID: "in-new", Date: time.Date(2024, 6, 2, 0, 0, 0, 0, time.UTC)},
					{Folder: folder, RemoteID: "in-old", Date: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)},
				}, nil
			}
			return []jmapListed{
				{Folder: folder, RemoteID: "ar-new", Date: time.Date(2024, 6, 4, 0, 0, 0, 0, time.UTC)},
				{Folder: folder, RemoteID: "ar-old", Date: time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)},
			}, nil
		},
		func(ctx context.Context, folder storage.Folder) error {
			ids := syncsched.RemoteIDs(ctx)
			mu.Lock()
			bodies[folder.IMAPPath] = append(bodies[folder.IMAPPath], ids...)
			mu.Unlock()
			return nil
		},
		done,
	)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("jmap phases: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("jmap phases did not finish")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(stubs) != 2 || !containsAll(stubs, "INBOX", "Archive") {
		t.Fatalf("stub folders %v, want INBOX and Archive", stubs)
	}
	for _, path := range stubs {
		if path == "Nope" {
			t.Fatal("excluded folder was listed")
		}
	}
	if got := bodies["INBOX"]; len(got) != 1 || got[0] != "in-new" {
		t.Fatalf("inbox bodies %v, want [in-new]", got)
	}
	if got := bodies["Archive"]; len(got) != 1 || got[0] != "ar-new" {
		t.Fatalf("archive bodies %v, want [ar-new]", got)
	}
	if _, ok := bodies["Nope"]; ok {
		t.Fatal("excluded folder fetched bodies")
	}
}

func TestJMAPFiniteXBodyJobsPreferInbox(t *testing.T) {
	folders := []storage.Folder{
		{ID: 2, Name: "Archive", IMAPPath: "Archive"},
		{ID: 1, Name: "INBOX", IMAPPath: "INBOX"},
	}
	var mu sync.Mutex
	var order []string

	s := syncsched.New(1)
	s.Start(context.Background(), pool.New(1), 0)
	t.Cleanup(s.Stop)
	done := make(chan error, 1)
	enqueueJMAPPhases(s, folders, 1,
		func(ctx context.Context, folder storage.Folder) ([]jmapListed, error) {
			return []jmapListed{{
				Folder: folder, RemoteID: folder.IMAPPath + "-1",
				Date: time.Date(2024, 6, int(folder.ID), 0, 0, 0, 0, time.UTC),
			}}, nil
		},
		func(ctx context.Context, folder storage.Folder) error {
			ids := syncsched.RemoteIDs(ctx)
			mu.Lock()
			order = append(order, folder.IMAPPath+":"+strings.Join(ids, ","))
			mu.Unlock()
			return nil
		},
		done,
	)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("jmap phases: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("jmap phases did not finish")
	}
	mu.Lock()
	defer mu.Unlock()
	want := []string{"INBOX:INBOX-1", "Archive:Archive-1"}
	if len(order) != len(want) {
		t.Fatalf("body order %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("body order %v, want %v", order, want)
		}
	}
}

func TestJMAPAllLocalBodiesNewestFirst(t *testing.T) {
	inbox := storage.Folder{ID: 1, Name: "INBOX", IMAPPath: "INBOX"}
	archive := storage.Folder{ID: 2, Name: "Archive", IMAPPath: "Archive"}
	folders := []storage.Folder{archive, inbox}

	var mu sync.Mutex
	var order []string
	s := syncsched.New(1)
	s.Start(context.Background(), pool.New(1), 0)
	t.Cleanup(s.Stop)
	done := make(chan error, 1)
	enqueueJMAPPhases(s, folders, 0,
		func(ctx context.Context, folder storage.Folder) ([]jmapListed, error) {
			if folder.ID == inbox.ID {
				return []jmapListed{
					{Folder: folder, RemoteID: "I1", Date: time.Date(2024, 6, 3, 0, 0, 0, 0, time.UTC)},
					{Folder: folder, RemoteID: "I2", Date: time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC)},
				}, nil
			}
			return []jmapListed{
				{Folder: folder, RemoteID: "A1", Date: time.Date(2024, 6, 4, 0, 0, 0, 0, time.UTC)},
				{Folder: folder, RemoteID: "A2", Date: time.Date(2024, 6, 2, 0, 0, 0, 0, time.UTC)},
			}, nil
		},
		func(ctx context.Context, folder storage.Folder) error {
			ids := syncsched.RemoteIDs(ctx)
			mu.Lock()
			for _, id := range ids {
				order = append(order, id)
			}
			mu.Unlock()
			return nil
		},
		done,
	)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("jmap phases: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("jmap phases did not finish")
	}
	mu.Lock()
	defer mu.Unlock()
	want := []string{"A1", "I1", "A2", "I2"}
	if len(order) != len(want) {
		t.Fatalf("body order %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("body order %v, want %v", order, want)
		}
	}
}

func TestJMAPBodySoftPauseYieldsToLiveThenResumes(t *testing.T) {
	folders := []storage.Folder{
		{ID: 2, Name: "Archive", IMAPPath: "Archive"},
		{ID: 1, Name: "INBOX", IMAPPath: "INBOX"},
	}
	var mu sync.Mutex
	var order []string
	var inboxFetchCalls int
	phaseDone := make(chan struct{})

	s := syncsched.New(1)
	// maxBg=1 so the second folder's body chunk cannot start until the first completes.
	s.Start(context.Background(), pool.New(2), 0)
	t.Cleanup(s.Stop)
	done := make(chan error, 1)
	rec := func(name string) {
		mu.Lock()
		order = append(order, name)
		if len(order) == 4 {
			close(phaseDone)
		}
		mu.Unlock()
	}
	enqueueJMAPPhases(s, folders, 1,
		func(ctx context.Context, folder storage.Folder) ([]jmapListed, error) {
			return []jmapListed{{
				Folder: folder, RemoteID: folder.IMAPPath + "-1",
				Date: time.Date(2024, 6, int(folder.ID), 0, 0, 0, 0, time.UTC),
			}}, nil
		},
		func(ctx context.Context, folder storage.Folder) error {
			mu.Lock()
			if folder.IMAPPath == "INBOX" {
				inboxFetchCalls++
			}
			n := inboxFetchCalls
			mu.Unlock()
			if folder.IMAPPath != "INBOX" {
				rec("bg:" + folder.IMAPPath)
				return nil
			}
			if n == 1 {
				rec("bg:INBOX")
				s.Enqueue(syncsched.Job{
					Priority: syncsched.PriorityLive,
					Kind:     syncsched.JobNewMail,
					Run: func(context.Context) error {
						rec("live")
						s.ClearSoftPause()
						return nil
					},
				})
				s.RequestSoftPause()
				return psync.ErrSoftPaused
			}
			rec("bg:INBOX-resume")
			return nil
		},
		done,
	)

	select {
	case <-phaseDone:
	case <-time.After(2 * time.Second):
		mu.Lock()
		o := append([]string(nil), order...)
		mu.Unlock()
		t.Fatalf("expected full body sequence, order so far %v", o)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("jmap phases: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("jmap phases did not finish after soft-pause resume")
	}

	mu.Lock()
	defer mu.Unlock()
	want := []string{"bg:INBOX", "live", "bg:INBOX-resume", "bg:Archive"}
	if len(order) != len(want) {
		t.Fatalf("order %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("order %v, want %v", order, want)
		}
	}
	if inboxFetchCalls != 2 {
		t.Fatalf("inbox fetch calls %d, want 2 (pause + resume)", inboxFetchCalls)
	}
}

func TestJMAPBodyChunksUseMultipleBackgroundSlots(t *testing.T) {
	folders := []storage.Folder{
		{ID: 1, Name: "INBOX", IMAPPath: "INBOX"},
		{ID: 2, Name: "A", IMAPPath: "A"},
		{ID: 3, Name: "B", IMAPPath: "B"},
	}
	var cur atomic.Int32
	var peak atomic.Int32
	release := make(chan struct{})

	s := syncsched.New(1)
	s.Start(context.Background(), pool.New(3), 0)
	t.Cleanup(s.Stop)
	done := make(chan error, 1)
	enqueueJMAPPhases(s, folders, 1,
		func(ctx context.Context, folder storage.Folder) ([]jmapListed, error) {
			return []jmapListed{{
				Folder: folder, RemoteID: folder.IMAPPath + "-1",
				Date: time.Date(2024, 6, int(folder.ID), 0, 0, 0, 0, time.UTC),
			}}, nil
		},
		func(ctx context.Context, folder storage.Folder) error {
			n := cur.Add(1)
			for {
				old := peak.Load()
				if n <= old || peak.CompareAndSwap(old, n) {
					break
				}
			}
			<-release
			cur.Add(-1)
			return nil
		},
		done,
	)
	deadline := time.After(2 * time.Second)
	for peak.Load() < 2 {
		select {
		case <-deadline:
			t.Fatalf("peak concurrent body jobs=%d, want >= 2 with sync pool N=3", peak.Load())
		default:
			time.Sleep(time.Millisecond)
		}
	}
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("jmap phases: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("jmap phases did not finish")
	}
}

func TestJMAPWatchLeavesPoolFreeAndMarksPush(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a := &App{ctx: ctx, log: slog.New(slog.DiscardHandler)}
	account := storage.Account{ID: 4, Protocol: "jmap", Email: "a@ex.test", JMAPMailAccountID: "mail"}
	a.ensureAccountScheduler(ctx, account.ID)
	rt := a.accountSync(account.ID)
	rt.pool.SetConfigured(1)

	a.jmapClientForTest = func(context.Context, storage.Account) (*pjmap.Client, error) {
		return &pjmap.Client{}, nil
	}
	orig := startWatch
	t.Cleanup(func() { startWatch = orig })

	started := make(chan struct{})
	startWatch = func(context.Context, *pjmap.Client, string, pjmap.WatchEvents) (*pjmap.Watch, error) {
		checkCtx, checkCancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer checkCancel()
		if err := rt.pool.Acquire(checkCtx, pool.Live); err != nil {
			t.Errorf("websocket setup held the sync pool: %v", err)
		} else {
			rt.pool.Release(pool.Live)
		}
		close(started)
		return pjmap.DiscardWatch(), nil
	}

	errCh := make(chan error, 1)
	go func() { errCh <- a.watchSession(ctx, account) }()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("watch did not start")
	}
	deadline := time.Now().Add(2 * time.Second)
	for !a.jmapPushHealthy(account.ID) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !a.jmapPushHealthy(account.ID) {
		t.Fatal("successful watch should mark push healthy")
	}
	cancel()
	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("watchSession: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("watchSession did not return")
	}
	if a.jmapPushHealthy(account.ID) {
		t.Fatal("ended watch should clear push health")
	}
}

func TestJMAPWatchNoWebSocketClearsPush(t *testing.T) {
	ctx := context.Background()
	a := &App{ctx: ctx, log: slog.New(slog.DiscardHandler)}
	account := storage.Account{ID: 5, Protocol: "jmap", JMAPMailAccountID: "mail"}
	a.setJMAPPushHealthy(account.ID, true)
	a.jmapClientForTest = func(context.Context, storage.Account) (*pjmap.Client, error) {
		return &pjmap.Client{}, nil
	}
	orig := startWatch
	t.Cleanup(func() { startWatch = orig })
	startWatch = func(context.Context, *pjmap.Client, string, pjmap.WatchEvents) (*pjmap.Watch, error) {
		return nil, pjmap.ErrNoWebSocket
	}
	err := a.watchSession(ctx, account)
	if !errors.Is(err, pjmap.ErrNoWebSocket) {
		t.Fatalf("watchSession err %v, want ErrNoWebSocket", err)
	}
	if a.jmapPushHealthy(account.ID) {
		t.Fatal("ErrNoWebSocket should clear push health")
	}
}

func TestJMAPPushFollowUpUsesHTTPPool(t *testing.T) {
	ctx := t.Context()
	store, err := storage.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.RunMigrations(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := store.SetInt(ctx, settingSyncMaxParallel, 1); err != nil {
		t.Fatalf("set parallel: %v", err)
	}
	a := &App{ctx: ctx, log: slog.New(slog.DiscardHandler), store: store}
	account := storage.Account{ID: 6, Protocol: "jmap", JMAPMailAccountID: "mail"}
	a.ensureAccountScheduler(ctx, account.ID)
	t.Cleanup(func() { a.stopAccountScheduler(account.ID) })
	rt := a.accountSync(account.ID)

	held := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	a.jmapClientForTest = func(context.Context, storage.Account) (*pjmap.Client, error) {
		once.Do(func() { close(held) })
		<-release
		return nil, errors.New("stop follow-up")
	}

	done := make(chan error, 1)
	go func() {
		done <- a.jmapWatchEvents(a.ctx, account).OnMailboxes([]string{"mb-1"})
	}()
	select {
	case <-held:
	case <-time.After(2 * time.Second):
		t.Fatal("follow-up did not reach HTTP")
	}
	checkCtx, checkCancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer checkCancel()
	if err := rt.pool.Acquire(checkCtx, pool.Live); err == nil {
		rt.pool.Release(pool.Live)
		t.Fatal("push follow-up HTTP left the sync pool free")
	}
	close(release)
	select {
	case err := <-done:
		if err == nil || err.Error() != "stop follow-up" {
			t.Fatalf("follow-up err %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("follow-up did not finish")
	}
}

// A push can floor a folder at X before the stub campaign runs. InitialLimit 0
// does not widen that floor; the stub phase must list every message anyway.
func TestJMAPStubPhaseWidensExistingFloor(t *testing.T) {
	ctx, stopBackground := testContext(t)
	t.Cleanup(stopBackground)
	db, err := storage.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.RunMigrations(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := db.SetInt(ctx, settingSyncMessageLimit, 2); err != nil {
		t.Fatalf("set limit: %v", err)
	}
	accountID, err := db.CreateAccount(ctx, &storage.Account{
		Email:    "user@example.com",
		Protocol: "jmap",
	})
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	folder := storage.Folder{
		AccountID: accountID,
		Name:      "INBOX",
		IMAPPath:  "INBOX",
		RemoteID:  "MbInbox",
	}
	if _, err := db.CreateFolder(ctx, &folder); err != nil {
		t.Fatalf("create folder: %v", err)
	}

	adapter := &jmapListAdapter{ids: []string{"e5", "e4", "e3", "e2", "e1"}}
	a := &App{ctx: ctx, store: db, log: slog.New(slog.DiscardHandler)}

	capped := a.newSyncEngine(adapter, accountID)
	if capped.InitialLimit != 2 {
		t.Fatalf("InitialLimit=%d, want 2", capped.InitialLimit)
	}
	if _, err := capped.SyncFolder(ctx, folder); err != nil {
		t.Fatalf("capped sync: %v", err)
	}
	floored, err := db.GetFolder(ctx, folder.ID)
	if err != nil {
		t.Fatalf("reload folder: %v", err)
	}
	if floored.SyncFloorID != "e4" {
		t.Fatalf("floor after X=2 sync = %q, want e4", floored.SyncFloorID)
	}
	if got := remoteIDsOf(t, db, ctx, folder.ID); len(got) != 2 || !containsAll(got, "e5", "e4") {
		t.Fatalf("cached after floor %v, want e5 and e4", got)
	}

	// The bug: InitialLimit 0 is ignored once the folder is initialized.
	narrow := a.newSyncEngine(adapter, accountID)
	narrow.InitialLimit = 0
	if _, err := narrow.SyncFolderStubs(ctx, *floored, 0); err != nil {
		t.Fatalf("narrow stubs: %v", err)
	}
	still, err := db.GetFolder(ctx, folder.ID)
	if err != nil {
		t.Fatalf("reload after narrow: %v", err)
	}
	if still.SyncFloorID != "e4" {
		t.Fatalf("floor after InitialLimit 0 = %q, want e4", still.SyncFloorID)
	}
	if got := remoteIDsOf(t, db, ctx, folder.ID); len(got) != 2 {
		t.Fatalf("InitialLimit 0 cached %v, want only the floored pair", got)
	}

	wide := a.jmapStubEngine(adapter, accountID)
	res, err := wide.SyncFolderStubs(ctx, *still, 0)
	if err != nil {
		t.Fatalf("stub phase: %v", err)
	}
	if res.HasOlder {
		t.Fatal("full stub list should clear the floor")
	}
	wantFetch := []string{"e3", "e2", "e1"}
	if len(res.ToFetch) != len(wantFetch) {
		t.Fatalf("ToFetch=%v, want %v", res.ToFetch, wantFetch)
	}
	for i := range wantFetch {
		if res.ToFetch[i] != wantFetch[i] {
			t.Fatalf("ToFetch=%v, want %v", res.ToFetch, wantFetch)
		}
	}
	opened, err := db.GetFolder(ctx, folder.ID)
	if err != nil {
		t.Fatalf("reload after stubs: %v", err)
	}
	if opened.SyncFloorID != "" {
		t.Fatalf("floor after stub phase = %q, want empty", opened.SyncFloorID)
	}
	msgs, err := db.ListMessages(ctx, folder.ID, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(msgs) != 5 {
		t.Fatalf("stubs %d, want 5", len(msgs))
	}
	byID := map[string]storage.Message{}
	for _, m := range msgs {
		byID[m.RemoteID] = m
	}
	for _, id := range []string{"e5", "e4"} {
		if !byID[id].BodyComplete {
			t.Fatalf("%s body should stay complete", id)
		}
	}
	for _, id := range []string{"e3", "e2", "e1"} {
		m, ok := byID[id]
		if !ok {
			t.Fatalf("missing stub %s", id)
		}
		if m.BodyComplete {
			t.Fatalf("%s should be a stub, not a body", id)
		}
	}
	if len(adapter.fetched) != 2 {
		t.Fatalf("fetched %v, stub phase must not download the older bodies", adapter.fetched)
	}
}

func TestJMAPManualSyncUsesLiveSlotNotBodyCampaign(t *testing.T) {
	ctx, stopBackground := testContext(t)
	t.Cleanup(stopBackground)
	db, err := storage.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.RunMigrations(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := db.SetInt(ctx, settingSyncMaxParallel, 2); err != nil {
		t.Fatalf("set parallel: %v", err)
	}
	account := storage.Account{ID: 42, Protocol: "jmap", Email: "user@example.com"}
	a := &App{ctx: ctx, store: db, log: slog.New(slog.DiscardHandler)}
	a.setJMAPPushHealthy(account.ID, true)
	t.Cleanup(func() { a.stopAccountScheduler(account.ID) })

	var calls atomic.Int32
	a.jmapManualSyncForTest = func(context.Context, storage.Account) error {
		calls.Add(1)
		rt := a.accountSync(account.ID)
		if rt == nil || rt.pool == nil {
			return errors.New("manual sync has no pool")
		}
		bgCtx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
		err := rt.pool.Acquire(bgCtx, pool.Background)
		cancel()
		if err != nil {
			return errors.New("manual sync held a background slot: " + err.Error())
		}
		defer rt.pool.Release(pool.Background)
		liveCtx, liveCancel := context.WithTimeout(ctx, 50*time.Millisecond)
		err = rt.pool.Acquire(liveCtx, pool.Live)
		liveCancel()
		if err == nil {
			rt.pool.Release(pool.Live)
			return errors.New("manual sync did not hold the live slot")
		}
		return nil
	}

	if err := a.syncAccountOnceCtx(ctx, account); err != nil {
		t.Fatalf("manual sync: %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("manual reconcile calls=%d, want 1", calls.Load())
	}
	if !a.jmapPushHealthy(account.ID) {
		t.Fatal("manual sync cleared a healthy push")
	}
}

func remoteIDsOf(t *testing.T, db *storage.DB, ctx context.Context, folderID int64) []string {
	t.Helper()
	msgs, err := db.ListMessages(ctx, folderID, 0)
	if err != nil {
		t.Fatalf("list messages: %v", err)
	}
	out := make([]string, len(msgs))
	for i, m := range msgs {
		out[i] = m.RemoteID
	}
	return out
}

// jmapListAdapter is a newest-first list with envelope metadata and no HTTP.
type jmapListAdapter struct {
	ids     []string
	fetched []string
}

func (a *jmapListAdapter) Addr() string { return "jmap.test" }

func (a *jmapListAdapter) ListMailboxes(context.Context) ([]psync.Mailbox, error) {
	return nil, nil
}

func (a *jmapListAdapter) ListMessages(context.Context, psync.RemoteMailbox) ([]psync.Header, string, string, error) {
	out := make([]psync.Header, len(a.ids))
	for i, id := range a.ids {
		out[i] = psync.Header{
			RemoteID:    id,
			HasListMeta: true,
			Subject:     id,
			Date:        time.Date(2024, 6, len(a.ids)-i, 0, 0, 0, 0, time.UTC),
		}
	}
	return out, "s1", "", nil
}

func (a *jmapListAdapter) Fetch(_ context.Context, _ string, ids []string) ([]psync.Fetched, error) {
	a.fetched = append(a.fetched, ids...)
	out := make([]psync.Fetched, len(ids))
	for i, id := range ids {
		out[i] = psync.Fetched{RemoteID: id, Subject: id, Text: "body-" + id}
	}
	return out, nil
}

func (a *jmapListAdapter) SetFlags(context.Context, string, string, storage.Flag) error {
	return nil
}
func (a *jmapListAdapter) Move(context.Context, string, []string, string) error { return nil }
func (a *jmapListAdapter) Delete(context.Context, string, []string) error       { return nil }
func (a *jmapListAdapter) CreateMailbox(context.Context, string, string) (psync.Mailbox, error) {
	return psync.Mailbox{}, nil
}
func (a *jmapListAdapter) RenameMailbox(context.Context, string, string) error { return nil }
func (a *jmapListAdapter) DeleteMailbox(context.Context, string) error         { return nil }

func containsAll(got []string, want ...string) bool {
	have := map[string]bool{}
	for _, g := range got {
		have[g] = true
	}
	for _, w := range want {
		if !have[w] {
			return false
		}
	}
	return true
}
