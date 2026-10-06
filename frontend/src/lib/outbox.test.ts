import { describe, expect, it } from 'vitest'
import { outboxFailureText } from './outbox'

const t = (key: string) => `[${key}]`

describe('outboxFailureText', () => {
  it('translates the maybe-sent cause', () => {
    expect(outboxFailureText('maybe-sent', t)).toBe('[common.outboxPanel.maybeSent]')
  })

  it('passes any other error through', () => {
    expect(outboxFailureText('550 mailbox unavailable', t)).toBe('550 mailbox unavailable')
  })

  it('stays empty for an empty error so the caller picks its own fallback', () => {
    expect(outboxFailureText('', t)).toBe('')
  })
})
