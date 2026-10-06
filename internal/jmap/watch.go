package jmap

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"sort"
	"sync"

	gojmap "github.com/Janso123/go-jmap"
	"github.com/Janso123/go-jmap/core/push/websocket"
	"github.com/Janso123/go-jmap/mail"
	"github.com/Janso123/go-jmap/mail/email"
	"github.com/Janso123/go-jmap/mail/mailbox"
)

// ErrNoWebSocket is returned by StartWatch when the session does not advertise
// urn:ietf:params:jmap:websocket. Callers treat it as “no watch”, not a sync failure.
var ErrNoWebSocket = errors.New("jmap: session has no websocket capability")

// Watch maintains account-wide Email/Mailbox states over a JMAP WebSocket and
// turns StateChange pushes into mailbox ids for the sync engine.
type Watch struct {
	client    *Client
	accountID gojmap.ID
	events    WatchEvents

	conn watchConn

	mu     sync.Mutex
	states watchStates

	ch     chan watchEvent
	cancel context.CancelFunc
	done   chan struct{}
}

// WatchEvents are callbacks invoked by the watch worker.
type WatchEvents struct {
	// OnMailboxes is called with the mailbox ids to resync.
	// A nil slice means the state could not be diffed; the caller syncs the account.
	OnMailboxes func(ids []string) error
	// OnReconnect is the full-sync callback after an established socket reconnects.
	OnReconnect func() error
}

type watchStates struct {
	emailState   string
	mailboxState string
	// membership maps email id → mailbox ids last observed via Email/get.
	membership map[string]map[string]struct{}
}

type watchEvent struct {
	ts   gojmap.TypeState
	full bool // reconnect / cannotCalculateChanges path
}

type readDoer interface {
	doRead(ctx context.Context, req *gojmap.Request) (*gojmap.Response, error)
}

type watchConn interface {
	SetHandler(fn func(*gojmap.StateChange))
	EnablePush(ctx context.Context, dataTypes []gojmap.EventType, pushState string) error
	Close() error
}

// dialWatch opens the JMAP WebSocket. Tests replace it to avoid a real socket.
var dialWatch = func(ctx context.Context, client *gojmap.Client, opts ...websocket.Options) (watchConn, error) {
	return websocket.Dial(ctx, client, opts...)
}

// StartWatch dials the session WebSocket, initializes Email/Mailbox states, and
// enables push for mail events only. Returns ErrNoWebSocket when the capability
// is absent.
func StartWatch(ctx context.Context, client *Client, mailAccountID string, events WatchEvents) (*Watch, error) {
	if client == nil || client.Client == nil {
		return nil, fmt.Errorf("jmap: nil client")
	}
	if client.Session == nil {
		return nil, fmt.Errorf("jmap: client has no session")
	}
	if _, ok := client.Session.Capabilities[websocket.URI]; !ok {
		return nil, ErrNoWebSocket
	}

	states, err := initWatchStates(ctx, client, mailAccountID)
	if err != nil {
		return nil, err
	}

	runCtx, cancel := context.WithCancel(ctx)
	w := &Watch{
		client:    client,
		accountID: gojmap.ID(mailAccountID),
		events:    events,
		states:    states,
		ch:        make(chan watchEvent, 1),
		cancel:    cancel,
		done:      make(chan struct{}),
	}

	conn, err := dialWatch(runCtx, client.Client, websocket.Options{
		Reconnect: &websocket.ReconnectOptions{
			OnReconnect: func() {
				w.enqueueFull()
			},
		},
	})
	if err != nil {
		cancel()
		close(w.done)
		return nil, err
	}
	w.conn = conn

	conn.SetHandler(func(sc *gojmap.StateChange) {
		if sc == nil {
			return
		}
		ts, ok := sc.Changed[w.accountID]
		if !ok || len(ts) == 0 {
			return
		}
		w.enqueue(ts)
	})

	if err := conn.EnablePush(runCtx, []gojmap.EventType{mail.EmailEvent, mail.MailboxEvent}, ""); err != nil {
		_ = conn.Close()
		cancel()
		close(w.done)
		return nil, err
	}

	go w.worker(runCtx)
	go func() {
		<-runCtx.Done()
		_ = conn.Close()
	}()
	return w, nil
}

// DiscardWatch is a Watch that is already stopped. Close returns immediately.
// Desktop tests use it in place of a live socket.
func DiscardWatch() *Watch {
	done := make(chan struct{})
	close(done)
	return &Watch{cancel: func() {}, done: done}
}

// Close stops the worker and closes the WebSocket.
func (w *Watch) Close() error {
	if w == nil {
		return nil
	}
	w.cancel()
	var err error
	if w.conn != nil {
		err = w.conn.Close()
	}
	<-w.done
	return err
}

func newTestWatch(d readDoer, mailAccountID string, events WatchEvents, states watchStates) *Watch {
	ctx, cancel := context.WithCancel(context.Background())
	w := &Watch{
		client:    &Client{}, // unused when doer is injected via process
		accountID: gojmap.ID(mailAccountID),
		events:    events,
		states:    states,
		ch:        make(chan watchEvent, 1),
		cancel:    cancel,
		done:      make(chan struct{}),
	}
	go func() {
		defer close(w.done)
		for {
			select {
			case <-ctx.Done():
				return
			case ev := <-w.ch:
				w.handleEvent(ctx, d, ev)
			}
		}
	}()
	return w
}

func (w *Watch) worker(ctx context.Context) {
	defer close(w.done)
	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-w.ch:
			w.handleEvent(ctx, w.client, ev)
		}
	}
}

func (w *Watch) enqueue(ts gojmap.TypeState) {
	w.enqueueEvent(watchEvent{ts: cloneTypeState(ts)})
}

func (w *Watch) enqueueFull() {
	w.enqueueEvent(watchEvent{full: true})
}

func (w *Watch) enqueueEvent(ev watchEvent) {
	select {
	case w.ch <- ev:
		return
	default:
	}
	select {
	case old := <-w.ch:
		if old.full {
			ev = old
		}
	default:
	}
	select {
	case w.ch <- ev:
	default:
	}
}

func (w *Watch) handleEvent(ctx context.Context, d readDoer, ev watchEvent) {
	if ev.full {
		w.handleFull(ctx, d, true)
		return
	}

	w.mu.Lock()
	current := cloneWatchStates(w.states)
	w.mu.Unlock()

	ids, next, full, err := mailboxIDsFromStates(ctx, d, w.client, string(w.accountID), ev.ts, current)
	if err != nil {
		return
	}
	if full {
		w.handleFull(ctx, d, false)
		return
	}
	if w.events.OnMailboxes != nil {
		if err := w.events.OnMailboxes(ids); err != nil {
			return
		}
	}
	w.mu.Lock()
	w.states = next
	w.mu.Unlock()
}

func (w *Watch) handleFull(ctx context.Context, d readDoer, reconnect bool) {
	if reconnect {
		if w.events.OnReconnect != nil {
			if err := w.events.OnReconnect(); err != nil {
				return
			}
		} else if w.events.OnMailboxes != nil {
			if err := w.events.OnMailboxes(nil); err != nil {
				return
			}
		}
	} else if w.events.OnMailboxes != nil {
		if err := w.events.OnMailboxes(nil); err != nil {
			return
		}
	}
	next, err := initWatchStates(ctx, d, string(w.accountID))
	if err != nil {
		return
	}
	w.mu.Lock()
	w.states = next
	w.mu.Unlock()
}

func initWatchStates(ctx context.Context, d readDoer, mailAccountID string) (watchStates, error) {
	acct := gojmap.ID(mailAccountID)
	emailState, err := fetchObjectState(ctx, d, &email.Get{
		Account:    acct,
		IDs:        gojmap.Some([]gojmap.ID{}),
		Properties: gojmap.Some([]string{"id"}),
	}, func(resp *gojmap.Response, id string) (string, error) {
		r, err := gojmap.As[*email.GetResponse](resp, id)
		if err != nil {
			return "", err
		}
		return r.State, nil
	})
	if err != nil {
		return watchStates{}, err
	}
	mailboxState, err := fetchObjectState(ctx, d, &mailbox.Get{
		Account:    acct,
		IDs:        gojmap.Some([]gojmap.ID{}),
		Properties: gojmap.Some([]string{"id"}),
	}, func(resp *gojmap.Response, id string) (string, error) {
		r, err := gojmap.As[*mailbox.GetResponse](resp, id)
		if err != nil {
			return "", err
		}
		return r.State, nil
	})
	if err != nil {
		return watchStates{}, err
	}
	return watchStates{
		emailState:   emailState,
		mailboxState: mailboxState,
		membership:   make(map[string]map[string]struct{}),
	}, nil
}

func fetchObjectState(ctx context.Context, d readDoer, m gojmap.Method, stateOf func(*gojmap.Response, string) (string, error)) (string, error) {
	req := &gojmap.Request{}
	id := req.Invoke(m)
	resp, err := d.doRead(ctx, req)
	if err != nil {
		return "", err
	}
	return stateOf(resp, id)
}

// mailboxIDsFromStates pages Mailbox/changes and Email/changes from current
// toward target and resolves affected mailbox ids. full is true when the
// server cannot calculate changes; next is then unchanged.
func mailboxIDsFromStates(ctx context.Context, d readDoer, c *Client, mailAccountID string, target gojmap.TypeState, current watchStates) (ids []string, next watchStates, full bool, err error) {
	next = cloneWatchStates(current)
	affected := make(map[string]struct{})
	acct := gojmap.ID(mailAccountID)

	if want, ok := target["Mailbox"]; ok && want != next.mailboxState {
		since := next.mailboxState
		for {
			ch, err := callChangesMailbox(ctx, d, acct, since)
			if err != nil {
				var me *gojmap.MethodError
				if errors.As(err, &me) && me.Type == string(gojmap.MethodErrCannotCalculateChanges) {
					return nil, current, true, nil
				}
				return nil, current, false, err
			}
			for _, id := range ch.Created {
				affected[string(id)] = struct{}{}
			}
			for _, id := range ch.Updated {
				affected[string(id)] = struct{}{}
			}
			for _, id := range ch.Destroyed {
				affected[string(id)] = struct{}{}
			}
			since = ch.NewState
			next.mailboxState = ch.NewState
			if !ch.HasMoreChanges || ch.NewState == want {
				break
			}
		}
	}

	if want, ok := target["Email"]; ok && want != next.emailState {
		since := next.emailState
		for {
			ch, err := callChangesEmail(ctx, d, acct, since)
			if err != nil {
				var me *gojmap.MethodError
				if errors.As(err, &me) && me.Type == string(gojmap.MethodErrCannotCalculateChanges) {
					return nil, current, true, nil
				}
				return nil, current, false, err
			}
			need := make([]gojmap.ID, 0, len(ch.Created)+len(ch.Updated))
			for _, id := range ch.Created {
				need = append(need, id)
			}
			for _, id := range ch.Updated {
				need = append(need, id)
			}
			for _, id := range ch.Destroyed {
				eid := string(id)
				for mid := range next.membership[eid] {
					affected[mid] = struct{}{}
				}
				delete(next.membership, eid)
			}
			if len(need) > 0 {
				list, err := getEmailMailboxIDs(ctx, d, c, acct, need)
				if err != nil {
					return nil, current, false, err
				}
				for _, em := range list {
					eid := string(em.ID)
					for mid := range next.membership[eid] {
						affected[mid] = struct{}{}
					}
					fresh := make(map[string]struct{}, len(em.MailboxIDs))
					for mid, ok := range em.MailboxIDs {
						if !ok {
							continue
						}
						s := string(mid)
						affected[s] = struct{}{}
						fresh[s] = struct{}{}
					}
					next.membership[eid] = fresh
				}
			}
			since = ch.NewState
			next.emailState = ch.NewState
			if !ch.HasMoreChanges || ch.NewState == want {
				break
			}
		}
	}

	ids = make([]string, 0, len(affected))
	for id := range affected {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids, next, false, nil
}

func callChangesMailbox(ctx context.Context, d readDoer, acct gojmap.ID, since string) (*mailbox.ChangesResponse, error) {
	req := &gojmap.Request{}
	id := req.Invoke(&mailbox.Changes{
		Account: acct, SinceState: since,
	})
	resp, err := d.doRead(ctx, req)
	if err != nil {
		return nil, err
	}
	return gojmap.As[*mailbox.ChangesResponse](resp, id)
}

func callChangesEmail(ctx context.Context, d readDoer, acct gojmap.ID, since string) (*gojmap.ChangesResponse, error) {
	req := &gojmap.Request{}
	id := req.Invoke(&email.Changes{
		Account: acct, SinceState: since,
	})
	resp, err := d.doRead(ctx, req)
	if err != nil {
		return nil, err
	}
	return gojmap.As[*email.ChangesResponse](resp, id)
}

func getEmailMailboxIDs(ctx context.Context, d readDoer, c *Client, acct gojmap.ID, ids []gojmap.ID) ([]email.Email, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	limit := coreLimit(c, true)
	var all []email.Email
	err := forIDChunks(ids, limit, func(chunk []gojmap.ID) error {
		req := &gojmap.Request{}
		callID := req.Invoke(&email.Get{
			Account:    acct,
			IDs:        gojmap.Some(chunk),
			Properties: gojmap.Some([]string{"id", "mailboxIds"}),
		})
		resp, err := d.doRead(ctx, req)
		if err != nil {
			return err
		}
		r, err := gojmap.As[*email.GetResponse](resp, callID)
		if err != nil {
			return err
		}
		all = append(all, r.List...)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return all, nil
}

func cloneWatchStates(s watchStates) watchStates {
	out := watchStates{
		emailState:   s.emailState,
		mailboxState: s.mailboxState,
		membership:   make(map[string]map[string]struct{}, len(s.membership)),
	}
	for eid, boxes := range s.membership {
		cp := make(map[string]struct{}, len(boxes))
		for mid := range boxes {
			cp[mid] = struct{}{}
		}
		out.membership[eid] = cp
	}
	return out
}

func cloneTypeState(ts gojmap.TypeState) gojmap.TypeState {
	out := make(gojmap.TypeState, len(ts))
	maps.Copy(out, ts)
	return out
}
