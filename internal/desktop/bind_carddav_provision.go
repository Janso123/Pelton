package desktop

import (
	"context"

	pcarddav "github.com/peltonapp/Pelton/internal/carddav"
	"github.com/peltonapp/Pelton/internal/credentials"
	"github.com/peltonapp/Pelton/internal/storage"
)

// discoverCardDAV is the seam tests replace so provision never hits the network.
var discoverCardDAV = pcarddav.Discover

// booksToAdd returns collections that are not already stored under the same
// URL and collection path.
func booksToAdd(found []pcarddav.Book, targetURL string, existing []storage.AddressBook) []pcarddav.Book {
	have := make(map[string]bool, len(existing))
	for _, b := range existing {
		have[b.URL+"|"+b.CollectionPath] = true
	}
	out := make([]pcarddav.Book, 0, len(found))
	for _, b := range found {
		if have[targetURL+"|"+b.Path] {
			continue
		}
		out = append(out, b)
	}
	return out
}

// provisionCardDAVBooks discovers CardDAV books for a password mailbox and
// adds any that are not already stored. It logs failures and always returns nil
// so account create/switch never fails because of contacts.
func (a *App) provisionCardDAVBooks(ctx context.Context, account storage.Account, password string) error {
	if password == "" {
		return nil
	}
	// the books live with the mailbox, so they take its route and trust, as
	// syncAddressBook does for them afterwards.
	httpClient, err := a.accountMailHTTPClient(account, account.Proxy.ContactsUseGlobal, contactsTimeout)
	if err != nil {
		a.log.Warn("carddav route", "account", account.ID, "err", err)
		return nil
	}
	target, err := discoverCardDAV(ctx, httpClient, account.Email)
	if err != nil {
		a.log.Warn("carddav discover", "account", account.ID, "err", err)
		return nil
	}
	client, err := pcarddav.Connect(ctx, pcarddav.Config{
		URL: target, Username: loginName(account), Password: password, HTTP: httpClient,
	})
	if err != nil {
		a.log.Warn("carddav connect", "account", account.ID, "err", err)
		return nil
	}
	found, err := client.Books(ctx)
	if err != nil {
		a.log.Warn("carddav books", "account", account.ID, "err", err)
		return nil
	}
	existing, err := a.store.ListAddressBooks(ctx)
	if err != nil {
		a.log.Warn("list address books", "err", err)
		return nil
	}
	for _, b := range booksToAdd(found, target, existing) {
		book := storage.AddressBook{
			AccountID: account.ID, Name: b.Name, URL: target,
			CollectionPath: b.Path, Username: loginName(account),
		}
		id, err := a.store.CreateAddressBook(ctx, &book)
		if err != nil {
			a.log.Warn("create address book", "name", b.Name, "err", err)
			continue
		}
		book.ID = id
		if err := credentials.StoreAddressBookPassword(id, password); err != nil {
			_ = a.store.DeleteAddressBook(ctx, id)
			a.log.Warn("store address book password", "book", id, "err", err)
			continue
		}
		if err := a.syncAddressBook(ctx, book); err != nil {
			a.log.Warn("first contact sync", "book", id, "err", err)
		}
	}
	return nil
}
