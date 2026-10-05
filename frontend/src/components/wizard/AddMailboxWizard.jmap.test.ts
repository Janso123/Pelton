import { beforeEach, describe, expect, it, vi } from 'vitest'
import { render, screen, waitFor } from '@testing-library/svelte'
import userEvent from '@testing-library/user-event'

const api = vi.hoisted(() => ({
  discoverConfig: vi.fn(),
  testConnection: vi.fn(),
  addPasswordAccount: vi.fn(),
  addOAuthAccount: vi.fn(),
  beginOAuthAccount: vi.fn(),
  finishAddAccount: vi.fn(),
  cancelAddAccount: vi.fn(),
  listFolders: vi.fn(),
  setFolderSyncExcluded: vi.fn(),
  startAccountSync: vi.fn(),
}))

vi.mock('../../lib/api', async (importOriginal) => ({
  ...(await importOriginal<typeof import('../../lib/api')>()),
  ...api,
}))

vi.mock('../../../wailsjs/runtime/runtime', () => ({
  BrowserOpenURL: vi.fn(),
}))

import AddMailboxWizard from './AddMailboxWizard.svelte'

function account(overrides: Record<string, unknown> = {}) {
  return {
    id: 7,
    email: 'user@example.com',
    displayName: '',
    localLabel: '',
    useLocalLabel: false,
    username: '',
    imapHost: 'imap.example.com',
    imapPort: 993,
    smtpHost: 'smtp.example.com',
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
    protocol: 'imap',
    ...overrides,
  }
}

async function openCustomConfig(user: ReturnType<typeof userEvent.setup>) {
  render(AddMailboxWizard, { props: { offerImport: false } })
  await user.click(screen.getByRole('button', { name: /Other \(IMAP\/JMAP \/ SMTP\)/i }))
  await user.type(screen.getByLabelText(/^Email$/i), 'user@example.com')
  await user.type(screen.getByLabelText(/^Password$/i), 'secret')
  const imap = screen.getByLabelText(/IMAP\/JMAP host/i)
  await user.clear(imap)
  await user.type(imap, 'imap.example.com')
}

beforeEach(() => {
  for (const fn of Object.values(api)) {
    fn.mockReset()
  }
  api.listFolders.mockResolvedValue([])
  api.startAccountSync.mockResolvedValue(undefined)
  api.addPasswordAccount.mockResolvedValue(account())
  api.finishAddAccount.mockResolvedValue(account({ protocol: 'jmap' }))
})

describe('AddMailboxWizard JMAP choice', () => {
  it('skips the question and adds IMAP when the probe is unavailable', async () => {
    const user = userEvent.setup()
    api.testConnection.mockResolvedValue({
      jmapAvailable: false,
      jmapWebSocket: false,
      jmapSessionURL: '',
      jmapMailAccountID: '',
    })

    await openCustomConfig(user)
    await user.click(screen.getByRole('button', { name: /Test connection/i }))
    await waitFor(() => expect(api.testConnection).toHaveBeenCalled())
    await user.click(screen.getByRole('button', { name: /^Add mailbox$/i }))

    await waitFor(() => expect(api.addPasswordAccount).toHaveBeenCalled())
    expect(api.addPasswordAccount.mock.calls[0][0].protocol).toBe('imap')
    expect(screen.queryByText(/Use JMAP for mail and send\?/i)).not.toBeInTheDocument()
  })

  it('renders both choices when JMAP is available and submits only the chosen protocol', async () => {
    const user = userEvent.setup()
    api.testConnection.mockResolvedValue({
      jmapAvailable: true,
      jmapWebSocket: true,
      jmapSessionURL: 'https://jmap.example/session',
      jmapMailAccountID: 'a1',
    })

    await openCustomConfig(user)
    await user.click(screen.getByRole('button', { name: /Test connection/i }))
    await waitFor(() => expect(api.testConnection).toHaveBeenCalled())
    await user.click(screen.getByRole('button', { name: /^Add mailbox$/i }))

    expect(await screen.findByText(/Use JMAP for mail and send\?/i)).toBeInTheDocument()
    expect(api.addPasswordAccount).not.toHaveBeenCalled()

    await user.click(screen.getByRole('button', { name: /^Use JMAP$/i }))
    await waitFor(() => expect(api.addPasswordAccount).toHaveBeenCalled())
    expect(api.addPasswordAccount.mock.calls[0][0].protocol).toBe('jmap')
  })

  it('submits IMAP when Keep IMAP is chosen on the JMAP step', async () => {
    const user = userEvent.setup()
    api.testConnection.mockResolvedValue({
      jmapAvailable: true,
      jmapWebSocket: false,
      jmapSessionURL: '',
      jmapMailAccountID: '',
    })

    await openCustomConfig(user)
    await user.click(screen.getByRole('button', { name: /Test connection/i }))
    await waitFor(() => expect(api.testConnection).toHaveBeenCalled())
    await user.click(screen.getByRole('button', { name: /^Add mailbox$/i }))
    await screen.findByText(/Use JMAP for mail and send\?/i)
    await user.click(screen.getByRole('button', { name: /^Keep IMAP$/i }))

    await waitFor(() => expect(api.addPasswordAccount).toHaveBeenCalled())
    expect(api.addPasswordAccount.mock.calls[0][0].protocol).toBe('imap')
  })

  it('creates nothing when closing the password JMAP choice step', async () => {
    const user = userEvent.setup()
    api.testConnection.mockResolvedValue({
      jmapAvailable: true,
      jmapWebSocket: false,
      jmapSessionURL: '',
      jmapMailAccountID: '',
    })

    await openCustomConfig(user)
    await user.click(screen.getByRole('button', { name: /Test connection/i }))
    await waitFor(() => expect(api.testConnection).toHaveBeenCalled())
    await user.click(screen.getByRole('button', { name: /^Add mailbox$/i }))
    await screen.findByText(/Use JMAP for mail and send\?/i)
    await user.click(screen.getByRole('button', { name: /^Close$/i }))

    expect(api.addPasswordAccount).not.toHaveBeenCalled()
    expect(api.cancelAddAccount).not.toHaveBeenCalled()
  })

  it('finishes OAuth with IMAP immediately when the probe is unavailable', async () => {
    const user = userEvent.setup()
    api.beginOAuthAccount.mockResolvedValue({
      id: 'pending-1',
      jmapAvailable: false,
      jmapWebSocket: false,
      jmapSessionURL: '',
      jmapMailAccountID: '',
    })
    api.finishAddAccount.mockResolvedValue(account())
    api.listFolders.mockResolvedValue([])

    render(AddMailboxWizard, { props: { offerImport: false } })
    await user.click(screen.getByRole('button', { name: /Outlook/i }))
    await user.type(screen.getByLabelText(/^Email$/i), 'user@example.com')
    await user.type(screen.getByLabelText(/OAuth client id/i), 'client-id')
    await user.click(screen.getByRole('button', { name: /Sign in with Outlook/i }))

    await waitFor(() => expect(api.beginOAuthAccount).toHaveBeenCalled())
    await waitFor(() => expect(api.finishAddAccount).toHaveBeenCalledWith('pending-1', 'imap'))
    expect(screen.queryByText(/Use JMAP for mail and send\?/i)).not.toBeInTheDocument()
  })

  it('asks after OAuth begin and finishes with the chosen protocol', async () => {
    const user = userEvent.setup()
    api.beginOAuthAccount.mockResolvedValue({
      id: 'pending-2',
      jmapAvailable: true,
      jmapWebSocket: true,
      jmapSessionURL: 'https://jmap.example/session',
      jmapMailAccountID: 'a1',
    })
    api.finishAddAccount.mockResolvedValue(account({ protocol: 'jmap' }))

    render(AddMailboxWizard, { props: { offerImport: false } })
    await user.click(screen.getByRole('button', { name: /Outlook/i }))
    await user.type(screen.getByLabelText(/^Email$/i), 'user@example.com')
    await user.type(screen.getByLabelText(/OAuth client id/i), 'client-id')
    await user.click(screen.getByRole('button', { name: /Sign in with Outlook/i }))

    expect(await screen.findByText(/Use JMAP for mail and send\?/i)).toBeInTheDocument()
    expect(api.finishAddAccount).not.toHaveBeenCalled()

    await user.click(screen.getByRole('button', { name: /^Use JMAP$/i }))
    await waitFor(() => expect(api.finishAddAccount).toHaveBeenCalledWith('pending-2', 'jmap'))
  })

  it('cancels a pending OAuth add when closing the JMAP choice step', async () => {
    const user = userEvent.setup()
    api.beginOAuthAccount.mockResolvedValue({
      id: 'pending-3',
      jmapAvailable: true,
      jmapWebSocket: false,
      jmapSessionURL: '',
      jmapMailAccountID: '',
    })
    api.cancelAddAccount.mockResolvedValue(undefined)

    render(AddMailboxWizard, { props: { offerImport: false } })
    await user.click(screen.getByRole('button', { name: /Outlook/i }))
    await user.type(screen.getByLabelText(/^Email$/i), 'user@example.com')
    await user.type(screen.getByLabelText(/OAuth client id/i), 'client-id')
    await user.click(screen.getByRole('button', { name: /Sign in with Outlook/i }))
    await screen.findByText(/Use JMAP for mail and send\?/i)

    await user.click(screen.getByRole('button', { name: /^Close$/i }))
    await waitFor(() => expect(api.cancelAddAccount).toHaveBeenCalledWith('pending-3'))
    expect(api.finishAddAccount).not.toHaveBeenCalled()
  })
})

describe('AddMailboxWizard first sync', () => {
  it('starts the first sync once on the regular flow when there is no folder choice', async () => {
    const user = userEvent.setup()
    api.testConnection.mockResolvedValue({
      jmapAvailable: false,
      jmapWebSocket: false,
      jmapSessionURL: '',
      jmapMailAccountID: '',
    })

    await openCustomConfig(user)
    await user.click(screen.getByRole('button', { name: /Test connection/i }))
    await waitFor(() => expect(api.testConnection).toHaveBeenCalled())
    await user.click(screen.getByRole('button', { name: /^Add mailbox$/i }))

    await waitFor(() => expect(api.startAccountSync).toHaveBeenCalledTimes(1))
    expect(api.startAccountSync).toHaveBeenCalledWith(7)
  })

  it('holds the first sync at the folder picker until the user continues, then starts it once', async () => {
    const user = userEvent.setup()
    api.listFolders.mockResolvedValue([
      { id: 1, accountId: 7, name: 'INBOX' },
      { id: 2, accountId: 7, name: 'Archive' },
    ])
    api.testConnection.mockResolvedValue({
      jmapAvailable: false,
      jmapWebSocket: false,
      jmapSessionURL: '',
      jmapMailAccountID: '',
    })

    await openCustomConfig(user)
    await user.click(screen.getByRole('button', { name: /Test connection/i }))
    await waitFor(() => expect(api.testConnection).toHaveBeenCalled())
    await user.click(screen.getByRole('button', { name: /^Add mailbox$/i }))
    await screen.findByText('Archive')
    expect(api.startAccountSync).not.toHaveBeenCalled()

    await user.click(screen.getByRole('button', { name: /^Start syncing$/i }))
    await waitFor(() => expect(api.startAccountSync).toHaveBeenCalledTimes(1))
  })
})
