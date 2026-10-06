package desktop

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/peltonapp/Pelton/internal/credentials"
	pjmap "github.com/peltonapp/Pelton/internal/jmap"
	"github.com/peltonapp/Pelton/internal/proxy"
	"github.com/peltonapp/Pelton/internal/storage"
)

// lockedBuffer is a log sink the switch and the workers it starts may write
// to from different goroutines.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// closeFlagIMAP records whether abortAccountSync closed it.
type closeFlagIMAP struct {
	fakeIMAP
	closed *atomic.Bool
}

func (c *closeFlagIMAP) Close() error {
	c.closed.Store(true)
	return nil
}

// setupSwitchTimeout prepares a jmap account for a switch to imap with a short
// stall budget and a Warn-level log captured in the returned buffer.
func setupSwitchTimeout(t *testing.T) (*App, int64, int64, *lockedBuffer) {
	t.Helper()
	orig := switchProtocolStallTimeout
	switchProtocolStallTimeout = 100 * time.Millisecond
	t.Cleanup(func() { switchProtocolStallTimeout = orig })

	a := newJMAPSwitchTestApp(t)
	logs := &lockedBuffer{}
	a.log = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelWarn}))
	id, folderID := seedSwitchAccount(t, a, "slow@example.test", "jmap")
	a.jmapClientForTest = func(context.Context, storage.Account) (*pjmap.Client, error) {
		return nil, errors.New("no jmap server in this test")
	}
	a.authenticateTarget = func(context.Context, storage.Account, proxy.Config, string, credentials.Secret) (targetAuth, error) {
		return targetAuth{}, nil
	}
	t.Cleanup(func() { a.store.TestingSetDeleteTxHook(nil) })
	return a, id, folderID, logs
}

// switchWithin runs SwitchProtocol and fails the test if it has not returned
// after limit, which is what an unbounded clear-cache step does.
func switchWithin(t *testing.T, a *App, id int64, protocol string, limit time.Duration) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- a.SwitchProtocol(id, protocol) }()
	select {
	case err := <-done:
		return err
	case <-time.After(limit):
		t.Fatalf("SwitchProtocol still running after %v", limit)
		return nil
	}
}

// A clear-cache step that stalls once is retried with a fresh budget after the
// account's sessions are closed again, and the switch then completes.
func TestSwitchProtocolRetriesClearCacheAfterStall(t *testing.T) {
	a, id, folderID, logs := setupSwitchTimeout(t)

	var calls atomic.Int32
	var closed atomic.Bool
	a.store.TestingSetDeleteTxHook(func(ctx context.Context) error {
		if calls.Add(1) > 1 {
			return nil
		}
		// a session opened while the first attempt hangs; the retry's abort
		// has to close it.
		a.trackIMAP(id, &closeFlagIMAP{closed: &closed})
		<-ctx.Done()
		return ctx.Err()
	})

	if err := switchWithin(t, a, id, "imap", 5*time.Second); err != nil {
		t.Fatalf("SwitchProtocol: %v", err)
	}
	if !closed.Load() {
		t.Error("the retry did not abort the account's sessions first")
	}
	acc, err := a.store.GetAccount(a.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if acc.Protocol != "imap" {
		t.Fatalf("protocol = %q, want imap", acc.Protocol)
	}
	if n := accountMessageCount(t, a, id, folderID); n != 0 {
		t.Errorf("%d old-protocol messages left after the retried switch", n)
	}
	if workerFor(a, id) == nil {
		t.Error("no worker running after the switch")
	}
	out := logs.String()
	if !strings.Contains(out, "level=WARN") || !strings.Contains(out, `step="clear cache"`) || !strings.Contains(out, "attempt=1") {
		t.Errorf("no Warn line for the stall, log:\n%s", out)
	}
	if strings.Contains(out, "attempt=2") {
		t.Errorf("Warn logged for the attempt that succeeded, log:\n%s", out)
	}
}

// A large but healthy cache takes longer than the stall budget in total, but
// every batch commits well within it, so the switch finishes on the first attempt.
func TestSwitchProtocolSlowButProgressingDeleteIsNotCancelled(t *testing.T) {
	a, id, folderID, logs := setupSwitchTimeout(t)
	switchProtocolStallTimeout = 300 * time.Millisecond
	var uid atomic.Uint32
	uid.Store(100)
	seedMessages(t, a, id, folderID, 1500, &uid)

	var calls atomic.Int32
	a.store.TestingSetDeleteTxHook(func(ctx context.Context) error {
		calls.Add(1)
		select {
		case <-time.After(100 * time.Millisecond):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})

	start := time.Now()
	if err := switchWithin(t, a, id, "imap", 10*time.Second); err != nil {
		t.Fatalf("SwitchProtocol: %v", err)
	}
	if elapsed := time.Since(start); elapsed <= switchProtocolStallTimeout {
		t.Fatalf("switch took %v, the test needs it to outlast the %v budget", elapsed, switchProtocolStallTimeout)
	}
	// 6 batches of the 1501 messages and the final transaction, once.
	if n := calls.Load(); n != 7 {
		t.Errorf("delete transactions = %d, want 7 (no retry)", n)
	}
	if out := logs.String(); strings.Contains(out, "level=WARN") {
		t.Errorf("Warn logged for a switch that kept making progress, log:\n%s", out)
	}
	acc, err := a.store.GetAccount(a.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if acc.Protocol != "imap" {
		t.Fatalf("protocol = %q, want imap", acc.Protocol)
	}
}

// When the retry stalls as well, the account stays on its old
// protocol with its cache, its old-protocol worker runs, the hold is lifted,
// and the caller gets errProtocolSwitchTimedOut.
func TestSwitchProtocolGivesUpAfterTwoStalls(t *testing.T) {
	a, id, folderID, logs := setupSwitchTimeout(t)
	oldWorker := plantQuiescentWorker(a, id)
	// debug logging on, so a stall leaves its goroutine dump behind.
	a.debug = true
	a.dataDir = t.TempDir()

	var calls atomic.Int32
	a.store.TestingSetDeleteTxHook(func(ctx context.Context) error {
		calls.Add(1)
		<-ctx.Done()
		return ctx.Err()
	})

	err := switchWithin(t, a, id, "imap", 5*time.Second)
	if !errors.Is(err, errProtocolSwitchTimedOut) {
		t.Fatalf("err = %v, want errProtocolSwitchTimedOut", err)
	}
	if n := calls.Load(); n != 2 {
		t.Errorf("clear-cache attempts = %d, want 2", n)
	}
	acc, gerr := a.store.GetAccount(a.ctx, id)
	if gerr != nil {
		t.Fatal(gerr)
	}
	if acc.Protocol != "jmap" || acc.JMAPSessionURL == "" {
		t.Fatalf("account = %q/%q, want it left on jmap", acc.Protocol, acc.JMAPSessionURL)
	}
	if _, err := a.store.GetFolder(a.ctx, folderID); err != nil {
		t.Fatalf("folder should remain: %v", err)
	}
	if n := accountMessageCount(t, a, id, folderID); n != 1 {
		t.Errorf("messages = %d, want the 1 cached message kept", n)
	}
	w := workerFor(a, id)
	if w == nil || w == oldWorker {
		t.Fatal("expected a fresh worker on the old protocol after giving up")
	}
	if err := trySync(a, id); err != nil {
		t.Errorf("sync still held after giving up: %v", err)
	}
	out := logs.String()
	for _, want := range []string{"attempt=1", "attempt=2", "db_in_use=", "db_wait_count=", "wal_bytes="} {
		if !strings.Contains(out, want) {
			t.Errorf("no Warn line with %s, log:\n%s", want, out)
		}
	}
	dumps, err := filepath.Glob(filepath.Join(a.logDir(), "switch-stall-*.txt"))
	if err != nil || len(dumps) == 0 {
		t.Fatalf("no goroutine dump in %s (err %v), log:\n%s", a.logDir(), err, out)
	}
	dump, err := os.ReadFile(dumps[0])
	if err != nil || !strings.Contains(string(dump), "goroutine ") {
		t.Errorf("dump %s holds no goroutine stacks (err %v)", dumps[0], err)
	}
}
