import { beforeEach, describe, expect, it, vi } from 'vitest'
import { get } from 'svelte/store'
import { render, screen } from '@testing-library/svelte'
import userEvent from '@testing-library/user-event'

const api = vi.hoisted(() => ({
  listAccounts: vi.fn(),
  setSetting: vi.fn(),
}))

vi.mock('../../lib/api', async (importOriginal) => ({
  ...(await importOriginal<typeof import('../../lib/api')>()),
  ...api,
}))

vi.mock('../../stores/contacts', async () => {
  const { writable } = await import('svelte/store')
  return { addressBooks: writable([]), loadContacts: vi.fn(), refreshContacts: vi.fn() }
})

import ContactsSection from './ContactsSection.svelte'
import { prefs } from '../../stores/prefs'

beforeEach(() => {
  api.listAccounts.mockReset().mockResolvedValue([])
  api.setSetting.mockReset().mockResolvedValue(undefined)
  prefs.update((p) => ({ ...p, addressLearning: 'sent' }))
})

describe('address learning level', () => {
  it('shows the stored level and saves a new one', async () => {
    render(ContactsSection)

    const level = await screen.findByRole('combobox', { name: 'Learn addresses from mail' })
    expect(level).toHaveTextContent('People I write to')

    await userEvent.click(level)
    await userEvent.click(screen.getByRole('option', { name: 'All senders except mailing lists' }))

    expect(api.setSetting).toHaveBeenCalledWith('address_learning', 'all')
    expect(get(prefs).addressLearning).toBe('all')
  })

  it('offers off, sent, trusted and all', async () => {
    render(ContactsSection)

    await userEvent.click(await screen.findByRole('combobox', { name: 'Learn addresses from mail' }))

    expect(screen.getAllByRole('option').map((o) => o.textContent?.trim())).toEqual([
      'Off',
      'People I write to',
      'Also trusted senders and VIPs',
      'All senders except mailing lists',
    ])
  })
})
