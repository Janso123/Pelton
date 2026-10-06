import { beforeEach, describe, expect, it, vi } from 'vitest'
import { get } from 'svelte/store'

vi.mock('../lib/api', () => ({ getSetting: vi.fn(async () => ({ value: '', found: false })), setSetting: vi.fn() }))

import { composeSessions, openReply } from './compose'
import type { MessageDetail } from '../lib/types'

function detail(overrides: Partial<MessageDetail>): MessageDetail {
  return {
    accountId: 1,
    accountEmail: 'me@example.com',
    fromName: 'Ann',
    fromAddress: 'ann@example.com',
    toAddresses: 'me@example.com, bob@example.com',
    ccAddresses: 'carol@example.com',
    replyTo: '',
    messageIdHeader: '<m1@example.com>',
    references: ['<r0@example.com>'],
    subject: 'Plans',
    date: '2026-10-01',
    bodyQuote: 'hello',
    ...overrides,
  } as MessageDetail
}

function last() {
  const list = get(composeSessions)
  return list[list.length - 1]
}

describe('openReply', () => {
  beforeEach(() => composeSessions.set([]))

  it('keeps a quoted name with a comma intact on reply all', () => {
    openReply(detail({ toAddresses: '"Doe, John" <j@x>, me@example.com' }), 'plaintext', true)
    expect(last().to).toContain('"Doe, John" <j@x>')
    expect(last().to.split('<j@x>')).toHaveLength(2)
  })

  it('keeps a name with a comma intact on reply all when the row stored it unquoted', () => {
    openReply(detail({ toAddresses: 'Doe, John <j@x>, me@example.com', ccAddresses: '' }), 'plaintext', true)
    expect(last().to).toBe('ann@example.com, "Doe, John" <j@x>')
  })

  it('replies to Reply-To when the message has one', () => {
    openReply(detail({ replyTo: 'list@example.com' }), 'plaintext', false)
    expect(last().to).toBe('list@example.com')
  })

  it('replies to From otherwise', () => {
    openReply(detail({}), 'plaintext', false)
    expect(last().to).toBe('ann@example.com')
    expect(last().cc).toBe('')
  })

  it('reply all keeps the original To and Cc without the user', () => {
    openReply(detail({}), 'plaintext', true)
    expect(last().to).toBe('ann@example.com, bob@example.com')
    expect(last().cc).toBe('carol@example.com')
    expect(last().showCc).toBe(true)
  })

  it('drops own address case-insensitively and removes duplicates', () => {
    openReply(
      detail({ toAddresses: 'Me <ME@Example.com>, ann@example.com', ccAddresses: 'me@example.com, Bob <bob@example.com>, bob@example.com' }),
      'plaintext',
      true,
    )
    expect(last().to).toBe('ann@example.com')
    expect(last().cc).toBe('Bob <bob@example.com>')
  })

  it('replying to own message goes to the original To', () => {
    const own = { fromAddress: 'ME@example.com', toAddresses: 'bob@example.com, me@example.com', ccAddresses: 'carol@example.com' }
    openReply(detail(own), 'plaintext', false)
    expect(last().to).toBe('bob@example.com')
    expect(last().cc).toBe('')
    openReply(detail(own), 'plaintext', true)
    expect(last().to).toBe('bob@example.com')
    expect(last().cc).toBe('carol@example.com')
  })

  it('threads the reply onto the original', () => {
    openReply(detail({}), 'plaintext', false)
    expect(last().inReplyTo).toBe('<m1@example.com>')
    expect(last().references).toEqual(['<r0@example.com>', '<m1@example.com>'])
  })

  it('does not repeat the original id already in its references', () => {
    openReply(detail({ references: ['<r0@example.com>', '<m1@example.com>'] }), 'plaintext', false)
    expect(last().references).toEqual(['<r0@example.com>', '<m1@example.com>'])
  })
})

describe('reply quote', () => {
  beforeEach(() => composeSessions.set([]))

  it('quotes as html in the rich editor, so its lines do not run together', () => {
    openReply(detail({ bodyQuote: 'line one\nline two\n> older' }), 'wysiwyg', false)
    const body = last().body
    expect(body).not.toContain('&gt; ')
    expect(body).toContain('<blockquote><p>line one<br>line two</p><blockquote><p>older</p></blockquote></blockquote>')
  })

  it('keeps the plain "> " quote outside the rich editor', () => {
    openReply(detail({ bodyQuote: 'line one\nline two' }), 'plaintext', false)
    expect(last().body).toContain('> line one\n> line two')
  })

  it('writes a readable date in the attribution, not the raw timestamp', () => {
    openReply(detail({ date: '2026-10-05T16:03:44Z' }), 'plaintext', false)
    expect(last().body).not.toContain('2026-10-05T16:03:44Z')
    expect(last().body).toMatch(/On .*2026.*, Ann wrote:/)
  })
})
