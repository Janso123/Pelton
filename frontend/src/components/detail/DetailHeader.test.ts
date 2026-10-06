import { afterEach, describe, expect, it, vi } from 'vitest'
import { render, screen } from '@testing-library/svelte'
import userEvent from '@testing-library/user-event'
import { get } from 'svelte/store'
import type { MessageDetail } from '../../lib/types'

vi.mock('../../lib/api', async (importOriginal) => ({
  ...(await importOriginal<typeof import('../../lib/api')>()),
  senderPhotos: vi.fn().mockResolvedValue({}),
}))

import DetailHeader from './DetailHeader.svelte'
import ContextMenu from '../common/ContextMenu.svelte'
import { closeContextMenu } from '../../stores/contextmenu'
import { composeSessions } from '../../stores/compose'

function message(fields: Partial<MessageDetail>): MessageDetail {
  return {
    id: 1,
    accountId: 3,
    subject: 'Offer',
    fromName: 'Dorota Komisarek',
    fromAddress: 'd.komisarek@example.com',
    toAddresses: 'me@example.com',
    ccAddresses: '',
    date: '2026-10-05T10:00:00Z',
    smime: { status: '' },
    bodyHtmlSafe: '',
    unsubscribe: null,
    ...fields,
  } as unknown as MessageDetail
}

describe('DetailHeader addresses', () => {
  afterEach(() => {
    closeContextMenu()
    composeSessions.set([])
  })

  it('lists each recipient on its own, comma separated', () => {
    render(DetailHeader, { detail: message({ toAddresses: 'Ann <ann@example.com>, bob@example.com' }) })
    expect(screen.getByRole('button', { name: 'Ann <ann@example.com>' })).toBeTruthy()
    expect(screen.getByRole('button', { name: 'bob@example.com' })).toBeTruthy()
    expect(screen.getByText(/^to/).textContent?.replace(/\s+/g, ' ').trim()).toBe(
      'to Ann <ann@example.com>, bob@example.com',
    )
  })

  // a malformed From header lands the whole `name <email>` in fromAddress; the
  // header still shows, and copies, only the address.
  it('copies the bare sender address even from a malformed From', async () => {
    const user = userEvent.setup()
    const writeText = vi.spyOn(navigator.clipboard, 'writeText').mockResolvedValue()
    render(ContextMenu)
    render(DetailHeader, { detail: message({ fromAddress: 'Dorota Komisarek <d.komisarek@example.com>' }) })
    await user.click(screen.getByRole('button', { name: 'd.komisarek@example.com' }))
    await user.click(screen.getByRole('menuitem', { name: 'Copy address' }))
    expect(writeText).toHaveBeenCalledWith('d.komisarek@example.com')
  })

  it('opens a message to the address from the account it arrived in', async () => {
    const user = userEvent.setup()
    render(ContextMenu)
    render(DetailHeader, { detail: message({}) })
    await user.click(screen.getByRole('button', { name: 'd.komisarek@example.com' }))
    await user.click(screen.getByRole('menuitem', { name: 'Write a message' }))
    const [session] = get(composeSessions)
    expect(session.accountId).toBe(3)
    expect(session.to).toBe('Dorota Komisarek <d.komisarek@example.com>')
  })
})
