package desktop

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/peltonapp/Pelton/internal/desktop/syncsched"
	pjmap "github.com/peltonapp/Pelton/internal/jmap"
	"github.com/peltonapp/Pelton/internal/outbox"
	"github.com/peltonapp/Pelton/internal/storage"
	psync "github.com/peltonapp/Pelton/internal/sync"
)

var _ mailProtocol = jmapProtocol{}

func (p jmapProtocol) manualSync(ctx context.Context, account storage.Account) error {
	return p.a.runJMAPManualSync(ctx, account)
}

func (p jmapProtocol) backfillStep(ctx context.Context, account storage.Account, folder storage.Folder, kind syncsched.JobKind, batch, bodyLimit int, stats *imapStepStats) ([]string, error) {
	return p.a.execJMAPDeltaStep(ctx, account, folder, kind, batch, bodyLimit, stats)
}

func (p jmapProtocol) onDemandBodies(ctx context.Context, account storage.Account, folder storage.Folder) error {
	return p.a.execJMAPOnDemandBodies(ctx, account, folder)
}

func (p jmapProtocol) reconcileBodies(ctx context.Context, account storage.Account, folder storage.Folder) error {
	return p.a.execJMAPBodies(ctx, account, folder, true)
}

// planDownload does not need the account: the JMAP plan reads the store.
func (p jmapProtocol) planDownload(ctx context.Context, _ storage.Account, folders []storage.Folder, since time.Time) ([]dlTask, []int64, error) {
	return p.a.planJMAPAccount(ctx, folders, since)
}

func (p jmapProtocol) backfillBodyLimit(ctx context.Context, folder storage.Folder, kind syncsched.JobKind, batch int) (int, error) {
	return p.a.jmapBackfillBodyLimit(ctx, folder, kind, batch)
}

// firstSync starts the watch with the first sync. The socket is outside the
// HTTP pool; push follow-up is a live job that checks a slot out.
// The first pass is the full stub list plus the body campaign.
// Later manual Sync is a live incremental reconcile, not this pass.
func (p jmapProtocol) firstSync(ctx context.Context, account storage.Account) {
	a := p.a
	goSafe("watching for new mail", func() { a.idleLoop(ctx, account) })
	err := a.syncJMAPInitial(ctx, account)
	if err != nil {
		if errors.Is(err, errNoCredentials) {
			a.log.Warn("mailbox has no password, not syncing", "account", account.Email)
		} else if ctx.Err() == nil {
			a.log.Error("account sync", "account", account.Email, "err", err)
		}
	}
	if ctx.Err() == nil && !errors.Is(err, errNoCredentials) {
		a.enqueueDueReconcile(ctx, account)
	}
}

func (p jmapProtocol) watch(ctx context.Context, account storage.Account) {
	a := p.a
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

// needsTimedSync leaves out an account whose WebSocket is healthy.
func (p jmapProtocol) needsTimedSync(accountID int64) bool {
	return !p.a.jmapPushHealthy(accountID)
}

// lockPerFolder: JMAP still locks per folder so ReleaseLock can yield between
// body batches.
func (p jmapProtocol) lockPerFolder() bool { return true }

// limitNeedsBodies: JMAP stores stubs for the full list and clears the floor, so
// the question is whether the new window still lacks bodies.
func (p jmapProtocol) limitNeedsBodies(ctx context.Context, f storage.Folder, newLimit int) bool {
	need, err := p.a.store.RemoteIDsNeedingBodyNewest(ctx, f.ID, newLimit)
	if err != nil {
		p.a.log.Error("bodies missing for limit delta", "folder", f.ID, "err", err)
		return false
	}
	return len(need) != 0 || newLimit == 0
}

func (p jmapProtocol) withListEngine(ctx context.Context, account storage.Account, fn func(*psync.Engine) error) error {
	client, err := p.a.jmapClient(ctx, &account)
	if err != nil {
		return err
	}
	return fn(p.a.jmapStubEngine(pjmap.NewAdapter(client, account.JMAPMailAccountID), account.ID))
}

func (p jmapProtocol) transmit(ctx context.Context, account storage.Account, m outbox.Message) error {
	return (&accountTransmitter{app: p.a}).transmitJMAP(ctx, account, m)
}

func (p jmapProtocol) moveMessage(m *storage.Message, source, dest storage.Folder, account storage.Account) (ArchiveUndoDTO, error) {
	return p.a.moveMessageJMAP(m, source, dest, account)
}

// moveBack uses the remote id: a JMAP email keeps its id across moves.
func (p jmapProtocol) moveBack(rfcMessageID, remoteID string, from, dest storage.Folder, account storage.Account) error {
	a := p.a
	if remoteID == "" {
		return fmt.Errorf("pelton: this message cannot be moved back")
	}
	if err := a.withAccountAdapter(account.ID, func(ad psync.Adapter) error {
		return ad.Move(a.ctx, from.RemoteID, []string{remoteID}, dest.RemoteID)
	}); err != nil {
		return err
	}
	a.emit(EventMailNew, MailNewEvent{AccountID: dest.AccountID, FolderID: dest.ID, Count: 1})
	return nil
}

func (p jmapProtocol) source(m storage.Message, folder storage.Folder, account storage.Account) (string, error) {
	a := p.a
	var raw []byte
	err := a.withAccountAdapter(account.ID, func(ad psync.Adapter) error {
		var ferr error
		raw, ferr = rawFromAdapter(a.ctx, ad, folder, m.RemoteID)
		return ferr
	})
	if err != nil {
		return "", offlineOrErr(err)
	}
	return string(raw), nil
}

func (p jmapProtocol) setColor(m storage.Message, folder storage.Folder, account storage.Account, color int) {
	a := p.a
	add, remove := colorKeywordChange(color)
	err := a.withAccountAdapter(account.ID, func(ad psync.Adapter) error {
		ks, ok := ad.(keywordSetter)
		if !ok {
			return fmt.Errorf("pelton: this account cannot store colours on the server")
		}
		return ks.SetKeywords(a.ctx, m.RemoteID, add, remove)
	})
	if err != nil {
		a.log.Error("color sync", "id", m.ID, "err", err)
	}
}

// withAdapter keeps the jmapAdapterForTest hook.
func (p jmapProtocol) withAdapter(account *storage.Account, fn func(psync.Adapter) error) error {
	a := p.a
	if a.jmapAdapterForTest != nil {
		return fn(a.jmapAdapterForTest(*account))
	}
	client, err := a.jmapClient(a.ctx, account)
	if err != nil {
		return err
	}
	return fn(pjmap.NewAdapter(client, account.JMAPMailAccountID))
}

// download takes the account lock per batch: a JMAP download can run for
// minutes, and a session is cheap to reopen.
func (p jmapProtocol) download(ctx context.Context, account storage.Account, groups map[int64]*folderTasks, includeAttachments bool, done *int, total int, start time.Time) error {
	a := p.a
	for _, ft := range groups {
		for chunk := range slices.Chunk(ft.remoteIDs, downloadBatch) {
			if err := ctx.Err(); err != nil {
				return err
			}
			if held, _ := a.syncBlock(account.ID); held {
				return errAccountSyncHeld
			}
			err := a.withAccountAdapter(account.ID, func(ad psync.Adapter) error {
				// a switch that finished between batches already replaced
				// these ids; the protocol only changes under this lock.
				if cur, err := a.store.GetAccount(a.ctx, account.ID); err != nil || cur.Protocol != "jmap" {
					return errAccountSyncHeld
				}
				a.fetchDownloadBatch(ctx, ad, account, ft, chunk, includeAttachments, done, total, start)
				return nil
			})
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// checkPassword is CheckAccountPassword for a JMAP mailbox: it fetches the
// session with the typed password, which is the sign-in a JMAP sync does. A
// mailbox with no session yet (restored from a backup) looks it up instead, as
// its first sync would, and stores nothing it finds.
func (p jmapProtocol) checkPassword(account storage.Account, password string) PasswordCheckDTO {
	return p.a.checkJMAPPassword(account, password)
}

// routeServers is the JMAP server alone, the only one the mailbox uses.
func (p jmapProtocol) routeServers(account storage.Account, _ []routeServer) ([]routeServer, error) {
	host, port, err := jmapServer(account)
	if err != nil {
		return nil, err
	}
	return []routeServer{{"JMAP", host, port}}, nil
}

func (p jmapProtocol) discoverFolders(ctx context.Context, account storage.Account) error {
	return p.a.discoverFoldersJMAP(ctx, account)
}

// folderPath is the path of a new folder under parent. JMAP: an empty
// delimiter still allows a parent via RemoteID.
func (p jmapProtocol) folderPath(parent storage.Folder, name string) (string, error) {
	if parent.Delimiter != "" {
		return parent.IMAPPath + parent.Delimiter + name, nil
	}
	return name, nil
}

// createdPath is the remote id: JMAP stores it as the path.
func (p jmapProtocol) createdPath(_, remoteID string) string { return remoteID }

// renameKeepsPath: when imap_path equals remote_id the adapter keeps the same
// id, so the path is not rewritten to a hierarchical name.
func (p jmapProtocol) renameKeepsPath(folder storage.Folder, remoteID string) bool {
	return folder.IMAPPath == remoteID
}
