import { describe, expect, it } from 'vitest'
import { connectionSummary } from './connection'

const base = { protocol: 'imap', imapHost: '127.0.0.1', imapPort: 993, jmapSessionUrl: '', local: false }

describe('connectionSummary', () => {
  it('shows the IMAP host and port', () => {
    expect(connectionSummary(base, 'Local')).toBe('IMAP · 127.0.0.1:993')
  })

  it('shows the JMAP session host without a default port', () => {
    const jmap = { ...base, protocol: 'jmap', jmapSessionUrl: 'https://127.0.0.1/.well-known/jmap' }
    expect(connectionSummary(jmap, 'Local')).toBe('JMAP · 127.0.0.1')
    expect(connectionSummary({ ...jmap, jmapSessionUrl: 'https://mail.example.org:443/jmap' }, 'Local')).toBe(
      'JMAP · mail.example.org',
    )
  })

  it('keeps a non-default JMAP port', () => {
    const jmap = { ...base, protocol: 'jmap', jmapSessionUrl: 'https://mail.example.org:8443/jmap' }
    expect(connectionSummary(jmap, 'Local')).toBe('JMAP · mail.example.org:8443')
    expect(connectionSummary({ ...jmap, jmapSessionUrl: 'http://mail.example.org:80/jmap' }, 'Local')).toBe(
      'JMAP · mail.example.org',
    )
  })

  it('falls back to the IMAP host when the session URL is empty or unparsable', () => {
    const jmap = { ...base, protocol: 'jmap', imapHost: 'imap.example.org' }
    expect(connectionSummary(jmap, 'Local')).toBe('JMAP · imap.example.org')
    expect(connectionSummary({ ...jmap, jmapSessionUrl: 'not a url' }, 'Local')).toBe('JMAP · imap.example.org')
    expect(connectionSummary({ ...jmap, jmapSessionUrl: undefined }, 'Local')).toBe('JMAP · imap.example.org')
  })

  it('uses the given label for the local account', () => {
    expect(connectionSummary({ ...base, local: true }, 'Local Folders')).toBe('Local Folders')
  })
})
