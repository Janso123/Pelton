package jmap

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	gojmap "github.com/Janso123/go-jmap"
	"github.com/Janso123/go-jmap/core"
	"github.com/Janso123/go-jmap/mail"
	"github.com/Janso123/go-jmap/mail/email"
	"github.com/Janso123/go-jmap/mail/mailbox"

	"github.com/peltonapp/Pelton/internal/rfc822"
	"github.com/peltonapp/Pelton/internal/storage"
	psync "github.com/peltonapp/Pelton/internal/sync"
)

// Conservative fallback when Core advertises a zero get/set limit.
const defaultMaxObjects = 50

// Adapter presents a JMAP mail account as a sync.Adapter.
type Adapter struct {
	client    *Client
	accountID gojmap.ID
	// requests counts JMAP API round trips for the sync log.
	requests atomic.Int64
	// BlobDownloadParallel is how many blob Download calls Fetch may run at
	// once after Email/get. Zero means serial (one at a time).
	BlobDownloadParallel int
}

// NewAdapter wraps a JMAP client for the sync engine.
func NewAdapter(client *Client, mailAccountID string) *Adapter {
	return &Adapter{client: client, accountID: gojmap.ID(mailAccountID)}
}

var _ psync.Adapter = (*Adapter)(nil)

// Addr returns the host of the session API URL, or the session URL string if
// parsing fails.
func (a *Adapter) Addr() string {
	if a.client == nil || a.client.Client == nil {
		return ""
	}
	raw := ""
	if a.client.Session != nil && a.client.Session.APIURL != "" {
		raw = a.client.Session.APIURL
	} else {
		raw = a.client.SessionEndpoint
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return raw
	}
	return u.Host
}

// ListMailboxes returns every JMAP mailbox with its role and parent. A mailbox
// the account may not read items in is returned as not selectable.
func (a *Adapter) ListMailboxes(ctx context.Context) ([]psync.Mailbox, error) {
	resp, err := a.callRead[*mailbox.GetResponse](ctx, &mailbox.Get{
		Account: a.accountID,
	})
	if err != nil {
		return nil, err
	}
	out := make([]psync.Mailbox, 0, len(resp.List))
	for _, m := range resp.List {
		mb := psync.Mailbox{
			RemoteID:   string(m.ID),
			Name:       m.Name,
			Generation: "",
			Selectable: true,
			Role:       mailboxRole(m.Role),
		}
		if pid, ok := m.ParentID.Value(); ok {
			mb.ParentID = string(pid)
		}
		if m.Rights != nil && !m.Rights.MayReadItems {
			mb.Selectable = false
		}
		out = append(out, mb)
	}
	return out, nil
}

// ListMessages lists the whole mailbox and returns its headers newest first
// with the Email state read before the query. Generation is always empty. The
// stored token is ignored: deltas go through ListChanges.
func (a *Adapter) ListMessages(ctx context.Context, box psync.RemoteMailbox) ([]psync.Header, string, string, error) {
	return a.listMessages(ctx, box, nil)
}

var _ psync.PagedLister = (*Adapter)(nil)
var _ psync.DeltaLister = (*Adapter)(nil)
var _ psync.RequestCounter = (*Adapter)(nil)

// ListMessagesPaged is ListMessages with early delivery: onPage is called
// after each Email/get chunk, in Email/query order (newest first). An onPage
// error stops the list and is returned.
func (a *Adapter) ListMessagesPaged(ctx context.Context, box psync.RemoteMailbox, onPage func([]psync.Header) error) ([]psync.Header, string, string, error) {
	return a.listMessages(ctx, box, onPage)
}

func (a *Adapter) listMessages(ctx context.Context, box psync.RemoteMailbox, onPage func([]psync.Header) error) ([]psync.Header, string, string, error) {
	headers, token, err := a.listMessagesFull(ctx, box.RemoteID, onPage)
	if err != nil {
		return nil, "", "", err
	}
	return headers, token, "", nil
}

// Requests returns how many JMAP API requests this adapter has sent.
func (a *Adapter) Requests() int64 { return a.requests.Load() }

// ListChanges reports what changed in one mailbox since box.StateToken. Each
// round trip carries Email/changes and two Email/get calls that reference its
// created and updated ids, so a quiet mailbox costs one request. A message
// that left the mailbox, was destroyed, or vanished before the get is
// Removed. ensure ids that the changes did not cover are read with one more
// Email/get. An empty token or cannotCalculateChanges returns ErrNeedFullList.
func (a *Adapter) ListChanges(ctx context.Context, box psync.RemoteMailbox, ensure []string) (psync.Delta, error) {
	if box.StateToken == "" {
		return psync.Delta{}, psync.ErrNeedFullList
	}
	maxChanges := gojmap.UnsignedInt(a.maxObjectsInGet())
	since := box.StateToken
	got := make(map[string]email.Email)
	gone := make(map[string]struct{})
	touched := false
	for {
		req := &gojmap.Request{}
		chID := req.Invoke(&email.Changes{Account: a.accountID, SinceState: since, MaxChanges: &maxChanges})
		getIDs := make([]string, 0, 2)
		for _, path := range []string{"/created", "/updated"} {
			getIDs = append(getIDs, req.Invoke(&email.Get{
				Account:      a.accountID,
				ReferenceIDs: &gojmap.ResultReference{ResultOf: chID, Name: "Email/changes", Path: path},
				Properties:   gojmap.Some(listEmailProps),
			}))
		}
		a.requests.Add(1)
		resp, err := a.client.doRead(ctx, req)
		if err != nil {
			return psync.Delta{}, err
		}
		ch, err := gojmap.As[*email.ChangesResponse](resp, chID)
		if err != nil {
			var me *gojmap.MethodError
			if errors.As(err, &me) && me.Type == string(gojmap.MethodErrCannotCalculateChanges) {
				return psync.Delta{}, psync.ErrNeedFullList
			}
			return psync.Delta{}, err
		}
		for _, id := range getIDs {
			gr, err := gojmap.As[*email.GetResponse](resp, id)
			if err != nil {
				return psync.Delta{}, err
			}
			for _, em := range gr.List {
				got[string(em.ID)] = em
				delete(gone, string(em.ID))
			}
			for _, nf := range gr.NotFound {
				delete(got, string(nf))
				gone[string(nf)] = struct{}{}
			}
		}
		for _, id := range ch.Destroyed {
			delete(got, string(id))
			gone[string(id)] = struct{}{}
		}
		if len(ch.Created)+len(ch.Updated)+len(ch.Destroyed) > 0 {
			touched = true
		}
		since = ch.NewState
		if !ch.HasMoreChanges {
			break
		}
	}

	var need []gojmap.ID
	for _, id := range ensure {
		_, have := got[id]
		_, lost := gone[id]
		if !have && !lost {
			need = append(need, gojmap.ID(id))
		}
	}
	if len(need) > 0 {
		list, err := a.getEmails(ctx, need, listEmailProps)
		if err != nil {
			return psync.Delta{}, err
		}
		for _, em := range list {
			got[string(em.ID)] = em
		}
		for _, id := range need {
			if _, ok := got[string(id)]; !ok {
				gone[string(id)] = struct{}{}
			}
		}
	}

	d := psync.Delta{Cursor: since, Unchanged: !touched && len(ensure) == 0}
	mbox := gojmap.ID(box.RemoteID)
	for id, em := range got {
		if em.MailboxIDs[mbox] {
			d.Changed = append(d.Changed, headerFromEmail(em))
		} else {
			d.Removed = append(d.Removed, id)
		}
	}
	for id := range gone {
		d.Removed = append(d.Removed, id)
	}
	slices.SortFunc(d.Changed, func(x, y psync.Header) int {
		if c := y.Date.Compare(x.Date); c != 0 {
			return c
		}
		return strings.Compare(x.RemoteID, y.RemoteID)
	})
	slices.Sort(d.Removed)
	return d, nil
}

// listMessagesFull lists the whole mailbox via Email/query + chunked
// Email/get. onPage, when set, gets each chunk's headers in query order as
// soon as the chunk is read. The returned state is read before the query, so
// an email that arrives mid-list is still reported by the next Email/changes.
func (a *Adapter) listMessagesFull(ctx context.Context, mailboxID string, onPage func([]psync.Header) error) ([]psync.Header, string, error) {
	state, err := a.emailState(ctx)
	if err != nil {
		return nil, "", err
	}
	ids, err := a.queryMailboxIDs(ctx, mailboxID)
	if err != nil {
		return nil, "", err
	}
	if len(ids) == 0 {
		return nil, state, nil
	}
	jids := make([]gojmap.ID, len(ids))
	for i, id := range ids {
		jids[i] = gojmap.ID(id)
	}
	var onChunk func([]email.Email) error
	if onPage != nil {
		pos := make(map[string]int, len(ids))
		for i, id := range ids {
			pos[id] = i
		}
		onChunk = func(chunk []email.Email) error {
			sorted := slices.Clone(chunk)
			slices.SortStableFunc(sorted, func(x, y email.Email) int {
				return pos[string(x.ID)] - pos[string(y.ID)]
			})
			page := make([]psync.Header, 0, len(sorted))
			for _, em := range sorted {
				page = append(page, headerFromEmail(em))
			}
			return onPage(page)
		}
	}
	list, _, err := a.getEmailsWithState(ctx, jids, listEmailProps, onChunk)
	if err != nil {
		return nil, "", err
	}
	byID := make(map[string]email.Email, len(list))
	for _, em := range list {
		byID[string(em.ID)] = em
	}
	headers := make([]psync.Header, 0, len(ids))
	for _, id := range ids {
		em, ok := byID[id]
		if !ok {
			continue
		}
		headers = append(headers, headerFromEmail(em))
	}
	return headers, state, nil
}

// listEmailProps is the Email/get property set for list snapshots: enough for
// the message list without downloading blobs.
var listEmailProps = []string{
	"id", "keywords", "mailboxIds",
	"subject", "from", "to", "receivedAt", "size", "hasAttachment", "preview",
}

func headerFromEmail(em email.Email) psync.Header {
	h := psync.Header{
		RemoteID:    string(em.ID),
		Flags:       FlagsFromKeywords(em.Keywords),
		HasListMeta: true,
		Preview:     em.Preview,
	}
	if subj, ok := em.Subject.Value(); ok {
		h.Subject = subj
	}
	if from, ok := em.From.Value(); ok {
		h.From, h.FromName = primaryMailAddress(from)
	}
	if to, ok := em.To.Value(); ok {
		h.To = joinMailEmails(to)
	}
	if em.ReceivedAt != nil {
		h.Date = time.Time(*em.ReceivedAt)
	}
	if em.Size != nil {
		h.Size = int64(*em.Size)
	}
	if em.HasAttachment != nil {
		h.HasAttachment = *em.HasAttachment
	}
	return h
}

func primaryMailAddress(addrs []*mail.Address) (emailAddr, name string) {
	if len(addrs) == 0 || addrs[0] == nil {
		return "", ""
	}
	a := addrs[0]
	emailAddr = a.Email
	if n, ok := a.Name.Value(); ok {
		name = n
	}
	return emailAddr, name
}

func joinMailEmails(addrs []*mail.Address) string {
	parts := make([]string, 0, len(addrs))
	for _, a := range addrs {
		if a == nil || a.Email == "" {
			continue
		}
		parts = append(parts, a.Email)
	}
	return strings.Join(parts, ", ")
}

// Fetch downloads the raw messages for remoteIDs by blob. The mailbox id is not
// needed: JMAP ids are account-wide. An id the server no longer has, one
// without a blob, or one whose blob fails to download or parse is skipped, and
// the first such error is returned alongside the messages that did download.
// If ctx is cancelled before every download ran, the context error is returned
// with whatever finished.
func (a *Adapter) Fetch(ctx context.Context, _ string, remoteIDs []string) ([]psync.Fetched, error) {
	if len(remoteIDs) == 0 {
		return nil, nil
	}
	jids := make([]gojmap.ID, len(remoteIDs))
	for i, id := range remoteIDs {
		jids[i] = gojmap.ID(id)
	}
	list, err := a.getEmails(ctx, jids, []string{"id", "blobId", "keywords"})
	if err != nil {
		return nil, err
	}
	byID := make(map[string]email.Email, len(list))
	for _, em := range list {
		byID[string(em.ID)] = em
	}

	type blobJob struct {
		idx int
		em  email.Email
	}
	var jobs []blobJob
	var firstErr error
	for i, id := range remoteIDs {
		em, ok := byID[id]
		if !ok {
			if firstErr == nil {
				firstErr = fmt.Errorf("jmap: email %q not found: %w", id, psync.ErrNotOnServer)
			}
			continue
		}
		if em.BlobID == "" {
			if firstErr == nil {
				firstErr = fmt.Errorf("jmap: email %q has no blobId", id)
			}
			continue
		}
		jobs = append(jobs, blobJob{idx: i, em: em})
	}
	if len(jobs) == 0 {
		return nil, firstErr
	}

	parallel := a.blobDownloadParallel()
	if parallel <= 1 {
		var out []psync.Fetched
		for _, job := range jobs {
			fetched, err := a.fetchBlob(ctx, job.em)
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			out = append(out, fetched)
		}
		return out, firstErr
	}

	type blobResult struct {
		idx     int
		fetched psync.Fetched
		err     error
		ran     bool
	}
	results := make([]blobResult, len(jobs))
	work := make(chan int, len(jobs))
	for i := range jobs {
		work <- i
	}
	close(work)

	var wg sync.WaitGroup
	for range parallel {
		wg.Go(func() {
			for ji := range work {
				if ctx.Err() != nil {
					return
				}
				fetched, err := a.fetchBlob(ctx, jobs[ji].em)
				results[ji] = blobResult{idx: jobs[ji].idx, fetched: fetched, err: err, ran: true}
			}
		})
	}
	wg.Wait()

	var out []psync.Fetched
	skipped := false
	for _, res := range results {
		if !res.ran {
			skipped = true
			continue
		}
		if res.err != nil {
			if firstErr == nil {
				firstErr = res.err
			}
			continue
		}
		out = append(out, res.fetched)
	}
	if skipped {
		firstErr = ctx.Err()
	}
	return out, firstErr
}

func (a *Adapter) blobDownloadParallel() int {
	n := 1
	if a != nil && a.BlobDownloadParallel > 0 {
		n = a.BlobDownloadParallel
	}
	if a != nil && a.client != nil && a.client.Client != nil && a.client.Session != nil {
		if coreCap, ok := a.client.Session.Capabilities[gojmap.CoreURI].(*core.Core); ok && coreCap.MaxConcurrentRequests > 0 {
			if max := int(coreCap.MaxConcurrentRequests); n > max {
				n = max
			}
		}
	}
	if n < 1 {
		return 1
	}
	return n
}

func (a *Adapter) fetchBlob(ctx context.Context, em email.Email) (psync.Fetched, error) {
	rc, err := a.client.Download(ctx, a.accountID, em.BlobID, gojmap.DownloadOptions{
		Type: "message/rfc822",
	})
	if err != nil {
		return psync.Fetched{}, fmt.Errorf("jmap: download %s: %w", em.ID, err)
	}
	defer rc.Close()
	raw, err := io.ReadAll(rc)
	if err != nil {
		return psync.Fetched{}, fmt.Errorf("jmap: read blob %s: %w", em.ID, err)
	}
	msg, err := rfc822.Parse(raw)
	if err != nil {
		return psync.Fetched{}, fmt.Errorf("jmap: parse %s: %w", em.ID, err)
	}
	atts := make([]psync.Attachment, 0, len(msg.Attachments))
	for _, at := range msg.Attachments {
		atts = append(atts, psync.Attachment{
			Filename:    at.Filename,
			ContentType: at.ContentType,
			ContentID:   at.ContentID,
			Content:     at.Content,
		})
	}
	return psync.Fetched{
		RemoteID:            string(em.ID),
		LegacyUID:           0,
		Flags:               FlagsFromKeywords(em.Keywords),
		Raw:                 raw,
		MessageID:           msg.MessageID,
		Subject:             msg.Subject,
		From:                msg.From,
		To:                  msg.To,
		Cc:                  msg.Cc,
		Text:                msg.Text,
		HTML:                msg.HTML,
		ListUnsubscribe:     msg.ListUnsubscribe,
		ReplyTo:             msg.ReplyTo,
		CharsetGuess:        msg.CharsetGuess,
		Date:                msg.Date,
		Size:                msg.Size,
		ListUnsubscribePost: msg.ListUnsubscribePost,
		AuthResults:         msg.AuthResults,
		Attachments:         atts,
	}, nil
}

// SetFlags sets the $seen and $flagged keywords of one email to match flags.
// An email the server no longer has is not an error.
func (a *Adapter) SetFlags(ctx context.Context, _, remoteID string, flags storage.Flag) error {
	patch := gojmap.Patch{
		"keywords/$seen":    patchMember(flags.Has(storage.FlagSeen)),
		"keywords/$flagged": patchMember(flags.Has(storage.FlagFlagged)),
	}
	resp, err := a.callMutation[*email.SetResponse](ctx, &email.Set{
		Account: a.accountID,
		Update:  gojmap.Some(map[gojmap.ID]gojmap.Patch{gojmap.ID(remoteID): patch}),
	})
	if err != nil {
		return err
	}
	return setFailure(optionalMap(resp.NotUpdated), true)
}

// SetKeywords adds and removes keywords on one email. Names are lowercased, as
// JMAP keywords are case-insensitive and servers store them lowercased. An
// email the server no longer has is not an error.
func (a *Adapter) SetKeywords(ctx context.Context, remoteID string, add, remove []string) error {
	patch := gojmap.Patch{}
	for _, k := range remove {
		patch["keywords/"+pointerEscape(strings.ToLower(k))] = patchMember(false)
	}
	for _, k := range add {
		patch["keywords/"+pointerEscape(strings.ToLower(k))] = patchMember(true)
	}
	if len(patch) == 0 {
		return nil
	}
	resp, err := a.callMutation[*email.SetResponse](ctx, &email.Set{
		Account: a.accountID,
		Update:  gojmap.Some(map[gojmap.ID]gojmap.Patch{gojmap.ID(remoteID): patch}),
	})
	if err != nil {
		return err
	}
	return setFailure(optionalMap(resp.NotUpdated), true)
}

// Move moves emails from sourceMailboxID to destMailboxID, in Email/set batches
// no larger than the server's maxObjectsInSet. An email the server no longer
// has is skipped; any other rejected email fails the move.
func (a *Adapter) Move(ctx context.Context, sourceMailboxID string, remoteIDs []string, destMailboxID string) error {
	if len(remoteIDs) == 0 {
		return nil
	}
	srcKey := "mailboxIds/" + pointerEscape(sourceMailboxID)
	dstKey := "mailboxIds/" + pointerEscape(destMailboxID)
	limit := a.maxObjectsInSet()
	for start := 0; start < len(remoteIDs); start += limit {
		end := min(start+limit, len(remoteIDs))
		upd := make(map[gojmap.ID]gojmap.Patch, end-start)
		for _, id := range remoteIDs[start:end] {
			upd[gojmap.ID(id)] = gojmap.Patch{
				srcKey: nil,
				dstKey: true,
			}
		}
		resp, err := a.callMutation[*email.SetResponse](ctx, &email.Set{
			Account: a.accountID,
			Update:  gojmap.Some(upd),
		})
		if err != nil {
			return err
		}
		if err := setFailure(optionalMap(resp.NotUpdated), true); err != nil {
			return err
		}
	}
	return nil
}

// Delete destroys emails, in Email/set batches no larger than maxObjectsInSet.
// An email the server no longer has counts as destroyed.
func (a *Adapter) Delete(ctx context.Context, _ string, remoteIDs []string) error {
	if len(remoteIDs) == 0 {
		return nil
	}
	limit := a.maxObjectsInSet()
	for start := 0; start < len(remoteIDs); start += limit {
		end := min(start+limit, len(remoteIDs))
		ids := make([]gojmap.ID, end-start)
		for i, id := range remoteIDs[start:end] {
			ids[i] = gojmap.ID(id)
		}
		resp, err := a.callMutation[*email.SetResponse](ctx, &email.Set{
			Account: a.accountID,
			Destroy: gojmap.Some(ids),
		})
		if err != nil {
			return err
		}
		if err := setFailure(optionalMap(resp.NotDestroyed), true); err != nil {
			return err
		}
	}
	return nil
}

// CreateMailbox creates name under parentID (top level when empty) and returns
// the mailbox the server created.
func (a *Adapter) CreateMailbox(ctx context.Context, name, parentID string) (psync.Mailbox, error) {
	createID := gojmap.ID("c0")
	mb := &mailbox.Mailbox{Name: name}
	if parentID != "" {
		mb.ParentID = gojmap.Some(gojmap.ID(parentID))
	}
	resp, err := a.callMutation[*mailbox.SetResponse](ctx, &mailbox.Set{
		Account: a.accountID,
		Create:  gojmap.Some(map[gojmap.ID]*mailbox.Mailbox{createID: mb}),
	})
	if err != nil {
		return psync.Mailbox{}, err
	}
	created, err := gojmap.SetCreated(resp.Created, optionalMap(resp.NotCreated), createID)
	if err != nil {
		return psync.Mailbox{}, err
	}
	out := psync.Mailbox{
		RemoteID:   string(created.ID),
		Name:       created.Name,
		ParentID:   parentID,
		Selectable: true,
		Role:       mailboxRole(created.Role),
	}
	if out.RemoteID == "" {
		out.RemoteID = string(createID)
	}
	if out.Name == "" {
		out.Name = name
	}
	if pid, ok := created.ParentID.Value(); ok {
		out.ParentID = string(pid)
	}
	return out, nil
}

// RenameMailbox renames a mailbox in place.
func (a *Adapter) RenameMailbox(ctx context.Context, remoteID, name string) error {
	resp, err := a.callMutation[*mailbox.SetResponse](ctx, &mailbox.Set{
		Account: a.accountID,
		Update: gojmap.Some(map[gojmap.ID]gojmap.Patch{
			gojmap.ID(remoteID): {"name": name},
		}),
	})
	if err != nil {
		return err
	}
	return setFailure(optionalMap(resp.NotUpdated), false)
}

// DeleteMailbox destroys a mailbox on the server together with its emails, as
// IMAP DELETE does; an email also in another mailbox only leaves this one. A
// mailbox the server no longer has counts as destroyed.
func (a *Adapter) DeleteMailbox(ctx context.Context, remoteID string) error {
	resp, err := a.callMutation[*mailbox.SetResponse](ctx, &mailbox.Set{
		Account:               a.accountID,
		Destroy:               gojmap.Some([]gojmap.ID{gojmap.ID(remoteID)}),
		OnDestroyRemoveEmails: true,
	})
	if err != nil {
		return err
	}
	return setFailure(optionalMap(resp.NotDestroyed), true)
}

func (a *Adapter) queryMailboxIDs(ctx context.Context, mailboxID string) ([]string, error) {
	var ids []string
	var pos gojmap.Int
	for {
		resp, err := a.callRead[*email.QueryResponse](ctx, &email.Query{
			Account:        a.accountID,
			Position:       pos,
			CalculateTotal: true,
			Filter:         email.InMailbox(gojmap.ID(mailboxID)),
			Sort:           []*gojmap.Comparator{email.Desc(email.SortReceivedAt)},
		})
		if err != nil {
			return nil, err
		}
		if len(resp.IDs) == 0 {
			break
		}
		for _, id := range resp.IDs {
			ids = append(ids, string(id))
		}
		pos += gojmap.Int(len(resp.IDs))
		if resp.Total != nil && gojmap.UnsignedInt(len(ids)) >= *resp.Total {
			break
		}
	}
	return ids, nil
}

func (a *Adapter) emailState(ctx context.Context) (string, error) {
	_, state, err := a.getEmailsWithState(ctx, []gojmap.ID{}, []string{"id"}, nil)
	return state, err
}

func (a *Adapter) getEmails(ctx context.Context, ids []gojmap.ID, props []string) ([]email.Email, error) {
	list, _, err := a.getEmailsWithState(ctx, ids, props, nil)
	return list, err
}

// getEmailsWithState runs Email/get in maxObjectsInGet chunks and returns every
// email plus the last state. onChunk, when set, gets each chunk's list as soon
// as it is read (server order); its error stops the remaining chunks.
func (a *Adapter) getEmailsWithState(ctx context.Context, ids []gojmap.ID, props []string, onChunk func([]email.Email) error) ([]email.Email, string, error) {
	if len(ids) == 0 {
		resp, err := a.callRead[*email.GetResponse](ctx, &email.Get{
			Account:    a.accountID,
			IDs:        gojmap.Some([]gojmap.ID{}),
			Properties: gojmap.Some(props),
		})
		if err != nil {
			return nil, "", err
		}
		return resp.List, resp.State, nil
	}
	limit := a.maxObjectsInGet()
	var all []email.Email
	var state string
	err := forIDChunks(ids, limit, func(chunk []gojmap.ID) error {
		resp, err := a.callRead[*email.GetResponse](ctx, &email.Get{
			Account:    a.accountID,
			IDs:        gojmap.Some(chunk),
			Properties: gojmap.Some(props),
		})
		if err != nil {
			return err
		}
		state = resp.State
		all = append(all, resp.List...)
		if onChunk != nil && len(resp.List) > 0 {
			return onChunk(resp.List)
		}
		return nil
	})
	if err != nil {
		return nil, "", err
	}
	return all, state, nil
}

func (a *Adapter) callRead[T gojmap.MethodResponse](ctx context.Context, m gojmap.Method) (T, error) {
	a.requests.Add(1)
	var zero T
	req := &gojmap.Request{}
	id := req.Invoke(m)
	resp, err := a.client.doRead(ctx, req)
	if err != nil {
		return zero, err
	}
	return gojmap.As[T](resp, id)
}

func (a *Adapter) callMutation[T gojmap.MethodResponse](ctx context.Context, m gojmap.Method) (T, error) {
	a.requests.Add(1)
	var zero T
	req := &gojmap.Request{}
	id := req.Invoke(m)
	resp, err := a.client.doMutation(ctx, req)
	if err != nil {
		return zero, err
	}
	return gojmap.As[T](resp, id)
}

func (a *Adapter) maxObjectsInGet() int {
	return coreLimit(a.client, true)
}

func (a *Adapter) maxObjectsInSet() int {
	return coreLimit(a.client, false)
}

func coreLimit(c *Client, get bool) int {
	if c == nil || c.Client == nil || c.Session == nil {
		return defaultMaxObjects
	}
	cap, ok := c.Session.Capabilities[gojmap.CoreURI].(*core.Core)
	if !ok {
		return defaultMaxObjects
	}
	n := cap.MaxObjectsInSet
	if get {
		n = cap.MaxObjectsInGet
	}
	if n == 0 {
		return defaultMaxObjects
	}
	return int(n)
}

// forIDChunks invokes fn for successive slices of ids at most limit long.
func forIDChunks(ids []gojmap.ID, limit int, fn func([]gojmap.ID) error) error {
	if len(ids) == 0 {
		return nil
	}
	if limit <= 0 {
		limit = defaultMaxObjects
	}
	for start := 0; start < len(ids); start += limit {
		end := min(start+limit, len(ids))
		if err := fn(ids[start:end]); err != nil {
			return err
		}
	}
	return nil
}

func mailboxRole(role gojmap.Optional[mailbox.Role]) string {
	r, ok := role.Value()
	if !ok {
		return ""
	}
	switch r {
	case mailbox.RoleInbox:
		return `\Inbox`
	case mailbox.RoleSent:
		return `\Sent`
	case mailbox.RoleDrafts:
		return `\Drafts`
	case mailbox.RoleTrash:
		return `\Trash`
	case mailbox.RoleJunk:
		return `\Junk`
	case mailbox.RoleArchive:
		return `\Archive`
	default:
		return ""
	}
}

// patchMember is the patch value for a keywords/ or mailboxIds/ path: true to
// add the member, null to remove it. RFC 8621 allows only true as a value.
func patchMember(on bool) any {
	if on {
		return true
	}
	return nil
}

// setFailure returns the per-record error of the lowest id in failed, or nil.
// With skipNotFound a notFound is ignored: the record is already gone, so an
// update or destroy aimed at it has nothing left to do. A null entry still
// means the server refused the record.
func setFailure(failed map[gojmap.ID]*gojmap.SetError, skipNotFound bool) error {
	for _, id := range slices.Sorted(maps.Keys(failed)) {
		se := failed[id]
		if se == nil {
			return fmt.Errorf("jmap: %s rejected without a reason", id)
		}
		if skipNotFound && se.Type == string(gojmap.SetErrNotFound) {
			continue
		}
		return fmt.Errorf("jmap: %s rejected: %w", id, se)
	}
	return nil
}

func pointerEscape(s string) string {
	s = strings.ReplaceAll(s, "~", "~0")
	s = strings.ReplaceAll(s, "/", "~1")
	return s
}

func optionalMap[V any](o gojmap.Optional[map[gojmap.ID]V]) map[gojmap.ID]V {
	v, ok := o.Value()
	if !ok {
		return nil
	}
	return v
}
