import { beforeEach, describe, expect, it, vi } from 'vitest'
import { get } from 'svelte/store'
import type { Account } from '../lib/types'

vi.mock('../lib/api', () => ({
  accountsNeedingPassword: vi.fn(async () => []),
  dismissPasswordPrompt: vi.fn(async () => {}),
}))

import { accountsNeedingPassword } from '../lib/api'

const needing = vi.mocked(accountsNeedingPassword)
const account = { id: 1, email: 'me@example.com', passwordPromptDismissed: false } as Account

// the store keeps who was skipped for the session, so each test loads a fresh copy.
async function load(): Promise<typeof import('./passwordprompt')> {
  vi.resetModules()
  return import('./passwordprompt')
}

function settle(): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve))
}

beforeEach(() => {
  needing.mockReset()
  needing.mockResolvedValue([account])
})

describe('promptForMissingPasswords', () => {
  // every sync that ends asks for the prompt, and a refused password usually
  // fails several syncs in a row. One answer has to be enough.
  it('asks once when a second sync ends while the prompt is open', async () => {
    const { promptForMissingPasswords, passwordPrompt, answerPasswordPrompt } = await load()
    const first = promptForMissingPasswords()
    await settle()
    const second = promptForMissingPasswords()
    await settle()
    expect(get(passwordPrompt)?.id).toBe(account.id)

    answerPasswordPrompt('saved')
    await Promise.all([first, second])
    expect(get(passwordPrompt)).toBeNull()
  })

  // closing the prompt means "not now"; the next background sync must not
  // bring it straight back.
  it('leaves an account skipped this session alone', async () => {
    const { promptForMissingPasswords, passwordPrompt, answerPasswordPrompt } = await load()
    const first = promptForMissingPasswords()
    await settle()
    answerPasswordPrompt('skipped')
    await first

    await promptForMissingPasswords()
    expect(get(passwordPrompt)).toBeNull()
  })

  it('leaves a dismissed account alone', async () => {
    needing.mockResolvedValue([{ ...account, passwordPromptDismissed: true }])
    const { promptForMissingPasswords, passwordPrompt } = await load()
    await promptForMissingPasswords()
    expect(get(passwordPrompt)).toBeNull()
  })
})
