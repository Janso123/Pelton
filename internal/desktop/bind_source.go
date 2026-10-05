package desktop

import (
	"context"
	"fmt"

	"github.com/emersion/go-imap/v2"

	"github.com/peltonapp/Pelton/internal/storage"
	psync "github.com/peltonapp/Pelton/internal/sync"
)

// rawFromAdapter fetches one message's raw source through a sync adapter the
// caller already holds.
func rawFromAdapter(ctx context.Context, ad psync.Adapter, folder storage.Folder, remoteID string) ([]byte, error) {
	got, err := ad.Fetch(ctx, folder.RemoteID, []string{remoteID})
	if err != nil {
		return nil, err
	}
	if len(got) == 0 {
		return nil, fmt.Errorf("pelton: message %q: %w", remoteID, psync.ErrNotOnServer)
	}
	return got[0].Raw, nil
}

// GetMessageSource returns a message's raw RFC 822 source (all headers plus
// body, exactly as fetched). The messages table only caches the parsed
// plain/html bodies, so the raw bytes are fetched on demand over the account's
// own protocol (IMAP or JMAP); nothing is stored.
func (a *App) GetMessageSource(id int64) (string, error) {
	if err := a.ready(); err != nil {
		return "", err
	}
	m, err := a.store.GetMessage(a.ctx, id)
	if err != nil {
		return "", err
	}
	folder, err := a.store.GetFolder(a.ctx, m.FolderID)
	if err != nil {
		return "", err
	}
	account, err := a.store.GetAccount(a.ctx, m.AccountID)
	if err != nil {
		return "", err
	}
	if account.Protocol == "jmap" {
		var raw []byte
		err := a.withAccountAdapter(account.ID, func(ad psync.Adapter) error {
			var ferr error
			raw, ferr = rawFromAdapter(a.ctx, ad, *folder, m.RemoteID)
			return ferr
		})
		if err != nil {
			return "", offlineOrErr(err)
		}
		return string(raw), nil
	}
	cfg, err := a.resolveIMAP(*account)
	if err != nil {
		return "", err
	}

	accountMu := a.accountLock(account.ID)
	accountMu.Lock()
	defer accountMu.Unlock()

	client, err := a.connectIMAP(cfg)
	if err != nil {
		return "", offlineOrErr(err)
	}
	defer client.Close()
	if err := client.Login(); err != nil {
		return "", offlineOrErr(err)
	}
	defer client.Logout()
	if _, err := client.Select(folder.IMAPPath); err != nil {
		return "", offlineOrErr(err)
	}

	raw, err := client.FetchRawMessage(imap.UID(m.UID))
	if err != nil {
		return "", offlineOrErr(err)
	}
	return string(raw), nil
}
