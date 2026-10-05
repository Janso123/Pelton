package desktop

import (
	"context"
	"fmt"

	"github.com/peltonapp/Pelton/internal/rfc822"
	"github.com/peltonapp/Pelton/internal/storage"
	psync "github.com/peltonapp/Pelton/internal/sync"
)

// fakeMove records one Move call.
type fakeMove struct {
	from, to string
	ids      []string
}

// fakeKeywords records one SetKeywords call.
type fakeKeywords struct {
	remoteID string
	add      []string
	remove   []string
}

// fakeJMAPAdapter is a psync.Adapter for message action tests. It serves
// messages from raw and records moves and keyword changes; everything else
// returns zero values.
type fakeJMAPAdapter struct {
	raw      map[string][]byte
	moves    []fakeMove
	keywords []fakeKeywords
	failMove error
	// onFetch, when set, runs at the start of every Fetch.
	onFetch func()
}

func (f *fakeJMAPAdapter) Addr() string { return "fake-jmap" }

func (f *fakeJMAPAdapter) ListMailboxes(context.Context) ([]psync.Mailbox, error) {
	return nil, nil
}

func (f *fakeJMAPAdapter) ListMessages(context.Context, psync.RemoteMailbox) ([]psync.Header, string, string, error) {
	return nil, "", "", nil
}

// Fetch parses the stored raw bytes the way the real adapter does, so callers
// see real bodies. An unknown id is left out and reported as not on the server.
func (f *fakeJMAPAdapter) Fetch(_ context.Context, _ string, remoteIDs []string) ([]psync.Fetched, error) {
	if f.onFetch != nil {
		f.onFetch()
	}
	var out []psync.Fetched
	var firstErr error
	for _, id := range remoteIDs {
		raw, ok := f.raw[id]
		if !ok {
			if firstErr == nil {
				firstErr = fmt.Errorf("fake jmap: message %q: %w", id, psync.ErrNotOnServer)
			}
			continue
		}
		msg, err := rfc822.Parse(raw)
		if err != nil {
			return out, err
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
		out = append(out, psync.Fetched{
			RemoteID:    id,
			Raw:         raw,
			MessageID:   msg.MessageID,
			Subject:     msg.Subject,
			From:        msg.From,
			To:          msg.To,
			Cc:          msg.Cc,
			Text:        msg.Text,
			HTML:        msg.HTML,
			ReplyTo:     msg.ReplyTo,
			References:  msg.References,
			Date:        msg.Date,
			Size:        msg.Size,
			AuthResults: msg.AuthResults,
			Attachments: atts,
		})
	}
	return out, firstErr
}

func (f *fakeJMAPAdapter) SetFlags(context.Context, string, string, storage.Flag) error {
	return nil
}

func (f *fakeJMAPAdapter) Move(_ context.Context, source string, remoteIDs []string, dest string) error {
	if f.failMove != nil {
		return f.failMove
	}
	f.moves = append(f.moves, fakeMove{from: source, to: dest, ids: remoteIDs})
	return nil
}

func (f *fakeJMAPAdapter) Delete(context.Context, string, []string) error { return nil }

func (f *fakeJMAPAdapter) CreateMailbox(context.Context, string, string) (psync.Mailbox, error) {
	return psync.Mailbox{}, nil
}

func (f *fakeJMAPAdapter) RenameMailbox(context.Context, string, string) error { return nil }

func (f *fakeJMAPAdapter) DeleteMailbox(context.Context, string) error { return nil }

// SetKeywords records the keyword change.
func (f *fakeJMAPAdapter) SetKeywords(_ context.Context, remoteID string, add, remove []string) error {
	f.keywords = append(f.keywords, fakeKeywords{remoteID: remoteID, add: add, remove: remove})
	return nil
}
