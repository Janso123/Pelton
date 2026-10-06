import type { Account } from './types'

const defaultPorts: Record<string, string> = { 'https:': '443', 'http:': '80' }

/**
 * connectionSummary says how an account reaches its server, for the mailbox
 * list: protocol plus host, with the port where it is not the scheme default.
 * The local account has no server, so it gets localLabel instead.
 */
export function connectionSummary(
  account: Pick<Account, 'protocol' | 'imapHost' | 'imapPort' | 'jmapSessionUrl' | 'local'>,
  localLabel: string,
): string {
  if (account.local) {
    return localLabel
  }
  if (account.protocol !== 'jmap') {
    return `IMAP · ${account.imapHost}:${account.imapPort}`
  }
  try {
    const url = new URL(account.jmapSessionUrl ?? '')
    if (url.hostname) {
      const port = url.port && url.port !== defaultPorts[url.protocol] ? `:${url.port}` : ''
      return `JMAP · ${url.hostname}${port}`
    }
  } catch {
    // empty or malformed: fall through to the IMAP host
  }
  return `JMAP · ${account.imapHost}`
}
