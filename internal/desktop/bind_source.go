package desktop

import (
	"context"
	"fmt"

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
	return a.protocolFor(*account).source(*m, *folder, *account)
}
