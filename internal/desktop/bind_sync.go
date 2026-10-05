package desktop

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	gojmap "github.com/Janso123/go-jmap"

	"github.com/peltonapp/Pelton/internal/credentials"
	"github.com/peltonapp/Pelton/internal/desktop/syncsched"
	pimap "github.com/peltonapp/Pelton/internal/imap"
	pjmap "github.com/peltonapp/Pelton/internal/jmap"
	"github.com/peltonapp/Pelton/internal/outbox"
	psmtp "github.com/peltonapp/Pelton/internal/smtp"
	"github.com/peltonapp/Pelton/internal/storage"
	psync "github.com/peltonapp/Pelton/internal/sync"
	"github.com/peltonapp/Pelton/internal/sync/pool"
)

// submit sends via JMAP EmailSubmission. Tests replace it to avoid the network.
var submit = pjmap.Submit

// startWatch opens a JMAP WebSocket watch. Tests replace it.
var startWatch = pjmap.StartWatch

const (
	// idleRetry is how long an idle loop waits after a connection it did not
	// expect to lose.
	idleRetry = 15 * time.Second
	// idleNoCredentialsRetry is how long it waits when the account has no
	// password. Longer, because nothing changes until the user types one.
	idleNoCredentialsRetry = 60 * time.Second
	// idleYieldPoll is how often an IDLE session looks for other work that
	// needs its pool slot. The check is local; it does not touch the server.
	idleYieldPoll = 100 * time.Millisecond
)

// errIdleYield means this IDLE session is dropping its pool slot so other
// sync work can use the only connection it was holding.
var errIdleYield = errors.New("pelton: idle yielding pool slot")

// errIdleUnsupported means the server has no IDLE capability. The session
// must not keep a pool slot while it waits.
var errIdleUnsupported = errors.New("pelton: server does not support idle")

// syncStepAttempts is how many times ListStubs or FetchBodies runs for one
// folder before the initial sync order (Inbox stubs, Inbox bodies, Sent, then
// the rest) marks that folder failed and moves on. The first try counts.
const syncStepAttempts = 3

// syncRetryBackoff is the wait before another try of a failed ListStubs or
// FetchBodies step. attempt is 0 after the first failure. Tests shorten it.
var syncRetryBackoff = func(attempt int) time.Duration {
	d := 500 * time.Millisecond << attempt
	if d > 8*time.Second {
		return 8 * time.Second
	}
	return d
}

// accountWorker is one account's sync+idle/watch goroutine.
type accountWorker struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// activeSync is one account's in-flight sync run. cancel aborts
// context-aware work; close tears down the IMAP connection so a blocked FETCH
// returns and the account's lock can be released.
type activeSync struct {
	cancel context.CancelFunc
	close  func()
}

// closerToken closes an IMAP session once, whether the job returns or abort
// wins the race.
type closerToken struct {
	once sync.Once
	fn   func()
}

func (t *closerToken) Close() {
	if t == nil || t.fn == nil {
		return
	}
	t.once.Do(t.fn)
}

// accountSync is one account's scheduler and sync-session pool.
type accountSync struct {
	sched    *syncsched.Scheduler
	pool     *pool.Pool
	stopOnce sync.Once
	ended    chan struct{}
	// ctx is what the scheduler runs under. Once it is done the runtime is
	// stale and ensureAccountScheduler replaces it.
	ctx       context.Context
	mu        sync.Mutex
	liveHolds int
	// Adaptive throttle: halve effective N after repeated network/rate-limit
	// errors; grow back toward configured after a success streak.
	throttleFails     int
	throttleSuccesses int
	// userN is the parallelism the user last set for this account, as
	// reconfigureAccountPool last applied it. A save that leaves it unchanged
	// must not undo a throttle.
	userN int
	// reconciling holds the folders with a full reconcile queued or running,
	// so a second request for the same folder is dropped. verifyDone and
	// verifyTotal drive the "checking folders" progress line.
	reconciling             map[int64]struct{}
	verifyDone, verifyTotal int
	// reconcileBodies holds the folders with a reconcile body job queued or
	// running, so a later check does not queue a second one.
	reconcileBodies map[int64]struct{}
	// listing holds the folders with a list in flight; see listFolderOnce.
	// The value is true once another request was coalesced into that list.
	listing map[int64]bool
}

const (
	syncThrottleFailThreshold    = 2
	syncThrottleRecoverThreshold = 3
)

func (rt *accountSync) stop() {
	if rt == nil || rt.sched == nil {
		return
	}
	rt.stopOnce.Do(func() {
		rt.sched.Stop()
		close(rt.ended)
	})
}

// accountWorkerKey marks a worker's context with its account id. Every sync
// call made under it (initial sync, IDLE, JMAP watch and its push follow-ups)
// counts as the worker's own; see ensureAccountSync.
type accountWorkerKey struct{}

// workerOwns reports whether ctx belongs to the account's worker.
func workerOwns(ctx context.Context, accountID int64) bool {
	id, ok := ctx.Value(accountWorkerKey{}).(int64)
	return ok && id == accountID
}

// startAccountWorker cancels any existing worker for the account, then starts
// a fresh sync and idle/watch loop under the profile session. While the
// account is held (protocol switch, removal) it does nothing: the holder
// starts the worker itself when it is done, with startHeldAccountWorker.
func (a *App) startAccountWorker(accountID int64) {
	if held, _ := a.syncBlock(accountID); held {
		return
	}
	a.launchAccountWorker(accountID, false)
}

// startHeldAccountWorker is startAccountWorker for the caller holding the
// account. The worker it starts runs while the hold is still in place. An
// account that left the active profile while it was held (a profile edit
// stops its worker) stays stopped; a lookup failure starts it, since a missed
// sync is worse than a redundant one.
func (a *App) startHeldAccountWorker(accountID int64) {
	if ids, err := a.profileAccountIDs(); err == nil && !slices.Contains(ids, accountID) {
		return
	}
	a.launchAccountWorker(accountID, true)
}

func (a *App) launchAccountWorker(accountID int64, holder bool) {
	a.stopAccountWorker(accountID)

	parent := a.sessionCtx()
	ctx, cancel := context.WithCancel(context.WithValue(parent, accountWorkerKey{}, accountID))
	done := make(chan struct{})

	a.workersMu.Lock()
	// Checked again under workersMu: a hold taken after the check above is
	// followed by the holder's stopAccountWorker, which also takes workersMu,
	// so this worker is either refused here or stopped there.
	if held, _ := a.syncBlock(accountID); held && !holder {
		a.workersMu.Unlock()
		cancel()
		return
	}
	if a.workers == nil {
		a.workers = make(map[int64]*accountWorker)
	}
	a.workers[accountID] = &accountWorker{cancel: cancel, done: done}
	a.workersMu.Unlock()

	goSafe("syncing a mailbox", func() {
		defer close(done)
		// The worker owns the scheduler it started or found. If it exits late,
		// after a replacement worker started its own, it must leave that one
		// running, so teardown only removes this exact runtime.
		var rt *accountSync
		defer func() { a.stopAccountSchedulerIf(accountID, rt) }()
		account, err := a.store.GetAccount(a.ctx, accountID)
		if err != nil {
			a.log.Error("load account for worker", "account", accountID, "err", err)
			return
		}
		// The scheduler owns this account's sync jobs until the worker exits.
		// Stop is the hard cancel for shutdown, protocol switch and removal.
		rt, err = a.ensureAccountSync(ctx, account.ID)
		if err != nil {
			return
		}
		// JMAP watch starts with the first sync. The socket is outside the
		// HTTP pool; push follow-up is a live job that checks a slot out.
		// The first pass is the full stub list plus the body campaign.
		// Later manual Sync is a live incremental reconcile, not this pass.
		if account.Protocol == "jmap" {
			goSafe("watching for new mail", func() { a.idleLoop(ctx, *account) })
			err := a.syncJMAPInitial(ctx, *account)
			if err != nil {
				if errors.Is(err, errNoCredentials) {
					a.log.Warn("mailbox has no password, not syncing", "account", account.Email)
				} else if ctx.Err() == nil {
					a.log.Error("account sync", "account", account.Email, "err", err)
				}
			}
			if ctx.Err() == nil && !errors.Is(err, errNoCredentials) {
				a.enqueueDueReconcile(ctx, *account)
			}
			<-ctx.Done()
			return
		}
		startIdle := sync.OnceFunc(func() {
			goSafe("watching for new mail", func() { a.idleLoop(ctx, *account) })
		})
		err = a.syncIMAPInitial(ctx, *account, a.idleAfterInbox(account.ID, startIdle))
		if err != nil {
			if errors.Is(err, errNoCredentials) {
				a.log.Warn("mailbox has no password, not syncing", "account", account.Email)
			} else if ctx.Err() == nil {
				a.log.Error("account sync", "account", account.Email, "err", err)
			}
		}
		// IDLE starts here at N=1, and also after a pass that never reached
		// Inbox (no password, no folder list); the idle loop waits for a
		// password on its own.
		startIdle()
		if ctx.Err() == nil && !errors.Is(err, errNoCredentials) {
			a.enqueueDueReconcile(ctx, *account)
		}
		<-ctx.Done()
	})
}

// idleAfterInbox returns the first pass's Inbox callback. At effective N>=2
// it starts IDLE there, so mail that arrives during the rest of the pass shows
// up at once while background keeps its own slots. At N=1 IDLE would trade the
// only session with every background chunk, a login each time, so it waits
// for the worker to start it after the whole pass. N is read when Inbox is in,
// since throttling can lower it.
func (a *App) idleAfterInbox(accountID int64, startIdle func()) func() {
	return func() {
		if a.accountEffectiveN(accountID) >= 2 {
			startIdle()
		}
	}
}

// stopAccountWorkerTimeout is how long SwitchProtocol (and similar) wait for an
// account worker to exit after cancel. A stuck IMAP FETCH must not freeze the
// settings JMAP toggle forever. Tests shorten it.
var stopAccountWorkerTimeout = 5 * time.Second

// stopAccountWorker cancels and joins the worker for one account, if any.
// It also aborts that account's in-flight sync (closes its IMAP sessions) so
// the account's lock can be released. Other accounts are untouched. After
// stopAccountWorkerTimeout it returns anyway.
func (a *App) stopAccountWorker(accountID int64) {
	a.workersMu.Lock()
	w := a.workers[accountID]
	if w != nil {
		delete(a.workers, accountID)
	}
	a.workersMu.Unlock()
	if w == nil {
		a.abortAccountSync(accountID)
		a.stopAccountScheduler(accountID)
		return
	}
	w.cancel()
	a.abortAccountSync(accountID)
	// Hard-cancel queued and in-flight sync jobs. Ordinary live work does not
	// come through here.
	started := time.Now()
	a.stopAccountScheduler(accountID)
	jobsJoined := time.Now()
	timer := time.NewTimer(stopAccountWorkerTimeout)
	defer timer.Stop()
	select {
	case <-w.done:
	case <-timer.C:
		if a.log != nil {
			a.log.Warn("account worker stop timed out", "accountID", accountID)
		}
	}
	if a.log != nil {
		a.log.Debug("account worker stopped", "account", accountID,
			"jobs_ms", jobsJoined.Sub(started).Milliseconds(), "worker_ms", time.Since(jobsJoined).Milliseconds())
	}
}

// hasAccountWorker reports whether the account has a registered worker.
func (a *App) hasAccountWorker(accountID int64) bool {
	a.workersMu.Lock()
	defer a.workersMu.Unlock()
	return a.workers[accountID] != nil
}

// joinAllAccountWorkers waits for every registered worker to exit. The session
// cancel should already have been fired so they leave their loops.
func (a *App) joinAllAccountWorkers() {
	a.workersMu.Lock()
	workers := make([]*accountWorker, 0, len(a.workers))
	for id, w := range a.workers {
		workers = append(workers, w)
		delete(a.workers, id)
	}
	a.workersMu.Unlock()
	for _, w := range workers {
		w.cancel()
		<-w.done
	}
}

// idleRetryWait says how long to wait before opening an account's idle
// connection again.
//
// A missing password used to end the loop for good. That made a mailbox
// unfixable without a restart: the user typed the password, the account still
// received nothing, and only the next launch brought it back. Waiting is the
// whole point, since a password can arrive at any moment.
//
// Tests replace idleRetryWaitFor to avoid multi-second sleeps.
var idleRetryWaitFor = idleRetryWait

func idleRetryWait(err error) time.Duration {
	if errors.Is(err, errNoCredentials) {
		return idleNoCredentialsRetry
	}
	return idleRetry
}

// startBackgroundServices launches the outbox worker and the initial sync plus
// per-account idle loops. Credentials come from the keyring (added by the
// wizard) with an environment fallback for the legacy cli account.
func (a *App) startBackgroundServices() {
	goSafe("sending queued mail", a.runOutboxWorker)
	goSafe("the first sync", a.runInitialSyncAndIdle)
	goSafe("waking snoozed mail", a.runSnoozePoller)
	goSafe("collecting addresses", a.harvestAddressBook)
	goSafe("the periodic sync", a.runAutoSyncLoop)
	goSafe("checking folders", a.runDueReconcileLoop)
	a.startMCPIfEnabled()
	goSafe("counting unread mail", a.refreshViewCounts)
}

// runAutoSyncLoop periodically runs a full sync pass across every account, on
// top of the always-on imap idle push (which not every server supports, and
// which can silently drop on flaky networks). the interval is a user setting
// (0 disables it); a short base tick lets a changed interval or low-power
// toggle take effect promptly without needing its own change-notification
// channel. it does nothing while low-power mode is on.
func (a *App) runAutoSyncLoop() {
	const baseTick = 5 * time.Second
	ticker := time.NewTicker(baseTick)
	defer ticker.Stop()
	lastRun := time.Now()
	for {
		select {
		case <-a.ctx.Done():
			return
		case <-ticker.C:
			interval := a.intSetting(settingAutoSync, 900)
			if interval <= 0 || a.lowPowerMode() {
				continue
			}
			if time.Since(lastRun) < time.Duration(interval)*time.Second {
				continue
			}
			lastRun = time.Now()
			if err := a.runAutoSyncPass(); err != nil && !errors.Is(err, errNoCredentials) {
				a.log.Error("auto sync", "err", err)
			}
		}
	}
}

// runOutboxWorker drains the outbox, resolving smtp credentials per message from
// the sending account. Messages whose account has no credentials stay queued and
// surface in the outbox view.
func (a *App) runOutboxWorker() {
	transmitter := &accountTransmitter{app: a}
	worker := outbox.NewWorker(a.queue, transmitter,
		outbox.WithLogger(a.log),
		// emit after every state change so the ui reflects sending -> sent/failed
		// promptly. without this the outbox banner stayed stuck on "sending".
		outbox.WithOnChange(func() {
			a.emit(EventOutboxChanged, nil)
		}),
	)
	if _, err := a.queue.RequeueStuck(a.ctx); err != nil {
		a.log.Error("requeue stuck outbox", "err", err)
	}
	if err := worker.Run(a.ctx); err != nil && a.ctx.Err() == nil {
		a.log.Error("outbox worker stopped", "err", err)
	}
}

// accountTransmitter sends a queued message using the credentials of its
// account, resolved fresh each attempt so refreshed oauth tokens are picked up.
type accountTransmitter struct {
	app *App
}

func (t *accountTransmitter) Transmit(ctx context.Context, m outbox.Message) error {
	// note: the worker emits EventOutboxChanged via WithOnChange after the state
	// is persisted, so we must not emit here (that fired before markSent and left
	// the ui stuck on "sending").
	// SMTP and JMAP EmailSubmission stay outside the sync pool and pause
	// nothing: background sync keeps running while mail is sent. Never
	// Acquire a slot here.
	account, err := t.app.store.GetAccount(ctx, m.AccountID)
	if err != nil {
		return err
	}
	if account.Protocol == "jmap" {
		return t.transmitJMAP(ctx, *account, m)
	}
	cfg, err := t.app.resolveSMTP(*account)
	if err != nil {
		return err
	}
	sender := psmtp.NewSender(cfg,
		psmtp.WithLogger(t.app.log),
		psmtp.WithSentAppender(func(raw []byte) (string, error) {
			return t.app.appendToSent(*account, raw)
		}),
	)
	return sender.Transmit(ctx, m)
}

// appendToSent puts a copy of a message that has just been sent in the
// account's Sent folder, so it is there in webmail and in every other client,
// not only in Pelton (#451).
//
// It opens its own imap session rather than borrowing the sync engine's. That
// one is parked in IDLE almost all the time, and interrupting it to append
// would cost more than a second connection does for something that happens
// once per sent message.
//
// The copy is appended to the server and not written locally: the folder's next
// sync pulls it down like any other message, which keeps one path for how mail
// arrives in the store.
func (a *App) appendToSent(account storage.Account, raw []byte) (string, error) {
	cfg, err := a.resolveIMAP(account)
	if err != nil {
		return "", err
	}
	// the account's lock (which replaced the process-wide syncMu) serializes
	// this with its mailbox mutations and a protocol switch, so the append
	// never races a teardown of the account's sessions.
	lock := a.accountLock(account.ID)
	lock.Lock()
	defer lock.Unlock()
	client, err := a.connectIMAP(cfg)
	if err != nil {
		return "", err
	}
	defer func() { _ = client.Close() }()
	if err := client.Login(); err != nil {
		return "", err
	}
	defer func() { _ = client.Logout() }()
	return client.AppendToSent(raw)
}

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

// sentMailboxRemoteID returns the remote id of the account's Sent folder, or
// empty when none is classified as sent (Submit then returns ErrNoSentMailbox).
func (a *App) sentMailboxRemoteID(ctx context.Context, accountID int64) (string, error) {
	folders, err := a.store.ListFolders(ctx, accountID)
	if err != nil {
		return "", err
	}
	for _, f := range folders {
		if folderRole(f) != roleSent {
			continue
		}
		if f.RemoteID != "" {
			return f.RemoteID, nil
		}
		return f.IMAPPath, nil
	}
	return "", nil
}

// runInitialSyncAndIdle syncs every account once, then parks each on idle.
func (a *App) runInitialSyncAndIdle() {
	accounts, err := a.store.ListAccounts(a.ctx)
	if err != nil {
		a.log.Error("list accounts for sync", "err", err)
		return
	}
	for _, account := range accounts {
		if account.Local {
			continue
		}
		a.startAccountWorker(account.ID)
	}
	// the marks from this pass are pushed like any other run's. without it a
	// mailbox that failed its first sync stayed unmarked until some later run
	// happened to end, which for a mailbox with no password is never.
	a.emitAccountSyncStates()
	// contacts ride along with the mail sync (#168). It is one cheap request
	// per address book when nothing changed, and it runs after the mail so a
	// slow contacts server never delays the inbox.
	a.syncContactsInBackground()
}

// TriggerSync syncs all accounts on demand (the ui refresh action). It returns a
// clear error only when no account could be synced for lack of credentials.
// Manual sync still runs for a JMAP account whose WebSocket push is healthy;
// only the timed auto-sync pass skips that account.
func (a *App) TriggerSync() error {
	return a.syncListedAccounts(nil, true)
}

// runAutoSyncPass is one tick of timed auto-sync. A JMAP account with a healthy
// WebSocket is left out; IMAP and a JMAP account without push still sync.
func (a *App) runAutoSyncPass() error {
	return a.syncListedAccounts(a.autoSyncAccount, false)
}

// autoSyncAccount reports whether timed auto-sync should touch this account.
func (a *App) autoSyncAccount(account storage.Account) bool {
	if account.Local {
		return false
	}
	if account.Protocol == "jmap" && a.jmapPushHealthy(account.ID) {
		return false
	}
	return true
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

// syncListedAccounts syncs every remote account include accepts (all when nil).
// manual is true when the user asked for the sync, which also queues the
// background full folder check of every folder; otherwise only due folders
// are queued.
func (a *App) syncListedAccounts(include func(storage.Account) bool, manual bool) error {
	if err := a.ready(); err != nil {
		return err
	}
	accounts, err := a.store.ListAccounts(a.ctx)
	if err != nil {
		return err
	}

	synced := 0
	netFailed := false
	syncable := 0
	for _, account := range accounts {
		if account.Local {
			continue
		}
		if include != nil && !include(account) {
			continue
		}
		syncable++
		if err := a.syncAccount(account, manual); err != nil {
			if errors.Is(err, errNoCredentials) {
				continue
			}
			if isNetworkError(err) {
				netFailed = true
			}
			a.log.Error("sync account", "account", account.Email, "err", err)
			continue
		}
		synced++
	}
	// the marks are what carries a partial failure now, so they are pushed
	// whatever this returns: an account that failed while the others got
	// through used to leave the ui saying the sync was clean.
	a.emitAccountSyncStates()
	// the address books refresh with the mail rather than on a timer of their
	// own, off the calling goroutine so a contacts server that is down cannot
	// make the refresh button hang (#168).
	a.syncContactsInBackground()
	// the Local Folders account is not counted: an install holding only
	// imported mail has nothing to sync, which is not a credentials problem.
	if synced == 0 && syncable > 0 {
		// a dropped connection must not masquerade as a credentials problem: if
		// nothing synced and any account failed for the network, report offline.
		if netFailed {
			return errOffline
		}
		return errNoCredentials
	}
	return nil
}

// syncAccount syncs one account and records how it went, so a failure survives
// the run it happened in. Every caller goes through here rather than
// syncAccountOnce, since an unrecorded failure is the bug (#322). manual is
// true only for a sync the user asked for: a successful one then queues the
// full folder check of every folder. A successful timed auto-sync queues it
// only for folders whose last check is older than the setting.
func (a *App) syncAccount(account storage.Account, manual bool) error {
	return a.syncAccountCtx(a.ctx, account, manual)
}

func (a *App) syncAccountCtx(ctx context.Context, account storage.Account, manual bool) error {
	// Local Folders has no server behind it: imported mail is never uploaded,
	// reconciled or expunged. It has no sync state either, since it is not
	// failing to do something it never does.
	if account.Local {
		return nil
	}
	err := a.syncAccountOnceCtx(ctx, account)
	if ctx.Err() != nil {
		// Aborted for a protocol switch or shutdown, not a sync failure.
		return ctx.Err()
	}
	if errors.Is(err, errAccountSyncHeld) {
		// The account is switching protocol or was just removed; the switch
		// starts its own sync when it is done.
		return nil
	}
	a.noteSyncOutcome(account.ID, err)
	if err == nil {
		if manual {
			a.enqueueReconcileAfterManual(ctx, account)
		} else {
			a.enqueueDueReconcile(ctx, account)
		}
	}
	return err
}

// syncAccountOnceCtx syncs one account on demand. Both protocols enqueue a
// live job: newest mail, not a fresh stub/body campaign, and not a
// process-wide lock. The JMAP body campaign is syncJMAPInitial.
func (a *App) syncAccountOnceCtx(ctx context.Context, account storage.Account) error {
	if account.Protocol == "jmap" {
		return a.runJMAPManualSync(ctx, account)
	}
	return a.runIMAPManualSync(ctx, account)
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

// syncFolders runs the sync engine over each stored folder of an account,
// emitting a progress event per folder and a new-mail event when one gained
// messages.
func (a *App) syncFolders(ctx context.Context, adapter psync.Adapter, accountID int64) error {
	all, err := a.store.ListFolders(ctx, accountID)
	if err != nil {
		return err
	}
	// folders the user unchecked are skipped here rather than inside the engine,
	// so they never reach a SELECT and never cost a round trip. They are also
	// left out of the progress total, since counting folders that are not being
	// synced makes the bar lie (#173).
	folders := make([]storage.Folder, 0, len(all))
	for _, f := range all {
		// a container the server marks \Noselect holds no mail and cannot be
		// selected, so syncing one fails every single time. It is kept as a row
		// because the tree needs it as a parent, but it is not synced.
		if !f.SyncExcluded && folderSelectable(f) {
			folders = append(folders, f)
		}
	}
	engine := a.newSyncEngine(adapter, accountID)
	email := a.accountEmail(accountID)
	server := ""
	if adapter != nil {
		server = adapter.Addr()
	}
	tally := a.accountTally(accountID)
	tally.begin(len(folders))
	// an early return (cancelled run) would otherwise leave the ui's line for
	// this account running forever.
	closed := false
	defer func() {
		if !closed {
			a.emitSyncProgress(accountID, email, server, tally.closing())
		}
	}()
	// registered after the closing defer so it stops first: the heartbeat must
	// be gone before the close is sent.
	stopBeat := a.startProgressHeartbeat(ctx, accountID)
	defer stopBeat()

	// JMAP still locks per folder so ReleaseLock can yield between body batches.
	// The lock is this account's only; other accounts never wait on it. IMAP
	// manual sync is one live pool job and does not hold it.
	lockPerFolder := false
	lock := a.accountLock(accountID)
	if acc, err := a.store.GetAccount(a.ctx, accountID); err == nil && acc.Protocol == "jmap" {
		lockPerFolder = true
	} else {
		a.applySchedulerPause(engine, accountID)
	}

	newTotal := 0
	// a folder that fails does not stop the others: mail the rest of the account
	// can still fetch is worth having. But the failure is carried out of here,
	// because reporting the account as synced when a folder did not sync is what
	// makes a mailbox go quiet with nothing to show for it.
	var failedFolders []string
	var firstFolderErr error
	for i, f := range folders {
		if err := ctx.Err(); err != nil {
			return err
		}
		tally.enterFolder(i, f.Name)
		a.emitSyncProgress(accountID, email, server, tally.counts())
		if lockPerFolder {
			lock.Lock()
		}
		var res psync.FolderSyncResult
		err := a.listFolderOnce(ctx, accountID, f.ID, func() error {
			r, err := engine.SyncFolder(ctx, f)
			res.New += r.New
			res.NewIDs = append(res.NewIDs, r.NewIDs...)
			res.RepairedIDs = append(res.RepairedIDs, r.RepairedIDs...)
			return err
		})
		if lockPerFolder {
			lock.Unlock()
		}
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			a.log.Error("sync folder", "folder", f.Name, "err", err)
			failedFolders = append(failedFolders, f.Name)
			if firstFolderErr == nil {
				firstFolderErr = err
			}
			continue
		}
		if res.New > 0 {
			newTotal += res.New
			a.emit(EventMailNew, MailNewEvent{AccountID: accountID, FolderID: f.ID, Count: res.New})
			goSafe("announcing new mail", func() { a.notifyNewMail(f, res.NewIDs) })
		}
		a.afterRepairs(f, res.RepairedIDs)
	}
	// an empty folder name is how the ui knows the run is over and clears its
	// line; the counts ride along so a finished bar reads full rather than
	// snapping back to nothing.
	final := tally.counts()
	final.Folder = ""
	final.FoldersDone = len(folders)
	closed = true
	stopBeat()
	a.emitSyncProgress(accountID, email, server, final)

	// index the freshly synced mail so it becomes searchable. run it off the sync
	// path so the search backfill never holds up the next sync.
	if newTotal > 0 {
		goSafe("indexing new mail", func() { _ = a.indexNewMessages() })
		goSafe("counting unread mail", a.refreshViewCounts)
		if !a.lowPowerMode() {
			goSafe("collecting addresses", a.harvestAddressBook)
		}
	}
	return folderSyncError(failedFolders, len(folders), firstFolderErr)
}

// folderSyncError turns the folders that failed in one run into the error the
// account's sync outcome records, or nil when they all got through.
//
// The first failure is wrapped rather than described, so the reason survives:
// the ui classifies a sync failure by what the error is, and the detail dialog
// shows the server's own words.
func folderSyncError(failed []string, total int, first error) error {
	if first == nil {
		return nil
	}
	if len(failed) == 1 {
		return fmt.Errorf("sync folder %q: %w", failed[0], first)
	}
	return fmt.Errorf("%d of %d folders failed to sync, starting with %q: %w",
		len(failed), total, failed[0], first)
}

// findInboxFolder returns the account's INBOX folder row. IMAP's INBOX is a
// case-insensitive special name, so the match ignores case.
func (a *App) findInboxFolder(accountID int64) (*storage.Folder, error) {
	folders, err := a.store.ListFolders(a.ctx, accountID)
	if err != nil {
		return nil, err
	}
	for i := range folders {
		if strings.EqualFold(folders[i].IMAPPath, "INBOX") {
			return &folders[i], nil
		}
	}
	return nil, fmt.Errorf("no inbox folder for account %d", accountID)
}

// syncOneFolder runs the sync engine over a single folder, emitting the same
// progress/new-mail events syncFolders would, without touching any other
// folder on the account. Used by the idle push handler so a single INBOX
// update does not pay for a full-account resync.
func (a *App) syncOneFolder(adapter psync.Adapter, folder storage.Folder) error {
	return a.syncOneFolderCtx(a.ctx, adapter, folder)
}

func (a *App) syncOneFolderCtx(ctx context.Context, adapter psync.Adapter, folder storage.Folder) error {
	// the idle push path reaches this directly, so it has to honour the
	// exclusion too. An excluded INBOX is unusual but it is the user's call.
	if folder.SyncExcluded {
		return nil
	}
	defer a.closeProgress(folder.AccountID)
	return a.syncFolderAndAnnounce(ctx, a.newSyncEngine(adapter, folder.AccountID), folder)
}

// idleEntryResync catches Inbox up when IDLE (re)enters, for mail that
// arrived while IDLE was away and raises no IDLE event now. It runs only with
// a stored cursor, where it is a cheap delta (one SELECT when nothing
// changed); without one it would fetch every message's flags, and IDLE's
// events and the timed sync cover that Inbox. It is not a counted run, so it
// neither draws nor closes the account's progress line, which a running sync
// may own.
func (a *App) idleEntryResync(ctx context.Context, adapter psync.Adapter, inbox storage.Folder) error {
	if inbox.SyncExcluded || inbox.StateToken == "" {
		return nil
	}
	engine := a.newSyncEngine(adapter, inbox.AccountID)
	engine.OnProgress = nil
	return a.syncFolderAndAnnounce(ctx, engine, inbox)
}

// syncFolderAndAnnounce syncs one folder on engine as the live job it runs
// under, then announces repairs and new mail.
func (a *App) syncFolderAndAnnounce(ctx context.Context, engine *psync.Engine, folder storage.Folder) error {
	a.applySchedulerPause(engine, folder.AccountID)
	return a.listFolderOnce(ctx, folder.AccountID, folder.ID, func() error {
		res, err := engine.SyncFolder(ctx, folder)
		if err != nil {
			return err
		}
		a.afterRepairs(folder, res.RepairedIDs)
		if res.New > 0 {
			a.emit(EventMailNew, MailNewEvent{AccountID: folder.AccountID, FolderID: folder.ID, Count: res.New})
			goSafe("announcing new mail", func() { a.notifyNewMail(folder, res.NewIDs) })
			goSafe("indexing new mail", func() { _ = a.indexNewMessages() })
			goSafe("counting unread mail", a.refreshViewCounts)
			if !a.lowPowerMode() {
				goSafe("collecting addresses", a.harvestAddressBook)
			}
		}
		return nil
	})
}

// idleLoop parks one account on imap idle or a JMAP watch and re-syncs when the
// server reports activity, reconnecting with a short backoff and exiting on
// worker or app shutdown.
func (a *App) idleLoop(ctx context.Context, account storage.Account) {
	if account.Local {
		return
	}
	if account.Protocol == "jmap" {
		a.idleLoopJMAP(ctx, account)
		return
	}
	for ctx.Err() == nil {
		if err := a.idleSession(ctx, account); err != nil && ctx.Err() == nil {
			if !errors.Is(err, errNoCredentials) {
				a.log.Error("idle session", "account", account.Email, "err", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(idleRetryWaitFor(err)):
			}
		}
	}
}

func (a *App) idleLoopJMAP(ctx context.Context, account storage.Account) {
	for ctx.Err() == nil {
		err := a.watchSession(ctx, account)
		if errors.Is(err, pjmap.ErrNoWebSocket) {
			return
		}
		if err != nil && ctx.Err() == nil {
			if !errors.Is(err, errNoCredentials) {
				a.log.Error("jmap watch", "account", account.Email, "err", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(idleRetryWaitFor(err)):
			}
		}
	}
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

// idleSession parks on IMAP IDLE using a Live slot from the account pool, so
// the IDLE connection counts toward N. New-mail resync runs on that same
// session and does not open a second one.
//
// N>=2: IDLE holds the reserved live slot. Background sync keeps the other
// slots. When manual sync or an on-demand body needs a slot the pool does not
// have free, IDLE logs out and releases Live, then takes it back when that
// job finishes.
//
// N=1: there is one sync session. IDLE soft-pauses background work so the
// current chunk finishes and the queue holds, then takes that session. It
// does not wait for the rest of the campaign to drain. While IDLE holds the
// slot, queued background work makes IDLE yield so the next chunk can run.
// Send stays on SMTP and does not take the slot.
func (a *App) idleSession(ctx context.Context, account storage.Account) error {
	cfg, err := a.resolveIMAP(account)
	if err != nil {
		return err
	}
	if _, err := a.ensureAccountScheduler(ctx, account.ID); err != nil {
		return err
	}

	for ctx.Err() == nil {
		if err := a.waitIdleCanHold(ctx, account.ID); err != nil {
			return err
		}
		release, holdsPause, err := a.checkoutIdleSlot(ctx, account.ID)
		if err != nil {
			return err
		}
		err = a.idleWithSlot(ctx, account, cfg, holdsPause)
		bgStarts := a.backgroundStarts(account.ID)
		release()
		if errors.Is(err, errIdleYield) {
			a.waitIdleHandoff(ctx, account.ID, bgStarts)
			continue
		}
		if errors.Is(err, errIdleUnsupported) {
			<-ctx.Done()
			return nil
		}
		return err
	}
	return ctx.Err()
}

// backgroundStarts is the scheduler's background start count, or 0 without
// a scheduler. Read it while IDLE still holds its slot.
func (a *App) backgroundStarts(accountID int64) uint64 {
	rt := a.accountSync(accountID)
	if rt == nil || rt.sched == nil {
		return 0
	}
	return rt.sched.BackgroundStarts()
}

// waitIdleHandoff runs after IDLE gave its slot back. At effective N=1 it
// waits until a queued background job has the slot and is past the
// scheduler's hold check, so IDLE's next soft-pause cannot requeue that job
// before it runs a chunk. It returns at once when nothing background is
// waiting to start, and gives up after idleYieldPoll*20 so IDLE never hangs.
// At N>=2 background has its own slots and there is nothing to wait for.
func (a *App) waitIdleHandoff(ctx context.Context, accountID int64, since uint64) {
	rt := a.accountSync(accountID)
	if rt == nil || rt.sched == nil || a.accountEffectiveN(accountID) >= 2 {
		return
	}
	waitCtx, cancel := context.WithTimeout(ctx, idleYieldPoll*20)
	defer cancel()
	_ = rt.sched.WaitBackgroundStarted(waitCtx, since)
}

// waitIdleCanHold blocks until IDLE may try to take a live slot. Another
// live job always waits. At N=1 a background campaign does not: checkout
// soft-pauses the current chunk and Acquire waits only until that slot is
// free, so IDLE runs before the campaign drains.
func (a *App) waitIdleCanHold(ctx context.Context, accountID int64) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !a.liveJobBlocksIdle(accountID) {
			return nil
		}
		timer := time.NewTimer(idleYieldPoll)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// liveJobBlocksIdle reports whether another P0 job needs the session IDLE
// would hold. Background list and body work is not included.
func (a *App) liveJobBlocksIdle(accountID int64) bool {
	rt := a.accountSync(accountID)
	if rt == nil || rt.sched == nil {
		return false
	}
	return rt.sched.LiveBusy()
}

// idleBlocked reports whether IDLE should give up a slot it already holds.
// Another live job always counts. Background work counts when IDLE holds a
// soft-pause, whatever N is now: the throttle may have raised N since
// checkout, and the pause would keep the queue held while IDLE parks. Without
// a pause, background counts only at N=1, read now since throttling can lower
// it; at N>=2 background keeps its own slots. Waiting to acquire the slot is
// liveJobBlocksIdle; a background campaign must not block that wait.
func (a *App) idleBlocked(accountID int64, holdsPause bool) bool {
	rt := a.accountSync(accountID)
	if rt == nil || rt.sched == nil {
		return false
	}
	if rt.sched.LiveBusy() {
		return true
	}
	if !holdsPause && a.accountEffectiveN(accountID) >= 2 {
		return false
	}
	return rt.sched.BackgroundBusy()
}

// checkoutIdleSlot checks a Live slot out of the account pool. At N=1 it
// soft-pauses background sync first so the in-flight chunk finishes and the
// single session can be reused, and reports that it holds that pause. The
// returned function gives the slot and the pause back.
func (a *App) checkoutIdleSlot(ctx context.Context, accountID int64) (func(), bool, error) {
	rt := a.accountSync(accountID)
	if rt == nil || rt.pool == nil {
		return nil, false, errors.New("pelton: no sync pool")
	}
	releasePause, holdsPause := a.beginLivePause(accountID)
	if err := rt.pool.Acquire(ctx, pool.Live); err != nil {
		releasePause()
		return nil, false, err
	}
	return func() {
		rt.pool.Release(pool.Live)
		releasePause()
	}, holdsPause, nil
}

// idleWithSlot connects, idles, and resyncs INBOX on that connection. It
// returns errIdleYield when another job needs the slot, after the connection
// is closed. It returns errIdleUnsupported without keeping the connection.
// holdsPause says whether the checkout soft-paused background work.
func (a *App) idleWithSlot(ctx context.Context, account storage.Account, cfg pimap.Config, holdsPause bool) error {
	// Queued background work must not bounce IDLE before it parks. At N=1
	// the campaign is soft-paused and this session is the live slot.
	// idleUntilYield still steps aside once IDLE is up, so a chunk can run.
	if a.liveJobBlocksIdle(account.ID) {
		return errIdleYield
	}
	client, err := a.connectIMAP(cfg)
	if err != nil {
		return err
	}
	releaseTrack := a.trackIMAP(account.ID, client)
	defer releaseTrack()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := client.Login(); err != nil {
		a.noteLoginResult(account.ID, err)
		return err
	}
	a.noteLoginResult(account.ID, nil)
	defer client.Logout()

	if !client.SupportsIdle() {
		return errIdleUnsupported
	}

	// IDLE requires a selected mailbox; the server reports unsolicited activity
	// for whichever mailbox is selected, so we monitor INBOX (where new mail
	// lands). without this SELECT the server rejects IDLE outright.
	inbox, err := a.findInboxFolder(account.ID)
	if err != nil {
		return fmt.Errorf("look up inbox folder: %w", err)
	}
	if _, err := client.Select(inbox.IMAPPath); err != nil {
		return fmt.Errorf("select inbox for idle: %w", err)
	}

	adapter := pimap.NewAdapter(client)
	if syncErr := a.idleEntryResync(ctx, adapter, *inbox); syncErr != nil && ctx.Err() == nil {
		a.log.Error("idle entry resync", "err", syncErr)
	}
	for ctx.Err() == nil {
		if a.liveJobBlocksIdle(account.ID) {
			return errIdleYield
		}
		gotUpdate, err := a.idleUntilYield(ctx, account.ID, client, holdsPause)
		if errors.Is(err, errIdleYield) {
			return err
		}
		if err != nil {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !gotUpdate {
			return nil
		}
		// Resync on this session. A second Live checkout would open another
		// TCP connection and, at N=1, deadlock on the slot this IDLE holds.
		if syncErr := a.syncOneFolderCtx(ctx, adapter, *inbox); syncErr != nil && ctx.Err() == nil {
			a.log.Error("idle resync", "err", syncErr)
		}
	}
	return ctx.Err()
}

// idleUntilYield parks in IDLE until the server reports mail, ctx ends, or
// another job needs this pool slot. A yield cancels only the IDLE wait.
func (a *App) idleUntilYield(ctx context.Context, accountID int64, client mailClient, holdsPause bool) (bool, error) {
	idleCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	yielded := make(chan struct{})
	var once sync.Once
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		ticker := time.NewTicker(idleYieldPoll)
		defer ticker.Stop()
		for {
			select {
			case <-idleCtx.Done():
				return
			case <-ticker.C:
				if a.idleBlocked(accountID, holdsPause) {
					once.Do(func() { close(yielded) })
					cancel()
					return
				}
			}
		}
	}()

	got, err := client.IdleUntil(idleCtx)
	cancel()
	<-watchDone
	select {
	case <-yielded:
		return false, errIdleYield
	default:
	}
	return got, err
}

// newSyncEngine builds a sync engine for one account with the settings every
// caller needs, including where deleted mail goes. Roles are resolved here
// because the sync package does not know about them.
func (a *App) newSyncEngine(adapter psync.Adapter, accountID int64) *psync.Engine {
	engine := psync.NewEngine(adapter, a.store, a.log)
	engine.ColorSync = a.boolSetting(settingFlagColorSync, false)
	engine.InitialLimit = a.syncMessageLimit()
	// Stored ids are announced as they land, so the list fills as mail arrives
	// rather than staying empty until the whole folder is down.
	engine.OnStored = a.announceStored
	// the counts behind the progress bar. The account and server are captured
	// here because the engine does not know them and the line names them.
	email := a.accountEmail(accountID)
	// the trash-folder tests build an engine with no adapter, and a progress line
	// is not worth a panic over.
	server := ""
	if adapter != nil {
		server = adapter.Addr()
	}
	engine.OnProgress = func(p psync.FolderProgress) {
		counts := a.accountTally(accountID).record(p)
		if a.syncProgressPhaseValue(accountID) == SyncPhaseBodies {
			counts.Phase = SyncPhaseBodies
		}
		a.emitSyncProgress(accountID, email, server, counts)
	}
	if trash, ok := a.findTrashFolder(accountID); ok {
		engine.TrashRemoteID = trash.RemoteID
		if engine.TrashRemoteID == "" {
			engine.TrashRemoteID = trash.IMAPPath
		}
		engine.TrashFolderID = trash.ID
	}
	return engine
}

// streamInterval is the shortest gap between two "mail arrived" events during a
// sync. Each one costs the ui a list reload, and a fast server can store a
// batch every few milliseconds; twice a second looks live without the list
// spending the sync redrawing itself.
const streamInterval = 500 * time.Millisecond

// streamGate rate-limits those events. It is a value on App rather than a
// package variable so two accounts syncing at once do not silence each other
// unfairly, and it is safe for the concurrent syncs that produce them.
type streamGate struct {
	mu   sync.Mutex
	last time.Time
}

func (g *streamGate) ready() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := time.Now()
	if now.Sub(g.last) < streamInterval {
		return false
	}
	g.last = now
	return true
}

// closeProgress ends the account's progress line for work that reports
// progress outside a counted run (push resyncs, on-demand body fetches), so the
// ui does not keep showing it as syncing.
func (a *App) closeProgress(accountID int64) {
	a.emitSyncProgress(accountID, a.accountEmail(accountID), "", a.accountTally(accountID).closing())
}

// emitSyncProgress sends one progress event. It is the only place the event is
// built, so the folder line and the message counts can never disagree.
func (a *App) emitSyncProgress(accountID int64, email, server string, c syncCounts) {
	if c.Phase == "" {
		c.Phase = a.syncProgressPhaseValue(accountID)
	}
	ev := SyncProgressEvent{
		AccountID: accountID, AccountEmail: email, Server: server,
		Folder: c.Folder, Done: c.Done, Total: c.Total,
		FolderDone: c.FolderDone, FolderTotal: c.FolderTotal,
		FoldersDone: c.FoldersDone, FoldersTotal: c.FoldersTotal,
		Phase: c.Phase,
	}
	// recording and sending under one lock keeps a heartbeat from slipping a
	// stale running event in after a closing one.
	st := a.accountStateFor(accountID)
	st.progressMu.Lock()
	defer st.progressMu.Unlock()
	// The verify line is the ui's separate line for the background check; the
	// heartbeat keeps only the running sync's line alive.
	switch {
	case ev.Phase == SyncPhaseVerify:
	case ev.Folder == "":
		st.lastProgress = nil
	default:
		st.lastProgress = &ev
	}
	a.emit(EventSyncProgress, ev)
}

// the coarse kinds of sync failure. The ui has a sentence for each; the raw
// error travels alongside as detail for whoever wants the server's own words.
const (
	syncFailAuth        = "auth"
	syncFailNetwork     = "network"
	syncFailCredentials = "credentials"
	syncFailCertificate = "certificate"
	syncFailOther       = "other"
)

// noteSyncOutcome records how an account's sync went. This is what makes a
// failure outlive the run it happened in: before it, one broken account among
// several left nothing behind but a log line, and logging is off by default
// (#322).
func (a *App) noteSyncOutcome(accountID int64, err error) {
	// A cancel is how stop, protocol switch and shutdown end a sync on
	// purpose. It says nothing about the account's health, so it is neither a
	// failure to show nor a throttle signal. Real network errors are not
	// Canceled and are still recorded.
	if errors.Is(err, context.Canceled) || errors.Is(err, errAccountSyncHeld) {
		return
	}
	a.noteSyncPoolOutcome(accountID, err)
	if err == nil {
		if e := a.store.RecordSyncOK(a.ctx, accountID); e != nil {
			a.log.Error("record sync ok", "account", accountID, "err", e)
		}
		return
	}
	if e := a.store.RecordSyncFailure(a.ctx, accountID, syncFailureReason(err), err.Error()); e != nil {
		a.log.Error("record sync failure", "account", accountID, "err", e)
	}
}

// isSyncThrottleError reports connection and rate-limit failures that should
// shrink the sync pool's effective parallelism.
func isSyncThrottleError(err error) bool {
	if err == nil {
		return false
	}
	if isNetworkError(err) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "rate limit") ||
		strings.Contains(msg, "too many requests") ||
		strings.Contains(msg, "429")
}

// noteSyncPoolOutcome adjusts adaptive throttle on the account pool. It does
// not rewrite the saved sync_max_parallel preference.
func (a *App) noteSyncPoolOutcome(accountID int64, err error) {
	rt := a.accountSync(accountID)
	if rt == nil || rt.pool == nil {
		return
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if isSyncThrottleError(err) {
		rt.throttleSuccesses = 0
		rt.throttleFails++
		if rt.throttleFails >= syncThrottleFailThreshold {
			eff := rt.pool.Effective()
			next := max(eff/2, 1)
			rt.pool.SetEffective(next)
			if next != eff {
				a.log.Debug("sync pool throttled", "account", accountID, "from", eff, "to", next)
			}
			rt.throttleFails = 0
		}
		return
	}
	if err != nil {
		return
	}
	rt.throttleFails = 0
	rt.throttleSuccesses++
	if rt.throttleSuccesses < syncThrottleRecoverThreshold {
		return
	}
	eff := rt.pool.Effective()
	cfg := rt.pool.Configured()
	next := min(eff*2, cfg)
	if next > eff {
		rt.pool.SetEffective(next)
		a.log.Debug("sync pool throttle eased", "account", accountID, "from", eff, "to", next)
	}
	rt.throttleSuccesses = 0
}

// syncFailureReason classifies a sync error into the kinds the ui can explain.
func syncFailureReason(err error) string {
	switch {
	case errors.Is(err, errNoCredentials):
		return syncFailCredentials
	case loginRefused(err):
		return syncFailAuth
	case isUntrustedCert(err):
		return syncFailCertificate
	case isNetworkError(err):
		return syncFailNetwork
	default:
		return syncFailOther
	}
}

// accountEmail is the address to name in a progress line, empty when the
// account cannot be read. A progress line is not worth failing a sync over.
func (a *App) accountEmail(accountID int64) string {
	account, err := a.store.GetAccount(a.ctx, accountID)
	if err != nil {
		return ""
	}
	return account.Email
}

// announceStored tells the ui about mail stored so far in a folder that is
// still syncing. The list reloads on it, which is cheap now that the read is
// indexed, so a first sync looks like mail arriving instead of a frozen window.
// Notifications are not sent from here: those still go out once per folder, so
// a first sync of fourteen thousand messages does not become fourteen thousand
// notifications.
func (a *App) announceStored(folder storage.Folder, ids []int64) {
	if !a.streamTick.ready() {
		return
	}
	a.emit(EventMailNew, MailNewEvent{
		AccountID: folder.AccountID, FolderID: folder.ID, Count: len(ids),
	})
}

// findTrashFolder returns the account's trash-role folder. Without one, a
// delete has nowhere to go and falls back to a permanent expunge, so the caller
// has to know whether there is one.
func (a *App) findTrashFolder(accountID int64) (storage.Folder, bool) {
	folders, err := a.store.ListFolders(a.ctx, accountID)
	if err != nil {
		a.log.Error("find trash folder", "account", accountID, "err", err)
		return storage.Folder{}, false
	}
	for _, f := range folders {
		if folderRole(f) == roleTrash {
			return f, true
		}
	}
	return storage.Folder{}, false
}

// afterRepairs deals with messages whose text was fetched again because what
// was cached could not be decoded. The list reloads so the reader sees the
// fixed subject without reopening the folder, and the search index is
// rewritten for those messages, which the incremental pass would never revisit.
func (a *App) afterRepairs(folder storage.Folder, ids []int64) {
	if len(ids) == 0 {
		return
	}
	a.log.Info("repaired cached mail", "folder", folder.Name, "count", len(ids))
	a.emit(EventMailRepaired, MailRepairedEvent{
		AccountID: folder.AccountID, FolderID: folder.ID, Count: len(ids),
	})
	goSafe("reindexing repaired mail", func() { a.reindexMessages(ids) })
}

// bindEnginePauseCheck is the engine's batch-boundary hook.
//
// effectiveN is read on every call, not captured at bind time: the adaptive
// throttle can lower the account's pool to 1 mid-run. Send is off-pool and
// pauses nothing, so only live work drives this. At effective N=1 the hook
// stays true while the scheduler is soft-paused, so a live job can take the
// only session after the in-flight chunk. At N>=2 a live soft-pause flag does
// not stop the in-flight chunk; live work uses the reserved slot.
func bindEnginePauseCheck(effectiveN func() int, s *syncsched.Scheduler) func() bool {
	return func() bool {
		if effectiveN != nil && effectiveN() >= 2 {
			return false
		}
		return s != nil && s.SoftPauseRequested()
	}
}

// foldersInSyncOrder keeps selectable, included folders and orders them Inbox,
// then Sent, then everything else (stable list order within each group).
func foldersInSyncOrder(folders []storage.Folder) []storage.Folder {
	var inbox, sent, rest []storage.Folder
	for _, f := range folders {
		if f.SyncExcluded || !folderSelectable(f) {
			continue
		}
		switch folderRole(f) {
		case roleInbox:
			inbox = append(inbox, f)
		case roleSent:
			sent = append(sent, f)
		default:
			rest = append(rest, f)
		}
	}
	out := append(inbox, sent...)
	return append(out, rest...)
}

// enqueueIMAPInitialSync runs Inbox stubs then Inbox bodies first, calls
// onInboxDone, and then starts every other folder's stubs-then-bodies chain at
// once, so they share the background slots (Sent is queued first). A
// soft-pause requeues the job and does not advance. A ListStubs or
// FetchBodies failure waits out syncRetryBackoff and tries again on the same
// folder; after syncStepAttempts that folder is marked failed and its chain
// ends. done receives nil when every chain finishes, or folderSyncError when
// any folder's retries were exhausted. Cancellation stops everything.
func enqueueIMAPInitialSync(s *syncsched.Scheduler, folders []storage.Folder, run func(ctx context.Context, folder storage.Folder, kind syncsched.JobKind) ([]string, error), onInboxDone func(), done chan<- error) {
	ordered := foldersInSyncOrder(folders)
	var reportOnce sync.Once
	report := func(err error) {
		if done == nil {
			return
		}
		reportOnce.Do(func() { done <- err })
	}
	inboxDone := sync.OnceFunc(func() {
		if onInboxDone != nil {
			onInboxDone()
		}
	})
	stop := func(ctx context.Context, err error) error {
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		report(err)
		return err
	}

	var mu sync.Mutex
	var failed []string
	var firstErr error
	remaining := len(ordered)
	chainDone := func(name string, err error) {
		mu.Lock()
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			failed = append(failed, name)
		}
		remaining--
		last := remaining == 0
		result := folderSyncError(failed, len(ordered), firstErr)
		mu.Unlock()
		if last {
			report(result)
		}
	}

	startChain := func(f storage.Folder, then func()) {
		finish := func(err error) {
			chainDone(f.Name, err)
			if then != nil {
				then()
			}
		}
		retryOrFail := func(ctx context.Context, attempt int, err error, again func(int)) error {
			if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return stop(ctx, err)
			}
			if errors.Is(err, psync.ErrSoftPaused) {
				return err
			}
			if attempt+1 < syncStepAttempts {
				scheduleSyncRetry(ctx, attempt, func() { again(attempt + 1) })
				return nil
			}
			finish(err)
			return nil
		}
		var enqueueBodies func(ids []string, attempt int)
		enqueueBodies = func(ids []string, attempt int) {
			s.Enqueue(syncsched.Job{
				Priority:  syncsched.PriorityBackgroundBody,
				Kind:      syncsched.JobFetchBodies,
				FolderID:  f.ID,
				RemoteIDs: append([]string(nil), ids...),
				Run: func(ctx context.Context) error {
					if err := ctx.Err(); err != nil {
						return stop(ctx, err)
					}
					// A body attempt may return the ids that still need a body.
					// Nil keeps this attempt's list. Soft-pause does not retry
					// here: the scheduler requeues the ids that were not started.
					pending, err := run(ctx, f, syncsched.JobFetchBodies)
					if err != nil {
						nextIDs := ids
						if pending != nil && !errors.Is(err, psync.ErrSoftPaused) {
							nextIDs = pending
						}
						return retryOrFail(ctx, attempt, err, func(next int) { enqueueBodies(nextIDs, next) })
					}
					finish(nil)
					return nil
				},
			})
		}
		var enqueueStubs func(attempt int)
		enqueueStubs = func(attempt int) {
			s.Enqueue(syncsched.Job{
				Priority: syncsched.PriorityBackgroundStubs,
				Kind:     syncsched.JobListStubs,
				FolderID: f.ID,
				Run: func(ctx context.Context) error {
					if err := ctx.Err(); err != nil {
						return stop(ctx, err)
					}
					ids, err := run(ctx, f, syncsched.JobListStubs)
					if err != nil {
						return retryOrFail(ctx, attempt, err, enqueueStubs)
					}
					enqueueBodies(ids, 0)
					return nil
				},
			})
		}
		enqueueStubs(0)
	}

	if len(ordered) == 0 {
		inboxDone()
		report(nil)
		return
	}
	startRest := func(from int) {
		inboxDone()
		for _, f := range ordered[from:] {
			startChain(f, nil)
		}
	}
	if folderRole(ordered[0]) == roleInbox {
		startChain(ordered[0], func() { startRest(1) })
		return
	}
	startRest(0)
}

// scheduleSyncRetry waits, then enqueues the next try. The wait is outside
// the job so the pool slot is not held during backoff. A cancelled context
// drops the retry; the caller is already selecting on that context.
func scheduleSyncRetry(ctx context.Context, attempt int, enqueue func()) {
	wait := syncRetryBackoff(attempt)
	if wait <= 0 {
		enqueue()
		return
	}
	go func() {
		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case <-ctx.Done():
		case <-timer.C:
			enqueue()
		}
	}()
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

// withSyncSlot checks one sync-pool slot out around fn. Send and the JMAP
// WebSocket do not use it. A job the scheduler already started must not call
// it again: that job is holding the slot.
func (a *App) withSyncSlot(ctx context.Context, accountID int64, kind pool.Kind, fn func() error) error {
	if _, err := a.ensureAccountScheduler(ctx, accountID); err != nil {
		return err
	}
	rt := a.accountSync(accountID)
	if rt == nil || rt.pool == nil {
		return errors.New("pelton: no sync pool")
	}
	if err := rt.pool.Acquire(ctx, kind); err != nil {
		return err
	}
	defer rt.pool.Release(kind)
	return fn()
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

// announceNewBodies tells the ui about bodies a background fetch stored and
// starts the follow-up work (notification, search index, unread counts).
func (a *App) announceNewBodies(folder storage.Folder, res psync.FolderSyncResult) {
	a.announceStoredBodies(folder, res)
	if res.New > 0 {
		goSafe("announcing new mail", func() { a.notifyNewMail(folder, res.NewIDs) })
	}
}

// bodyAnnouncer returns how a body job announces what it stored. Bodies a
// full folder check found are old mail healed into the cache, not news, so
// they raise no notification. That check shows its own calm line, so their
// engine reports no folder progress either: it would open the running-sync
// line, which only a sync's own close ends.
func (a *App) bodyAnnouncer(engine *psync.Engine, fromReconcile bool) func(storage.Folder, psync.FolderSyncResult) {
	if !fromReconcile {
		return a.announceNewBodies
	}
	engine.OnProgress = nil
	return a.announceStoredBodies
}

// announceStoredBodies is announceNewBodies without the new-mail
// notification, for bodies that are not news: the full folder check heals
// old mail that was missing from the cache.
func (a *App) announceStoredBodies(folder storage.Folder, res psync.FolderSyncResult) {
	if res.New == 0 {
		return
	}
	a.emit(EventMailNew, MailNewEvent{AccountID: folder.AccountID, FolderID: folder.ID, Count: res.New})
	goSafe("indexing new mail", func() { _ = a.indexNewMessages() })
	goSafe("counting unread mail", a.refreshViewCounts)
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

// ensureAccountScheduler returns the account's scheduler, starting one if it
// has none. See ensureAccountSync.
func (a *App) ensureAccountScheduler(ctx context.Context, accountID int64) (*syncsched.Scheduler, error) {
	rt, err := a.ensureAccountSync(ctx, accountID)
	if err != nil {
		return nil, err
	}
	return rt.sched, nil
}

// ensureAccountSync returns the account's scheduler runtime, starting one if
// it has none or the one it has already ended. A new scheduler runs under the
// profile session, not ctx: callers pass the context of one sync run, and a
// scheduler bound to it would die with that run and leave the next run waiting
// on a queue nobody serves. Hard stops come from stopAccountScheduler (worker
// exit, protocol switch, removal) and from the session ending. An app with no
// ctx (bare test fixtures) falls back to ctx.
//
// While the account is held (protocol switch) or after it was removed it
// returns errAccountSyncHeld and starts nothing, so no sync job of the account
// can begin. The one exception is a call under the account's live worker
// context: the switch starts the new worker before it lets go of the hold,
// and every sync call of that worker has to get through, not only its first.
// A cancelled worker's calls (the old worker exiting late) are refused, and a
// removed account is refused for everyone.
func (a *App) ensureAccountSync(ctx context.Context, accountID int64) (*accountSync, error) {
	owner := workerOwns(ctx, accountID)
	for {
		a.syncsMu.Lock()
		// Checked under syncsMu: holdAccountSync runs before the switch stops
		// the scheduler, which takes syncsMu, so a runtime started just before
		// the hold is the one that stop removes.
		held, removed := a.syncBlock(accountID)
		if removed || (held && !owner) {
			a.syncsMu.Unlock()
			return nil, errAccountSyncHeld
		}
		if owner && ctx.Err() != nil {
			a.syncsMu.Unlock()
			return nil, ctx.Err()
		}
		if a.syncs == nil {
			a.syncs = make(map[int64]*accountSync)
		}
		rt := a.syncs[accountID]
		if rt == nil {
			base := ctx
			if a.ctx != nil {
				base = a.sessionCtx()
			}
			n := a.accountSyncMaxParallel(accountID)
			p := pool.New(n)
			sched := syncsched.New(accountID)
			sched.Start(base, p, 0)
			rt = &accountSync{sched: sched, pool: p, ended: make(chan struct{}), ctx: base, userN: n}
			a.syncs[accountID] = rt
			a.syncsMu.Unlock()
			return rt, nil
		}
		if rt.ctx == nil || rt.ctx.Err() == nil {
			if rt.pool != nil {
				rt.pool.SetConfigured(a.accountSyncMaxParallel(accountID))
			}
			a.syncsMu.Unlock()
			return rt, nil
		}
		// The context it ran under is gone, so its loop has exited. Drop it
		// and start a fresh one. Stop runs unlocked: it joins jobs that may
		// look the runtime up.
		delete(a.syncs, accountID)
		a.syncsMu.Unlock()
		a.stopSyncRuntime(accountID, rt)
	}
}

func (a *App) accountSync(accountID int64) *accountSync {
	a.syncsMu.Lock()
	defer a.syncsMu.Unlock()
	if a.syncs == nil {
		return nil
	}
	return a.syncs[accountID]
}

// stopAccountScheduler hard-stops and forgets whatever scheduler the account
// has now. Only paths that own the account's whole runtime call it: protocol
// switch, stopAccountWorker, tests.
func (a *App) stopAccountScheduler(accountID int64) {
	a.syncsMu.Lock()
	rt := a.syncs[accountID]
	if rt != nil {
		delete(a.syncs, accountID)
	}
	a.syncsMu.Unlock()
	if rt != nil {
		a.stopSyncRuntime(accountID, rt)
	}
}

// stopAccountSchedulerIf stops rt and removes it from the map only if the map
// still holds that same runtime. A worker that exits after its replacement
// started a new scheduler leaves the new one alone. rt is still stopped even
// when it is no longer mapped, since nothing else will stop it. A nil rt is a
// no-op.
func (a *App) stopAccountSchedulerIf(accountID int64, rt *accountSync) {
	if rt == nil {
		return
	}
	a.syncsMu.Lock()
	if a.syncs[accountID] == rt {
		delete(a.syncs, accountID)
	}
	a.syncsMu.Unlock()
	a.stopSyncRuntime(accountID, rt)
}

// stopSyncRuntime stops rt's scheduler and closes the "checking folders" line
// if full reconciles were still queued: they will never run, so no job is left
// to close it.
func (a *App) stopSyncRuntime(accountID int64, rt *accountSync) {
	rt.stop()
	if rt.dropReconciles() {
		a.emitSyncProgress(accountID, a.accountEmail(accountID), "", syncCounts{Phase: SyncPhaseVerify})
	}
}

// accountEffectiveN is the account's pool size right now. The adaptive
// throttle can lower it below the configured size mid-run, so pause and
// live-slot decisions read it at call time and never the global setting.
func (a *App) accountEffectiveN(accountID int64) int {
	if rt := a.accountSync(accountID); rt != nil && rt.pool != nil {
		return rt.pool.Effective()
	}
	return a.accountSyncMaxParallel(accountID)
}

// beginLivePause soft-pauses background work when the account's effective N is 1 so the in-flight chunk
// can finish and the live job can take the only session. N>=2 leaves the
// background chunk alone; the scheduler checks the reserved live slot out.
// It reports whether it took a hold; the returned func drops it either way.
func (a *App) beginLivePause(accountID int64) (func(), bool) {
	rt := a.accountSync(accountID)
	if rt == nil || rt.sched == nil {
		return func() {}, false
	}
	if rt.pool != nil && rt.pool.Effective() >= 2 {
		return func() {}, false
	}
	rt.mu.Lock()
	rt.liveHolds++
	rt.mu.Unlock()
	rt.sched.RequestSoftPause()
	return func() {
		rt.mu.Lock()
		if rt.liveHolds > 0 {
			rt.liveHolds--
		}
		holds := rt.liveHolds
		rt.mu.Unlock()
		if holds == 0 {
			rt.sched.ClearSoftPause()
		}
	}, true
}

func (a *App) applySchedulerPause(engine *psync.Engine, accountID int64) {
	if engine == nil {
		return
	}
	rt := a.accountSync(accountID)
	if rt == nil || rt.sched == nil {
		return
	}
	// Batch boundaries are the pause points. Do not block inside a chunk.
	// Callers that are themselves the live job get no pause check, so they do
	// not soft-pause on the hold they requested. Background jobs use
	// applyBackgroundPause and, at effective N=1, stop after the current chunk
	// while a live hold is set.
	engine.PauseCheck = nil
}

func (a *App) applyBackgroundPause(engine *psync.Engine, accountID int64) {
	if engine == nil {
		return
	}
	rt := a.accountSync(accountID)
	if rt == nil || rt.sched == nil {
		return
	}
	engine.PauseCheck = a.enginePause(rt)
}

func (a *App) enginePause(rt *accountSync) func() bool {
	return bindEnginePauseCheck(func() int {
		if rt.pool != nil {
			return rt.pool.Effective()
		}
		return 1
	}, rt.sched)
}

func (a *App) enqueueLiveAndWait(ctx context.Context, accountID int64, kind syncsched.JobKind, folderID int64, remoteIDs []string, run func(context.Context) error) error {
	sched, err := a.ensureAccountScheduler(ctx, accountID)
	if err != nil {
		return err
	}
	release, _ := a.beginLivePause(accountID)
	defer release()
	done := make(chan error, 1)
	ended := a.schedulerEnded(accountID)
	sched.Enqueue(syncsched.Job{
		Priority:  syncsched.PriorityLive,
		Kind:      kind,
		FolderID:  folderID,
		RemoteIDs: append([]string(nil), remoteIDs...),
		Run: func(jobCtx context.Context) error {
			err := run(jobCtx)
			// The scheduler requeues a soft-paused job. Wait for the run that
			// actually finishes.
			if errors.Is(err, psync.ErrSoftPaused) {
				return err
			}
			select {
			case done <- err:
			default:
			}
			return err
		},
	})
	select {
	case err := <-done:
		return err
	case <-ended:
		// A switch or removal that stopped the scheduler under this job is
		// not a failure of the job.
		if held, _ := a.syncBlock(accountID); held {
			return errAccountSyncHeld
		}
		return context.Canceled
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (a *App) schedulerEnded(accountID int64) <-chan struct{} {
	rt := a.accountSync(accountID)
	if rt == nil || rt.ended == nil {
		ch := make(chan struct{})
		close(ch)
		return ch
	}
	return rt.ended
}

// fetchMessageBodyOnDemand runs a P0 live job that fetches one stub's body.
// The list row stays on failure; GetMessage surfaces the error. While the
// account switches protocol it fetches nothing and returns nil, so the stub
// shows.
func (a *App) fetchMessageBodyOnDemand(m *storage.Message) error {
	if m == nil || (m.BodyComplete && !a.needsRefetch(m.ID)) {
		return nil
	}
	account, err := a.store.GetAccount(a.ctx, m.AccountID)
	if err != nil {
		return err
	}
	if account.Local {
		return nil
	}
	folder, err := a.store.GetFolder(a.ctx, m.FolderID)
	if err != nil {
		return err
	}
	remoteIDs := []string{m.RemoteID}
	err = a.enqueueLiveAndWait(a.ctx, account.ID, syncsched.JobOnDemandBody, folder.ID, remoteIDs, func(jobCtx context.Context) error {
		return a.runOnDemandBodyFetch(jobCtx, *account, *folder)
	})
	if errors.Is(err, errAccountSyncHeld) {
		// Switching protocol: the stub stays on screen, and the switch is about
		// to replace it anyway.
		return nil
	}
	return err
}

func (a *App) runOnDemandBodyFetch(ctx context.Context, account storage.Account, folder storage.Folder) error {
	if a.onDemandFetchForTest != nil {
		return a.onDemandFetchForTest(ctx, account, folder, syncsched.RemoteIDs(ctx))
	}
	if account.Protocol == "jmap" {
		return a.execJMAPOnDemandBodies(ctx, account, folder)
	}
	_, err := a.execIMAPOnDemandBodies(ctx, account, folder)
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

func (a *App) execIMAPOnDemandBodies(ctx context.Context, account storage.Account, folder storage.Folder) ([]string, error) {
	var ids []string
	err := a.withIMAPSession(ctx, account, func(client mailClient) error {
		engine := a.newSyncEngine(pimap.NewAdapter(client), account.ID)
		a.applySchedulerPause(engine, account.ID)
		defer a.closeProgress(account.ID)
		remote := syncsched.RemoteIDs(ctx)
		res, err := engine.FetchBodies(ctx, folder, remote)
		if err == nil {
			a.announceNewBodies(folder, res)
		}
		if err == nil {
			var repaired []int64
			repaired, err = engine.RepairRemoteIDs(ctx, folder, remote)
			a.afterRepairs(folder, repaired)
		}
		if err != nil && ctx.Err() == nil && !errors.Is(err, psync.ErrSoftPaused) && a.store != nil {
			if still, nerr := a.store.RemoteIDsNeedingBody(ctx, folder.ID, remote); nerr == nil {
				ids = still
			}
		}
		if ctx.Err() == nil && !errors.Is(err, psync.ErrSoftPaused) {
			if ferr := a.finishIMAPFolder(ctx, engine, folder); err == nil {
				err = ferr
			}
		}
		return err
	})
	return ids, err
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

// syncIMAPInitial records the initial sync pass the way syncAccountCtx records
// every other account sync. onInboxDone runs once Inbox is in, so IDLE can
// start before the other folders.
func (a *App) syncIMAPInitial(ctx context.Context, account storage.Account, onInboxDone func()) error {
	if account.Local {
		return nil
	}
	err := a.syncIMAPInitialPass(ctx, account, onInboxDone)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	a.noteSyncOutcome(account.ID, err)
	return err
}

// syncIMAPInitialPass discovers folders, then enqueues Inbox stubs and Inbox
// bodies, and only afterwards the other selected folders, all at once.
// onInboxDone runs once Inbox is in, so IDLE can start before the other folders.
// X is the engine's InitialLimit (sync_message_limit).
func (a *App) syncIMAPInitialPass(ctx context.Context, account storage.Account, onInboxDone func()) error {
	if err := a.withIMAPSession(ctx, account, func(client mailClient) error {
		return a.ensureFolders(client, account.ID)
	}); err != nil {
		return err
	}
	all, err := a.store.ListFolders(ctx, account.ID)
	if err != nil {
		return err
	}
	ordered := foldersInSyncOrder(all)
	a.emit(EventSyncState, SyncStateEvent{Running: true})
	defer a.emit(EventSyncState, SyncStateEvent{Running: false})
	tally := a.accountTally(account.ID)
	tally.begin(len(ordered))
	closed := false
	defer func() {
		if !closed {
			a.emitSyncProgress(account.ID, account.Email, "", tally.closing())
		}
	}()
	// registered after the closing defer so it stops first: the heartbeat must
	// be gone before the close is sent.
	stopBeat := a.startProgressHeartbeat(ctx, account.ID)
	defer stopBeat()

	sched, err := a.ensureAccountScheduler(ctx, account.ID)
	if err != nil {
		return err
	}
	done := make(chan error, 1)
	enqueueIMAPInitialSync(sched, ordered, func(jobCtx context.Context, folder storage.Folder, kind syncsched.JobKind) ([]string, error) {
		if kind == syncsched.JobListStubs {
			for i := range ordered {
				if ordered[i].ID == folder.ID {
					tally.enterFolder(i, folder.Name)
					a.emitSyncProgress(account.ID, account.Email, "", tally.counts())
					break
				}
			}
		}
		return a.execIMAPStep(jobCtx, account, folder, kind, 0, nil)
	}, onInboxDone, done)

	var runErr error
	select {
	case runErr = <-done:
	case <-a.schedulerEnded(account.ID):
		return context.Canceled
	case <-ctx.Done():
		return ctx.Err()
	}
	final := tally.counts()
	final.Folder = ""
	final.FoldersDone = len(ordered)
	closed = true
	stopBeat()
	a.emitSyncProgress(account.ID, account.Email, "", final)
	return runErr
}

// runIMAPManualSync is the P0 refresh behind TriggerSync and SyncAccountNow.
// It does not hold the account lock and does not restart the initial sync.
func (a *App) runIMAPManualSync(ctx context.Context, account storage.Account) error {
	a.emit(EventSyncState, SyncStateEvent{Running: true})
	defer a.emit(EventSyncState, SyncStateEvent{Running: false})
	return a.enqueueLiveAndWait(ctx, account.ID, syncsched.JobManualSync, 0, nil, func(jobCtx context.Context) error {
		if a.protocolSwitchedSince(jobCtx, account) {
			return nil
		}
		return a.withIMAPSession(jobCtx, account, func(client mailClient) error {
			if err := a.ensureFolders(client, account.ID); err != nil {
				return err
			}
			return a.syncFolders(jobCtx, pimap.NewAdapter(client), account.ID)
		})
	})
}

// protocolSwitchedSince reports whether account was loaded before a protocol
// switch that has finished since. TriggerSync lists the accounts once and
// syncs them in turn, and scroll backfill and a limit raise load the account
// and its folders before they queue, so a job can reach the scheduler with the
// old protocol; it must not sync with it. Jobs are refused while the account
// is held, except those under the worker the switch starts itself with
// startHeldAccountWorker. That worker starts once SwitchAccountProtocol has
// returned and loads the account then, so in both cases the stored protocol is
// settled when this reads it.
func (a *App) protocolSwitchedSince(ctx context.Context, account storage.Account) bool {
	if a.store == nil {
		return false
	}
	cur, err := a.store.GetAccount(ctx, account.ID)
	return err == nil && cur.Protocol != account.Protocol
}

// imapStepStats collects body and floor results from one initial sync step.
type imapStepStats struct {
	newCount int
	hasOlder bool
}

func (a *App) execIMAPStep(ctx context.Context, account storage.Account, folder storage.Folder, kind syncsched.JobKind, backfill int, stats *imapStepStats) ([]string, error) {
	if kind == syncsched.JobFetchBodies {
		return a.execIMAPBodies(ctx, account, folder, stats, false)
	}
	if kind != syncsched.JobListStubs {
		return nil, nil
	}
	var ids []string
	list := func() error {
		err := a.withIMAPSession(ctx, account, func(client mailClient) error {
			engine := a.newSyncEngine(pimap.NewAdapter(client), account.ID)
			a.applyBackgroundPause(engine, account.ID)
			res, err := engine.SyncFolderStubs(ctx, folder, backfill)
			ids = mergeNewestIDs(res.ToFetch, ids)
			if stats != nil {
				stats.hasOlder = stats.hasOlder || res.HasOlder
			}
			// Bodies will not run for this attempt. Repair and color still
			// happen, matching SyncFolder after a list failure.
			if err != nil && ctx.Err() == nil && !errors.Is(err, psync.ErrSoftPaused) {
				if ferr := a.finishIMAPFolder(ctx, engine, folder); err == nil {
					err = ferr
				}
			}
			return err
		})
		if !errors.Is(err, psync.ErrSoftPaused) {
			a.noteSyncPoolOutcome(account.ID, err)
		}
		return err
	}
	// A backfill widens the window, which a coalesced follow-up would not do.
	if backfill > 0 {
		err := list()
		return ids, err
	}
	err := a.listFolderOnce(ctx, account.ID, folder.ID, list)
	return ids, err
}

// mergeNewestIDs puts the ids of a follow-up list, which are newer, ahead of
// the ones an earlier list of the same folder returned, without repeats.
func mergeNewestIDs(newer, older []string) []string {
	out := append([]string(nil), newer...)
	seen := make(map[string]struct{}, len(newer))
	for _, id := range newer {
		seen[id] = struct{}{}
	}
	for _, id := range older {
		if _, dup := seen[id]; !dup {
			out = append(out, id)
		}
	}
	return out
}

// execIMAPBodies is a background body job: it fetches the job attempt's ids
// and soft-pauses between chunks. On a failure it returns the ids that still
// need a body. fromReconcile marks bodies a full folder check found.
func (a *App) execIMAPBodies(ctx context.Context, account storage.Account, folder storage.Folder, stats *imapStepStats, fromReconcile bool) ([]string, error) {
	var ids []string
	err := a.withIMAPSession(ctx, account, func(client mailClient) error {
		engine := a.newSyncEngine(pimap.NewAdapter(client), account.ID)
		a.applyBackgroundPause(engine, account.ID)
		announce := a.bodyAnnouncer(engine, fromReconcile)
		remote := syncsched.RemoteIDs(ctx)
		res, err := engine.FetchBodies(ctx, folder, remote)
		if stats != nil && err == nil {
			stats.newCount += res.New
		}
		if err == nil {
			announce(folder, res)
		}
		// A failed attempt retries only ids that are still incomplete.
		// Soft-pause keeps the not-started list on the scheduler job.
		if err != nil && ctx.Err() == nil && !errors.Is(err, psync.ErrSoftPaused) && a.store != nil {
			if still, nerr := a.store.RemoteIDsNeedingBody(ctx, folder.ID, remote); nerr == nil {
				ids = still
			}
		}
		// Soft-pause still has bodies left, so repair waits for the
		// resumed job. Every other outcome finishes the folder.
		if ctx.Err() == nil && !errors.Is(err, psync.ErrSoftPaused) {
			if ferr := a.finishIMAPFolder(ctx, engine, folder); err == nil {
				err = ferr
			}
		}
		return err
	})
	if !errors.Is(err, psync.ErrSoftPaused) {
		a.noteSyncPoolOutcome(account.ID, err)
	}
	return ids, err
}

// finishIMAPFolder repairs mangled cached text and adopts server flag colors,
// then tells the UI about repairs. It is the SyncFolder tail for the initial sync
// and scroll backfill.
func (a *App) finishIMAPFolder(ctx context.Context, engine *psync.Engine, folder storage.Folder) error {
	if engine == nil {
		return nil
	}
	res, err := engine.CompleteFolder(ctx, folder)
	a.afterRepairs(folder, res.RepairedIDs)
	return err
}

// withIMAPSession connects, logs in, and closes. The session is checked out
// for this job only (connect-on-acquire) and closed if a protocol switch
// aborts that account's in-flight work. It does not take the account lock.
func (a *App) withIMAPSession(ctx context.Context, account storage.Account, fn func(mailClient) error) error {
	cfg, err := a.resolveIMAP(account)
	if err != nil {
		return err
	}
	started := time.Now()
	client, err := a.connectIMAP(cfg)
	if err != nil {
		return err
	}
	release := a.trackIMAP(account.ID, client)
	defer release()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := client.Login(); err != nil {
		a.noteLoginResult(account.ID, err)
		return err
	}
	a.noteLoginResult(account.ID, nil)
	a.log.Debug("imap session opened", "account", account.ID, "connect_ms", time.Since(started).Milliseconds())
	defer client.Logout()
	return fn(client)
}
