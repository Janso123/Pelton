// message.ts owns the detail pane: the full message for the open id. it loads on
// demand and exposes a way to swap in the remote-allowed body when the user opts
// to load remote images.

import { writable, get } from 'svelte/store'
import type { MessageDetail } from '../lib/types'
import { getMessage } from '../lib/api'
import { type AsyncState, idle, loading, ready, failed } from '../lib/async'
import { errorMessage } from './toast'

export const messageDetail = writable<AsyncState<MessageDetail>>(idle())

// bodyLoading is true while a stub's preview is on screen and the full body is
// still being fetched in the background.
export const bodyLoading = writable(false)

// generation bumps whenever the pane switches message or closes. a getMessage
// that resolves after a bump is stale and must not touch the pane.
let generation = 0
let bodySequence = 0
let currentMessageId: number | null = null
let initialLoadGeneration: number | null = null
let pendingRefresh = false
let detailSequence = 0

/** Identifies one remote-body action in one opening of the detail pane. */
export interface BodyRequest {
  readonly id: number
  readonly generation: number
  readonly sequence: number
}

/** Starts a remote-body action only for the ready, displayed message. */
export function beginBodyRequest(id: number): BodyRequest | null {
  const state = get(messageDetail)
  if (state.status !== 'ready' || state.data?.id !== id) return null
  return { id, generation, sequence: ++bodySequence }
}

/** Checks whether navigation or a later remote action replaced this action. */
export function isBodyRequestCurrent(request: BodyRequest): boolean {
  const state = get(messageDetail)
  return request.generation === generation && request.sequence === bodySequence &&
    state.status === 'ready' && state.data?.id === request.id
}

function applyDetail(detail: MessageDetail): void {
  messageDetail.set(ready(detail))
  bodyLoading.set(detail.bodyComplete === false)
}

/** Loads the selected detail and rereads once for updates received while opening. */
export async function loadMessage(id: number): Promise<void> {
  const gen = ++generation
  currentMessageId = id
  initialLoadGeneration = gen
  pendingRefresh = false
  const request = ++detailSequence
  const prev = get(messageDetail)
  if (prev.data?.id !== id) {
    messageDetail.update((s) => loading(s))
    bodyLoading.set(false)
  }
  let succeeded = false
  try {
    const detail = await getMessage(id)
    if (gen !== generation || request !== detailSequence) return
    applyDetail(detail)
    succeeded = true
  } catch (err) {
    if (gen === generation && request === detailSequence) {
      bodyLoading.set(false)
      messageDetail.set(failed(errorMessage(err)))
    }
  } finally {
    if (gen === generation) initialLoadGeneration = null
  }
  if (gen !== generation) return
  const reread = succeeded && pendingRefresh
  pendingRefresh = false
  if (reread) await refreshMessage(id)
}

/** Refreshes the selected message after mail:updated without blanking the pane. */
export async function refreshMessage(id: number): Promise<void> {
  if (id !== currentMessageId) return
  if (initialLoadGeneration === generation) {
    pendingRefresh = true
    return
  }
  if (get(messageDetail).data?.id !== id) return
  const gen = generation
  const request = ++detailSequence
  bodyLoading.set(true)
  try {
    const detail = await getMessage(id)
    if (gen === generation && request === detailSequence) applyDetail(detail)
  } catch {
    if (gen === generation && request === detailSequence) bodyLoading.set(false)
  }
}

/** Empties the pane and cancels any queued reread. */
export function clearMessage(): void {
  generation++
  currentMessageId = null
  initialLoadGeneration = null
  pendingRefresh = false
  bodyLoading.set(false)
  messageDetail.set(idle())
}

/** Applies remote HTML only to its current action; returns whether it was applied. */
export function setBodyHtml(request: BodyRequest, html: string): boolean {
  if (!isBodyRequestCurrent(request)) return false
  messageDetail.update((s) => ready({ ...s.data!, bodyHtmlSafe: html }))
  return true
}
