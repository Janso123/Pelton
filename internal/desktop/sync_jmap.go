package desktop

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"sync"
	"time"

	gojmap "github.com/Janso123/go-jmap"
	"github.com/peltonapp/Pelton/internal/credentials"
	"github.com/peltonapp/Pelton/internal/desktop/syncsched"
	pjmap "github.com/peltonapp/Pelton/internal/jmap"
	"github.com/peltonapp/Pelton/internal/outbox"
	"github.com/peltonapp/Pelton/internal/storage"
	psync "github.com/peltonapp/Pelton/internal/sync"
	"github.com/peltonapp/Pelton/internal/sync/pool"
)

// jmapState is the App state only JMAP accounts use, embedded in App so the
// fields keep their names.
type jmapState struct {
	// jmapPushMu guards jmapPushOK. A healthy JMAP WebSocket sets the account's
	// entry; timed auto-sync skips that account until the watch fails or ends.
	jmapPushMu sync.Mutex
	jmapPushOK map[int64]bool

	// jmapHTTPMu guards jmapHTTP, each JMAP mailbox's shared http client
	// (see accountJMAPHTTPClient).
	jmapHTTPMu sync.Mutex
	jmapHTTP   map[int64]jmapHTTPEntry

	// jmapClientForTest, when set, replaces jmapClient (watch/sync tests).
	jmapClientForTest func(ctx context.Context, account storage.Account) (*pjmap.Client, error)
	// jmapAdapterForTest, when set, is the adapter withAccountAdapter hands out
	// for JMAP accounts (message action tests).
	jmapAdapterForTest func(account storage.Account) psync.Adapter
	// jmapManualSyncForTest, when set, replaces the incremental reconcile inside
	// the manual Sync live job. Production leaves it nil.
	jmapManualSyncForTest func(ctx context.Context, account storage.Account) error
}

// submit sends via JMAP EmailSubmission. Tests replace it to avoid the network.
var submit = pjmap.Submit

// startWatch opens a JMAP WebSocket watch. Tests replace it.
var startWatch = pjmap.StartWatch

func (t *accountTransmitter) transmitJMAP(ctx context.Context, account storage.Account, m outbox.Message) error {
	client, err := t.app.jmapClient(ctx, &account)
	if err != nil {
		return err
	}
	sentID, err := t.app.sentMailboxRemoteID(ctx, account.ID)
	if err != nil {
		return err
	}
	return submit(ctx, client, account.JMAPMailAccountID, sentID, m.Raw, account.Email, m.EnvelopeFrom, m.Recipients)
}

// jmapClient builds an authenticated JMAP client from the stored session URL and
// the account's keyring secret (Basic password or refreshed Bearer token). It
// connects along the account's route with the certificates it trusts, and
// fails rather than going direct when the route cannot be used. An account
// with no session yet finds and stores it first (see findJMAPSession), and
// account is updated so the caller uses the mail account id that was found.
// A refused sign-in marks the account for the password prompt (see
// noteLoginResult), and one that works clears the mark.
func (a *App) jmapClient(ctx context.Context, account *storage.Account) (*pjmap.Client, error) {
	if a.jmapClientForTest != nil {
		return a.jmapClientForTest(ctx, *account)
	}
	secret, err := credentials.Load(account.ID)
	if errors.Is(err, credentials.ErrNotFound) {
		return nil, errNoCredentials
	}
	if err != nil {
		return nil, err
	}
	if account.JMAPSessionURL == "" {
		// a restored mailbox finds its session by signing in, so a refusal
		// here is the same news as one from jmapSignIn.
		if err := a.findJMAPSession(ctx, account, secret); err != nil {
			a.noteLoginResult(account.ID, err)
			return nil, err
		}
	}

	client, err := a.jmapSignIn(ctx, *account, secret)
	a.noteLoginResult(account.ID, err)
	if err != nil {
		return nil, err
	}
	return client, nil
}

// jmapSignIn fetches the account's session with secret, along the account's
// route. A refusal carries pjmap.ErrAuthFailed. It records nothing, so the
// password prompt can try what the user typed without marking the mailbox.
func (a *App) jmapSignIn(ctx context.Context, account storage.Account, secret credentials.Secret) (*pjmap.Client, error) {
	httpClient, err := a.accountJMAPHTTPClient(account)
	if err != nil {
		return nil, err
	}
	// the http client goes first: the auth options wrap its transport, and
	// every request, upload, download and the push socket go out over it.
	opts := []gojmap.Option{gojmap.WithHTTPClient(httpClient)}
	switch secret.Method {
	case credentials.MethodOAuth:
		token, err := a.freshAccessToken(account, secret)
		if err != nil {
			return nil, err
		}
		opts = append(opts, gojmap.WithBearer(token))
	default:
		opts = append(opts, gojmap.WithBasic(loginName(account), secret.Password))
	}

	jc := gojmap.NewClient(account.JMAPSessionURL, opts...)
	if err := jc.Authenticate(ctx); err != nil {
		return nil, pjmap.AuthError(err)
	}
	return pjmap.NewClient(jc), nil
}

// findJMAPSession looks up the session of a JMAP account that has none, which
// is what a backup restored without its password leaves, and stores it. It
// signs in along the account's route only: a route that cannot be resolved
// fails the lookup. A failed lookup stores nothing, so the next sync tries
// again and the account stays JMAP.
func (a *App) findJMAPSession(ctx context.Context, account *storage.Account, secret credentials.Secret) error {
	// sync jobs hold their own copy of the account, so another job may have
	// stored the session since this copy was read.
	stored, err := a.store.GetAccount(ctx, account.ID)
	if err != nil {
		return err
	}
	if stored.JMAPSessionURL != "" {
		account.JMAPSessionURL, account.JMAPMailAccountID = stored.JMAPSessionURL, stored.JMAPMailAccountID
		return nil
	}
	route, err := a.accountRoute(*account)
	if err != nil {
		return err
	}
	auth, err := a.runAuthenticateTarget(ctx, *account, route, "jmap", secret)
	if err != nil {
		return fmt.Errorf("pelton: find the JMAP session of account %d: %w", account.ID, err)
	}
	if auth.SessionURL == "" || auth.MailAccountID == "" {
		return fmt.Errorf("pelton: account %d: the JMAP server named no session or mail account", account.ID)
	}
	err = a.store.SetAccountJMAPSession(ctx, account.ID, auth.SessionURL, auth.MailAccountID)
	if errors.Is(err, storage.ErrAccountNotFound) {
		// SetAccountJMAPSession changes only a JMAP account, so the mailbox
		// was switched (or deleted) while the lookup ran.
		return fmt.Errorf("pelton: account %d: protocol changed during JMAP session lookup: %w", account.ID, err)
	}
	if err != nil {
		return err
	}
	account.JMAPSessionURL, account.JMAPMailAccountID = auth.SessionURL, auth.MailAccountID
	return nil
}

func (a *App) setJMAPPushHealthy(accountID int64, healthy bool) {
	a.jmapPushMu.Lock()
	defer a.jmapPushMu.Unlock()
	if a.jmapPushOK == nil {
		a.jmapPushOK = make(map[int64]bool)
	}
	if !healthy {
		delete(a.jmapPushOK, accountID)
		return
	}
	a.jmapPushOK[accountID] = true
}

func (a *App) jmapPushHealthy(accountID int64) bool {
	a.jmapPushMu.Lock()
	defer a.jmapPushMu.Unlock()
	return a.jmapPushOK[accountID]
}

func (a *App) syncAccountOnceJMAP(ctx context.Context, account storage.Account) error {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	as := &activeSync{cancel: cancel}
	a.setActiveSync(account.ID, as)
	defer a.clearActiveSync(account.ID, as)

	a.emit(EventSyncState, SyncStateEvent{Running: true})
	defer a.emit(EventSyncState, SyncStateEvent{Running: false})

	if err := runCtx.Err(); err != nil {
		return err
	}

	client, err := a.jmapClient(runCtx, &account)
	if err != nil {
		return err
	}
	adapter := pjmap.NewAdapter(client, account.JMAPMailAccountID)

	// Mailbox discovery is one sync HTTP call, so it takes a pool slot and
	// releases it before the stub jobs run. It does not hold the account lock.
	if err := a.withSyncSlot(runCtx, account.ID, pool.Background, func() error {
		engine := a.newSyncEngine(adapter, account.ID)
		a.applyBackgroundPause(engine, account.ID)
		return engine.SyncMailboxes(runCtx, account.ID)
	}); err != nil {
		return err
	}
	all, err := a.store.ListFolders(runCtx, account.ID)
	if err != nil {
		return err
	}
	ordered := foldersInSyncOrder(all)
	tally := a.accountTally(account.ID)
	tally.begin(len(ordered))
	a.setSyncProgressPhase(account.ID, SyncPhaseStubs)
	defer a.setSyncProgressPhase(account.ID, "")
	closed := false
	defer func() {
		if !closed {
			a.emitSyncProgress(account.ID, account.Email, adapter.Addr(), tally.closing())
		}
	}()
	// registered after the closing defer so it stops first: the heartbeat must
	// be gone before the close is sent.
	stopBeat := a.startProgressHeartbeat(runCtx, account.ID)
	defer stopBeat()

	sched, err := a.ensureAccountScheduler(runCtx, account.ID)
	if err != nil {
		return err
	}
	done := make(chan error, 1)
	var bodyPhaseOnce sync.Once
	enqueueJMAPPhases(sched, ordered, a.syncMessageLimit(), func(jobCtx context.Context, folder storage.Folder) ([]jmapListed, error) {
		for i := range ordered {
			if ordered[i].ID == folder.ID {
				tally.enterFolder(i, folder.Name)
				counts := tally.counts()
				counts.Phase = SyncPhaseStubs
				a.emitSyncProgress(account.ID, account.Email, adapter.Addr(), counts)
				break
			}
		}
		listed, _, err := a.execJMAPList(jobCtx, account, folder, 0, a.syncMessageLimit())
		return listed, err
	}, func(jobCtx context.Context, folder storage.Folder) error {
		bodyPhaseOnce.Do(func() {
			a.setSyncProgressPhase(account.ID, SyncPhaseBodies)
			counts := tally.counts()
			counts.Phase = SyncPhaseBodies
			a.emitSyncProgress(account.ID, account.Email, adapter.Addr(), counts)
		})
		return a.execJMAPBodies(jobCtx, account, folder, false)
	}, done)

	var runErr error
	select {
	case runErr = <-done:
	case <-a.schedulerEnded(account.ID):
		return context.Canceled
	case <-runCtx.Done():
		return runCtx.Err()
	}
	final := tally.counts()
	final.Folder = ""
	final.FoldersDone = len(ordered)
	closed = true
	stopBeat()
	a.emitSyncProgress(account.ID, account.Email, adapter.Addr(), final)
	return runErr
}

func (a *App) watchSession(ctx context.Context, account storage.Account) error {
	client, err := a.jmapClient(ctx, &account)
	if err != nil {
		a.setJMAPPushHealthy(account.ID, false)
		return err
	}
	// The scheduler must exist so a push can enqueue a live job. Opening the
	// socket does not check a pool slot out.
	if _, err := a.ensureAccountScheduler(ctx, account.ID); err != nil {
		a.setJMAPPushHealthy(account.ID, false)
		return err
	}
	watch, err := startWatch(ctx, client, account.JMAPMailAccountID, a.jmapWatchEvents(ctx, account))
	if err != nil {
		a.setJMAPPushHealthy(account.ID, false)
		return err
	}
	a.setJMAPPushHealthy(account.ID, true)
	defer a.setJMAPPushHealthy(account.ID, false)
	defer watch.Close()
	<-ctx.Done()
	return ctx.Err()
}

// jmapWatchEvents turns a WebSocket callback into a live sync job. The socket
// stays outside the pool; the job's HTTP borrows a slot when it runs. ctx is
// the watch's, so a follow-up is the worker's own and ends with it.
func (a *App) jmapWatchEvents(ctx context.Context, account storage.Account) pjmap.WatchEvents {
	return pjmap.WatchEvents{
		OnMailboxes: func(ids []string) error {
			return a.enqueueJMAPFollowUp(ctx, account, ids)
		},
		OnReconnect: func() error {
			return a.enqueueJMAPFollowUp(ctx, account, nil)
		},
	}
}

// enqueueJMAPFollowUp runs the push HTTP on a live pool slot. A nil mailbox
// list is the full resync (cannot diff, or the socket reconnected). The
// callback waits so the watch does not advance state before that HTTP finishes.
func (a *App) enqueueJMAPFollowUp(ctx context.Context, account storage.Account, mailboxIDs []string) error {
	full := mailboxIDs == nil
	ids := append([]string(nil), mailboxIDs...)
	return a.enqueueLiveAndWait(ctx, account.ID, syncsched.JobNewMail, 0, nil, func(ctx context.Context) error {
		if full {
			return a.syncJMAPFollowUpAll(ctx, account)
		}
		return a.syncJMAPMailboxesCtx(ctx, account, ids)
	})
}

func (a *App) syncJMAPMailboxesCtx(ctx context.Context, account storage.Account, remoteIDs []string) error {
	client, err := a.jmapClient(ctx, &account)
	if err != nil {
		return err
	}
	adapter := pjmap.NewAdapter(client, account.JMAPMailAccountID)
	if a.store == nil {
		return errors.New("pelton: no store")
	}
	folders, err := a.store.ListFolders(ctx, account.ID)
	if err != nil {
		return err
	}
	want := make(map[string]struct{}, len(remoteIDs))
	for _, id := range remoteIDs {
		want[id] = struct{}{}
	}
	for _, f := range folders {
		rid := f.RemoteID
		if rid == "" {
			rid = f.IMAPPath
		}
		if _, ok := want[rid]; !ok {
			continue
		}
		if err := a.syncOneFolderCtx(ctx, adapter, f); err != nil {
			a.log.Error("jmap watch resync", "folder", f.Name, "err", err)
		}
	}
	return nil
}

// syncJMAPFollowUpAll is the P0 resync after a push that could not name
// mailboxes. It runs inside the live job, which already holds the pool slot.
func (a *App) syncJMAPFollowUpAll(ctx context.Context, account storage.Account) error {
	client, err := a.jmapClient(ctx, &account)
	if err != nil {
		return err
	}
	adapter := pjmap.NewAdapter(client, account.JMAPMailAccountID)
	engine := a.newSyncEngine(adapter, account.ID)
	a.applySchedulerPause(engine, account.ID)
	if err := engine.SyncMailboxes(ctx, account.ID); err != nil {
		return err
	}
	if a.store == nil {
		return errors.New("pelton: no store")
	}
	folders, err := a.store.ListFolders(ctx, account.ID)
	if err != nil {
		return err
	}
	var failed []string
	var first error
	ordered := foldersInSyncOrder(folders)
	for _, f := range ordered {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := a.syncOneFolderCtx(ctx, adapter, f); err != nil {
			a.log.Error("jmap watch resync", "folder", f.Name, "err", err)
			if first == nil {
				first = err
			}
			failed = append(failed, f.Name)
		}
	}
	return folderSyncError(failed, len(ordered), first)
}

// jmapBodyChunk is how many remote ids one JMAP FetchBodies job carries.
// The engine batches inside the job; this keeps a cross-folder newest-first
// queue from holding one slot for the whole account.
const jmapBodyChunk = 50

// jmapBodyFetchBatch is how many bodies one JMAP engine batch fetches before
// a soft-pause boundary (IMAP keeps the global default).
const jmapBodyFetchBatch = 10

// jmapBlobDownloadMax caps concurrent blob downloads inside one Fetch.
const jmapBlobDownloadMax = 3

// jmapBlobDownloadParallel caps concurrent blob downloads inside one Fetch.
// It follows the pool's effective size, so a throttled account also opens
// fewer downloads.
func (a *App) jmapBlobDownloadParallel(accountID int64) int {
	var n int
	if rt := a.accountSync(accountID); rt != nil && rt.pool != nil {
		n = rt.pool.Effective()
	} else {
		n = a.accountSyncMaxParallel(accountID)
	}
	if n > jmapBlobDownloadMax {
		n = jmapBlobDownloadMax
	}
	return n
}

func (a *App) newJMAPSyncAdapter(client *pjmap.Client, accountID int64, mailAccountID string) *pjmap.Adapter {
	ad := pjmap.NewAdapter(client, mailAccountID)
	ad.BlobDownloadParallel = a.jmapBlobDownloadParallel(accountID)
	return ad
}

// jmapListed is one stub that still needs a body, with the date used to order
// an unlimited fill across folders.
type jmapListed struct {
	Folder   storage.Folder
	RemoteID string
	Date     time.Time
}

type jmapBodyChunkJob struct {
	folder storage.Folder
	ids    []string
}

// jmapBodyChunkRunner hands out body chunks in global newest-first order while
// keeping up to maxBg FetchBodies jobs in flight on the sync pool.
type jmapBodyChunkRunner struct {
	s      *syncsched.Scheduler
	chunks []jmapBodyChunkJob
	next   int
	maxBg  int

	mu       sync.Mutex
	inflight int

	stop      func(context.Context, error) error
	fetch     func(context.Context, storage.Folder) error
	noteBody  func(string, error)
	finishAll func()
}

func (r *jmapBodyChunkRunner) start() {
	if r.maxBg < 1 {
		r.maxBg = 1
	}
	r.maybeEnqueue()
}

func (r *jmapBodyChunkRunner) maybeEnqueue() {
	r.mu.Lock()
	for r.inflight < r.maxBg && r.next < len(r.chunks) {
		i := r.next
		r.next++
		r.inflight++
		r.mu.Unlock()
		r.dispatch(i, 0)
		r.mu.Lock()
	}
	done := r.next >= len(r.chunks) && r.inflight == 0
	r.mu.Unlock()
	if done {
		r.finishAll()
	}
}

func (r *jmapBodyChunkRunner) jobDone() {
	r.mu.Lock()
	r.inflight--
	done := r.next >= len(r.chunks) && r.inflight == 0
	r.mu.Unlock()
	if done {
		r.finishAll()
		return
	}
	r.maybeEnqueue()
}

func (r *jmapBodyChunkRunner) scheduleRetry(i, attempt int) {
	r.mu.Lock()
	r.inflight++
	r.mu.Unlock()
	r.dispatch(i, attempt)
}

func (r *jmapBodyChunkRunner) dispatch(i, attempt int) {
	if i >= len(r.chunks) {
		r.jobDone()
		return
	}
	ch := r.chunks[i]
	r.s.Enqueue(syncsched.Job{
		Priority:  syncsched.PriorityBackgroundBody,
		Kind:      syncsched.JobFetchBodies,
		FolderID:  ch.folder.ID,
		RemoteIDs: append([]string(nil), ch.ids...),
		Run: func(ctx context.Context) error {
			if err := ctx.Err(); err != nil {
				r.jobDone()
				return r.stop(ctx, err)
			}
			err := r.fetch(ctx, ch.folder)
			if errors.Is(err, psync.ErrSoftPaused) {
				return err
			}
			if err != nil {
				if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					r.jobDone()
					return r.stop(ctx, err)
				}
				if attempt+1 < syncStepAttempts {
					attempt := attempt
					scheduleSyncRetry(ctx, attempt, func() { r.scheduleRetry(i, attempt+1) })
					r.jobDone()
					return nil
				}
				r.noteBody(ch.folder.Name, err)
				r.jobDone()
				return nil
			}
			r.jobDone()
			return nil
		},
	})
}

// enqueueJMAPPhases lists every selected folder, then fills bodies.
// Stub jobs are all queued first so they can run together up to the
// background slots. limit > 0 keeps the X newest bodies per folder and
// enqueues Inbox, then Sent, before the other folders. limit <= 0 is one newest-first
// queue across folders.
func enqueueJMAPPhases(s *syncsched.Scheduler, folders []storage.Folder, limit int, list func(ctx context.Context, folder storage.Folder) ([]jmapListed, error), fetch func(ctx context.Context, folder storage.Folder) error, done chan<- error) {
	ordered := foldersInSyncOrder(folders)
	var once sync.Once
	report := func(err error) {
		if done == nil {
			return
		}
		once.Do(func() { done <- err })
	}
	if len(ordered) == 0 {
		report(nil)
		return
	}

	var mu sync.Mutex
	aborted := false
	pendingLists := len(ordered)
	var listed []jmapListed
	var failed []string
	var firstErr error

	noteFailure := func(name string, err error) {
		if firstErr == nil {
			firstErr = err
		}
		failed = append(failed, name)
	}
	stop := func(ctx context.Context, err error) error {
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		mu.Lock()
		aborted = true
		mu.Unlock()
		report(err)
		return err
	}

	var enqueueBodies func(items []jmapListed)
	finishLists := func() {
		mu.Lock()
		if aborted {
			mu.Unlock()
			return
		}
		items := append([]jmapListed(nil), listed...)
		mu.Unlock()
		enqueueBodies(items)
	}

	var enqueueList func(folder storage.Folder, attempt int)
	enqueueList = func(folder storage.Folder, attempt int) {
		f := folder
		s.Enqueue(syncsched.Job{
			Priority: syncsched.PriorityBackgroundStubs,
			Kind:     syncsched.JobListStubs,
			FolderID: f.ID,
			Run: func(ctx context.Context) error {
				if err := ctx.Err(); err != nil {
					return stop(ctx, err)
				}
				items, err := list(ctx, f)
				if errors.Is(err, psync.ErrSoftPaused) {
					return err
				}
				if err != nil {
					if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
						return stop(ctx, err)
					}
					if attempt+1 < syncStepAttempts {
						scheduleSyncRetry(ctx, attempt, func() { enqueueList(f, attempt+1) })
						return nil
					}
					mu.Lock()
					if aborted {
						mu.Unlock()
						return nil
					}
					noteFailure(f.Name, err)
					pendingLists--
					doneLists := pendingLists == 0
					mu.Unlock()
					if doneLists {
						finishLists()
					}
					return nil
				}
				mu.Lock()
				if aborted {
					mu.Unlock()
					return nil
				}
				listed = append(listed, items...)
				pendingLists--
				doneLists := pendingLists == 0
				mu.Unlock()
				if doneLists {
					finishLists()
				}
				return nil
			},
		})
	}

	enqueueBodies = func(items []jmapListed) {
		chunks := planJMAPBodyChunks(ordered, items, limit)
		mu.Lock()
		if aborted {
			mu.Unlock()
			return
		}
		listErr := folderSyncError(failed, len(ordered), firstErr)
		mu.Unlock()
		if len(chunks) == 0 {
			report(listErr)
			return
		}

		var bmu sync.Mutex
		var bodyFailed []string
		var bodyFirst error
		noteBody := func(name string, err error) {
			bmu.Lock()
			if bodyFirst == nil {
				bodyFirst = err
			}
			bodyFailed = append(bodyFailed, name)
			bmu.Unlock()
		}
		finishBodies := func() {
			bmu.Lock()
			ferr := bodyFirst
			names := append([]string(nil), bodyFailed...)
			bmu.Unlock()
			if ferr != nil {
				report(folderSyncError(names, len(chunks), ferr))
				return
			}
			report(listErr)
		}

		runner := &jmapBodyChunkRunner{
			s:         s,
			chunks:    chunks,
			maxBg:     s.MaxBackgroundSlots(),
			stop:      stop,
			fetch:     fetch,
			noteBody:  noteBody,
			finishAll: finishBodies,
		}
		runner.start()
	}

	for _, f := range ordered {
		enqueueList(f, 0)
	}
}

func planJMAPBodyChunks(folders []storage.Folder, items []jmapListed, limit int) []jmapBodyChunkJob {
	var ordered []jmapListed
	if limit <= 0 {
		ordered = append([]jmapListed(nil), items...)
		sortJMAPListed(ordered)
	} else {
		byFolder := make(map[int64][]jmapListed, len(folders))
		for _, it := range items {
			byFolder[it.Folder.ID] = append(byFolder[it.Folder.ID], it)
		}
		for _, f := range folders {
			rows := byFolder[f.ID]
			sortJMAPListed(rows)
			if len(rows) > limit {
				rows = rows[:limit]
			}
			ordered = append(ordered, rows...)
		}
	}

	var chunks []jmapBodyChunkJob
	for len(ordered) > 0 {
		folder := ordered[0].Folder
		n := 1
		for n < len(ordered) && n < jmapBodyChunk && ordered[n].Folder.ID == folder.ID {
			n++
		}
		ids := make([]string, n)
		for i := 0; i < n; i++ {
			ids[i] = ordered[i].RemoteID
		}
		chunks = append(chunks, jmapBodyChunkJob{folder: folder, ids: ids})
		ordered = ordered[n:]
	}
	return chunks
}

func sortJMAPListed(items []jmapListed) {
	sort.Slice(items, func(i, j int) bool {
		if !items[i].Date.Equal(items[j].Date) {
			return items[i].Date.After(items[j].Date)
		}
		if items[i].Folder.ID != items[j].Folder.ID {
			return items[i].Folder.ID < items[j].Folder.ID
		}
		return items[i].RemoteID < items[j].RemoteID
	})
}

// execJMAPList stores the full folder list. X caps bodies later, not stubs.
// A folder push already floored at X still widens here. The returned items are
// the body targets: the stored stubs among the folder's bodyLimit newest rows that
// still lack a body (0 = all incomplete), not the reconcile ToFetch, which is empty once stubs exist.
// Without a backfill the list goes through listFolderOnce. A backfill always
// lists: it widens the window, which a coalesced follow-up would not do.
func (a *App) execJMAPList(ctx context.Context, account storage.Account, folder storage.Folder, backfill, bodyLimit int) ([]jmapListed, bool, error) {
	client, err := a.jmapClient(ctx, &account)
	if err != nil {
		a.noteSyncPoolOutcome(account.ID, err)
		return nil, false, err
	}
	engine := a.jmapStubEngine(pjmap.NewAdapter(client, account.JMAPMailAccountID), account.ID)
	hasOlder := false
	list := func() error {
		res, err := engine.SyncFolderStubs(ctx, folder, backfill)
		hasOlder = hasOlder || res.HasOlder
		a.noteSyncPoolOutcome(account.ID, err)
		return err
	}
	if backfill > 0 {
		err = list()
	} else {
		err = a.listFolderOnce(ctx, account.ID, folder.ID, list)
	}
	if err != nil {
		return nil, false, err
	}
	ids, err := a.store.RemoteIDsNeedingBodyNewest(ctx, folder.ID, bodyLimit)
	if err != nil {
		return nil, false, err
	}
	return a.jmapListedFromStore(ctx, folder, ids), hasOlder, nil
}

// execJMAPDeltaStep runs one delta job. backfill widens the stub window;
// bodyLimit is the total X newest bodies per folder (0 = all).
func (a *App) execJMAPDeltaStep(ctx context.Context, account storage.Account, folder storage.Folder, kind syncsched.JobKind, backfill, bodyLimit int, stats *imapStepStats) ([]string, error) {
	switch kind {
	case syncsched.JobListStubs:
		listed, hasOlder, err := a.execJMAPList(ctx, account, folder, backfill, bodyLimit)
		if err != nil {
			return nil, err
		}
		if stats != nil {
			stats.hasOlder = stats.hasOlder || hasOlder
		}
		ids := make([]string, len(listed))
		for i, item := range listed {
			ids[i] = item.RemoteID
		}
		return ids, nil
	case syncsched.JobFetchBodies:
		err := a.execJMAPBodies(ctx, account, folder, false)
		if stats != nil && err == nil {
			stats.newCount++
		}
		return nil, err
	default:
		return nil, nil
	}
}

func (a *App) jmapListedFromStore(ctx context.Context, folder storage.Folder, ids []string) []jmapListed {
	dates := map[string]time.Time{}
	if a.store != nil && len(ids) > 0 {
		if got, err := a.store.MessageDatesByRemoteID(ctx, folder.ID, ids); err == nil {
			dates = got
		}
	}
	out := make([]jmapListed, 0, len(ids))
	for _, id := range ids {
		out = append(out, jmapListed{Folder: folder, RemoteID: id, Date: dates[id]})
	}
	return out
}

// execJMAPBodies is a background body job: it fetches the job attempt's ids
// and soft-pauses between chunks. fromReconcile marks bodies a full folder
// check found.
func (a *App) execJMAPBodies(ctx context.Context, account storage.Account, folder storage.Folder, fromReconcile bool) error {
	client, err := a.jmapClient(ctx, &account)
	if err != nil {
		a.noteSyncPoolOutcome(account.ID, err)
		return err
	}
	engine := a.newSyncEngine(a.newJMAPSyncAdapter(client, account.ID, account.JMAPMailAccountID), account.ID)
	engine.FetchBatchSize = jmapBodyFetchBatch
	engine.AbsorbArrivals = false
	a.applyBackgroundPause(engine, account.ID)
	announce := a.bodyAnnouncer(engine, fromReconcile)
	remote := syncsched.RemoteIDs(ctx)
	res, err := engine.FetchBodies(ctx, folder, remote)
	if err == nil {
		announce(folder, res)
	}
	if ctx.Err() == nil && !errors.Is(err, psync.ErrSoftPaused) {
		if ferr := a.finishIMAPFolder(ctx, engine, folder); ferr != nil && err == nil {
			err = ferr
		}
	}
	if !errors.Is(err, psync.ErrSoftPaused) {
		a.noteSyncPoolOutcome(account.ID, err)
	}
	return err
}

func (a *App) execJMAPOnDemandBodies(ctx context.Context, account storage.Account, folder storage.Folder) error {
	client, err := a.jmapClient(ctx, &account)
	if err != nil {
		return err
	}
	engine := a.newSyncEngine(a.newJMAPSyncAdapter(client, account.ID, account.JMAPMailAccountID), account.ID)
	engine.FetchBatchSize = jmapBodyFetchBatch
	a.applySchedulerPause(engine, account.ID)
	defer a.closeProgress(account.ID)
	remote := syncsched.RemoteIDs(ctx)
	res, err := engine.FetchBodies(ctx, folder, remote)
	if err == nil {
		a.announceNewBodies(folder, res)
	}
	if err == nil {
		// a failed repair fails the job, so the pane is not told the message
		// changed and does not fetch it again in a loop.
		var repaired []int64
		repaired, err = engine.RepairRemoteIDs(ctx, folder, remote)
		a.afterRepairs(folder, repaired)
	}
	return err
}

// jmapStubEngine lists every message in the folder. InitialLimit 0 only
// applies while the folder is uninitialized; FullList clears a floor that
// push or reconnect already stored at X. Body jobs still cap at X.
func (a *App) jmapStubEngine(adapter psync.Adapter, accountID int64) *psync.Engine {
	engine := a.newSyncEngine(adapter, accountID)
	engine.InitialLimit = 0
	engine.FullList = true
	a.applyBackgroundPause(engine, accountID)
	return engine
}

// syncJMAPInitial records the full stub-and-body campaign the way
// syncIMAPInitial records the initial sync. Manual Sync does not call it.
func (a *App) syncJMAPInitial(ctx context.Context, account storage.Account) error {
	if account.Local {
		return nil
	}
	err := a.syncAccountOnceJMAP(ctx, account)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	a.noteSyncOutcome(account.ID, err)
	return err
}

// runJMAPManualSync is the P0 refresh behind TriggerSync and SyncAccountNow.
// It reconciles newest mail on the reserved live slot. It does not enqueue
// the stub list or the X/all body campaign.
func (a *App) runJMAPManualSync(ctx context.Context, account storage.Account) error {
	a.emit(EventSyncState, SyncStateEvent{Running: true})
	defer a.emit(EventSyncState, SyncStateEvent{Running: false})
	return a.enqueueLiveAndWait(ctx, account.ID, syncsched.JobManualSync, 0, nil, func(jobCtx context.Context) error {
		if a.protocolSwitchedSince(jobCtx, account) {
			return nil
		}
		if a.jmapManualSyncForTest != nil {
			return a.jmapManualSyncForTest(jobCtx, account)
		}
		return a.syncJMAPIncremental(jobCtx, account)
	})
}

// syncJMAPIncremental refreshes mailboxes and reconciles each selected folder
// inside the live job that already holds the pool slot. SyncFolder fetches
// bodies only for messages that are not cached yet, so a manual refresh does
// not replay the body campaign for stubs already stored.
func (a *App) syncJMAPIncremental(ctx context.Context, account storage.Account) error {
	return a.syncJMAPFollowUpAll(ctx, account)
}

// jmapBackfillBodyLimit is the body window a JMAP backfill list step admits.
// Stub widening clears the JMAP floor, so the window is the bodies already
// cached (they are newest-first and contiguous) plus this page. Only the list
// step uses it, so other jobs skip the count. Unlimited stays 0.
func (a *App) jmapBackfillBodyLimit(ctx context.Context, folder storage.Folder, kind syncsched.JobKind, batch int) (int, error) {
	if kind != syncsched.JobListStubs || batch <= 0 {
		return 0, nil
	}
	have, err := a.store.CountBodyComplete(ctx, folder.ID)
	if err != nil {
		return 0, err
	}
	return have + batch, nil
}

// planJMAPAccount splits the cached rows since the cutoff into stubs to fetch
// and complete messages to pin.
func (a *App) planJMAPAccount(ctx context.Context, folders []storage.Folder, since time.Time) ([]dlTask, []int64, error) {
	var (
		tasks []dlTask
		pin   []int64
	)
	for _, folder := range folders {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		states, err := a.store.MessageBodyStates(a.ctx, folder.ID, since)
		if err != nil {
			return nil, nil, err
		}
		for _, s := range states {
			if s.BodyComplete {
				pin = append(pin, s.ID)
			} else {
				tasks = append(tasks, dlTask{folder: folder, remoteID: s.RemoteID, fillsStub: true})
			}
		}
	}
	return tasks, pin, nil
}

// moveMessageJMAP is moveMessageTo for a JMAP account: the move is an Email/set
// on the message's mailboxes, and the email keeps its id.
func (a *App) moveMessageJMAP(m *storage.Message, source, dest storage.Folder, account storage.Account) (ArchiveUndoDTO, error) {
	var (
		raw         []byte
		exportError string
	)
	err := a.withAccountAdapter(account.ID, func(ad psync.Adapter) error {
		if exportWanted(account, dest) {
			var ferr error
			raw, ferr = rawFromAdapter(a.ctx, ad, source, m.RemoteID)
			if ferr != nil {
				raw = nil
				exportError = ferr.Error()
				a.log.Error("archive export: fetch source", "id", m.ID, "err", ferr)
			}
		}
		return ad.Move(a.ctx, source.RemoteID, []string{m.RemoteID}, dest.RemoteID)
	})
	if err != nil {
		return ArchiveUndoDTO{}, err
	}
	return a.finishMove(m, source, dest, account, raw, exportError)
}

// jmapHTTPEntry is a JMAP mailbox's shared http client and the settings it
// was built for.
type jmapHTTPEntry struct {
	key    string
	client *http.Client
}

// accountJMAPHTTPClient is the http client every JMAP client of an account
// shares, so sync jobs reuse open connections instead of each paying for a
// new TCP, proxy and TLS handshake. A change of route or trust replaces it and
// closes the old one's idle connections, which still carry the old settings.
// A route that cannot be resolved fails as accountRoute does.
func (a *App) accountJMAPHTTPClient(account storage.Account) (*http.Client, error) {
	route, err := a.accountRoute(account)
	if err != nil {
		return nil, err
	}
	trust := accountTrust(account)
	key := mailClientKey(route, trust)

	a.jmapHTTPMu.Lock()
	defer a.jmapHTTPMu.Unlock()
	old, ok := a.jmapHTTP[account.ID]
	if ok && old.key == key {
		return old.client, nil
	}
	if ok {
		old.client.CloseIdleConnections()
	}
	if a.jmapHTTP == nil {
		a.jmapHTTP = make(map[int64]jmapHTTPEntry)
	}
	client := mailHTTPClient(route, trust, 0)
	a.jmapHTTP[account.ID] = jmapHTTPEntry{key: key, client: client}
	return client, nil
}

// dropJMAPHTTPClient forgets an account's shared JMAP client and closes its
// idle connections, for a mailbox that was removed or left JMAP.
func (a *App) dropJMAPHTTPClient(accountID int64) {
	a.jmapHTTPMu.Lock()
	old, ok := a.jmapHTTP[accountID]
	delete(a.jmapHTTP, accountID)
	a.jmapHTTPMu.Unlock()
	if ok {
		old.client.CloseIdleConnections()
	}
}
