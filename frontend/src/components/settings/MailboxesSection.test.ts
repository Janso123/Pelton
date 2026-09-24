import { beforeEach, describe, expect, it, vi } from 'vitest'
import { render, screen } from '@testing-library/svelte'
import userEvent from '@testing-library/user-event'
import type { Account } from '../../lib/types'

const api = vi.hoisted(() => ({
  listAccounts: vi.fn(),
  accountsNeedingPassword: vi.fn(),
  accountOAuthProvider: vi.fn(),
  reauthorizeOAuthAccount: vi.fn(),
  previewArchiveExportName: vi.fn(),
}))

vi.mock('../../lib/api', async (importOriginal) => ({
  ...(await importOriginal<typeof import('../../lib/api')>()),
  ...api,
}))

vi.mock('../../stores/accounts', () => ({
  refreshSidebar: vi.fn(),
}))

import MailboxesSection from './MailboxesSection.svelte'

// the editor opens in a modal with a transition, and jsdom has no web
// animations. A finished stub lets svelte run the transition to its end.
Element.prototype.animate ??= function () {
  const animation = { onfinish: null as (() => void) | null, cancel() {}, finished: Promise.resolve() }
  queueMicrotask(() => animation.onfinish?.())
  return animation as unknown as Animation
}

const account: Account = {
  id: 7,
  email: 'student@uni.example',
  displayName: '',
  localLabel: '',
  useLocalLabel: false,
  username: '',
  imapHost: 'imap.gmail.com',
  imapPort: 993,
  smtpHost: 'smtp.gmail.com',
  smtpPort: 465,
  local: false,
  imapTls: 'ssl',
  smtpTls: 'ssl',
  exportOnArchive: false,
  exportDir: '',
  exportSubfolders: 'none',
  exportNameTemplate: '',
  pgpDefault: '',
  passwordPromptDismissed: false,
  trustedCerts: [],
  caSubjects: [],
}

async function openEditor(): Promise<void> {
  render(MailboxesSection)
  await userEvent.click(await screen.findByRole('button', { name: `Edit ${account.email}` }))
}

beforeEach(() => {
  for (const fn of Object.values(api)) {
    fn.mockReset()
  }
  api.listAccounts.mockResolvedValue([account])
  api.accountsNeedingPassword.mockResolvedValue([])
  api.previewArchiveExportName.mockResolvedValue('')
  api.reauthorizeOAuthAccount.mockResolvedValue(undefined)
})

describe('mailbox editor sign-in', () => {
  it('offers signing in again instead of a password for an oauth mailbox', async () => {
    api.accountOAuthProvider.mockResolvedValue('google')
    await openEditor()

    const again = await screen.findByRole('button', { name: 'Sign in again' })
    expect(screen.getByText(/signs in with Google/)).toBeInTheDocument()
    expect(screen.queryByLabelText('Password')).not.toBeInTheDocument()

    await userEvent.click(again)
    expect(api.reauthorizeOAuthAccount).toHaveBeenCalledWith(account.id)
  })

  it('keeps the password field for a password mailbox', async () => {
    api.accountOAuthProvider.mockResolvedValue('')
    await openEditor()

    await vi.waitFor(() => expect(api.accountOAuthProvider).toHaveBeenCalledWith(account.id))
    expect(screen.getByLabelText('Password')).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Sign in again' })).not.toBeInTheDocument()
  })
})
