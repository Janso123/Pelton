// errors.ts turns a raw thrown value into a message fit for a toast. its main
// job is to spot the confusing network failures (DNS "no such host", refused
// dials, timeouts, TLS handshakes, and the backend's offline sentinel) and
// replace them with one honest "no internet connection" line, instead of the
// technical error or the misleading "no credentials" the sync path used to
// report when the real cause was a dropped connection.

import { get } from 'svelte/store'
import { t } from './i18n'
import { errorMessage } from '../stores/toast'
import { online, setOnline } from '../stores/network'

// patterns that mean "the network, not the request, is the problem".
const networkError =
  /no such host|dial tcp|connection refused|i\/o timeout|network is unreachable|no route to host|tls handshake|lookup .* on |pelton: ?offline|offline|econnrefused|enotfound|failed to fetch/i

// isNetworkError reports whether err looks like a connectivity failure.
export function isNetworkError(err: unknown): boolean {
  if (typeof navigator !== 'undefined' && !navigator.onLine) {
    return true
  }
  return networkError.test(errorMessage(err))
}

// friendlyError returns a localized message for err, collapsing every
// connectivity failure to the single offline line. it also flips the offline
// signal so the status bar agrees with the toast the user just saw.
export function friendlyError(err: unknown): string {
  if (isNetworkError(err)) {
    if (get(online)) {
      setOnline(false)
    }
    return get(t)('common.network.offlineError')
  }
  return errorMessage(err)
}

// matches the backend's errProtocolSwitchTimedOut, which may carry the cause
// after a colon.
const protocolSwitchTimedOut = /pelton: protocol switch timed out/

// protocolSwitchError returns the message for a failed SwitchProtocol call.
// a switch that ran out of time left the mailbox on its old protocol, so it
// gets a localized line saying so instead of the raw context error.
export function protocolSwitchError(err: unknown): string {
  if (protocolSwitchTimedOut.test(errorMessage(err))) {
    return get(t)('settings.protocol.switchFailed')
  }
  return errorMessage(err)
}
