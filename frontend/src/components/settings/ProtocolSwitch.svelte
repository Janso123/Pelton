<script lang="ts">
  // the IMAP / JMAP switch in the mailbox editor: probes the server, reviews a
  // JMAP certificate the mailbox does not trust yet, and switches the stored
  // protocol. The editor keeps the draft and the account list; it applies the
  // result when this calls `onSwitched`.
  import { onDestroy } from 'svelte'
  import ToggleSwitch from '../common/ToggleSwitch.svelte'
  import CertificateReview from '../common/CertificateReview.svelte'
  import { probeAccount, switchProtocol, probeAccountCertificatesFor, trustAccountCertificate } from '../../lib/api'
  import { missingPassword } from '../../stores/passwordprompt'
  import { errorMessage, toastError } from '../../stores/toast'
  import type { Account, TestConnectionResult, UntrustedCert } from '../../lib/types'
  import { t } from '../../lib/i18n'
  import { protocolSwitchError } from '../../lib/errors'

  // the account being edited (the editor's draft).
  export let account: Account
  // shared with the editor's own certificate buttons, so they all lock together.
  export let certBusy = false
  // reads the stored account back after a certificate is trusted.
  export let reloadTrust: (id: number) => Promise<void>
  // the switch committed on the backend; called even when the editor was
  // closed meanwhile, so the editor decides whether the result still applies.
  export let onSwitched: (id: number, protocol: 'imap' | 'jmap') => void

  // JMAP probe for the account being edited; null until ProbeAccount returns.
  let probe: TestConnectionResult | null = null
  let protocolError = ''
  let switchingProtocol = false
  // what the JMAP server presents that the mailbox does not trust, held for
  // review before switching to it; null when there is nothing to review.
  let jmapCerts: UntrustedCert[] | null = null
  let loadedId: number | null = null
  // set once the editor closes, so a switch still in flight changes nothing.
  let gone = false

  onDestroy(() => {
    gone = true
  })

  // a different mailbox in the editor starts from a clean state.
  $: if (account.id !== loadedId) {
    loadedId = account.id
    probe = null
    protocolError = ''
    switchingProtocol = false
    jmapCerts = null
    if (!account.local) {
      void loadProbe(account.id)
    }
  }

  $: jmapUnavailable = probe !== null && !probe.jmapAvailable
  $: jmapChecking = !account.local && probe === null
  // enabling JMAP is blocked when the probe says it is unavailable; switching
  // back to IMAP stays allowed even then, so a stored JMAP protocol is kept.
  $: jmapToggleDisabled =
    switchingProtocol ||
    jmapChecking ||
    jmapCerts !== null ||
    (!account.local && jmapUnavailable && account.protocol !== 'jmap')

  async function loadProbe(accountId: number): Promise<void> {
    try {
      probe = await probeAccount(accountId)
    } catch {
      // a failed probe is treated as unavailable: the stored protocol stays,
      // and switching to JMAP is not offered.
      probe = {
        jmapAvailable: false,
        jmapWebSocket: false,
        jmapSessionURL: '',
        jmapMailAccountID: '',
      }
    }
  }

  async function setUseJmap(on: boolean): Promise<void> {
    if (account.local || switchingProtocol || jmapChecking) {
      return
    }
    const next = on ? 'jmap' : 'imap'
    if (next === account.protocol) {
      return
    }
    if (next === 'jmap' && jmapUnavailable) {
      return
    }
    switchingProtocol = true
    protocolError = ''
    const accountId = account.id
    try {
      // the switch signs in to the JMAP server, so a certificate it does not
      // trust is reviewed first; until it is trusted nothing is switched.
      if (next === 'jmap') {
        const certs = await probeAccountCertificatesFor(accountId, 'jmap')
        if (gone || account.id !== accountId) {
          return
        }
        if (certs.length > 0) {
          jmapCerts = certs
          return
        }
      }
      await switchProtocol(accountId, next)
      onSwitched(accountId, next)
    } catch (err) {
      protocolError = protocolSwitchError(err)
    } finally {
      switchingProtocol = false
    }
  }

  // trustJmapAndSwitch pins the JMAP certificate the user reviewed, then
  // switches; the switch checks the certificates again before signing in.
  async function trustJmapAndSwitch(event: CustomEvent<UntrustedCert[]>): Promise<void> {
    const id = account.id
    certBusy = true
    try {
      for (const fp of new Set(event.detail.map((c) => c.fingerprint))) {
        await trustAccountCertificate(id, fp)
      }
      jmapCerts = null
      await reloadTrust(id)
    } catch (err) {
      toastError(errorMessage(err))
      return
    } finally {
      certBusy = false
    }
    // the editor may have closed or moved to another mailbox meanwhile, and
    // switching that one would delete its cached mail.
    if (gone || account.id !== id) {
      return
    }
    await setUseJmap(true)
  }

  function keepImap(): void {
    jmapCerts = null
  }
</script>

<div class="toggle">
  <span class:muted={jmapToggleDisabled}>{$t('settings.protocol.jmap')}</span>
  <ToggleSwitch
    checked={account.protocol === 'jmap'}
    label={$t('settings.protocol.jmap')}
    disabled={jmapToggleDisabled}
    on:change={(e) => void setUseJmap(e.detail)}
  />
</div>
{#if jmapChecking}
  <p class="hint">{$t('settings.protocol.checking')}</p>
{:else if switchingProtocol}
  <p class="hint">{$t('settings.protocol.switching')}</p>
{:else if jmapUnavailable && !$missingPassword.has(account.id)}
  <!-- without a password the probe cannot sign in, which says nothing
       about the server; the password hint below is the real answer. -->
  <p class="hint">{$t('settings.protocol.unavailable')}</p>
{/if}
{#if protocolError}
  <p class="err">{protocolError}</p>
{/if}
{#if jmapCerts}
  <p class="hint">{$t('settings.protocol.certReview')}</p>
  <CertificateReview certs={jmapCerts} busy={certBusy} on:trust={trustJmapAndSwitch} />
  <button type="button" class="ghost small" disabled={certBusy} on:click={keepImap}>{$t('settings.protocol.keepImap')}</button>
{/if}

<style>
  .toggle {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-3);
    font-size: var(--fz-label);
    color: var(--text-primary);
  }

  .muted {
    opacity: 0.6;
  }

  .hint {
    margin: 0 0 var(--space-4);
    font-size: var(--fz-label);
    color: var(--text-tertiary);
    line-height: 1.5;
  }

  .err {
    margin: 0 0 var(--space-3);
    font-size: var(--fz-label);
    color: var(--danger);
    line-height: 1.5;
  }

  /* the same ghost button the editor uses. */
  .ghost {
    display: inline-flex;
    align-items: center;
    gap: var(--space-2);
    padding: var(--space-2) var(--space-4);
    border-radius: var(--radius-control);
    font-size: var(--fz-label);
    font-weight: var(--fw-medium);
    cursor: var(--cursor-action);
    border: var(--hairline) solid var(--border-default);
    background: transparent;
    color: var(--text-secondary);
  }
  .ghost.small {
    padding: var(--space-1) var(--space-3);
    font-size: var(--fz-meta);
  }
  .ghost:hover {
    background: var(--surface-hover);
    color: var(--text-primary);
  }
</style>
