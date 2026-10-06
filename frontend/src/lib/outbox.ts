/** outboxFailureText is the text for a failed outbox row's cause: the translated notice for a send that may have been delivered, the raw error otherwise. */
export function outboxFailureText(lastError: string, t: (key: string) => string): string {
  return lastError === 'maybe-sent' ? t('common.outboxPanel.maybeSent') : lastError
}
