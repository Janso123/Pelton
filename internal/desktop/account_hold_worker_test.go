package desktop

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/peltonapp/Pelton/internal/desktop/syncsched"
	pimap "github.com/peltonapp/Pelton/internal/imap"
	pjmap "github.com/peltonapp/Pelton/internal/jmap"
	"github.com/peltonapp/Pelton/internal/storage"
)

// selectRecordingIMAP counts SELECTs, which only the initial sync stub and body
// jobs issue in the worker's initial sync.
type selectRecordingIMAP struct {
	fakeIMAP
	selects *atomic.Int32
}

func (c *selectRecordingIMAP) Select(mailbox string) (*pimap.Mailbox, error) {
	c.selects.Add(1)
	return &pimap.Mailbox{Name: mailbox}, nil
}

const holdTestJMAPSession = `{
  "capabilities": {
    "urn:ietf:params:jmap:core": {
      "maxSizeUpload": 50000000, "maxConcurrentUpload": 8,
      "maxSizeRequest": 10000000, "maxConcurrentRequest": 8,
      "maxCallsInRequest": 32, "maxObjectsInGet": 256, "maxObjectsInSet": 128,
      "collationAlgorithms": ["i;ascii-casemap"]
    },
    "urn:ietf:params:jmap:mail": {}
  },
  "accounts": {"A1": {"name": "u", "isPersonal": true, "isReadOnly": false,
    "accountCapabilities": {"urn:ietf:params:jmap:mail": {}}}},
  "primaryAccounts": {"urn:ietf:params:jmap:mail": "A1"},
  "username": "u",
  "apiUrl": "/api/",
  "downloadUrl": "/download/{accountId}/{blobId}/{name}?accept={type}",
  "uploadUrl": "/upload/{accountId}/",
  "eventSourceUrl": "/eventsource/",
  "state": "s1"
}`

// waitAtomic waits up to d for n to reach at least want.
func waitAtomic(n *atomic.Int32, want int32, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for n.Load() < want {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(5 * time.Millisecond)
	}
	return true
}

// The switch starts the new worker while it still holds the account and lets
// go afterwards. Every sync call that worker makes must get through the hold,
// not only its first: here the hold outlives the worker's whole start, the
// worst interleaving, and the post-switch initial sync must still run.
func TestWorkerStartedUnderHoldRunsInitialSync(t *testing.T) {
	t.Run("jmap", func(t *testing.T) {
		// Restored after the fixture joins its background goroutines (cleanups
		// run last-in first-out), since the watch loop reads it.
		orig := startWatch
		t.Cleanup(func() { startWatch = orig })
		startWatch = func(context.Context, *pjmap.Client, string, pjmap.WatchEvents) (*pjmap.Watch, error) {
			return nil, pjmap.ErrNoWebSocket
		}
		a := newJMAPSwitchTestApp(t)
		var apiCalls atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/.well-known/jmap") {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, holdTestJMAPSession)
				return
			}
			if strings.HasPrefix(r.URL.Path, "/api") {
				apiCalls.Add(1)
			}
			http.Error(w, "not in this test", http.StatusInternalServerError)
		}))
		t.Cleanup(srv.Close)
		id, _ := seedSwitchAccount(t, a, "held-jmap@example.test", "imap")
		if err := a.store.SwitchAccountProtocol(a.ctx, id, "jmap", srv.URL+"/.well-known/jmap", "A1", nil); err != nil {
			t.Fatal(err)
		}

		release := a.holdAccountSync(id)
		a.startHeldAccountWorker(id)
		ran := waitAtomic(&apiCalls, 1, 2*time.Second)
		release()
		if !ran {
			t.Fatal("the worker started under the hold never ran its JMAP initial sync")
		}
	})
	t.Run("imap", func(t *testing.T) {
		a := newJMAPSwitchTestApp(t)
		var selects atomic.Int32
		a.newIMAPClient = func(pimap.Config) (mailClient, error) {
			return &selectRecordingIMAP{selects: &selects}, nil
		}
		id, _ := seedSwitchAccount(t, a, "held-imap@example.test", "imap")

		release := a.holdAccountSync(id)
		a.startHeldAccountWorker(id)
		ran := waitAtomic(&selects, 1, 2*time.Second)
		release()
		if !ran {
			t.Fatal("the worker started under the hold never ran its IMAP initial sync")
		}
	})
}

// A worker started by anything but the holder (profile start, backup restore)
// while the account is held must not run: an IMAP one writes folder rows
// before its first scheduler call, in the middle of the cache delete.
func TestOtherWorkerStartDuringHoldDoesNothing(t *testing.T) {
	a := newJMAPSwitchTestApp(t)
	var dials atomic.Int32
	a.newIMAPClient = func(pimap.Config) (mailClient, error) {
		dials.Add(1)
		return &fakeIMAP{}, nil
	}
	id, _ := seedSwitchAccount(t, a, "alien@example.test", "imap")

	release := a.holdAccountSync(id)
	defer release()
	a.startAccountWorker(id)
	time.Sleep(100 * time.Millisecond)
	if n := dials.Load(); n != 0 {
		t.Errorf("a worker started during the hold opened %d IMAP sessions", n)
	}
	if workerFor(a, id) != nil {
		t.Error("a worker was registered during the hold")
	}
}

// A limit raise loads the account and its folders before it queues. If the
// account switched in between, the queued jobs must not sync with the old
// protocol.
func TestLimitDeltaSkipsAccountLoadedBeforeSwitch(t *testing.T) {
	a := newJMAPSwitchTestApp(t)
	id, folderID := seedSwitchAccount(t, a, "delta@example.test", "imap")
	stale, err := a.store.GetAccount(a.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	folder, err := a.store.GetFolder(a.ctx, folderID)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.store.SwitchAccountProtocol(a.ctx, id, "jmap", "https://jmap.example", "A1", nil); err != nil {
		t.Fatal(err)
	}
	var dials atomic.Int32
	a.newIMAPClient = func(pimap.Config) (mailClient, error) {
		dials.Add(1)
		return &fakeIMAP{}, nil
	}
	ran := make(chan struct{})
	a.enqueueAccountDelta(*stale, []storage.Folder{*folder}, 50, 0)
	sched, err := a.ensureAccountScheduler(a.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	// A later background job runs after the delta's two jobs.
	sched.Enqueue(syncsched.Job{
		Priority: syncsched.PriorityBackgroundBody,
		Kind:     syncsched.JobFetchBodies,
		Run:      func(context.Context) error { close(ran); return nil },
	})
	select {
	case <-ran:
	case <-time.After(2 * time.Second):
		t.Fatal("delta jobs never finished")
	}
	if n := dials.Load(); n != 0 {
		t.Errorf("stale limit-raise job opened %d IMAP sessions on a JMAP account", n)
	}
}
