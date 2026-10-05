import { beforeEach, describe, expect, it, vi } from 'vitest'
import { render, screen, waitFor } from '@testing-library/svelte'
import userEvent from '@testing-library/user-event'
import type { Account, AddressBookEntry, Folder } from '../../lib/types'
import { ready } from '../../lib/async'
import { sidebar } from '../../stores/accounts'
import { selection, defaultSelection } from '../../stores/selection'

const api = vi.hoisted(() => ({
  searchAddresses: vi.fn(),
}))

vi.mock('../../lib/api', async (importOriginal) => ({
  ...(await importOriginal<typeof import('../../lib/api')>()),
  searchAddresses: api.searchAddresses,
}))

import SearchBar from './SearchBar.svelte'
import type { SearchFilter } from '../../stores/messages'

function entry(email: string, name: string): AddressBookEntry {
  return { email, name, useCount: 1, sentCount: 0, lastUsed: '', createdAt: '' }
}

function options(): string[] {
  return screen.queryAllByRole('option').map((o) => o.textContent?.replace(/\s+/g, ' ').trim() ?? '')
}

function setup() {
  const filters: SearchFilter[] = []
  render(SearchBar, { events: { filter: (e: CustomEvent<SearchFilter>) => filters.push(e.detail) } })
  const input = screen.getByRole('combobox')
  return { input, filters, lastFilter: () => filters[filters.length - 1] }
}

function folder(id: number, accountId: number, name: string, imapPath = name): Folder {
  return { id, accountId, name, imapPath, role: '', attributes: [] } as unknown as Folder
}

function account(id: number, email: string): Account {
  return { id, email, displayName: '', localLabel: '', useLocalLabel: false } as unknown as Account
}

describe('SearchBar suggestions', () => {
  beforeEach(() => {
    api.searchAddresses.mockReset()
    api.searchAddresses.mockResolvedValue([])
    selection.set(defaultSelection)
    sidebar.set(
      ready({
        accounts: [account(1, 'a@example.com')],
        foldersByAccount: {
          1: [folder(10, 1, 'INBOX'), folder(11, 1, 'Junk'), folder(12, 1, '2024', 'Archives/2024')],
        },
        views: [],
        pinned: [],
      }),
    )
  })

  it('lists every filter when the empty field is focused', async () => {
    const { input } = setup()
    await userEvent.click(input)
    const keys = options().map((o) => o.split(' ')[0])
    expect(keys).toEqual(['from:', 'to:', 'subject:', 'has:attachment', 'is:unread', 'in:', 'after:', 'before:'])
  })

  it('leaves out filters that are already a chip', async () => {
    const { input } = setup()
    await userEvent.type(input, 'is:unread ')
    const keys = options().map((o) => o.split(' ')[0])
    expect(keys).not.toContain('is:unread')
    expect(keys).toContain('from:')
  })

  it('narrows the filters to what is being typed', async () => {
    const { input } = setup()
    await userEvent.type(input, 'ha')
    expect(options().map((o) => o.split(' ')[0])).toEqual(['has:attachment'])
  })

  it('turns a picked value suggestion into a chip', async () => {
    const { input, lastFilter } = setup()
    await userEvent.type(input, 'has:a')
    await userEvent.keyboard('{Enter}')
    await waitFor(() => expect(lastFilter()?.hasAttachment).toBe(true))
    expect((input as HTMLInputElement).value).toBe('')
  })

  it('suggests addresses after from:', async () => {
    api.searchAddresses.mockResolvedValue([entry('jan@example.com', 'Jan Kowalski'), entry('janina@example.com', '')])
    const { input } = setup()
    await userEvent.type(input, 'from:jan')
    await waitFor(() => expect(options()).toHaveLength(2))
    expect(api.searchAddresses).toHaveBeenLastCalledWith('jan', 6)
    expect(options()[0]).toContain('jan@example.com')
    expect(options()[0]).toContain('Jan Kowalski')
  })

  it('moves the highlight with the arrow keys and commits it with Enter', async () => {
    api.searchAddresses.mockResolvedValue([entry('jan@example.com', ''), entry('janina@example.com', '')])
    const { input, lastFilter } = setup()
    await userEvent.type(input, 'to:jan')
    await waitFor(() => expect(options()).toHaveLength(2))
    expect(screen.getAllByRole('option')[0]).toHaveAttribute('aria-selected', 'true')
    await userEvent.keyboard('{ArrowDown}')
    expect(screen.getAllByRole('option')[1]).toHaveAttribute('aria-selected', 'true')
    expect(input).toHaveAttribute('aria-activedescendant', screen.getAllByRole('option')[1].id)
    await userEvent.keyboard('{Enter}')
    await waitFor(() => expect(lastFilter()?.to).toBe('janina@example.com'))
    expect(options()).not.toContain('janina@example.com')
  })

  it('completes the highlighted keyword with Tab', async () => {
    const { input } = setup()
    await userEvent.click(input)
    await userEvent.keyboard('{ArrowDown}{ArrowDown}{Tab}')
    expect((input as HTMLInputElement).value).toBe('to:')
  })

  it('does not take Enter from free text when nothing is highlighted', async () => {
    const { input } = setup()
    await userEvent.type(input, 'invoice {Enter}')
    expect((input as HTMLInputElement).value).toBe('invoice ')
  })

  it('searches a word that starts like a filter on Enter', async () => {
    const { input } = setup()
    await userEvent.type(input, 'invoice to{Enter}')
    expect((input as HTMLInputElement).value).toBe('invoice to')
  })

  it('completes the first filter with Tab while typing its name', async () => {
    const { input } = setup()
    await userEvent.type(input, 'fr{Tab}')
    expect((input as HTMLInputElement).value).toBe('from:')
  })

  it('closes the list with Escape', async () => {
    const { input } = setup()
    await userEvent.click(input)
    expect(options().length).toBeGreaterThan(0)
    await userEvent.keyboard('{Escape}')
    expect(options()).toHaveLength(0)
  })

  it('offers the date picker after after:', async () => {
    const { input } = setup()
    await userEvent.type(input, 'after:')
    expect(options()).toHaveLength(1)
    await userEvent.keyboard('{Enter}')
    expect((input as HTMLInputElement).value).toBe('')
    expect(screen.getByRole('menu')).toBeInTheDocument()
    // the filter list would otherwise open over the calendar.
    expect(options()).toHaveLength(0)
  })

  it('reopens the list when the input is clicked after Escape', async () => {
    const { input } = setup()
    await userEvent.click(input)
    await userEvent.keyboard('{Escape}')
    expect(options()).toHaveLength(0)
    await userEvent.click(input)
    expect(options().length).toBeGreaterThan(0)
  })

  it('does not let a hovered filter take Enter from free text', async () => {
    const { input } = setup()
    await userEvent.type(input, 'invoice to')
    await userEvent.hover(screen.getByRole('option', { name: /^to:/ }))
    await userEvent.type(input, '{Enter}', { skipClick: true })
    expect((input as HTMLInputElement).value).toBe('invoice to')
  })

  it('offers every folder and the folders after in:', async () => {
    const { input } = setup()
    await userEvent.type(input, 'in:')
    const keys = options().map((o) => o.split(' ')[0])
    expect(keys).toEqual(['in:all', 'INBOX', 'Junk', '2024'])
    // a nested folder names its path, so "2024" says where it is.
    expect(options()[3]).toContain('Archives/2024')
  })

  it('narrows the folders to the typed name', async () => {
    const { input } = setup()
    await userEvent.type(input, 'folder:ju')
    expect(options().map((o) => o.split(' ')[0])).toEqual(['Junk'])
  })

  it('scopes the search to a picked folder', async () => {
    const { input, lastFilter } = setup()
    await userEvent.type(input, 'in:ju')
    await userEvent.keyboard('{Enter}')
    await waitFor(() => expect(lastFilter()?.folder).toBe(11))
    expect(screen.getByText('In: Junk')).toBeInTheDocument()
  })

  it('widens the search to every folder with in:all', async () => {
    const { input, lastFilter } = setup()
    await userEvent.type(input, 'in:all ')
    await waitFor(() => expect(lastFilter()?.folder).toBe('all'))
    expect(screen.getByText('All folders')).toBeInTheDocument()
  })

  it('leaves the scope to the list being shown without an in: chip', async () => {
    const { input, lastFilter } = setup()
    await userEvent.type(input, 'is:unread ')
    await waitFor(() => expect(lastFilter()?.unreadOnly).toBe(true))
    expect(lastFilter()?.folder).toBeNull()
  })

  it('names the list being searched in the placeholder', () => {
    selection.set({ kind: 'folder', folderId: 10, accountId: 1, label: 'Inbox' })
    const { input } = setup()
    expect(input).toHaveAttribute('placeholder', 'Search Inbox')
  })

  // a folder without attributes reaches the ui with attributes: null. Reading
  // that as an array threw and froze the bar, so a chip could not be removed.
  it('still removes a chip when a folder has no attributes', async () => {
    const errors: unknown[] = []
    const onError = (e: ErrorEvent) => errors.push(e.error)
    window.addEventListener('error', onError)
    sidebar.set(
      ready({
        accounts: [account(1, 'a@example.com')],
        foldersByAccount: { 1: [{ ...folder(10, 1, 'Notes'), attributes: null } as unknown as Folder] },
        views: [],
        pinned: [],
      }),
    )
    const { input, lastFilter } = setup()
    await userEvent.type(input, 'is:unread ')
    await waitFor(() => expect(lastFilter()?.unreadOnly).toBe(true))
    await userEvent.click(screen.getByRole('button', { name: 'Remove filter' }))
    await waitFor(() => expect(lastFilter()?.unreadOnly).toBe(false))
    expect(screen.queryByText('Unread')).toBeNull()
    window.removeEventListener('error', onError)
    expect(errors).toEqual([])
  })
})
