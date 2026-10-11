import { afterEach, describe, expect, it, vi } from 'vitest'
import { render, screen } from '@testing-library/svelte'
import userEvent from '@testing-library/user-event'

vi.mock('../../stores/signatures', async (importOriginal) => ({
  ...(await importOriginal<typeof import('../../stores/signatures')>()),
  getAccountSignatures: vi.fn().mockResolvedValue({ headerId: 0, footerId: 7 }),
}))

import Compose from './Compose.svelte'
import { composeSessions, getSession, openCompose, updateCompose } from '../../stores/compose'
import { signatures } from '../../stores/signatures'
import type { EditorMode } from '../../lib/types'

let unsubscribe: (() => void) | undefined

afterEach(() => {
  unsubscribe?.()
  composeSessions.set([])
  signatures.set([])
})

function openPane(mode: EditorMode, body: string, signaturesApplied = true): number {
  const id = openCompose(1, mode)
  updateCompose(id, { body, signaturesApplied })
  const { rerender } = render(Compose, { session: getSession(id)! })
  unsubscribe = composeSessions.subscribe((sessions) => {
    const session = sessions.find((s) => s.id === id)
    if (session) void rerender({ session })
  })
  return id
}

describe('composer mode switching', () => {
  it('renders Markdown on entering Rich and restores Markdown on returning', async () => {
    const id = openPane('markdown', '**Hello**\n\n> Original message')
    await userEvent.click(screen.getByRole('tab', { name: 'Rich' }))
    await vi.waitFor(() => expect(getSession(id)!.body).toContain('<strong>Hello</strong>'))
    await userEvent.click(screen.getByRole('tab', { name: 'Markdown' }))
    expect(getSession(id)!.body).toContain('**Hello**')
    expect(getSession(id)!.body).toContain('> Original message')
    expect(getSession(id)!.body).not.toMatch(/<\/?(?:p|blockquote|strong)>/)
  })

  it('returns plain replies from Rich with line breaks, decoded entities and quote markers', async () => {
    const id = openPane('plaintext', 'Hello & welcome\nSecond line\n\n> Original\n>> Older')
    await userEvent.click(screen.getByRole('tab', { name: 'Rich' }))
    await vi.waitFor(() => expect(getSession(id)!.body).toContain('<blockquote>'))
    await userEvent.click(screen.getByRole('tab', { name: 'Plain' }))
    expect(getSession(id)!.body).toContain('Hello & welcome\nSecond line')
    expect(getSession(id)!.body).toContain('> Original')
    expect(getSession(id)!.body).toContain('> > Older')
    expect(getSession(id)!.body).not.toContain('<p>')
  })

  it('converts the current edited rich body instead of restoring an older source', async () => {
    const id = openPane('wysiwyg', '<p>Edited <strong>answer</strong></p><blockquote><p>Original<br>Second line</p></blockquote>')
    await userEvent.click(screen.getByRole('tab', { name: 'Markdown' }))
    expect(getSession(id)!.body).toContain('Edited **answer**')
    expect(getSession(id)!.body).toContain('> Original')
    expect(getSession(id)!.body).not.toContain('<blockquote>')
  })

  it('treats HTML-looking plain text as literal text when entering Rich', async () => {
    const id = openPane('plaintext', '<p>Example HTML</p>')
    await userEvent.click(screen.getByRole('tab', { name: 'Rich' }))
    expect(getSession(id)!.body).toContain('&lt;p&gt;Example HTML&lt;/p&gt;')
  })
})

describe('reply through Markdown preview', () => {
  it('keeps source lines and the sender address when returning to Plain after Preview', async () => {
    const source = 'On Sat, 10 Oct 2026, Revolut <no-reply@revolut.com> wrote:\n\n> Update your Vault information\n> Hi Jan,\n> Please follow these steps:\n> Log in to your app'
    const id = openPane('plaintext', source)
    await userEvent.click(screen.getByRole('tab', { name: 'Markdown' }))
    await userEvent.click(screen.getByRole('button', { name: 'Preview' }))
    expect(getSession(id)!.body).toBe(source)
    await userEvent.click(screen.getByRole('tab', { name: 'Plain' }))
    expect(getSession(id)!.body).toBe(source)
  })
})

describe('default signature in Rich', () => {
  it('renders a Markdown signature instead of showing its markup', async () => {
    signatures.set([{ id: 7, name: 'Work', kind: 'footer', format: 'markdown', content: '**Jan**\n[site](https://example.com)' }])
    const id = openPane('wysiwyg', '<p>Hello</p>', false)
    await vi.waitFor(() => expect(getSession(id)!.signaturesApplied).toBe(true))
    expect(getSession(id)!.body).toContain('<p>-- </p><p><strong>Jan</strong><br><a href="https://example.com">site</a></p>')
  })
})
