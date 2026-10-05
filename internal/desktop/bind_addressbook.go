package desktop

import (
	"slices"

	"github.com/peltonapp/Pelton/internal/storage"
)

// address learning levels, from least to most learned. Each one offers
// everything the one before it does.
const (
	// learnOff learns nothing: autocomplete offers only synced contacts.
	learnOff = "off"
	// learnSent learns the people the user writes to, from sends and from
	// the cached Sent folders.
	learnSent = "sent"
	// learnTrusted adds the senders the user trusts with remote images and
	// the VIPs.
	learnTrusted = "trusted"
	// learnAll adds every other sender except mailing lists and automated
	// mailboxes.
	learnAll = "all"
)

// AddressBookEntryDTO is one autocomplete/contact entry for the frontend.
type AddressBookEntryDTO struct {
	Email     string `json:"email"`
	Name      string `json:"name"`
	UseCount  int    `json:"useCount"`
	SentCount int    `json:"sentCount"`
	LastUsed  string `json:"lastUsed"`
	CreatedAt string `json:"createdAt"`
	// Contact is true for an entry from a synced address book rather than one
	// harvested from mail, so the ui can mark which is which.
	Contact bool `json:"contact"`
}

// SearchAddresses returns compose-autocomplete candidates matching query. The
// real address book comes first, then the addresses learned from mail that the
// learning level admits, people written to most first. Where the
// two hold the same address the contact's name wins: that is the one the user
// maintains, rather than whatever a sender once put in a From header (#168).
func (a *App) SearchAddresses(query string, limit int) ([]AddressBookEntryDTO, error) {
	if err := a.ready(); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 8
	}
	contacts, err := a.store.SearchContacts(a.ctx, query, limit)
	if err != nil {
		return nil, err
	}
	out := make([]AddressBookEntryDTO, 0, limit)
	seen := make(map[string]bool, limit)
	for _, c := range contacts {
		if seen[c.Email] {
			continue
		}
		seen[c.Email] = true
		out = append(out, AddressBookEntryDTO{Email: c.Email, Name: c.Name, Contact: true})
	}
	filter, learning := a.learnedFilter()
	if len(out) >= limit || !learning {
		return out[:min(len(out), limit)], nil
	}

	entries, err := a.store.SearchAddresses(a.ctx, query, limit, filter)
	if err != nil {
		return nil, err
	}
	for _, e := range toAddressDTOs(entries) {
		if seen[e.Email] || len(out) >= limit {
			continue
		}
		seen[e.Email] = true
		out = append(out, e)
	}
	return out, nil
}

// ListAddresses returns the whole harvested address book for the settings
// manager, so the user can review and remove entries.
func (a *App) ListAddresses() ([]AddressBookEntryDTO, error) {
	if err := a.ready(); err != nil {
		return nil, err
	}
	entries, err := a.store.ListAddresses(a.ctx)
	if err != nil {
		return nil, err
	}
	return toAddressDTOs(entries), nil
}

// DeleteAddress removes one contact from the address book.
func (a *App) DeleteAddress(email string) error {
	if err := a.ready(); err != nil {
		return err
	}
	return a.store.DeleteAddress(a.ctx, email)
}

// addressLearning returns the learning level. Unset, it follows the on/off
// switch it replaced: off stays off, anything else starts at sent. An unknown
// value reads as sent too.
func (a *App) addressLearning() string {
	switch level := a.stringSetting(settingAddressLearning, ""); level {
	case learnOff, learnSent, learnTrusted, learnAll:
		return level
	case "":
		if !a.boolSetting(settingHarvestAddressesLegacy, true) {
			return learnOff
		}
	}
	return learnSent
}

// learnedFilter is which received-only addresses the learning level admits,
// and false when the level is off and the learned book is not used at all.
func (a *App) learnedFilter() (storage.AddressFilter, bool) {
	var filter storage.AddressFilter
	switch a.addressLearning() {
	case learnOff:
		return filter, false
	case learnSent:
		return filter, true
	case learnAll:
		filter.Received = true
	}
	filter.Trusted = a.remoteSenders()
	for _, vip := range a.vipSenders() {
		if vip = bareAddress(vip); !slices.Contains(filter.Trusted, vip) {
			filter.Trusted = append(filter.Trusted, vip)
		}
	}
	return filter, true
}

// harvestAddressBook learns from cached mail what the learning level admits:
// the recipients in Sent folders, then the senders the level lets in. It runs
// in the background at startup, after syncs and on a level change, so
// autocomplete keeps learning.
func (a *App) harvestAddressBook() {
	if a.store == nil {
		return
	}
	filter, learning := a.learnedFilter()
	if !learning {
		return
	}
	sent, err := a.sentFolderIDs()
	if err != nil {
		a.log.Error("list sent folders for address book", "err", err)
		return
	}
	if err := a.store.BackfillSentCounts(a.ctx, sent); err != nil {
		a.log.Error("count sent addresses", "err", err)
	}
	if !filter.Received && len(filter.Trusted) == 0 {
		return
	}
	if err := a.store.HarvestSenders(a.ctx, filter); err != nil {
		a.log.Error("harvest address book", "err", err)
	}
}

// sentFolderIDs returns the folders of every account classified as Sent.
func (a *App) sentFolderIDs() ([]int64, error) {
	accounts, err := a.store.ListAccounts(a.ctx)
	if err != nil {
		return nil, err
	}
	var ids []int64
	for _, account := range accounts {
		folders, err := a.store.ListFolders(a.ctx, account.ID)
		if err != nil {
			return nil, err
		}
		for _, f := range folders {
			if folderRole(f) == roleSent {
				ids = append(ids, f.ID)
			}
		}
	}
	return ids, nil
}

func toAddressDTOs(entries []storage.AddressBookEntry) []AddressBookEntryDTO {
	out := make([]AddressBookEntryDTO, 0, len(entries))
	for _, e := range entries {
		out = append(out, AddressBookEntryDTO{
			Email:     e.Email,
			Name:      e.Name,
			UseCount:  e.UseCount,
			SentCount: e.SentCount,
			LastUsed:  e.LastUsed,
			CreatedAt: e.CreatedAt,
		})
	}
	return out
}
