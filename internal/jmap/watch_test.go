package jmap

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	gojmap "github.com/Janso123/go-jmap"
	"github.com/Janso123/go-jmap/core"
	"github.com/Janso123/go-jmap/core/push/websocket"
	"github.com/Janso123/go-jmap/mail"
	"github.com/Janso123/go-jmap/mail/email"
	"github.com/Janso123/go-jmap/mail/mailbox"
)

func TestMailboxIDsFromStates(t *testing.T) {
	current := watchStates{emailState: "e0", mailboxState: "m0"}
	target := gojmap.TypeState{"Email": "e2", "Mailbox": "m2"}

	var mu sync.Mutex
	var concurrent atomic.Int32
	var maxConcurrent atomic.Int32
	var mailboxPages, emailPages int

	d := &scriptDoer{fn: func(ctx context.Context, req *gojmap.Request) (*gojmap.Response, error) {
		n := concurrent.Add(1)
		for {
			old := maxConcurrent.Load()
			if n <= old || maxConcurrent.CompareAndSwap(old, n) {
				break
			}
		}
		defer concurrent.Add(-1)

		if len(req.Calls) != 1 {
			t.Fatalf("expected 1 call, got %d", len(req.Calls))
		}
		call := req.Calls[0]
		switch call.Name {
		case "Mailbox/changes":
			mu.Lock()
			mailboxPages++
			page := mailboxPages
			mu.Unlock()
			ch, ok := call.Args.(*mailbox.Changes)
			if !ok {
				t.Fatalf("Mailbox/changes args: %T", call.Args)
			}
			switch page {
			case 1:
				if ch.SinceState != "m0" {
					t.Fatalf("mailbox page1 since: %q", ch.SinceState)
				}
				return methodOK(call.CallID, "Mailbox/changes", &mailbox.ChangesResponse{
					Account: "A1", OldState: "m0", NewState: "m1",
					HasMoreChanges: true, Created: []gojmap.ID{"M1"},
				}), nil
			case 2:
				if ch.SinceState != "m1" {
					t.Fatalf("mailbox page2 since: %q", ch.SinceState)
				}
				return methodOK(call.CallID, "Mailbox/changes", &mailbox.ChangesResponse{
					Account: "A1", OldState: "m1", NewState: "m2",
					HasMoreChanges: false,
				}), nil
			default:
				t.Fatalf("unexpected Mailbox/changes page %d", page)
			}
		case "Email/changes":
			mu.Lock()
			emailPages++
			page := emailPages
			mu.Unlock()
			ch, ok := call.Args.(*email.Changes)
			if !ok {
				t.Fatalf("Email/changes args: %T", call.Args)
			}
			switch page {
			case 1:
				if ch.SinceState != "e0" {
					t.Fatalf("email page1 since: %q", ch.SinceState)
				}
				return methodOK(call.CallID, "Email/changes", &gojmap.ChangesResponse{
					Account: "A1", OldState: "e0", NewState: "e1",
					HasMoreChanges: true, Created: []gojmap.ID{"E1"},
				}), nil
			case 2:
				if ch.SinceState != "e1" {
					t.Fatalf("email page2 since: %q", ch.SinceState)
				}
				return methodOK(call.CallID, "Email/changes", &gojmap.ChangesResponse{
					Account: "A1", OldState: "e1", NewState: "e2",
					HasMoreChanges: false, Updated: []gojmap.ID{"E2"},
				}), nil
			default:
				t.Fatalf("unexpected Email/changes page %d", page)
			}
		case "Email/get":
			g, ok := call.Args.(*email.Get)
			if !ok {
				t.Fatalf("Email/get args: %T", call.Args)
			}
			ids, _ := g.IDs.Value()
			list := make([]email.Email, 0, len(ids))
			for _, id := range ids {
				switch id {
				case "E1":
					list = append(list, email.Email{ID: "E1", MailboxIDs: map[gojmap.ID]bool{"M1": true}})
				case "E2":
					list = append(list, email.Email{ID: "E2", MailboxIDs: map[gojmap.ID]bool{"M2": true}})
				}
			}
			return methodOK(call.CallID, "Email/get", &email.GetResponse{
				Account: "A1", State: "e2", List: list,
			}), nil
		default:
			t.Fatalf("unexpected method %s", call.Name)
		}
		return nil, nil
	}}

	ids, next, full, err := mailboxIDsFromStates(context.Background(), d, nil, "A1", target, current)
	if err != nil {
		t.Fatalf("mailboxIDsFromStates: %v", err)
	}
	if full {
		t.Fatal("expected full=false")
	}
	if next.emailState != "e2" || next.mailboxState != "m2" {
		t.Fatalf("next states: email=%q mailbox=%q", next.emailState, next.mailboxState)
	}
	if got := setOf(ids); !mapsEqual(got, map[string]struct{}{"M1": {}, "M2": {}}) {
		t.Fatalf("ids: %v", ids)
	}
	if mailboxPages != 2 || emailPages != 2 {
		t.Fatalf("pages: mailbox=%d email=%d", mailboxPages, emailPages)
	}
}

func TestMailboxIDsFromStatesCannotCalculate(t *testing.T) {
	current := watchStates{emailState: "e0", mailboxState: "m0"}
	target := gojmap.TypeState{"Email": "e9", "Mailbox": "m0"}
	d := &scriptDoer{fn: func(ctx context.Context, req *gojmap.Request) (*gojmap.Response, error) {
		call := req.Calls[0]
		if call.Name != "Email/changes" {
			t.Fatalf("unexpected %s", call.Name)
		}
		return &gojmap.Response{
			SessionState: "s1",
			Responses: []*gojmap.Invocation{{
				Name: "error", CallID: call.CallID,
				Args: &gojmap.MethodError{Type: string(gojmap.MethodErrCannotCalculateChanges)},
			}},
		}, nil
	}}
	ids, next, full, err := mailboxIDsFromStates(context.Background(), d, nil, "A1", target, current)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !full {
		t.Fatal("expected full=true")
	}
	if ids != nil {
		t.Fatalf("ids: %v want nil", ids)
	}
	if next.emailState != current.emailState || next.mailboxState != current.mailboxState {
		t.Fatalf("states changed: %+v", next)
	}
}

func TestMailboxIDsFromStatesChunksEmailGet(t *testing.T) {
	current := watchStates{emailState: "e0", mailboxState: "m0"}
	target := gojmap.TypeState{"Email": "e1"}

	var getSizes []int
	d := &scriptDoer{fn: func(ctx context.Context, req *gojmap.Request) (*gojmap.Response, error) {
		call := req.Calls[0]
		switch call.Name {
		case "Email/changes":
			return methodOK(call.CallID, "Email/changes", &gojmap.ChangesResponse{
				Account: "A1", OldState: "e0", NewState: "e1",
				Created: []gojmap.ID{"E1", "E2", "E3", "E4", "E5"},
			}), nil
		case "Email/get":
			g, ok := call.Args.(*email.Get)
			if !ok {
				t.Fatalf("Email/get args: %T", call.Args)
			}
			ids, _ := g.IDs.Value()
			getSizes = append(getSizes, len(ids))
			list := make([]email.Email, 0, len(ids))
			for _, id := range ids {
				list = append(list, email.Email{ID: id, MailboxIDs: map[gojmap.ID]bool{"M1": true}})
			}
			return methodOK(call.CallID, "Email/get", &email.GetResponse{
				Account: "A1", State: "e1", List: list,
			}), nil
		default:
			t.Fatalf("unexpected %s", call.Name)
		}
		return nil, nil
	}}

	c := &Client{Client: &gojmap.Client{
		Session: &gojmap.Session{
			Capabilities: map[gojmap.URI]gojmap.Capability{
				gojmap.CoreURI: &core.Core{MaxObjectsInGet: 2},
			},
		},
	}}

	ids, next, full, err := mailboxIDsFromStates(context.Background(), d, c, "A1", target, current)
	if err != nil {
		t.Fatalf("mailboxIDsFromStates: %v", err)
	}
	if full {
		t.Fatal("expected full=false")
	}
	if next.emailState != "e1" {
		t.Fatalf("email state: %q", next.emailState)
	}
	if got := setOf(ids); !mapsEqual(got, map[string]struct{}{"M1": {}}) {
		t.Fatalf("ids: %v", ids)
	}
	if len(getSizes) < 2 {
		t.Fatalf("expected multiple Email/get chunks, got %v", getSizes)
	}
	for _, n := range getSizes {
		if n > 2 {
			t.Fatalf("chunk size %d exceeds maxObjectsInGet 2: %v", n, getSizes)
		}
	}
	total := 0
	for _, n := range getSizes {
		total += n
	}
	if total != 5 {
		t.Fatalf("expected 5 ids across chunks, got %v (sum=%d)", getSizes, total)
	}
}

func TestWatchWorkerSerializesAndCallbackError(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var concurrent atomic.Int32
		var maxConcurrent int32
		started := make(chan struct{}, 8)
		release := make(chan struct{})

		d := &scriptDoer{fn: func(ctx context.Context, req *gojmap.Request) (*gojmap.Response, error) {
			n := concurrent.Add(1)
			for {
				old := atomic.LoadInt32(&maxConcurrent)
				if n <= old || atomic.CompareAndSwapInt32(&maxConcurrent, old, n) {
					break
				}
			}
			defer concurrent.Add(-1)

			select {
			case started <- struct{}{}:
			default:
			}
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(5 * time.Second):
				t.Fatal("timed out waiting for release")
			}

			call := req.Calls[0]
			switch call.Name {
			case "Mailbox/changes":
				ch := call.Args.(*mailbox.Changes)
				return methodOK(call.CallID, "Mailbox/changes", &mailbox.ChangesResponse{
					Account: "A1", OldState: ch.SinceState, NewState: ch.SinceState + "-m",
				}), nil
			case "Email/changes":
				ch := call.Args.(*email.Changes)
				return methodOK(call.CallID, "Email/changes", &gojmap.ChangesResponse{
					Account: "A1", OldState: ch.SinceState, NewState: ch.SinceState + "-e",
				}), nil
			default:
				t.Fatalf("unexpected %s", call.Name)
			}
			return nil, nil
		}}

		var calls atomic.Int32
		cbErr := errors.New("callback failed")
		secondCB := make(chan struct{})
		w := newTestWatch(d, "A1", WatchEvents{
			OnMailboxes: func(ids []string) error {
				if calls.Add(1) == 2 {
					close(secondCB)
				}
				return cbErr
			},
		}, watchStates{emailState: "e0", mailboxState: "m0"})
		releaseOnce := sync.OnceFunc(func() { close(release) })
		t.Cleanup(func() {
			releaseOnce()
			w.Close()
		})

		w.enqueue(gojmap.TypeState{"Email": "e1", "Mailbox": "m1"})
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			t.Fatal("worker did not start first /changes")
		}
		if atomic.LoadInt32(&maxConcurrent) != 1 {
			t.Fatalf("concurrent /changes: %d", maxConcurrent)
		}
		// Second push while the worker is still in the first transition.
		w.enqueue(gojmap.TypeState{"Email": "e2", "Mailbox": "m2"})
		synctest.Wait()
		if atomic.LoadInt32(&maxConcurrent) != 1 {
			t.Fatalf("second push started concurrent /changes: %d", maxConcurrent)
		}
		releaseOnce()

		select {
		case <-secondCB:
		case <-time.After(2 * time.Second):
			t.Fatal("expected both events to reach OnMailboxes")
		}

		w.mu.Lock()
		st := w.states
		w.mu.Unlock()
		if st.emailState != "e0" || st.mailboxState != "m0" {
			t.Fatalf("state advanced after callback error: %+v", st)
		}
		if atomic.LoadInt32(&maxConcurrent) != 1 {
			t.Fatalf("concurrent /changes after release: %d", maxConcurrent)
		}
		if calls.Load() != 2 {
			t.Fatalf("OnMailboxes calls: %d want 2", calls.Load())
		}
	})
}

func TestDialReconnect(t *testing.T) {
	var initGets atomic.Int32
	var reinitGets atomic.Int32
	srv := newJMAPServer(t, sessionDocWithWebSocket(), func(w http.ResponseWriter, r *http.Request, calls []methodCall) {
		_ = r
		name := calls[0].Name
		switch name {
		case "Email/get", "Mailbox/get":
			ids := stringIDs(calls[0].Args["ids"])
			if len(ids) != 0 {
				t.Fatalf("%s expected empty ids, got %v", name, ids)
			}
			n := initGets.Add(1)
			state := "init"
			if n > 2 {
				reinitGets.Add(1)
				state = "reinit"
			}
			writeMethod(w, "s1", calls[0].ID, name, map[string]any{
				"accountId": "A1", "state": state, "list": []any{}, "notFound": []string{},
			})
		default:
			t.Fatalf("unexpected method %s", name)
		}
	})
	defer srv.Close()

	client := newTestClient(t, srv)
	client.Session.Capabilities[websocket.URI] = &websocket.WebSocket{
		URL: "wss://example.invalid/jmap/ws", SupportsPush: true,
	}

	var reconnectOpts *websocket.ReconnectOptions
	fake := &fakeWatchConn{}
	oldDial := dialWatch
	dialWatch = func(ctx context.Context, c *gojmap.Client, opts ...websocket.Options) (watchConn, error) {
		_ = ctx
		_ = c
		if len(opts) > 0 {
			reconnectOpts = opts[0].Reconnect
		}
		return fake, nil
	}
	defer func() { dialWatch = oldDial }()

	var reconnectCalls atomic.Int32
	done := make(chan struct{})
	w, err := StartWatch(context.Background(), client, "A1", WatchEvents{
		OnMailboxes: func(ids []string) error {
			t.Fatalf("OnMailboxes called: %v", ids)
			return nil
		},
		OnReconnect: func() error {
			if reconnectCalls.Add(1) == 1 {
				close(done)
			}
			return nil
		},
	})
	if err != nil {
		t.Fatalf("StartWatch: %v", err)
	}
	defer w.Close()

	if reconnectOpts == nil || reconnectOpts.OnReconnect == nil {
		t.Fatal("Dial options missing Reconnect.OnReconnect")
	}
	if !fake.enableCalled {
		t.Fatal("EnablePush not called")
	}
	wantTypes := map[gojmap.EventType]struct{}{mail.EmailEvent: {}, mail.MailboxEvent: {}}
	if got := setOfEventTypes(fake.dataTypes); !eventMapsEqual(got, wantTypes) {
		t.Fatalf("EnablePush types: %v", fake.dataTypes)
	}
	for _, typ := range fake.dataTypes {
		if typ == "ContactCard" || typ == "AddressBook" {
			t.Fatalf("must not subscribe to %s", typ)
		}
	}

	reconnectOpts.OnReconnect()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("OnReconnect callback not invoked")
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if reinitGets.Load() >= 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if reconnectCalls.Load() != 1 {
		t.Fatalf("OnReconnect calls: %d", reconnectCalls.Load())
	}
	if reinitGets.Load() < 2 {
		t.Fatalf("expected Email+Mailbox reinit gets, got %d", reinitGets.Load())
	}
}

func TestStartWatchNoWebSocket(t *testing.T) {
	srv := newJMAPServer(t, sessionDocLimits(50, 50), func(w http.ResponseWriter, r *http.Request, calls []methodCall) {
		t.Fatal("no API calls expected")
	})
	defer srv.Close()
	client := newTestClient(t, srv)
	_, err := StartWatch(context.Background(), client, "A1", WatchEvents{})
	if err == nil {
		t.Fatal("expected error when websocket capability missing")
	}
}

type scriptDoer struct {
	fn func(ctx context.Context, req *gojmap.Request) (*gojmap.Response, error)
}

func (d *scriptDoer) doRead(ctx context.Context, req *gojmap.Request) (*gojmap.Response, error) {
	return d.fn(ctx, req)
}

func methodOK(callID, name string, args any) *gojmap.Response {
	return &gojmap.Response{
		SessionState: "s1",
		Responses:    []*gojmap.Invocation{{Name: name, CallID: callID, Args: args}},
	}
}

func setOf(ids []string) map[string]struct{} {
	m := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		m[id] = struct{}{}
	}
	return m
}

func mapsEqual(a, b map[string]struct{}) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if _, ok := b[k]; !ok {
			return false
		}
	}
	return true
}

func setOfEventTypes(types []gojmap.EventType) map[gojmap.EventType]struct{} {
	m := make(map[gojmap.EventType]struct{}, len(types))
	for _, t := range types {
		m[t] = struct{}{}
	}
	return m
}

func eventMapsEqual(a, b map[gojmap.EventType]struct{}) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if _, ok := b[k]; !ok {
			return false
		}
	}
	return true
}

type fakeWatchConn struct {
	enableCalled bool
	dataTypes    []gojmap.EventType
	handler      func(*gojmap.StateChange)
}

func (f *fakeWatchConn) SetHandler(fn func(*gojmap.StateChange)) { f.handler = fn }
func (f *fakeWatchConn) EnablePush(_ context.Context, dataTypes []gojmap.EventType, _ string) error {
	f.enableCalled = true
	f.dataTypes = append([]gojmap.EventType(nil), dataTypes...)
	return nil
}
func (f *fakeWatchConn) Close() error { return nil }

func sessionDocWithWebSocket() string {
	return `{
  "capabilities": {
    "urn:ietf:params:jmap:core": {
      "maxSizeUpload": 50000000,
      "maxConcurrentUpload": 8,
      "maxSizeRequest": 10000000,
      "maxConcurrentRequest": 8,
      "maxCallsInRequest": 32,
      "maxObjectsInGet": 50,
      "maxObjectsInSet": 50,
      "collationAlgorithms": ["i;ascii-casemap"]
    },
    "urn:ietf:params:jmap:mail": {},
    "urn:ietf:params:jmap:submission": {},
    "urn:ietf:params:jmap:websocket": {
      "url": "wss://example.invalid/jmap/ws",
      "supportsPush": true
    }
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
  "state": "s1"
}`
}
