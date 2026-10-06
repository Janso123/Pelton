<script lang="ts">
  // the wizard's JMAP step: the server answered a JMAP probe, so the user picks
  // JMAP or stays on IMAP. The wizard keeps the flow and acts on `choose`.
  import { createEventDispatcher } from 'svelte'
  import { IconArrowRight } from '@tabler/icons-svelte'
  import { t } from '../../lib/i18n'

  const dispatch = createEventDispatcher<{ choose: 'imap' | 'jmap' }>()
</script>

<h3>{$t('wizard.jmap.ask')}</h3>
<div class="start-choices">
  <button type="button" class="choice" on:click={() => dispatch('choose', 'jmap')}>
    <span class="choice-text">
      <span class="choice-title">{$t('wizard.jmap.yes')}</span>
    </span>
    <IconArrowRight size={16} stroke={1.8} />
  </button>
  <button type="button" class="choice" on:click={() => dispatch('choose', 'imap')}>
    <span class="choice-text">
      <span class="choice-title">{$t('wizard.jmap.no')}</span>
    </span>
    <IconArrowRight size={16} stroke={1.8} />
  </button>
</div>

<style>
  h3 {
    margin: 0 0 var(--space-2);
    font-size: var(--fz-title);
    font-weight: var(--fw-semibold);
  }

  /* the same choice rows as the wizard's start step. */
  .start-choices {
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
    margin-top: var(--space-4);
  }

  .choice {
    display: flex;
    align-items: center;
    gap: var(--space-4);
    width: 100%;
    padding: var(--space-4);
    border: var(--hairline) solid var(--border-default);
    border-radius: var(--radius-card);
    background: var(--surface-raised);
    color: var(--text-primary);
    text-align: start;
    cursor: pointer;
  }
  .choice:hover {
    background: var(--surface-hover);
    border-color: var(--accent);
  }

  .choice-text {
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
    flex: 1;
    min-width: 0;
  }
  .choice-title {
    font-size: var(--fz-body);
    font-weight: var(--fw-medium);
  }
</style>
