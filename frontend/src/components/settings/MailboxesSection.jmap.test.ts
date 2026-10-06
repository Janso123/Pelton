import { beforeAll, beforeEach, describe, expect, it, vi } from 'vitest'
import { render, screen, waitFor } from '@testing-library/svelte'
import userEvent from '@testing-library/user-event'
import type { Account } from '../../lib/types'
import { blankAccountProxy } from '../../lib/proxyroute'

// Modal uses the Web Animations API; jsdom does not implement it.
beforeAll(() => {
  if (!Element.prototype.animate) {
    Element.prototype.animate = () =>
      ({
        finished: Promise.resolve(),
        cancel: () => {},
        finish: () => {},
        onfinish: null,
      }) as unknown as Animation
  }
})

const api = vi.hoisted(() => ({
  listAccounts: vi.fn(),
  updateAccount: vi.fn(),
  deleteAccount: vi.fn(),
  getLogStatus: vi.fn(),
  deleteLogs: vi.fn(),
  chooseArchiveExportFolder: vi.fn(),
  previewArchiveExportName: vi.fn(),
  probeAccount: vi.fn(),
  switchProtocol: vi.fn(),
  probeAccountCertificatesFor: vi.fn(),
  syncAccountNow: vi.fn(),
}))

const accountsStore = vi.hoisted(() => ({
  refreshSidebar: vi.fn(),
}))

vi.mock('../../lib/api', async (importOriginal) => ({
  ...(await importOriginal<typeof import('../../lib/api')>()),
  ...api,
}))

vi.mock('../../stores/accounts', async (importOriginal) => ({
  ...(await importOriginal<typeof import('../../stores/accounts')>()),
  refreshSidebar: accountsStore.refreshSidebar,
}))

// accounts with no stored password, as the prompt store reports them.
const missing = vi.hoisted(() => new Set<number>())

vi.mock('../../stores/passwordprompt', () => ({
  missingPassword: {
    subscribe: (fn: (v: Set<number>) => void) => {
      fn(missing)
      return () => {}
    },
  },
  askForPassword: vi.fn(),
  refreshMissingPasswords: vi.fn().mockResolvedValue(undefined),
}))

import MailboxesSection from './MailboxesSection.svelte'

function account(overrides: Partial<Account> = {}): Account {
  return {
    id: 1,
    email: 'user@example.com',
    displayName: 'User',
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
    trustedCerts: [],
    caSubjects: [],
    proxy: blankAccountProxy(),
    ...overrides,
  }
}

async function openEditor(user: ReturnType<typeof userEvent.setup>) {
  render(MailboxesSection)
  await screen.findByText('user@example.com')
  await user.click(screen.getByRole('button', { name: /Edit user@example.com/i }))
  await waitFor(() => expect(api.probeAccount).toHaveBeenCalledWith(1))
  // wait out the "Checking…" state so the toggle reflects the probe result
  await waitFor(() => {
    expect(screen.queryByText(/Checking whether this server supports JMAP/i)).not.toBeInTheDocument()
  })
}

beforeEach(() => {
  for (const fn of Object.values(api)) {
    fn.mockReset()
  }
  accountsStore.refreshSidebar.mockReset()
  missing.clear()
  api.listAccounts.mockResolvedValue([account()])
  api.previewArchiveExportName.mockResolvedValue('sample.eml')
  api.probeAccount.mockResolvedValue({
    jmapAvailable: true,
    jmapWebSocket: true,
    jmapSessionURL: 'https://jmap.example/session',
    jmapMailAccountID: 'a1',
  })
  api.switchProtocol.mockResolvedValue(undefined)
  api.probeAccountCertificatesFor.mockResolvedValue([])
  api.syncAccountNow.mockResolvedValue(undefined)
})

describe('MailboxesSection protocol switch', () => {
  it('waits for SwitchProtocol and keeps the old control while pending', async () => {
    const user = userEvent.setup()
    let finish!: () => void
    const pending = new Promise<void>((resolve) => {
      finish = resolve
    })
    api.switchProtocol.mockReturnValue(pending)

    await openEditor(user)
    const toggle = await screen.findByRole('switch', { name: /Use JMAP for mail and send/i })
    expect(toggle).toHaveAttribute('aria-checked', 'false')

    await user.click(toggle)
    await waitFor(() => expect(api.switchProtocol).toHaveBeenCalledWith(1, 'jmap'))
    // control stays on IMAP until the call resolves; sidebar folders are not refreshed yet
    expect(toggle).toHaveAttribute('aria-checked', 'false')
    expect(accountsStore.refreshSidebar).not.toHaveBeenCalled()

    finish()
    await waitFor(() => expect(toggle).toHaveAttribute('aria-checked', 'true'))
    await waitFor(() => expect(accountsStore.refreshSidebar).toHaveBeenCalled())
  })

  it('applies a switch that finishes after the editor was closed and reopened', async () => {
    const user = userEvent.setup()
    // animations that end at once, so closing the editor really removes it
    const animate = Element.prototype.animate
    Element.prototype.animate = () => {
      const a = {
        finished: Promise.resolve(),
        cancel: () => {},
        finish: () => {},
        set onfinish(fn: (() => void) | null) {
          if (fn) queueMicrotask(fn)
        },
      }
      return a as unknown as Animation
    }
    let finish!: () => void
    api.switchProtocol.mockReturnValue(
      new Promise<void>((resolve) => {
        finish = resolve
      }),
    )

    await openEditor(user)
    await user.click(await screen.findByRole('switch', { name: /Use JMAP for mail and send/i }))
    await waitFor(() => expect(api.switchProtocol).toHaveBeenCalledWith(1, 'jmap'))

    await user.click(screen.getByRole('button', { name: 'Cancel' }))
    // the closing editor is only gone once its outro finishes
    await waitFor(() => expect(screen.queryByRole('switch', { name: /Use JMAP for mail and send/i })).not.toBeInTheDocument())
    await user.click(await screen.findByRole('button', { name: /Edit user@example.com/i }))
    await waitFor(() => expect(screen.getByRole('switch', { name: /Use JMAP for mail and send/i })).toBeInTheDocument())

    finish()
    await waitFor(() => expect(accountsStore.refreshSidebar).toHaveBeenCalled())
    await waitFor(() =>
      expect(screen.getByRole('switch', { name: /Use JMAP for mail and send/i })).toHaveAttribute('aria-checked', 'true'),
    )
    Element.prototype.animate = animate
  })

  it('rolls back and shows the error when SwitchProtocol fails', async () => {
    const user = userEvent.setup()
    api.switchProtocol.mockRejectedValue(new Error('switch failed'))

    await openEditor(user)
    const toggle = await screen.findByRole('switch', { name: /Use JMAP for mail and send/i })
    await user.click(toggle)

    await waitFor(() => expect(screen.getByText(/switch failed/i)).toBeInTheDocument())
    expect(toggle).toHaveAttribute('aria-checked', 'false')
    expect(accountsStore.refreshSidebar).not.toHaveBeenCalled()
  })

  it('says the switch did not happen when it timed out', async () => {
    const user = userEvent.setup()
    api.switchProtocol.mockRejectedValue(
      'pelton: protocol switch timed out: context deadline exceeded',
    )

    await openEditor(user)
    const toggle = await screen.findByRole('switch', { name: /Use JMAP for mail and send/i })
    await user.click(toggle)

    await waitFor(() =>
      expect(screen.getByText("Couldn't switch the protocol. Try again.")).toBeInTheDocument(),
    )
    expect(screen.queryByText(/context deadline exceeded/i)).not.toBeInTheDocument()
    expect(screen.queryByText(/Switching protocol/i)).not.toBeInTheDocument()
    expect(toggle).toHaveAttribute('aria-checked', 'false')
  })

  it('disables switching to JMAP when the probe is unavailable and keeps a stored JMAP protocol', async () => {
    const user = userEvent.setup()
    api.listAccounts.mockResolvedValue([account({ protocol: 'jmap' })])
    api.probeAccount.mockResolvedValue({
      jmapAvailable: false,
      jmapWebSocket: false,
      jmapSessionURL: '',
      jmapMailAccountID: '',
    })

    await openEditor(user)
    const toggle = await screen.findByRole('switch', { name: /Use JMAP for mail and send/i })
    expect(toggle).toHaveAttribute('aria-checked', 'true')
    expect(toggle).not.toBeDisabled()
    expect(screen.getByText(/JMAP is not available on this server/i)).toBeInTheDocument()
  })

  it('asks a restored JMAP mailbox for its password instead of calling JMAP unavailable', async () => {
    const user = userEvent.setup()
    missing.add(1)
    api.listAccounts.mockResolvedValue([account({ protocol: 'jmap', jmapSessionUrl: '' })])
    api.probeAccount.mockRejectedValue(new Error('pelton: no credentials for account'))
    api.updateAccount.mockResolvedValue(account({ protocol: 'jmap', jmapSessionUrl: '' }))

    render(MailboxesSection)
    expect(await screen.findByText('JMAP · imap.example.com')).toBeInTheDocument()
    await openEditor(user)
    const toggle = await screen.findByRole('switch', { name: /Use JMAP for mail and send/i })
    expect(toggle).toHaveAttribute('aria-checked', 'true')
    expect(screen.queryByText(/JMAP is not available on this server/i)).not.toBeInTheDocument()
    expect(screen.getByText(/Pelton cannot sign in to this mailbox/i)).toBeInTheDocument()

    await user.type(screen.getByPlaceholderText('Enter a password'), 'pw')
    await user.click(screen.getByRole('button', { name: /^Save$/i }))
    await waitFor(() => expect(api.updateAccount).toHaveBeenCalled())
    expect(api.updateAccount.mock.calls[0][0].password).toBe('pw')
    expect(api.switchProtocol).not.toHaveBeenCalled()
    await waitFor(() => expect(api.syncAccountNow).toHaveBeenCalledWith(1))
  })

  it('does not start a sync when the password field is left empty', async () => {
    const user = userEvent.setup()
    api.updateAccount.mockResolvedValue(account())
    await openEditor(user)

    await user.click(screen.getByRole('button', { name: /^Save$/i }))
    await waitFor(() => expect(api.updateAccount).toHaveBeenCalled())
    expect(api.syncAccountNow).not.toHaveBeenCalled()
  })

  it('disables enabling JMAP when unavailable on an IMAP account', async () => {
    const user = userEvent.setup()
    api.probeAccount.mockResolvedValue({
      jmapAvailable: false,
      jmapWebSocket: false,
      jmapSessionURL: '',
      jmapMailAccountID: '',
    })

    await openEditor(user)
    const toggle = await screen.findByRole('switch', { name: /Use JMAP for mail and send/i })
    expect(toggle).toHaveAttribute('aria-checked', 'false')
    expect(toggle).toBeDisabled()
    expect(screen.getByText(/JMAP is not available on this server/i)).toBeInTheDocument()
    expect(api.switchProtocol).not.toHaveBeenCalled()
  })
})

describe('MailboxesSection parallel sync override', () => {
  it('shows the global default and saves null while it is on', async () => {
    const user = userEvent.setup()
    api.updateAccount.mockResolvedValue(account())
    await openEditor(user)

    const toggle = screen.getByRole('switch', { name: /Use default \(3\)/i })
    expect(toggle).toHaveAttribute('aria-checked', 'true')
    expect(screen.queryByRole('slider', { name: /Parallel sync connections/i })).not.toBeInTheDocument()

    await user.click(screen.getByRole('button', { name: /^Save$/i }))
    await waitFor(() => expect(api.updateAccount).toHaveBeenCalled())
    expect(api.updateAccount.mock.calls[0][0].syncMaxParallel).toBeNull()
  })

  it('turning the default off reveals a slider and saves the override', async () => {
    const user = userEvent.setup()
    api.updateAccount.mockResolvedValue(account({ syncMaxParallel: 3 }))
    await openEditor(user)

    await user.click(screen.getByRole('switch', { name: /Use default \(3\)/i }))
    const slider = screen.getByRole('slider', { name: /Parallel sync connections/i })
    expect(slider).toHaveAttribute('aria-valuetext', '3')

    await user.click(screen.getByRole('button', { name: /^Save$/i }))
    await waitFor(() => expect(api.updateAccount).toHaveBeenCalled())
    expect(api.updateAccount.mock.calls[0][0].syncMaxParallel).toBe(3)
  })

  it('names the field in the toggle instead of a label with no control under it', async () => {
    const user = userEvent.setup()
    await openEditor(user)

    expect(screen.getByRole('switch', { name: 'Parallel sync connections: use default (3)' })).toBeInTheDocument()
    expect(screen.queryByText('Parallel sync connections')).not.toBeInTheDocument()
  })

  it('opens an overridden account with the slider on its value', async () => {
    const user = userEvent.setup()
    api.listAccounts.mockResolvedValue([account({ syncMaxParallel: 2 })])
    await openEditor(user)

    expect(screen.getByRole('switch', { name: /Use default/i })).toHaveAttribute('aria-checked', 'false')
    expect(screen.getByRole('slider', { name: /Parallel sync connections/i })).toHaveAttribute('aria-valuetext', '2')
  })
})

describe('MailboxesSection connection summary', () => {
  it('shows how each account is connected and follows a protocol switch', async () => {
    api.listAccounts.mockResolvedValue([
      account({ imapHost: '127.0.0.1', imapPort: 993 }),
      account({
        id: 2,
        email: 'jm@example.com',
        displayName: 'Jm',
        protocol: 'jmap',
        jmapSessionUrl: 'https://jmap.example.com/.well-known/jmap',
      }),
    ])
    render(MailboxesSection)
    expect(await screen.findByText('IMAP · 127.0.0.1:993')).toBeInTheDocument()
    expect(screen.getByText('JMAP · jmap.example.com')).toBeInTheDocument()
  })

  it('does not repeat the Local Folders name as its connection', async () => {
    api.listAccounts.mockResolvedValue([
      account({ id: 9, email: 'local@pelton.invalid', displayName: 'Local Folders', local: true }),
    ])
    render(MailboxesSection)
    expect(await screen.findByText('local@pelton.invalid')).toBeInTheDocument()
    expect(screen.getAllByText('Local Folders')).toHaveLength(1)
  })

  it('reads the account back after switching to JMAP, so the summary names its JMAP host', async () => {
    const user = userEvent.setup()
    await openEditor(user)
    expect(screen.getByText('IMAP · imap.example.com:993')).toBeInTheDocument()
    api.listAccounts.mockResolvedValue([
      account({ protocol: 'jmap', jmapSessionUrl: 'https://jmap.example.com/.well-known/jmap' }),
    ])
    await user.click(await screen.findByRole('switch', { name: /Use JMAP for mail and send/i }))
    expect(await screen.findByText('JMAP · jmap.example.com')).toBeInTheDocument()
  })
})
