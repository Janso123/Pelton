package jmap

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/mail"
	"slices"
	"strings"

	gojmap "github.com/Janso123/go-jmap"
	"github.com/Janso123/go-jmap/mail/email"
	"github.com/Janso123/go-jmap/mail/emailsubmission"
	"github.com/Janso123/go-jmap/mail/identity"
)

var (
	// ErrNoSentMailbox is returned by Submit when no Sent mailbox id is given.
	ErrNoSentMailbox = errors.New("account has no Sent mailbox")
	// ErrNoMatchingIdentity is returned by Submit when none of the account's
	// JMAP identities matches the sender address.
	ErrNoMatchingIdentity = errors.New("account has no matching JMAP identity")
	// ErrAmbiguousIdentity is returned by Submit when more than one of the
	// account's JMAP identities matches the sender address.
	ErrAmbiguousIdentity = errors.New("account has multiple matching JMAP identities")
)

const importCreateID = "email"
const submissionCreateID = "sub"
const seenKeyword = "$seen"

// reconcileScan is how much of the newest mail in Sent is searched for an
// earlier attempt's copy when the server cannot filter by header and the size
// lookup finds nothing or is unsupported. It is the last resort: a retry soon
// after the lost response finds the copy among the newest, a retry after a
// long gap may not.
const reconcileScan = 50

// sizeLookupPage is the limit asked for each page of the size lookup.
const sizeLookupPage = 256

// sizeLookupCap bounds the size lookup's candidates. Past it the lookup fails
// rather than guess, and the outbox retries later.
const sizeLookupCap = 2048

// Submit uploads raw RFC822 mail, imports it into Sent, and creates an
// EmailSubmission in the same JMAP request.
//
// Sent is checked first, by the mail's Message-ID: an earlier attempt's
// request may have been carried out without its response arriving, and the
// outbox then sends the message again. On a server that cannot filter by
// header, the mail in Sent of the same size is searched for it instead, then
// the newest mail in Sent when that finds nothing or the server cannot filter
// by size either. A copy in Sent
// that has a submission, or the $seen keyword, means the mail already went
// out, so nothing is sent.
// The submission alone is not enough: a server may destroy EmailSubmission
// objects once they are delivered, so a successful submission also marks the
// imported copy $seen, which outlives it. A copy with neither is removed
// before sending, so Sent does not end up with two.
func Submit(ctx context.Context, client *Client, mailAccountID, sentMailboxID string, raw []byte, accountEmail, from string, recipients []string) error {
	if sentMailboxID == "" {
		return ErrNoSentMailbox
	}
	if client == nil {
		return fmt.Errorf("jmap: nil client")
	}

	identityID, err := selectIdentity(ctx, client, mailAccountID, accountEmail)
	if err != nil {
		return err
	}

	account := gojmap.ID(mailAccountID)
	sent, err := reconcileSent(ctx, client, account, gojmap.ID(sentMailboxID), raw)
	if err != nil {
		return err
	}
	if sent {
		return nil
	}

	if err := client.ensureFresh(ctx); err != nil {
		return err
	}
	uploaded, err := client.Upload(ctx, account, bytes.NewReader(raw), "message/rfc822")
	if err != nil {
		return err
	}

	req := &gojmap.Request{}
	importCall := req.Invoke(&email.Import{
		Account: account,
		Emails: map[string]*email.EmailImport{
			importCreateID: {
				BlobID:     uploaded.ID,
				MailboxIDs: map[gojmap.ID]bool{gojmap.ID(sentMailboxID): true},
			},
		},
	})

	rcptTo := make([]*emailsubmission.Address, 0, len(recipients))
	for _, r := range recipients {
		rcptTo = append(rcptTo, &emailsubmission.Address{Email: r})
	}
	subCall := req.Invoke(&emailsubmission.Set{
		Set: gojmap.Set[emailsubmission.EmailSubmission]{
			Account: account,
			Create: gojmap.Some(map[gojmap.ID]*emailsubmission.EmailSubmission{
				submissionCreateID: {
					IdentityID: identityID,
					EmailID:    gojmap.CreationRef(importCreateID),
					Envelope: gojmap.Some(emailsubmission.Envelope{
						MailFrom: &emailsubmission.Address{Email: from},
						RcptTo:   rcptTo,
					}),
				},
			}),
		},
		OnSuccessUpdateEmail: gojmap.Some(map[gojmap.ID]gojmap.Patch{
			gojmap.CreationRef(submissionCreateID): {"keywords/" + seenKeyword: true},
		}),
	})

	resp, err := client.doMutation(ctx, req)
	if err != nil {
		return err
	}

	importResp, err := gojmap.As[*email.ImportResponse](resp, importCall)
	if err != nil {
		return err
	}
	var importedID gojmap.ID
	if importResp != nil {
		if se := importResp.NotCreated[importCreateID]; se != nil {
			return se
		}
		if created := importResp.Created[importCreateID]; created != nil {
			importedID = created.ID
		}
	}

	subResp, subErr := gojmap.As[*emailsubmission.SetResponse](resp, subCall)
	if subErr == nil {
		_, subErr = gojmap.SetCreated(subResp.Created, optionalMap(subResp.NotCreated), submissionCreateID)
	}
	if subErr == nil {
		return nil
	}

	if importedID != "" {
		_ = destroyEmail(ctx, client, account, importedID)
	}
	return subErr
}

// reconcileSent looks for an earlier attempt's copy of raw in Sent by its
// Message-ID. It reports true when that copy was submitted (it has a
// submission or $seen, see Submit), and destroys a copy that was not. Mail
// with no Message-ID cannot be matched and is sent again. A server that cannot
// filter by header gets the mail in Sent of raw's size searched instead, and
// the newest mail in Sent when no copy of that size matches or the server
// cannot filter by size either, since giving up there would send a retried
// mail twice.
func reconcileSent(ctx context.Context, client *Client, account, sentID gojmap.ID, raw []byte) (bool, error) {
	msgID := messageIDOf(raw)
	if msgID == "" {
		return false, nil
	}

	ids, err := sentCopiesByHeader(ctx, client, account, sentID, msgID)
	if unsupportedFilter(err) {
		ids, err = sentCopiesBySize(ctx, client, account, sentID, msgID, len(raw))
		// A server that rewrites the message on import stores a copy of
		// another size, which the newest mail still holds after a quick retry.
		if unsupportedFilter(err) || (err == nil && len(ids) == 0) {
			ids, err = scanSentCopies(ctx, client, account, sentID, msgID)
		}
	}
	if err != nil {
		return false, err
	}
	if len(ids) == 0 {
		return false, nil
	}

	req := &gojmap.Request{}
	getCall := req.Invoke(&email.Get{
		Account:    account,
		IDs:        gojmap.Some(ids),
		Properties: gojmap.Some([]string{"keywords"}),
	})
	subCall := req.Invoke(&emailsubmission.Query{
		Query:  gojmap.Query[emailsubmission.EmailSubmission]{Account: account},
		Filter: &emailsubmission.FilterCondition{EmailIDs: ids},
	})
	resp, err := client.doRead(ctx, req)
	if err != nil {
		return false, err
	}
	copies, err := gojmap.As[*email.GetResponse](resp, getCall)
	if err != nil {
		return false, err
	}
	for _, c := range copies.List {
		if c.Keywords[seenKeyword] {
			return true, nil
		}
	}
	subs, err := gojmap.As[*emailsubmission.QueryResponse](resp, subCall)
	if err != nil {
		return false, err
	}
	if len(subs.IDs) > 0 {
		return true, nil
	}

	for _, id := range ids {
		if err := destroyEmail(ctx, client, account, id); err != nil {
			return false, err
		}
	}
	return false, nil
}

// sentCopiesByHeader returns the ids of the mail in Sent whose Message-ID
// header is msgID.
func sentCopiesByHeader(ctx context.Context, client *Client, account, sentID gojmap.ID, msgID string) ([]gojmap.ID, error) {
	req := &gojmap.Request{}
	callID := req.Invoke(&email.Query{
		Query:  gojmap.Query[email.Email]{Account: account},
		Filter: &email.FilterCondition{InMailbox: sentID, Header: []string{"Message-ID", msgID}},
	})
	resp, err := client.doRead(ctx, req)
	if err != nil {
		return nil, err
	}
	found, err := gojmap.As[*email.QueryResponse](resp, callID)
	if err != nil {
		return nil, err
	}
	return found.IDs, nil
}

func unsupportedFilter(err error) bool {
	var me *gojmap.MethodError
	return errors.As(err, &me) && me.Type == string(gojmap.MethodErrUnsupportedFilter)
}

// sentCopiesBySize returns the ids of the mail in Sent of size bytes whose
// messageId is msgID. An imported copy is the uploaded blob byte for byte, so
// its size is the raw message's, which narrows Sent to a handful of
// candidates whatever their age.
func sentCopiesBySize(ctx context.Context, client *Client, account, sentID gojmap.ID, msgID string, size int) ([]gojmap.ID, error) {
	limit := gojmap.UnsignedInt(sizeLookupPage)
	filter := &email.FilterCondition{
		InMailbox: sentID,
		MinSize:   gojmap.Uint64Ptr(uint64(size)),
		// maxSize is exclusive (RFC 8621 4.4.1).
		MaxSize: gojmap.Uint64Ptr(uint64(size) + 1),
	}
	var candidates []gojmap.ID
	for {
		req := &gojmap.Request{}
		queryCall := req.Invoke(&email.Query{
			Query:  gojmap.Query[email.Email]{Account: account, Position: gojmap.Int(len(candidates)), Limit: &limit},
			Filter: filter,
		})
		resp, err := client.doRead(ctx, req)
		if err != nil {
			return nil, err
		}
		page, err := gojmap.As[*email.QueryResponse](resp, queryCall)
		if err != nil {
			return nil, err
		}
		// A server may return fewer ids than asked for, so only an empty page
		// ends the results.
		if len(page.IDs) == 0 {
			break
		}
		candidates = append(candidates, page.IDs...)
		if len(candidates) > sizeLookupCap {
			return nil, fmt.Errorf("jmap: more than %d mails in Sent of %d bytes", sizeLookupCap, size)
		}
	}
	return withMessageID(ctx, client, account, candidates, msgID)
}

// scanSentCopies returns the ids of the newest reconcileScan mails in Sent
// whose messageId is msgID.
func scanSentCopies(ctx context.Context, client *Client, account, sentID gojmap.ID, msgID string) ([]gojmap.ID, error) {
	limit := gojmap.UnsignedInt(reconcileScan)
	req := &gojmap.Request{}
	queryCall := req.Invoke(&email.Query{
		Query:  gojmap.Query[email.Email]{Account: account, Limit: &limit},
		Filter: email.InMailbox(sentID),
		Sort:   []*gojmap.Comparator{email.Desc(email.SortReceivedAt)},
	})
	resp, err := client.doRead(ctx, req)
	if err != nil {
		return nil, err
	}
	newest, err := gojmap.As[*email.QueryResponse](resp, queryCall)
	if err != nil {
		return nil, err
	}
	return withMessageID(ctx, client, account, newest.IDs, msgID)
}

// withMessageID returns the ids among candidates whose messageId is msgID.
func withMessageID(ctx context.Context, client *Client, account gojmap.ID, candidates []gojmap.ID, msgID string) ([]gojmap.ID, error) {
	var ids []gojmap.ID
	err := forIDChunks(candidates, coreLimit(client, true), func(chunk []gojmap.ID) error {
		req := &gojmap.Request{}
		getCall := req.Invoke(&email.Get{
			Account:    account,
			IDs:        gojmap.Some(chunk),
			Properties: gojmap.Some([]string{"messageId"}),
		})
		resp, err := client.doRead(ctx, req)
		if err != nil {
			return err
		}
		got, err := gojmap.As[*email.GetResponse](resp, getCall)
		if err != nil {
			return err
		}
		for _, e := range got.List {
			if mids, _ := e.MessageID.Value(); slices.Contains(mids, msgID) {
				ids = append(ids, e.ID)
			}
		}
		return nil
	})
	return ids, err
}

// messageIDOf returns the Message-ID of raw without its angle brackets, or "".
func messageIDOf(raw []byte) string {
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return ""
	}
	return strings.Trim(strings.TrimSpace(msg.Header.Get("Message-ID")), "<>")
}

func selectIdentity(ctx context.Context, client *Client, mailAccountID, accountEmail string) (gojmap.ID, error) {
	req := &gojmap.Request{}
	callID := req.Invoke(&identity.Get{
		Account: gojmap.ID(mailAccountID),
	})
	resp, err := client.doRead(ctx, req)
	if err != nil {
		return "", err
	}
	got, err := gojmap.As[*identity.GetResponse](resp, callID)
	if err != nil {
		return "", err
	}

	want := normalizeAddr(accountEmail)
	var matches []gojmap.ID
	for _, idn := range got.List {
		if normalizeAddr(idn.Email) == want {
			matches = append(matches, idn.ID)
		}
	}
	switch len(matches) {
	case 0:
		return "", ErrNoMatchingIdentity
	case 1:
		return matches[0], nil
	default:
		return "", ErrAmbiguousIdentity
	}
}

func destroyEmail(ctx context.Context, client *Client, account, emailID gojmap.ID) error {
	req := &gojmap.Request{}
	callID := req.Invoke(&email.Set{
		Account: account,
		Destroy: gojmap.Some([]gojmap.ID{emailID}),
	})
	resp, err := client.doMutation(ctx, req)
	if err != nil {
		return err
	}
	_, err = gojmap.As[*email.SetResponse](resp, callID)
	return err
}

func normalizeAddr(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	addr, err := mail.ParseAddress(s)
	if err != nil {
		return strings.ToLower(s)
	}
	return strings.ToLower(addr.Address)
}
