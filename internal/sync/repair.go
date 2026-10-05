package sync

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/peltonapp/Pelton/internal/storage"
)

// repairBatch caps how many mangled messages one folder sync refetches. The
// mark is left on the rest, so a mailbox full of them is repaired over several
// syncs rather than turning one into a full redownload.
const repairBatch = 50

// repairMangled refetches messages marked for refetch: cached before charset
// detection existed with text that is not valid utf-8, or missing inline
// pictures the parser used to drop. The text is replaced and missing inline
// parts are added: the message is the same message, so its flags, colour and
// stored attachments stay as they are.
//
// A message the server no longer has loses its mark instead, otherwise every
// sync from here on would try it again.
func (e *Engine) repairMangled(ctx context.Context, folder storage.Folder, res *FolderSyncResult) {
	broken, err := e.store.MessagesNeedingRefetch(ctx, folder.ID, repairBatch)
	if err != nil {
		e.log.Error("list messages needing refetch", "folder", folder.IMAPPath, "err", err)
		return
	}
	for _, m := range broken {
		if err := ctx.Err(); err != nil {
			return
		}
		if err := e.repairOne(ctx, folder, m); err != nil {
			e.log.Error("refetch mangled message", "folder", folder.IMAPPath, "uid", m.UID, "err", err)
			continue
		}
		res.Repaired++
		res.RepairedIDs = append(res.RepairedIDs, m.ID)
	}
}

// RepairRemoteIDs repairs the marked messages among remoteIDs now, rather than
// at the next sync of the folder: the reader has one of them open. It returns
// the storage ids it repaired and the first failure, so a caller does not tell
// the reading pane a message changed when it did not.
func (e *Engine) RepairRemoteIDs(ctx context.Context, folder storage.Folder, remoteIDs []string) ([]int64, error) {
	marked, err := e.store.MarkedForRefetch(ctx, folder.ID, remoteIDs)
	if err != nil {
		return nil, err
	}
	var (
		repaired []int64
		firstErr error
	)
	for _, m := range marked {
		if err := ctx.Err(); err != nil {
			return repaired, err
		}
		if err := e.repairOne(ctx, folder, m); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		repaired = append(repaired, m.ID)
	}
	return repaired, firstErr
}

func (e *Engine) repairOne(ctx context.Context, folder storage.Folder, m storage.MangledMessage) error {
	stored, err := e.store.GetMessage(ctx, m.ID)
	if err != nil {
		return err
	}
	remoteID := stored.RemoteID
	if remoteID == "" {
		return fmt.Errorf("sync: mangled message %d has empty remote id", m.ID)
	}
	fetched, err := e.adapter.Fetch(ctx, folder.RemoteID, []string{remoteID})
	if err != nil || len(fetched) == 0 {
		// only a message the server no longer has loses its mark; anything else
		// (offline, a timeout) is tried again on a later sync.
		if (err == nil || errors.Is(err, ErrNotOnServer)) && ctx.Err() == nil {
			if clearErr := e.store.ClearRefetchMark(ctx, m.ID); clearErr != nil {
				return errors.Join(err, clearErr)
			}
		}
		if err != nil {
			return fmt.Errorf("sync: refetch message %q: %w", remoteID, err)
		}
		return fmt.Errorf("sync: refetch message %q: %w", remoteID, ErrNotOnServer)
	}
	msg := fetched[0]
	// before the text, whose write clears the mark: a failed fill is retried.
	inline := make([]storage.IncomingAttachment, 0, len(msg.Attachments))
	for _, a := range msg.Attachments {
		if a.ContentID != "" {
			inline = append(inline, storage.IncomingAttachment{
				Filename: a.Filename, ContentType: a.ContentType, ContentID: a.ContentID,
				Content: bytes.NewReader(a.Content),
			})
		}
	}
	if _, err := e.store.AddMissingInlineAttachments(ctx, m.ID, inline); err != nil {
		return err
	}
	if err := e.store.RepairMessageText(ctx, m.ID, msg.Subject, msg.Text, msg.HTML, msg.CharsetGuess); err != nil {
		return err
	}
	return nil
}
