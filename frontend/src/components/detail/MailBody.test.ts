import { beforeEach, describe, expect, it, vi } from 'vitest'
import { render, screen, waitFor } from '@testing-library/svelte'
import type { MessageDetail } from '../../lib/types'
import userEvent from '@testing-library/user-event'
import { get } from 'svelte/store'
import { clearMessage, loadMessage, messageDetail } from '../../stores/message'
import * as toast from '../../stores/toast'
import MailBody from './MailBody.svelte'

const api = vi.hoisted(() => ({
  getMessage: vi.fn(), getMessageHtml: vi.fn(), trustSenderImages: vi.fn(),
  allowDomainImages: vi.fn(), allowRemoteForMessage: vi.fn(),
}))
vi.mock('../../lib/api', async (original) => ({ ...await original<typeof import('../../lib/api')>(), ...api }))
function deferred<T>() {
  let resolve!: (value: T) => void
  let reject!: (error: unknown) => void
  const promise = new Promise<T>((res, rej) => { resolve = res; reject = rej })
  return { promise, resolve, reject }
}
beforeEach(() => {
  clearMessage()
  vi.restoreAllMocks()
  Object.values(api).forEach((mock) => mock.mockReset())
  vi.spyOn(toast, 'toastSuccess').mockImplementation(() => {})
  vi.spyOn(toast, 'toastError').mockImplementation(() => {})
})

function detail(over: Partial<MessageDetail> = {}): MessageDetail {
  return {
    id: 7,
    accountId: 1,
    folderId: 1,
    accountEmail: 'me@example.com',
    folderName: 'INBOX',
    subject: 'Hello',
    fromName: 'InPost',
    fromAddress: 'info@paczkomaty.pl',
    snippet: '',
    date: '2026-09-21T12:00:00Z',
    seen: true,
    flagged: false,
    hasAttachments: false,
    pgp: '',
    auth: '',
    flagColor: 0,
    offline: false,
    snoozeUntil: '',
    senderVip: false,
    smime: { status: '', signer: '', email: '', issuer: '', detail: '' },
    toAddresses: '',
    ccAddresses: '',
    replyTo: '',
    messageIdHeader: '',
    references: [],
    bodyPlain: '',
    bodyQuote: '',
    bodyHtmlSafe: '<p>hi</p><img src="https://cdn.example/logo.png">',
    isHtml: true,
    hasRemoteContent: true,
    remoteAllowed: false,
    remoteHosts: ['cdn.example'],
    trackingPixels: [],
    attachments: [],
    phishing: { level: 'none' },
    unsubscribe: null,
    charsetGuess: '',
    pgpState: '',
    bodyComplete: true,
    ...over,
  }
}

function frameCSP(container: HTMLElement): string {
  const doc = container.querySelector('iframe')?.getAttribute('srcdoc') ?? ''
  return /img-src ([^;]*);/.exec(doc)?.[1] ?? ''
}

describe('MailBody remote content', () => {
  // a JMAP stub of a trusted sender's mail can arrive with remoteAllowed false
  // and only turn true once the body is fetched and the message refreshes in
  // place. the same message must then show its images without reopening it.
  it('follows remoteAllowed turning true when the same message refreshes', async () => {
    const { container, rerender } = render(MailBody, { detail: detail() })
    expect(frameCSP(container)).toBe('data:')
    expect(container.querySelector('.remote-bar')).not.toBeNull()

    await rerender({ detail: detail({ remoteAllowed: true }) })
    expect(frameCSP(container)).toBe('data: https: http:')
    expect(container.querySelector('.remote-bar')).toBeNull()
  })

  it('blocks remote content again when a different message opens', async () => {
    const { container, rerender } = render(MailBody, { detail: detail({ remoteAllowed: true }) })
    expect(frameCSP(container)).toBe('data: https: http:')

    await rerender({ detail: detail({ id: 8, remoteAllowed: false }) })
    expect(frameCSP(container)).toBe('data:')
  })
})


describe('MailBody remote action identity', () => {
  async function openA() {
    const a = detail({ subject: 'A', bodyHtmlSafe: '<p>A blocked</p>' })
    api.getMessage.mockResolvedValueOnce(a)
    await loadMessage(7)
    return render(MailBody, { detail: a })
  }
  async function openB(view: Awaited<ReturnType<typeof openA>>) {
    const b = detail({ id: 8, subject: 'B', bodyHtmlSafe: '<p>B blocked</p>' })
    api.getMessage.mockResolvedValueOnce(b)
    await loadMessage(8)
    await view.rerender({ detail: b })
  }
  function expectBlockedB(view: Awaited<ReturnType<typeof openA>>) {
    expect(get(messageDetail).data).toMatchObject({ id: 8, subject: 'B', bodyHtmlSafe: '<p>B blocked</p>' })
    expect(frameCSP(view.container)).toBe('data:')
    expect(screen.getByRole('button', { name: 'Load once' })).toBeTruthy()
    expect(toast.toastSuccess).not.toHaveBeenCalled()
    expect(toast.toastError).not.toHaveBeenCalled()
  }
  it.each(['resolve', 'reject'] as const)('ignores stale HTML %s and leaves B blocked', async (outcome) => {
    const late = deferred<string>()
    api.getMessageHtml.mockReturnValueOnce(late.promise)
    const view = await openA()
    await userEvent.setup().click(screen.getByRole('button', { name: 'Load once' }))
    expect(api.getMessageHtml).toHaveBeenCalledWith(7, true, false)
    await openB(view)
    if (outcome === 'resolve') late.resolve('<p>A remote</p>')
    else late.reject(new Error('old A failed'))
    await late.promise.catch(() => {})
    await waitFor(() => expectBlockedB(view))
  })
  it.each([
    ['This sender', 'trustSenderImages'], ['This domain', 'allowDomainImages'], ['This email', 'allowRemoteForMessage'],
  ] as const)('keeps delayed %s trust attached to A', async (label, method) => {
    const late = deferred<void>()
    api[method].mockReturnValueOnce(late.promise)
    const view = await openA()
    await userEvent.setup().click(screen.getByRole('button', { name: label }))
    expect(api[method]).toHaveBeenCalledExactlyOnceWith(7)
    await openB(view)
    late.resolve()
    await late.promise
    await Promise.resolve()
    await Promise.resolve()
    await waitFor(() => { expect(api.getMessageHtml).not.toHaveBeenCalled(); expectBlockedB(view) })
  })
  it('loads current HTML and allows remote images', async () => {
    api.getMessageHtml.mockResolvedValue('<p>A remote</p>')
    const view = await openA()
    await userEvent.setup().click(screen.getByRole('button', { name: 'Load once' }))
    await waitFor(() => expect(get(messageDetail).data?.bodyHtmlSafe).toBe('<p>A remote</p>'))
    await view.rerender({ detail: get(messageDetail).data! })
    expect(view.container.querySelector('iframe')?.getAttribute('srcdoc')).toContain('<p>A remote</p>')
    expect(frameCSP(view.container)).toBe('data: https: http:')
    expect(screen.queryByRole('button', { name: 'Load once' })).toBeNull()
  })
  it.each(['close', 'return', 'destroy'] as const)('ignores HTML after %s', async (mode) => {
    const late = deferred<string>()
    api.getMessageHtml.mockReturnValueOnce(late.promise)
    const view = await openA()
    await userEvent.setup().click(screen.getByRole('button', { name: 'Load once' }))
    if (mode === 'destroy') view.unmount()
    else {
      if (mode === 'close') clearMessage()
      else {
        await openB(view)
        const a = detail({ bodyHtmlSafe: '<p>A reopened</p>' })
        api.getMessage.mockResolvedValueOnce(a)
        await loadMessage(7)
        await view.rerender({ detail: a })
      }
    }
    const before = get(messageDetail)
    late.resolve('<p>old A remote</p>')
    await late.promise
    await Promise.resolve()
    expect(get(messageDetail)).toEqual(before)
    if (mode !== 'destroy') expect(frameCSP(view.container)).toBe('data:')
    expect(toast.toastError).not.toHaveBeenCalled()
  })

  it.each([
    ['This sender', 'trustSenderImages'], ['This domain', 'allowDomainImages'], ['This email', 'allowRemoteForMessage'],
  ] as const)('ignores stale %s trust failure', async (label, method) => {
    const late = deferred<void>()
    api[method].mockReturnValueOnce(late.promise)
    const view = await openA()
    await userEvent.setup().click(screen.getByRole('button', { name: label }))
    await openB(view)
    late.reject(new Error('old trust failed'))
    await late.promise.catch(() => {})
    await Promise.resolve()
    expect(api.getMessageHtml).not.toHaveBeenCalled()
    expectBlockedB(view)
  })

  it('keeps the latest tracker policy when same-message HTML finishes backwards', async () => {
    const older = deferred<string>()
    const newer = deferred<string>()
    const a = detail({ trackingPixels: [{ host: 'tracker.example', url: 'https://tracker.example/p', reasons: ['tiny'] }] })
    api.getMessage.mockResolvedValueOnce(a)
    await loadMessage(7)
    const view = render(MailBody, { detail: a })
    api.getMessageHtml.mockReturnValueOnce(older.promise).mockReturnValueOnce(newer.promise)
    const user = userEvent.setup()
    await user.click(screen.getByRole('button', { name: 'Load once' }))
    await user.click(screen.getAllByRole('button', { name: 'More loading options' })[0])
    await user.click(screen.getByRole('button', { name: 'Load tracking pixels too' }))
    expect(api.getMessageHtml.mock.calls).toEqual([[7, true, false], [7, true, true]])
    newer.resolve('<p>with trackers</p>')
    await newer.promise
    await waitFor(() => expect(get(messageDetail).data?.bodyHtmlSafe).toBe('<p>with trackers</p>'))
    older.resolve('<p>without trackers</p>')
    await older.promise
    await Promise.resolve()
    expect(get(messageDetail).data?.bodyHtmlSafe).toBe('<p>with trackers</p>')
    expect(screen.queryByText(/tracking pixels stayed blocked/)).toBeNull()
    expect(frameCSP(view.container)).toBe('data: https: http:')
  })

  it.each([
    ['This sender', 'trustSenderImages'], ['This domain', 'allowDomainImages'], ['This email', 'allowRemoteForMessage'],
  ] as const)('forwards tracker opt-in and captured ID for active %s', async (label, method) => {
    const a = detail({ trackingPixels: [{ host: 'tracker.example', url: 'https://tracker.example/p', reasons: ['tiny'] }] })
    api.getMessage.mockResolvedValueOnce(a)
    await loadMessage(7)
    const view = render(MailBody, { detail: a })
    api[method].mockResolvedValueOnce(undefined)
    api.getMessageHtml.mockResolvedValueOnce('<p>trusted</p>')
    const user = userEvent.setup()
    const index = ['This email', 'This sender', 'This domain'].indexOf(label) + 1
    await user.click(screen.getAllByRole('button', { name: 'More loading options' })[index])
    await user.click(screen.getByRole('button', { name: 'Load tracking pixels too' }))
    await waitFor(() => expect(get(messageDetail).data?.bodyHtmlSafe).toBe('<p>trusted</p>'))
    expect(api[method]).toHaveBeenCalledExactlyOnceWith(7)
    expect(api.getMessageHtml).toHaveBeenCalledExactlyOnceWith(7, true, true)
    expect(frameCSP(view.container)).toBe('data: https: http:')
    expect(screen.queryByRole('button', { name: label })).toBeNull()
    if (method !== 'allowRemoteForMessage') expect(toast.toastSuccess).toHaveBeenCalledOnce()
  })

  it('preserves B tracker menu when stale A HTML settles', async () => {
    const late = deferred<string>()
    api.getMessageHtml.mockReturnValueOnce(late.promise)
    const view = await openA()
    const user = userEvent.setup()
    await user.click(screen.getByRole('button', { name: 'Load once' }))
    const b = detail({ id: 8, trackingPixels: [{ host: 'tracker.example', url: 'https://tracker.example/p', reasons: ['tiny'] }] })
    api.getMessage.mockResolvedValueOnce(b)
    await loadMessage(8)
    await view.rerender({ detail: b })
    await user.click(screen.getAllByRole('button', { name: 'More loading options' })[0])
    late.resolve('<p>A remote</p>')
    await late.promise
    await Promise.resolve()
    expect(screen.getByRole('button', { name: 'Load tracking pixels too' })).toBeTruthy()
    expect(screen.getAllByRole('button', { name: 'More loading options' })[0].getAttribute('aria-expanded')).toBe('true')
    expect(frameCSP(view.container)).toBe('data:')
    expect(screen.queryByText(/tracking pixels stayed blocked/)).toBeNull()
  })

  it.each([
    ['This sender', 'trustSenderImages'], ['This domain', 'allowDomainImages'], ['This email', 'allowRemoteForMessage'],
  ] as const)('supersedes pending %s trust with a newer same-message action', async (label, method) => {
    const late = deferred<void>()
    api[method].mockReturnValueOnce(late.promise)
    api.getMessageHtml.mockResolvedValueOnce('<p>newer choice</p>')
    const view = await openA()
    const user = userEvent.setup()
    await user.click(screen.getByRole('button', { name: label }))
    await user.click(screen.getByRole('button', { name: 'Load once' }))
    await waitFor(() => expect(get(messageDetail).data?.bodyHtmlSafe).toBe('<p>newer choice</p>'))
    late.resolve()
    await late.promise
    await Promise.resolve()
    await Promise.resolve()
    expect(api.getMessageHtml).toHaveBeenCalledExactlyOnceWith(7, true, false)
    expect(toast.toastSuccess).not.toHaveBeenCalled()
    expect(toast.toastError).not.toHaveBeenCalled()
    expect(frameCSP(view.container)).toBe('data: https: http:')
  })

})

describe('MailBody frame per message', () => {
  // WebKit can keep painting the previous document when one iframe's srcdoc is
  // swapped while an earlier swap is still settling, which left a new header
  // over the last message's body. Each message gets a frame of its own.
  it('replaces the body frame when another message opens', async () => {
    const { container, rerender } = render(MailBody, { detail: detail({ bodyHtmlSafe: '<p>first</p>' }) })
    const first = container.querySelector('iframe')

    await rerender({ detail: detail({ id: 8, bodyHtmlSafe: '<p>second</p>' }) })
    const second = container.querySelector('iframe')
    expect(second).not.toBeNull()
    expect(second).not.toBe(first)
    expect(second?.getAttribute('srcdoc')).toContain('second')
  })

  // marking the open message read refreshes it with the same body. That must
  // not rebuild the document and reload the frame.
  it('keeps the document when the same message refreshes with the same body', async () => {
    const { container, rerender } = render(MailBody, { detail: detail() })
    const frame = container.querySelector('iframe')
    const before = frame?.getAttribute('srcdoc')

    await rerender({ detail: detail({ seen: false }) })
    expect(container.querySelector('iframe')).toBe(frame)
    expect(container.querySelector('iframe')?.getAttribute('srcdoc')).toBe(before)
  })

  // the new frame stays hidden until its document has loaded, with a spinner
  // once that takes long enough to notice, so a slow switch shows that it is
  // loading rather than an empty or stale pane.
  it('hides a new frame until it loads and shows a spinner meanwhile', async () => {
    vi.useFakeTimers()
    // jsdom loads the srcdoc document at once. Holding back its load event and
    // the fallback readiness poll stands in for a document that is slow to load.
    vi.stubGlobal('requestAnimationFrame', () => 0)
    const holdLoad = (e: Event) => e.stopPropagation()
    document.addEventListener('load', holdLoad, true)
    try {
      const { container } = render(MailBody, { detail: detail() })
      const frame = container.querySelector('iframe')!
      expect(frame.classList.contains('loading')).toBe(true)
      expect(container.querySelector('[role="status"]')).toBeNull()

      await vi.advanceTimersByTimeAsync(200)
      expect(container.querySelector('[role="status"]')).not.toBeNull()

      document.removeEventListener('load', holdLoad, true)
      frame.dispatchEvent(new Event('load'))
      await vi.advanceTimersByTimeAsync(0)
      expect(frame.classList.contains('loading')).toBe(false)
      expect(container.querySelector('[role="status"]')).toBeNull()
    } finally {
      document.removeEventListener('load', holdLoad, true)
      vi.useRealTimers()
      vi.unstubAllGlobals()
    }
  })
})
