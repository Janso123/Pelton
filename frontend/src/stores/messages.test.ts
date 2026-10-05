import { beforeEach, describe, expect, it, vi } from 'vitest'
import { get } from 'svelte/store'
import { idle, ready } from '../lib/async'
import type { MessageSummary } from '../lib/types'

// the sidebar refresh is stubbed so the assertions are about which changes ask
// for one, not about the request it would make.
vi.mock('./sidebarcounts', () => ({ refreshCountsSoon: vi.fn() }))

const api = vi.hoisted(() => ({
  listFolderMessages: vi.fn(),
  search: vi.fn(),
}))

vi.mock('../lib/api', async (importOriginal) => ({
  ...(await importOriginal<typeof import('../lib/api')>()),
  listFolderMessages: api.listFolderMessages,
  search: api.search,
}))

import {
  messageList,
  neighbourInList,
  patchInList,
  refreshListHead,
  removeFromList,
  restoreToList,
  loadList,
  isSearching,
  reloadList,
  rerunSearch,
  runSearch,
  searchScope,
  emptyFilter,
  PAGE_SIZE,
} from './messages'
import { refreshCountsSoon } from './sidebarcounts'

const refreshed = vi.mocked(refreshCountsSoon)

function summary(id: number, over: Partial<MessageSummary> = {}): MessageSummary {
  return {
    id,
    accountId: 1,
    folderId: 1,
    accountEmail: 'me@example.com',
    folderName: 'INBOX',
    subject: `message ${id}`,
    fromName: 'Ada',
    fromAddress: 'ada@example.com',
    snippet: '',
    // descending by id, matching the newest-first order the list is kept in.
    date: `2026-09-${String(30 - id).padStart(2, '0')}T12:00:00Z`,
    seen: false,
    flagged: false,
    hasAttachments: false,
    pgp: '',
    auth: '',
    flagColor: 0,
    offline: false,
    snoozeUntil: '',
    senderVip: false,
    smime: { status: '', signer: '', email: '', issuer: '', detail: '' },
    ...over,
  }
}

function load(ids: number[], total = ids.length): void {
  messageList.set(
    ready({
      items: ids.map((id) => summary(id)),
      total,
      searching: false,
      hasOlder: false,
      backfilling: false,
    }),
  )
}

function ids(): number[] {
  return get(messageList).data?.items.map((m) => m.id) ?? []
}

beforeEach(() => {
  api.listFolderMessages.mockReset()
  api.search.mockReset()
  messageList.set(idle())
  refreshed.mockClear()
})

describe('refreshListHead', () => {
  it('leaves a paged list alone when body sync announces existing rows', async () => {
    const firstPage = Array.from({ length: PAGE_SIZE }, (_, i) => summary(i + 1))
    const secondPage = [summary(51), summary(52)]
    api.listFolderMessages.mockResolvedValue({
      messages: firstPage,
      total: 52,
      hasOlder: false,
    })

    await loadList({ kind: 'folder', folderId: 1, accountId: 1, label: 'INBOX' })
    messageList.set(
      ready({
        items: [...firstPage, ...secondPage],
        total: 52,
        searching: false,
        hasOlder: false,
        backfilling: false,
      }),
    )

    await refreshListHead({ kind: 'folder', folderId: 1, accountId: 1, label: 'INBOX' })

    expect(get(messageList).data!.items).toHaveLength(52)
    expect(get(messageList).data!.items[0].id).toBe(1)
    expect(get(messageList).data!.items.at(-1)?.id).toBe(52)
    expect(api.listFolderMessages).toHaveBeenCalledTimes(2)
  })

  it('prepends stubs that arrived during body fill', async () => {
    const existing = [summary(2), summary(3)]
    api.listFolderMessages.mockResolvedValue({
      messages: [summary(1), ...existing],
      total: 3,
      hasOlder: false,
    })
    await loadList({ kind: 'folder', folderId: 1, accountId: 1, label: 'INBOX' })
    messageList.set(
      ready({
        items: existing,
        total: 2,
        searching: false,
        hasOlder: false,
        backfilling: false,
      }),
    )

    await refreshListHead({ kind: 'folder', folderId: 1, accountId: 1, label: 'INBOX' })

    expect(get(messageList).data!.items.map((m) => m.id)).toEqual([1, 2, 3])
    expect(get(messageList).data!.total).toBe(3)
  })
})

// #403: the sidebar used to wait for the next sync, so a folder kept its unread
// count and its bold highlight long after the mail had been dealt with.
describe('sidebar counts follow local changes', () => {
  it('refreshes when a message leaves the list', () => {
    load([1, 2, 3])
    removeFromList(2)
    expect(refreshed).toHaveBeenCalledTimes(1)
  })

  // deleting from the reading pane can name a message off the loaded page. The
  // row is not there to remove, but the folder's count still changed.
  it('refreshes even when the row was not loaded', () => {
    load([1, 2])
    removeFromList(99)
    expect(refreshed).toHaveBeenCalledTimes(1)
  })

  it('refreshes when an undone delete puts a message back', () => {
    load([1, 3])
    restoreToList(summary(2))
    expect(refreshed).toHaveBeenCalledTimes(1)
  })

  it('refreshes on the two flags the sidebar counts', () => {
    load([1, 2])
    patchInList(1, { seen: true })
    patchInList(2, { flagged: true })
    expect(refreshed).toHaveBeenCalledTimes(2)
  })

  // a color label and an offline copy change nothing the sidebar shows, so
  // re-reading the whole tree for them would be work for no visible result.
  it('stays put for changes the sidebar does not show', () => {
    load([1, 2])
    patchInList(1, { flagColor: 3 })
    patchInList(2, { offline: true })
    patchInList(1, { snoozeUntil: '2026-10-01T09:00:00Z' })
    expect(refreshed).not.toHaveBeenCalled()
  })

  // marking an already-read message read is still a statement about read state;
  // the sidebar is asked either way rather than this guessing at the outcome.
  it('refreshes on a seen patch that changes nothing', () => {
    load([1])
    patchInList(1, { seen: false })
    expect(refreshed).toHaveBeenCalledTimes(1)
  })
})

describe('removeFromList', () => {
  it('drops the row and decrements the total', () => {
    load([1, 2, 3])
    removeFromList(2)
    expect(ids()).toEqual([1, 3])
    expect(get(messageList).data!.total).toBe(2)
  })

  // deleting from the reading pane can name a message that is not on the
  // loaded page; decrementing then would drift the total away from what the
  // server actually holds.
  it('leaves the total alone for a row that was not loaded', () => {
    load([1, 2, 3], 50)
    removeFromList(99)
    expect(ids()).toEqual([1, 2, 3])
    expect(get(messageList).data!.total).toBe(50)
  })

  it('never drives the total below zero', () => {
    load([1], 0)
    removeFromList(1)
    expect(get(messageList).data!.total).toBe(0)
  })

  it('does nothing while the list is not loaded', () => {
    removeFromList(1)
    expect(get(messageList).status).toBe('idle')
  })
})

describe('restoreToList', () => {
  it('puts a row back in date order', () => {
    load([1, 2, 4])
    restoreToList(summary(3))
    expect(ids()).toEqual([1, 2, 3, 4])
    expect(get(messageList).data!.total).toBe(4)
  })

  it('restores to the top when it is the newest', () => {
    load([2, 3])
    restoreToList(summary(1))
    expect(ids()).toEqual([1, 2, 3])
  })

  // undo can be pressed after a sync already brought the message back, and a
  // second copy would break the list, which renders rows keyed by id.
  it('refuses to add a row that is already there', () => {
    load([1, 2])
    restoreToList(summary(2))
    expect(ids()).toEqual([1, 2])
    expect(get(messageList).data!.total).toBe(2)
  })

  it('does nothing while the list is not loaded', () => {
    restoreToList(summary(1))
    expect(get(messageList).status).toBe('idle')
  })
})

describe('patchInList', () => {
  it('updates only the named row', () => {
    load([1, 2])
    patchInList(2, { seen: true })
    expect(get(messageList).data!.items.map((m) => m.seen)).toEqual([false, true])
  })

  it('leaves the other fields alone', () => {
    load([1])
    patchInList(1, { flagged: true })
    const row = get(messageList).data!.items[0]
    expect(row.flagged).toBe(true)
    expect(row.subject).toBe('message 1')
    expect(row.seen).toBe(false)
  })

  it('ignores an id that is not loaded', () => {
    load([1])
    patchInList(99, { seen: true })
    expect(get(messageList).data!.items[0].seen).toBe(false)
  })

  it('does nothing while the list is not loaded', () => {
    patchInList(1, { seen: true })
    expect(get(messageList).status).toBe('idle')
  })
})

describe('neighbourInList', () => {
  it('picks the row below', () => {
    load([1, 2, 3])
    expect(neighbourInList(2)).toBe(3)
  })

  it('falls back to the row above for the last one', () => {
    load([1, 2, 3])
    expect(neighbourInList(3)).toBe(2)
  })

  it('has nothing to pick in a list of one', () => {
    load([1])
    expect(neighbourInList(1)).toBeNull()
  })

  // a bulk delete would otherwise land on a row that is about to go as well.
  it('skips rows that are going too', () => {
    load([1, 2, 3, 4])
    expect(neighbourInList(1, new Set([2, 3]))).toBe(4)
  })

  it('searches upwards past skipped rows', () => {
    load([1, 2, 3, 4])
    expect(neighbourInList(3, new Set([4, 2]))).toBe(1)
  })

  it('has nothing to pick when the whole list is going', () => {
    load([1, 2, 3])
    expect(neighbourInList(2, new Set([1, 2, 3]))).toBeNull()
  })

  it('returns null for a row that is not loaded', () => {
    load([1, 2])
    expect(neighbourInList(99)).toBeNull()
  })

  it('returns null while the list is not loaded', () => {
    expect(neighbourInList(1)).toBeNull()
  })
})

describe('search state', () => {
  it('is searching after a search and not after the folder list loads', async () => {
    api.search.mockResolvedValue({ messages: [summary(1)], total: 1 })
    api.listFolderMessages.mockResolvedValue({ messages: [summary(2)], total: 1, hasOlder: false })
    expect(isSearching()).toBe(false)
    await runSearch('invoice')
    expect(isSearching()).toBe(true)
    await loadList({ kind: 'folder', folderId: 1, accountId: 1, label: 'INBOX' })
    expect(isSearching()).toBe(false)
  })

  it('rerunSearch reads the current query again and keeps the result set', async () => {
    api.search.mockResolvedValue({ messages: [summary(1)], total: 1 })
    await runSearch('invoice')
    api.search.mockResolvedValue({ messages: [summary(3), summary(1)], total: 2 })
    await rerunSearch()
    expect(api.search).toHaveBeenLastCalledWith(expect.objectContaining({ query: 'invoice' }))
    expect(ids()).toEqual([3, 1])
    expect(get(messageList).data?.searching).toBe(true)
    expect(api.listFolderMessages).not.toHaveBeenCalled()
  })

  it('rerunSearch does nothing when no search is active', async () => {
    api.listFolderMessages.mockResolvedValue({ messages: [summary(2)], total: 1, hasOlder: false })
    await loadList({ kind: 'folder', folderId: 1, accountId: 1, label: 'INBOX' })
    await rerunSearch()
    expect(api.search).not.toHaveBeenCalled()
  })
})

describe('reloadList', () => {
  const sel = { kind: 'folder', folderId: 1, accountId: 1, label: 'INBOX' } as const

  // the active search is module state that outlives a test, so leave it first.
  beforeEach(async () => {
    api.listFolderMessages.mockResolvedValue({ messages: [], total: 0, hasOlder: false })
    await loadList(sel)
    api.listFolderMessages.mockClear()
  })

  it('re-runs the search instead of loading the folder while searching', async () => {
    api.search.mockResolvedValue({ messages: [summary(1)], total: 1 })
    await runSearch('invoice')
    await reloadList(sel)
    expect(api.search).toHaveBeenCalledTimes(2)
    expect(api.listFolderMessages).not.toHaveBeenCalled()
    expect(get(messageList).data?.searching).toBe(true)
  })

  it('loads the selection when no search is active', async () => {
    api.listFolderMessages.mockResolvedValue({ messages: [summary(2)], total: 1, hasOlder: false })
    await reloadList(sel)
    expect(api.listFolderMessages).toHaveBeenCalled()
    expect(api.search).not.toHaveBeenCalled()
  })
})

describe('search scope', () => {
  const inbox = { kind: 'folder', folderId: 7, accountId: 1, label: 'INBOX' } as const

  it('stays in the folder the search was started from', () => {
    expect(searchScope(null, inbox)).toEqual({ folderId: 7, view: '' })
  })

  it('stays in the unified view the search was started from', () => {
    expect(searchScope(null, { kind: 'view', view: 'inbox', label: 'Unified Inbox' })).toEqual({ folderId: 0, view: 'inbox' })
  })

  it('covers every folder from a saved view or with in:all', () => {
    expect(searchScope(null, { kind: 'savedView', viewId: 3, label: 'Bills' })).toEqual({ folderId: 0, view: '' })
    expect(searchScope('all', inbox)).toEqual({ folderId: 0, view: '' })
  })

  it('uses a folder picked with in: wherever the search started', () => {
    expect(searchScope(12, inbox)).toEqual({ folderId: 12, view: '' })
  })

  it('sends the scope of the list on screen with the search', async () => {
    api.listFolderMessages.mockResolvedValue({ messages: [], total: 0, hasOlder: false })
    api.search.mockResolvedValue({ messages: [], total: 0 })
    await loadList(inbox)
    await runSearch('invoice', { ...emptyFilter, unreadOnly: true })
    expect(api.search).toHaveBeenLastCalledWith(expect.objectContaining({ folderId: 7, view: '', unreadOnly: true }))
  })
})
