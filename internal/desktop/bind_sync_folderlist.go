package desktop

import (
	"context"
	"errors"
	"time"

	psync "github.com/peltonapp/Pelton/internal/sync"
)

// listFolderOnce runs list unless a list of the same folder is already in
// flight on the account. Then it only marks the folder dirty and returns nil
// at once, and the running list runs list once more when it ends, so a change
// the server reported meanwhile is still picked up. Any number of coalesced
// requests make one follow-up. Once the folder has a cursor that follow-up is
// a delta.
//
// A live request (push follow-up, manual Sync, IDLE) that finds a background
// list running returns without waiting: holding the live slot to wait for a
// background job would starve the work the slot is reserved for. The
// follow-up runs inside the holder right after its list, before it gives its
// pool slot back, so it is not queued behind other background work.
//
// A list that ends soft-paused or cancelled lets go of the folder without a
// follow-up: the scheduler requeues a soft-paused job, which lists again.
func (a *App) listFolderOnce(ctx context.Context, accountID, folderID int64, list func() error) error {
	rt := a.accountSync(accountID)
	if rt == nil {
		return list()
	}
	if !rt.claimFolderList(folderID) {
		return nil
	}
	for {
		err := list()
		if ctx.Err() != nil || errors.Is(err, psync.ErrSoftPaused) {
			rt.dropFolderList(folderID)
			return err
		}
		if !rt.releaseFolderList(folderID) {
			return err
		}
	}
}

// claimFolderList marks a folder as being listed. When it already is, it
// marks it dirty instead and returns false.
func (rt *accountSync) claimFolderList(folderID int64) bool {
	return rt.claimFolderListOr(folderID, true)
}

// folderListPoll is how often awaitFolderList looks whether the folder's
// running list has ended. Tests shorten it.
var folderListPoll = 100 * time.Millisecond

// awaitFolderList claims a folder for a list, waiting while another list of
// it runs, without marking it dirty. A full reconcile takes the folder this
// way: run beside a delta it would store the flags and cursor it read before
// the delta's newer ones.
func (rt *accountSync) awaitFolderList(ctx context.Context, folderID int64) error {
	for !rt.claimFolderListOr(folderID, false) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(folderListPoll):
		}
	}
	return nil
}

// claimFolderListOr marks a folder as being listed. When it already is, it
// returns false, marking it dirty when markDirty is set.
func (rt *accountSync) claimFolderListOr(folderID int64, markDirty bool) bool {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.listing == nil {
		rt.listing = make(map[int64]bool)
	}
	if _, busy := rt.listing[folderID]; busy {
		if markDirty {
			rt.listing[folderID] = true
		}
		return false
	}
	rt.listing[folderID] = false
	return true
}

// releaseFolderList ends a folder's list. When the folder was marked dirty
// meanwhile it keeps the claim, clears the mark and returns true: the caller
// lists once more.
func (rt *accountSync) releaseFolderList(folderID int64) (again bool) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.listing[folderID] {
		rt.listing[folderID] = false
		return true
	}
	delete(rt.listing, folderID)
	return false
}

// dropFolderList ends a folder's list and forgets a dirty mark.
func (rt *accountSync) dropFolderList(folderID int64) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	delete(rt.listing, folderID)
}
