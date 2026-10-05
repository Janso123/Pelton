package desktop

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/peltonapp/Pelton/internal/desktop/syncsched"
	pimap "github.com/peltonapp/Pelton/internal/imap"
	"github.com/peltonapp/Pelton/internal/storage"
	psync "github.com/peltonapp/Pelton/internal/sync"
)

// closeRecordingIMAP is a fakeIMAP that reports Close. Its Login can be made
// to block until release closes, ignoring Close, which is how a stuck FETCH on
// a slow server looks to the worker that owns it.
type closeRecordingIMAP struct {
	fakeIMAP
	closed    atomic.Bool
	loginGate chan struct{}
	loginErr  error
	loggingIn chan struct{}
}

func (c *closeRecordingIMAP) Close() error { c.closed.Store(true); return nil }

func (c *closeRecordingIMAP) Login() error {
	if c.loggingIn != nil {
		close(c.loggingIn)
	}
	if c.loginGate != nil {
		<-c.loginGate
	}
	return c.loginErr
}

// Switching protocol on one account must not close another account's IMAP
// sessions or cancel its scheduler jobs. Before per-account runtimes the abort
// closed every tracked session in the app, so both mailboxes reported a failed
// sync after one was switched.
func TestStopAccountWorkerLeavesOtherAccountSessionsAndJobs(t *testing.T) {
	a := newJMAPSwitchTestApp(t)
	idA, _ := seedSwitchAccount(t, a, "a@example.test", "imap")
	idB, _ := seedSwitchAccount(t, a, "b@example.test", "imap")
	accA, err := a.store.GetAccount(a.ctx, idA)
	if err != nil {
		t.Fatal(err)
	}
	accB, err := a.store.GetAccount(a.ctx, idB)
	if err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	clients := map[string]*closeRecordingIMAP{}
	a.newIMAPClient = func(cfg pimap.Config) (mailClient, error) {
		c := &closeRecordingIMAP{}
		mu.Lock()
		clients[cfg.Username] = c
		mu.Unlock()
		return c, nil
	}
	clientFor := func(user string) *closeRecordingIMAP {
		mu.Lock()
		defer mu.Unlock()
		return clients[user]
	}

	// One open session per account, each parked inside withIMAPSession.
	release := make(chan struct{})
	var sessions sync.WaitGroup
	opened := make(chan struct{}, 2)
	for _, acc := range []*storage.Account{accA, accB} {
		acc := *acc
		sessions.Go(func() {
			_ = a.withIMAPSession(a.ctx, acc, func(mailClient) error {
				opened <- struct{}{}
				<-release
				return nil
			})
		})
	}
	defer func() { close(release); sessions.Wait() }()
	for range 2 {
		select {
		case <-opened:
		case <-time.After(2 * time.Second):
			t.Fatal("sessions never opened")
		}
	}

	// A background job running on B's scheduler.
	schedB, err := a.ensureAccountScheduler(a.ctx, idB)
	if err != nil {
		t.Fatal(err)
	}
	jobRunning := make(chan struct{})
	jobCancelled := make(chan struct{})
	jobRelease := make(chan struct{})
	schedB.Enqueue(syncsched.Job{
		Priority: syncsched.PriorityBackgroundBody,
		Kind:     syncsched.JobFetchBodies,
		Run: func(ctx context.Context) error {
			close(jobRunning)
			select {
			case <-ctx.Done():
				close(jobCancelled)
				return ctx.Err()
			case <-jobRelease:
				return nil
			}
		},
	})
	defer close(jobRelease)
	select {
	case <-jobRunning:
	case <-time.After(2 * time.Second):
		t.Fatal("B's job never started")
	}
	a.ensureAccountScheduler(a.ctx, idA)

	a.stopAccountWorker(idA)

	if c := clientFor(accA.Email); c == nil || !c.closed.Load() {
		t.Fatal("A's session was not closed by A's stop")
	}
	if c := clientFor(accB.Email); c == nil || c.closed.Load() {
		t.Fatal("B's session was closed by A's stop")
	}
	select {
	case <-jobCancelled:
		t.Fatal("B's scheduler job was cancelled by A's stop")
	case <-time.After(100 * time.Millisecond):
	}
	if a.accountSync(idB) == nil {
		t.Fatal("B's scheduler was removed by A's stop")
	}
	if a.accountSync(idA) != nil {
		t.Fatal("A's scheduler survived A's stop")
	}
}

// A worker that outlives stopAccountWorker's wait must not tear down the
// scheduler its replacement started. Before, the late exit deleted the map
// entry by account id, so Sync did nothing after a protocol switch.
func TestLateOldWorkerExitKeepsNewScheduler(t *testing.T) {
	old := stopAccountWorkerTimeout
	stopAccountWorkerTimeout = 50 * time.Millisecond
	t.Cleanup(func() { stopAccountWorkerTimeout = old })

	a := newJMAPSwitchTestApp(t)
	id, _ := seedSwitchAccount(t, a, "late@example.test", "imap")

	// The first connection's Login hangs past the stop timeout and ignores
	// Close. Later connections are refused at once, so the new worker settles
	// into its retry wait holding its scheduler.
	stuck := &closeRecordingIMAP{
		loginGate: make(chan struct{}),
		loginErr:  errors.New("aborted"),
		loggingIn: make(chan struct{}),
	}
	var calls atomic.Int32
	a.newIMAPClient = func(pimap.Config) (mailClient, error) {
		if calls.Add(1) == 1 {
			return stuck, nil
		}
		return &closeRecordingIMAP{loginErr: errors.New("refused")}, nil
	}
	var releaseOnce sync.Once
	releaseStuck := func() { releaseOnce.Do(func() { close(stuck.loginGate) }) }
	t.Cleanup(releaseStuck)

	a.startAccountWorker(id)
	select {
	case <-stuck.loggingIn:
	case <-time.After(2 * time.Second):
		t.Fatal("first worker never reached login")
	}
	a.workersMu.Lock()
	oldDone := a.workers[id].done
	a.workersMu.Unlock()

	a.startAccountWorker(id) // waits stopAccountWorkerTimeout, then gives up
	var rt *accountSync
	deadline := time.Now().Add(2 * time.Second)
	for rt == nil && time.Now().Before(deadline) {
		rt = a.accountSync(id)
		if rt == nil {
			time.Sleep(10 * time.Millisecond)
		}
	}
	if rt == nil {
		t.Fatal("new worker never started a scheduler")
	}

	releaseStuck()
	select {
	case <-oldDone:
	case <-time.After(2 * time.Second):
		t.Fatal("old worker never exited")
	}

	if got := a.accountSync(id); got != rt {
		t.Fatalf("scheduler after late exit = %p, want new worker's %p", got, rt)
	}
	select {
	case <-rt.ended:
		t.Fatal("new worker's scheduler was stopped by the old worker's exit")
	default:
	}
}

// blockingJMAPAdapter parks the first ListMessages until release closes, so a
// test can hold one account's folder sync inside its critical section.
type blockingJMAPAdapter struct {
	jmapListAdapter
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *blockingJMAPAdapter) ListMessages(ctx context.Context, box psync.RemoteMailbox) ([]psync.Header, string, string, error) {
	b.once.Do(func() { close(b.entered) })
	select {
	case <-b.release:
	case <-ctx.Done():
		return nil, "", "", ctx.Err()
	}
	return b.jmapListAdapter.ListMessages(ctx, box)
}

// One account's JMAP folder sync must not hold up an archive on another. The
// process-wide sync lock made every mailbox wait for the slowest one.
func TestJMAPFolderSyncDoesNotBlockOtherAccountArchive(t *testing.T) {
	a, db, ctx := moveTestApp(t)
	inboxB, _, messageB := moveTestAccount(t, db, ctx, false)
	useFake(t, a, inboxB.AccountID, &fakeIMAP{})

	idA, err := db.CreateAccount(ctx, &storage.Account{
		Email: "jmap@example.test", Protocol: "jmap",
		JMAPSessionURL: "https://jmap.example/.well-known/jmap", JMAPMailAccountID: "mail-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateFolder(ctx, &storage.Folder{
		AccountID: idA, Name: "INBOX", IMAPPath: "INBOX", RemoteID: "mb-inbox",
	}); err != nil {
		t.Fatal(err)
	}

	adapter := &blockingJMAPAdapter{
		ids:     []string{"m1"},
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	syncDone := make(chan error, 1)
	go func() { syncDone <- a.syncFolders(ctx, adapter, idA) }()
	var releaseOnce sync.Once
	releaseA := func() { releaseOnce.Do(func() { close(adapter.release) }) }
	defer func() {
		releaseA()
		<-syncDone
	}()
	select {
	case <-adapter.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("A's folder sync never reached the server")
	}

	archived := make(chan error, 1)
	go func() {
		_, err := a.ArchiveMessage(messageB)
		archived <- err
	}()
	select {
	case err := <-archived:
		if err != nil {
			t.Fatalf("archive on B: %v", err)
		}
	case <-time.After(2 * time.Second):
		releaseA()
		<-archived
		t.Fatal("archive on B waited for A's JMAP folder sync")
	}
}

// A scheduler first started by one sync run must keep serving the account
// after that run's context ends. Bound to the run, it died with it and the
// next run's jobs waited on a queue nobody served.
func TestSchedulerOutlivesTheRunThatStartedIt(t *testing.T) {
	a := newJMAPSwitchTestApp(t)
	id, _ := seedSwitchAccount(t, a, "run@example.test", "imap")
	t.Cleanup(func() { a.stopAccountScheduler(id) })

	runCtx, endRun := context.WithCancel(a.ctx)
	a.ensureAccountScheduler(runCtx, id)
	endRun()

	ran := make(chan struct{})
	err := a.enqueueLiveAndWait(a.ctx, id, syncsched.JobManualSync, 0, nil, func(context.Context) error {
		close(ran)
		return nil
	})
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	select {
	case <-ran:
	default:
		t.Fatal("second run's job never ran")
	}
}
