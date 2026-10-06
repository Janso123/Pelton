import { describe, expect, it, vi } from 'vitest'
import { render, screen } from '@testing-library/svelte'
import OutboxPanel from './OutboxPanel.svelte'
import { outbox } from '../../stores/outbox'
import type { OutboxRow } from '../../lib/types'

vi.mock('../../lib/api', async (importOriginal) => ({
  ...(await importOriginal<typeof import('../../lib/api')>()),
  cancelSend: vi.fn(),
  retrySend: vi.fn(),
  discardFailedSend: vi.fn(),
  listOutbox: vi.fn().mockResolvedValue([]),
  clearSentOutbox: vi.fn(),
}))

function failedRow(lastError: string): OutboxRow {
  return {
    id: 1,
    accountId: 1,
    recipients: ['ann@example.com'],
    state: 'failed',
    attempts: 1,
    lastError,
    nextAttemptAt: '',
    createdAt: '',
  }
}

describe('OutboxPanel failure text', () => {
  it('explains a send that may have gone out instead of showing the raw cause', () => {
    outbox.set([failedRow('maybe-sent')])
    render(OutboxPanel)

    expect(screen.getByText(/so it may have been delivered/)).toBeTruthy()
    expect(screen.queryByText('maybe-sent')).toBeNull()
  })

  it('shows the server error for any other failure', () => {
    outbox.set([failedRow('550 mailbox unavailable')])
    render(OutboxPanel)

    expect(screen.getByText('550 mailbox unavailable')).toBeTruthy()
  })
})
