import { beforeEach, describe, expect, it, vi } from 'vitest'
import { render, screen, waitFor } from '@testing-library/svelte'
import userEvent from '@testing-library/user-event'
import { get } from 'svelte/store'
import type { MessageList as MessageListData, MessageSummary } from '../../lib/types'

const api = vi.hoisted(() => ({
  listViewMessages: vi.fn(),
  listFolderMessages: vi.fn(),
  listSavedViewMessages: vi.fn(),
  setSetting: vi.fn(() => Promise.resolve()),
  search: vi.fn(),
}))

vi.mock('../../lib/api', async (importOriginal) => ({
  ...(await importOriginal<typeof import('../../lib/api')>()),
  listViewMessages: api.listViewMessages,
  listFolderMessages: api.listFolderMessages,
  listSavedViewMessages: api.listSavedViewMessages,
  setSetting: api.setSetting,
  search: api.search,
}))

import MessageList from './MessageList.svelte'
import { messageList, reloadList, refreshListHead } from '../../stores/messages'
import { openMessageId, searchQuery, selection, selectFolder, selectView, selectSavedView } from '../../stores/selection'
import { clearSelection } from '../../stores/listselect'
import { prefs } from '../../stores/prefs'
import { idle } from '../../lib/async'

vi.stubGlobal(
  'ResizeObserver',
  class ResizeObserver {
    observe(): void {}
    unobserve(): void {}
    disconnect(): void {}
  },
)

function summary(id: number, subject: string): MessageSummary {
  return {
    id,
    accountId: 1,
    folderId: 1,
    accountEmail: 'me@example.com',
    folderName: 'INBOX',
    subject,
    fromName: 'Ada',
    fromAddress: 'ada@example.com',
    snippet: '',
    date: '2026-09-21T12:00:00Z',
    seen: true,
    flagged: false,
    hasAttachments: false,
    pgp: '',
    auth: '',
    flagColor: 0,
    offline: false,
    snoozeUntil: '',
    senderVip: false,
    smime: { status: '', signer: '', email: '', issuer: '', detail: '' },
  }
}

function page(messages: MessageSummary[]): MessageListData {
  return { messages, total: messages.length, hasOlder: false }
}

beforeEach(() => {
  api.listViewMessages.mockReset()
  api.listFolderMessages.mockReset()
  api.listSavedViewMessages.mockReset()
  api.search.mockReset()
  messageList.set(idle())
  selection.set({ kind: 'view', view: 'inbox', label: 'Unified Inbox' })
  searchQuery.set('')
  openMessageId.set(null)
  clearSelection()
  prefs.update((p) => ({ ...p, rowShowAvatar: false }))
})

describe('MessageList search selection', () => {
  it('drops a stale row highlight when clearing search changes the list', async () => {
    const normal = summary(1, 'Normal message')
    const result = summary(42, 'Search result')
    let finishReload!: (value: MessageListData) => void
    const reload = new Promise<MessageListData>((resolve) => {
      finishReload = resolve
    })

    api.listViewMessages.mockResolvedValueOnce(page([normal])).mockImplementation(() => reload)
    api.search.mockResolvedValue({ messages: [result], total: 1 })

    render(MessageList)
    await screen.findByText('Normal message')

    const user = userEvent.setup()
    await user.type(screen.getByRole('combobox'), 'needle')
    await waitFor(() => expect(api.search).toHaveBeenCalled())
    const resultRow = (await screen.findByText('Search result')).closest('[role="option"]')
    expect(resultRow).not.toBeNull()
    await user.click(resultRow!)
    expect(get(openMessageId)).toBe(42)
    expect(resultRow).toHaveAttribute('aria-selected', 'true')

    await user.click(screen.getByRole('button', { name: 'Clear search' }))
    finishReload(page([normal]))

    await waitFor(() => {
      const normalRow = screen.getByText('Normal message').closest('[role="option"]')
      expect(normalRow).toHaveAttribute('aria-selected', 'false')
    })
    // Clearing a filter should not also discard the message being read; it only
    // stops a different row at the old numeric index from looking selected.
    expect(get(openMessageId)).toBe(42)
  })
})

describe('MessageList search over a folder', () => {
  const folder = { id: 7, accountId: 1, name: 'Work' } as Parameters<typeof selectFolder>[0]

  async function searchInFolder(): Promise<ReturnType<typeof userEvent.setup>> {
    api.listFolderMessages.mockResolvedValue(page([summary(1, 'Folder message')]))
    api.search.mockResolvedValue({ messages: [summary(42, 'Search result')], total: 1 })
    selectFolder(folder)
    render(MessageList)
    await screen.findByText('Folder message')
    const user = userEvent.setup()
    await user.type(screen.getByRole('combobox'), 'needle')
    await screen.findByText('Search result')
    return user
  }

  it('reloads the folder list when the same folder is selected again', async () => {
    await searchInFolder()
    const loads = api.listFolderMessages.mock.calls.length
    selectFolder(folder)
    await screen.findByText('Folder message')
    expect(api.listFolderMessages.mock.calls.length).toBe(loads + 1)
    expect(get(messageList).data?.searching).toBe(false)
  })

  it('keeps the search across reloadList and refreshListHead', async () => {
    await searchInFolder()
    const loads = api.listFolderMessages.mock.calls.length
    await reloadList(get(selection))
    await refreshListHead(get(selection))
    expect(api.listFolderMessages.mock.calls.length).toBe(loads)
    expect(get(messageList).data?.searching).toBe(true)
  })

  it('clearing the search box returns to the folder list', async () => {
    const user = await searchInFolder()
    const loads = api.listFolderMessages.mock.calls.length
    await user.click(screen.getByRole('button', { name: 'Clear search' }))
    await screen.findByText('Folder message')
    expect(api.listFolderMessages.mock.calls.length).toBeGreaterThan(loads)
    expect(get(messageList).data?.searching).toBe(false)
  })

  it('re-clicking the folder resets a chip-only search', async () => {
    api.listFolderMessages.mockResolvedValue(page([summary(1, 'Folder message')]))
    api.search.mockResolvedValue({ messages: [summary(42, 'Search result')], total: 1 })
    selectFolder(folder)
    render(MessageList)
    await screen.findByText('Folder message')
    const user = userEvent.setup()
    await user.type(screen.getByRole('combobox'), 'is:unread{Enter}')
    await screen.findByText('Search result')
    const loads = api.listFolderMessages.mock.calls.length
    selectFolder(folder)
    await screen.findByText('Folder message')
    expect(api.listFolderMessages.mock.calls.length).toBe(loads + 1)
    expect(get(messageList).data?.searching).toBe(false)
  })

  it('switching folder while searching loads exactly once', async () => {
    await searchInFolder()
    const loads = api.listFolderMessages.mock.calls.length
    selectFolder({ id: 8, accountId: 1, name: 'Other' } as typeof folder)
    await screen.findByText('Folder message')
    expect(api.listFolderMessages.mock.calls.length).toBe(loads + 1)
    expect(get(messageList).data?.searching).toBe(false)
  })

  it('re-selecting the same view ends a search', async () => {
    api.listViewMessages.mockResolvedValue(page([summary(1, 'Normal message')]))
    api.search.mockResolvedValue({ messages: [summary(42, 'Search result')], total: 1 })
    render(MessageList)
    await screen.findByText('Normal message')
    const user = userEvent.setup()
    await user.type(screen.getByRole('combobox'), 'needle')
    await screen.findByText('Search result')
    const loads = api.listViewMessages.mock.calls.length
    selectView('inbox', 'Unified Inbox')
    await screen.findByText('Normal message')
    expect(api.listViewMessages.mock.calls.length).toBe(loads + 1)
    expect(get(messageList).data?.searching).toBe(false)
  })

  it('re-selecting a view under a different label ends a search', async () => {
    api.listViewMessages.mockResolvedValue(page([summary(1, 'Normal message')]))
    api.search.mockResolvedValue({ messages: [summary(42, 'Search result')], total: 1 })
    render(MessageList)
    await screen.findByText('Normal message')
    const user = userEvent.setup()
    await user.type(screen.getByRole('combobox'), 'needle')
    await screen.findByText('Search result')
    const loads = api.listViewMessages.mock.calls.length
    selectView('inbox', 'Inbox')
    await screen.findByText('Normal message')
    expect(api.listViewMessages.mock.calls.length).toBe(loads + 1)
    expect(get(messageList).data?.searching).toBe(false)
  })

  it('re-selecting a renamed saved view ends a search', async () => {
    api.listSavedViewMessages.mockResolvedValue(page([summary(1, 'Normal message')]))
    api.search.mockResolvedValue({ messages: [summary(42, 'Search result')], total: 1 })
    selectSavedView(5, 'Old name')
    render(MessageList)
    await screen.findByText('Normal message')
    const user = userEvent.setup()
    await user.type(screen.getByRole('combobox'), 'needle')
    await screen.findByText('Search result')
    selectSavedView(5, 'New name')
    await screen.findByText('Normal message')
    expect(get(messageList).data?.searching).toBe(false)
  })

  it('a language relabel keeps the list, the filter and the search bar chips', async () => {
    api.listViewMessages.mockResolvedValue(page([summary(1, 'Normal message')]))
    api.search.mockResolvedValue({ messages: [summary(42, 'Search result')], total: 1 })
    render(MessageList)
    await screen.findByText('Normal message')
    const user = userEvent.setup()
    await user.type(screen.getByRole('combobox'), 'is:unread{Enter}')
    await screen.findByText('Search result')
    const searches = api.search.mock.calls.length
    selection.set({ kind: 'view', view: 'inbox', label: 'Posteingang' })
    await new Promise((r) => setTimeout(r, 50))
    expect(get(messageList).data?.searching).toBe(true)
    expect(api.search.mock.calls.length).toBe(searches)
    expect(screen.getByRole('button', { name: 'Clear search' })).toBeTruthy()
    expect(screen.getByText(/unread/i)).toBeTruthy()
  })
})
