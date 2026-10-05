<script lang="ts">
  // the search field at the top of the message list. besides free text it accepts
  // typed keyword chips (from:/sender:, to:, subject:, has:attachment), a folder
  // chip (in:/folder:, a folder or all) and date chips (before:/after:). Without
  // a folder chip the search stays in the folder or view the list is showing. a keyword token is committed to a chip on space or
  // enter; a dropdown suggests filters on focus and values once a filter is typed
  // (addresses for from:/to:); the calendar button inserts before/after date
  // chips. everything is emitted as free text plus a structured SearchFilter so
  // the list re-runs the ranked search.
  import { createEventDispatcher, tick } from 'svelte'
  import { IconSearch, IconX, IconCalendar, IconBookmarkPlus, IconArrowsSort, IconCheck } from '@tabler/icons-svelte'
  import { prefs } from '../../stores/prefs'
  import { shortcutLabel, t } from '../../lib/i18n'
  import { searchAddresses } from '../../lib/api'
  import type { AddressBookEntry, Folder } from '../../lib/types'
  import { emptyFilter, type SearchFilter } from '../../stores/messages'
  import { sidebar } from '../../stores/accounts'
  import { selection } from '../../stores/selection'
  import { accountLabel } from '../../lib/format'
  import { activeSortPref, searchSortKind } from '../../stores/messages'
  import { searchSorts, automaticSort } from '../../lib/searchsort'
  import type { SearchSortPref } from '../../lib/types'
  import { selectionEpoch } from '../../stores/selection'
  import { openViewEditor } from '../../stores/views'
  import DateTimePicker from '../common/DateTimePicker.svelte'

  export let value: string = ''

  // switching folder/view clears the query upstream (searchQuery), so drop any
  // typed text and chips here to match. Keyed off the epoch of user selections,
  // not the selection object, which a language switch rewrites.
  let lastEpoch = $selectionEpoch
  $: if ($selectionEpoch !== lastEpoch) {
    lastEpoch = $selectionEpoch
    text = ''
    chips = []
  }

  const searchHint = shortcutLabel('mod+f')
  const dispatch = createEventDispatcher<{ search: string; filter: SearchFilter; sort: SearchSortPref }>()

  // chip fields and the aliases that produce them ("sender:" -> from).
  type ChipField = 'from' | 'to' | 'subject' | 'has' | 'is' | 'in' | 'before' | 'after'
  const alias: Record<string, ChipField> = {
    from: 'from',
    sender: 'from',
    to: 'to',
    subject: 'subject',
    has: 'has',
    is: 'is',
    in: 'in',
    folder: 'in',
    before: 'before',
    after: 'after',
  }
  interface Chip {
    field: ChipField
    value: string
  }

  // a dropdown row: a keyword to complete ("from:"), a whole chip to add
  // (has:attachment, or an address after from:/to:), or the date picker.
  type Suggestion =
    | { kind: 'keyword'; keyword: string; field: ChipField }
    | { kind: 'chip'; chip: Chip; label: string; note?: string }
    | { kind: 'date' }

  // the filters offered, in the order the dropdown lists them. has: and is: each
  // have one value, so they are offered whole. sender: and folder: are aliases
  // of from: and in: and only show up when typed towards.
  const filterSuggestions: Suggestion[] = [
    { kind: 'keyword', keyword: 'from', field: 'from' },
    { kind: 'keyword', keyword: 'sender', field: 'from' },
    { kind: 'keyword', keyword: 'to', field: 'to' },
    { kind: 'keyword', keyword: 'subject', field: 'subject' },
    { kind: 'chip', chip: { field: 'has', value: 'attachment' }, label: 'has:attachment' },
    { kind: 'chip', chip: { field: 'is', value: 'unread' }, label: 'is:unread' },
    { kind: 'keyword', keyword: 'in', field: 'in' },
    { kind: 'keyword', keyword: 'folder', field: 'in' },
    { kind: 'keyword', keyword: 'after', field: 'after' },
    { kind: 'keyword', keyword: 'before', field: 'before' },
  ]

  // the in: chip's value for every folder.
  const allFolders = 'all'

  // the folders in:/folder: offers, the current mailbox's first so a bare name
  // resolves to the folder of that name where the search was started. A folder
  // that cannot hold mail (\Noselect) has nothing to find.
  interface FolderOption {
    folder: Folder
    note: string
  }
  $: folderOptions = listFolderOptions($sidebar.data, $selection)
  $: folderById = new Map(folderOptions.map((o) => [o.folder.id, o.folder]))

  function listFolderOptions(data: typeof $sidebar.data, sel: typeof $selection): FolderOption[] {
    if (!data) {
      return []
    }
    const current = sel.kind === 'folder' ? sel.accountId : 0
    const accounts = [...data.accounts].sort((a, b) => Number(b.id === current) - Number(a.id === current))
    const out: FolderOption[] = []
    for (const acc of accounts) {
      for (const folder of data.foldersByAccount[acc.id] ?? []) {
        // a folder with no attributes arrives as null rather than [] (the store
        // keeps them as one joined string). Reading it as an array threw inside
        // a reactive statement, which stopped the bar updating at all: a chip
        // could not be removed and switching folder did not clear the search.
        if ((folder.attributes ?? []).some((a) => a.toLowerCase() === '\\noselect')) {
          continue
        }
        // a nested folder's name alone ("2024") says little, its path says where.
        const path = folder.imapPath !== folder.name ? folder.imapPath : ''
        const parts = data.accounts.length > 1 ? [path, accountLabel(acc)] : [path]
        out.push({ folder, note: parts.filter(Boolean).join(' · ') })
      }
    }
    return out
  }

  // findFolder resolves a typed in: value to a folder by name, then by path.
  function findFolder(raw: string): Folder | undefined {
    const want = raw.toLowerCase()
    return (
      folderOptions.find((o) => o.folder.name.toLowerCase() === want)?.folder ??
      folderOptions.find((o) => o.folder.imapPath.toLowerCase() === want)?.folder
    )
  }

  function suggestionKey(s: Suggestion): string {
    if (s.kind === 'keyword') {
      return `${s.keyword}:`
    }
    if (s.kind === 'chip') {
      // field and value rather than the label: two mailboxes both have an
      // Inbox, and the rows are keyed on this.
      return `${s.chip.field}:${s.chip.value}`
    }
    return 'date'
  }

  let chips: Chip[] = []
  // free text (and the keyword currently being typed) live in the input.
  let text = value
  let inputEl: HTMLInputElement
  let timer: ReturnType<typeof setTimeout> | undefined
  let showDate = false
  let afterDate = ''
  let beforeDate = ''

  const day = 86400

  // fieldLabel is the human label shown on a chip.
  function fieldLabel(field: ChipField): string {
    return $t(`messageList.search.chip.${field}`)
  }

  // chipText renders a chip's value; has:attachment and is:unread each stand for
  // one thing, so they read as a plain label rather than a field and a value.
  function chipText(chip: Chip): string {
    if (chip.field === 'has') {
      return $t('messageList.search.chip.hasAttachment')
    }
    if (chip.field === 'is') {
      return $t('messageList.search.chip.isUnread')
    }
    if (chip.field === 'in') {
      if (chip.value === allFolders) {
        return $t('messageList.search.chip.inAll')
      }
      return `${fieldLabel('in')}: ${folderById.get(Number(chip.value))?.name ?? chip.value}`
    }
    return `${fieldLabel(chip.field)}: ${chip.value}`
  }

  // parseToken turns a "keyword:value" token into a chip, or null if it is not a
  // recognized, valid keyword token.
  function parseToken(token: string): Chip | null {
    const at = token.indexOf(':')
    if (at <= 0) {
      return null
    }
    const field = alias[token.slice(0, at).toLowerCase()]
    const raw = token.slice(at + 1).trim()
    if (!field || raw === '') {
      return null
    }
    if (field === 'has') {
      return raw.toLowerCase().startsWith('attach') ? { field, value: 'attachment' } : null
    }
    if (field === 'is') {
      return raw.toLowerCase().startsWith('unread') ? { field, value: 'unread' } : null
    }
    if (field === 'in') {
      if (raw.toLowerCase() === allFolders) {
        return { field, value: allFolders }
      }
      const folder = findFolder(raw)
      return folder ? { field, value: String(folder.id) } : null
    }
    if ((field === 'before' || field === 'after') && Number.isNaN(Date.parse(raw))) {
      return null
    }
    return { field, value: raw }
  }

  // addChip stores a chip, replacing any existing chip of the same field so each
  // constraint appears once.
  function addChip(chip: Chip): void {
    chips = [...chips.filter((c) => c.field !== chip.field), chip]
    emit()
  }

  function removeChip(index: number): void {
    chips = chips.filter((_, i) => i !== index)
    emit()
  }

  // buildFilter maps the chips onto the structured SearchFilter.
  function buildFilter(): SearchFilter {
    const f: SearchFilter = { ...emptyFilter }
    for (const c of chips) {
      if (c.field === 'from') {
        f.from = c.value
      } else if (c.field === 'to') {
        f.to = c.value
      } else if (c.field === 'subject') {
        f.subject = c.value
      } else if (c.field === 'has') {
        f.hasAttachment = true
      } else if (c.field === 'is') {
        f.unreadOnly = true
      } else if (c.field === 'in') {
        f.folder = c.value === allFolders ? 'all' : Number(c.value)
      } else if (c.field === 'after') {
        f.afterUnix = Math.floor(Date.parse(c.value) / 1000) || 0
      } else if (c.field === 'before') {
        // include the whole "before" day.
        f.beforeUnix = Math.floor(Date.parse(c.value) / 1000) + day - 1 || 0
      }
    }
    return f
  }

  // emit debounces and dispatches the current free text and chip filter.
  function emit(): void {
    clearTimeout(timer)
    timer = setTimeout(() => {
      dispatch('search', text.trim())
      dispatch('filter', buildFilter())
    }, 180)
  }

  // the token currently being typed (after the last space), used for autocomplete.
  $: partial = text.slice(text.lastIndexOf(' ') + 1)

  // the dropdown shows while the input has focus, until Escape dismisses it;
  // typing or clicking the input brings it back. highlight is the row Enter and
  // Tab act on; it follows the keyboard only, since a row the pointer happens to
  // rest on must not take Enter from the text being searched.
  let focused = false
  let dismissed = false
  let highlight = -1
  let addresses: AddressBookEntry[] = []
  const listId = `search-suggest-${Math.random().toString(36).slice(2, 9)}`

  $: suggestions = suggestFor(partial, chips, addresses, folderOptions)
  $: showSuggest = focused && !dismissed && suggestions.length > 0
  $: resetHighlight(suggestions, partial)
  $: lookupAddresses(partial)

  // suggestFor lists the dropdown rows for the token being typed. Without a colon
  // it offers the filters, all of them on an empty token minus the ones already
  // applied; after one it offers values for that filter.
  function suggestFor(token: string, current: Chip[], found: AddressBookEntry[], folders: FolderOption[]): Suggestion[] {
    const applied = new Set(current.map((c) => c.field))
    const at = token.indexOf(':')
    if (at < 0) {
      const typed = token.toLowerCase()
      return filterSuggestions.filter((s) => {
        if (s.kind === 'chip' && applied.has(s.chip.field)) {
          return false
        }
        if (typed === '') {
          return s.kind !== 'keyword' || (s.keyword !== 'sender' && s.keyword !== 'folder' && !applied.has(s.field))
        }
        return suggestionKey(s).startsWith(typed)
      })
    }
    const field = alias[token.slice(0, at).toLowerCase()]
    const value = token.slice(at + 1).toLowerCase()
    if (field === 'from' || field === 'to') {
      return found.map((e) => ({ kind: 'chip', chip: { field, value: e.email }, label: e.email, note: e.name }))
    }
    if (field === 'has' || field === 'is') {
      return filterSuggestions.filter(
        (s) => s.kind === 'chip' && s.chip.field === field && s.chip.value.startsWith(value) && !applied.has(field),
      )
    }
    if (field === 'in') {
      return folderSuggestions(value, folders)
    }
    if (field === 'after' || field === 'before') {
      return [{ kind: 'date' }]
    }
    return []
  }

  // the most folder rows the dropdown lists; the rest are reached by typing more
  // of the name.
  const maxFolderSuggestions = 8

  // folderSuggestions offers every folder first, then folders whose name holds
  // what was typed, those starting with it ahead of the rest.
  function folderSuggestions(value: string, folders: FolderOption[]): Suggestion[] {
    const out: Suggestion[] = []
    if (allFolders.startsWith(value)) {
      out.push({
        kind: 'chip',
        chip: { field: 'in', value: allFolders },
        label: `in:${allFolders}`,
        note: $t('messageList.search.suggest.inAll'),
      })
    }
    const matching = folders.filter((o) => o.folder.name.toLowerCase().includes(value))
    const ranked = [
      ...matching.filter((o) => o.folder.name.toLowerCase().startsWith(value)),
      ...matching.filter((o) => !o.folder.name.toLowerCase().startsWith(value)),
    ]
    for (const o of ranked.slice(0, maxFolderSuggestions)) {
      out.push({
        kind: 'chip',
        chip: { field: 'in', value: String(o.folder.id) },
        label: o.folder.name,
        note: o.note || undefined,
      })
    }
    return out
  }

  // only a value (after the colon) is highlighted up front. A filter name is
  // not, because a word being searched can start like one: Enter on "invoice
  // to" has to search for it, not turn it into "to:". Tab still completes it.
  function resetHighlight(list: Suggestion[], token: string): void {
    highlight = token.includes(':') && list.length > 0 ? 0 : -1
  }

  // lookupAddresses fetches address suggestions for a from:/to: value, debounced
  // like the compose fields, dropping answers that arrive after a newer query.
  let lookupSeq = 0
  let lookupTimer: ReturnType<typeof setTimeout> | undefined
  function lookupAddresses(token: string): void {
    clearTimeout(lookupTimer)
    const seq = ++lookupSeq
    const at = token.indexOf(':')
    const field = at > 0 ? alias[token.slice(0, at).toLowerCase()] : undefined
    const query = token.slice(at + 1).trim()
    if ((field !== 'from' && field !== 'to') || query === '') {
      addresses = []
      return
    }
    lookupTimer = setTimeout(async () => {
      let found: AddressBookEntry[] = []
      try {
        found = await searchAddresses(query, 6)
      } catch {
        found = []
      }
      if (seq === lookupSeq) {
        addresses = found
      }
    }, 140)
  }

  function onInput(event: Event): void {
    text = (event.currentTarget as HTMLInputElement).value
    dismissed = false
    // a trailing space commits a completed keyword token immediately.
    if (text.endsWith(' ')) {
      commitTrailingToken()
    }
    emit()
  }

  // commitTrailingToken pulls a finished "keyword:value" token out of the input
  // into a chip, leaving the remaining free text behind.
  function commitTrailingToken(): void {
    const parts = text.split(/\s+/).filter(Boolean)
    if (parts.length === 0) {
      return
    }
    const last = parts[parts.length - 1]
    const chip = parseToken(last)
    if (chip) {
      addChip(chip)
      text = parts.slice(0, -1).join(' ')
      text = text ? `${text} ` : ''
    }
  }

  // withoutPartial is the input text with the token being typed removed.
  function withoutPartial(): string {
    const rest = text.slice(0, text.lastIndexOf(' ') + 1).replace(/\s*$/, '')
    return rest ? `${rest} ` : ''
  }

  function onKeydown(event: KeyboardEvent): void {
    if (showSuggest) {
      if (event.key === 'ArrowDown' || event.key === 'ArrowUp') {
        event.preventDefault()
        const n = suggestions.length
        const delta = event.key === 'ArrowDown' ? 1 : -1
        highlight = highlight < 0 ? (delta > 0 ? 0 : n - 1) : (highlight + delta + n) % n
        return
      }
      if (event.key === 'Escape') {
        event.preventDefault()
        dismissed = true
        return
      }
      if ((event.key === 'Enter' || event.key === 'Tab') && highlight >= 0) {
        event.preventDefault()
        pick(suggestions[highlight])
        return
      }
      if (event.key === 'Tab' && partial !== '') {
        event.preventDefault()
        pick(suggestions[0])
        return
      }
    }
    if (event.key === 'Enter') {
      const chip = parseToken(partial)
      if (chip) {
        event.preventDefault()
        addChip(chip)
        text = withoutPartial()
        emit()
      }
      return
    }
    if (event.key === 'Backspace' && text === '' && chips.length > 0) {
      removeChip(chips.length - 1)
    }
  }

  // pick applies a dropdown row: a keyword is completed in the input for its
  // value to be typed, a chip row is added straight away, and the date row opens
  // the calendar popover in place of a typed date.
  async function pick(s: Suggestion): Promise<void> {
    if (s.kind === 'keyword') {
      text = `${withoutPartial()}${s.keyword}:`
    } else if (s.kind === 'chip') {
      text = withoutPartial()
      addChip(s.chip)
    } else {
      text = withoutPartial()
      emit()
      // the input keeps focus, so the filter list would open over the calendar.
      dismissed = true
      showDate = true
      return
    }
    await tick()
    inputEl?.focus()
  }

  function clearAll(): void {
    clearTimeout(timer)
    text = ''
    chips = []
    // the popover's own pickers, or reopening it offers the dates that were
    // just cleared as though they were still applied.
    afterDate = ''
    beforeDate = ''
    dispatch('search', '')
    dispatch('filter', { ...emptyFilter })
  }

  // applyDates turns the popover's picked dates into before/after chips.
  function applyDates(): void {
    if (afterDate) {
      addChip({ field: 'after', value: afterDate })
    }
    if (beforeDate) {
      addChip({ field: 'before', value: beforeDate })
    }
    showDate = false
  }

  $: hasContent = text !== '' || chips.length > 0

  // the placeholder names where a search will look, since that is now the folder
  // or view on screen rather than every folder. A saved view is already a search
  // over every folder, so it keeps the plain wording.
  $: placeholder =
    $selection.kind === 'savedView'
      ? $t('messageList.search.placeholder')
      : $t('messageList.search.placeholderIn').replace('{name}', $selection.label)

  let showSort = false

  // closing the popovers when the search is cleared, so neither reopens over a
  // list that is no longer a result set.
  $: if (!hasContent) {
    showSort = false
  }

  // what "automatic" resolves to right now, shown beside the option so the
  // choice is not between a named order and a word that explains nothing.
  $: autoLabel = $searchSortKind ? $t(`messageList.search.sort.${automaticSort($searchSortKind)}`) : ''

  // the list owns re-running the search: reordering the rows has to reset the
  // scroll and the selection the same way any other search does.
  function pickSort(pref: SearchSortPref): void {
    dispatch('sort', pref)
    showSort = false
  }

  // viewName is what the new view is called before the user renames it.
  //
  // A keyword token becomes a chip as soon as it is typed, which leaves the text
  // box empty: searching "from:bob" and pressing save gave the editor a blank
  // name, and a view cannot be saved without one. The save button was then
  // greyed out with nothing on screen saying why. The chips describe the search
  // just as well, so they name it.
  function viewName(): string {
    const typed = text.trim()
    if (typed !== '') {
      return typed
    }
    return chips.map(chipText).join(' ')
  }

  // saveAsView opens the view editor seeded from the current query and chips, so
  // a search the user just ran becomes a saved View. The relative date window is
  // left for the editor since the chips carry absolute dates.
  function saveAsView(): void {
    const f = buildFilter()
    openViewEditor({
      name: viewName(),
      queryText: text.trim(),
      queryFrom: f.from ? [f.from] : [],
      queryTo: f.to ? [f.to] : [],
      querySubject: f.subject,
      hasAttachment: f.hasAttachment,
      unreadOnly: f.unreadOnly,
    })
  }
</script>

<div class="bar">
  <div class="search">
    <IconSearch size={15} stroke={1.6} class="search-icon" />
    <div class="field">
      {#each chips as chip, i (chip.field + chip.value)}
        <span class="chip">
          <span class="chip-text">{chipText(chip)}</span>
          <button type="button" class="chip-x" aria-label={$t('messageList.search.removeChip')} on:click={() => removeChip(i)}>
            <IconX size={11} stroke={2} />
          </button>
        </span>
      {/each}
      <input
        type="text"
        bind:this={inputEl}
        placeholder={chips.length === 0 ? placeholder : ''}
        aria-label={$t('messageList.search.placeholder')}
        role="combobox"
        aria-autocomplete="list"
        aria-expanded={showSuggest}
        aria-controls={listId}
        aria-activedescendant={showSuggest && highlight >= 0 ? `${listId}-${highlight}` : undefined}
        autocomplete="off"
        value={text}
        on:input={onInput}
        on:keydown={onKeydown}
        on:focus={() => (focused = true)}
        on:mousedown={() => (dismissed = false)}
        on:blur={() => {
          focused = false
          dismissed = false
        }}
      />
    </div>
    {#if hasContent}
      <button type="button" class="clear" aria-label={$t('messageList.search.clearSearch')} on:click={clearAll}>
        <IconX size={14} stroke={1.8} />
      </button>
    {:else if $prefs.showShortcutHints}
      <kbd class="hint">{searchHint}</kbd>
    {/if}

    {#if showSuggest}
      <div class="autocomplete" id={listId} role="listbox">
        {#each suggestions as s, i (suggestionKey(s))}
          <!-- mousedown is held back so the input keeps focus and the list stays
               open until the click lands. -->
          <button
            type="button"
            id={`${listId}-${i}`}
            class="ac-opt"
            class:active={i === highlight}
            role="option"
            aria-selected={i === highlight}
            tabindex="-1"
            on:mousedown|preventDefault
            on:click={() => pick(s)}
          >
            {#if s.kind === 'keyword'}
              <span class="ac-key">{s.keyword}:</span>
              <span class="ac-desc">{$t(`messageList.search.suggest.${s.keyword}`)}</span>
            {:else if s.kind === 'chip'}
              <span class="ac-key">{s.label}</span>
              <span class="ac-desc">{s.note ?? $t(`messageList.search.suggest.${s.chip.field}`)}</span>
            {:else}
              <IconCalendar size={13} stroke={1.7} class="ac-icon" />
              <span class="ac-desc">{$t('messageList.search.suggest.pickDate')}</span>
            {/if}
          </button>
        {/each}
      </div>
    {/if}
  </div>

  {#if hasContent && $prefs.viewsPlacement !== 'hidden'}
    <button
      type="button"
      class="filter-btn"
      aria-label={$t('views.saveAsView')}
      title={$t('views.saveAsView')}
      on:click={saveAsView}
    >
      <IconBookmarkPlus size={16} stroke={1.7} />
    </button>
  {/if}

  {#if $searchSortKind}
    <div class="filter-wrap">
      <button
        type="button"
        class="filter-btn"
        aria-label={$t('messageList.search.sortBy')}
        aria-expanded={showSort}
        title={$t('messageList.search.sortBy')}
        on:click={() => (showSort = !showSort)}
      >
        <IconArrowsSort size={16} stroke={1.7} />
      </button>

      {#if showSort}
        <!-- svelte-ignore a11y-click-events-have-key-events a11y-no-static-element-interactions -->
        <div class="scrim" on:click={() => (showSort = false)}></div>
        <div class="menu sort-menu" role="menu">
          <span class="menu-label">{$t('messageList.search.sortBy')}</span>
          <button
            type="button"
            class="sort-opt"
            role="menuitemradio"
            aria-checked={$activeSortPref === 'auto'}
            on:click={() => pickSort('auto')}
          >
            <span class="sort-check">
              {#if $activeSortPref === 'auto'}<IconCheck size={13} stroke={2} />{/if}
            </span>
            <span class="sort-name">{$t('messageList.search.sort.auto')}</span>
            <span class="sort-note">{autoLabel}</span>
          </button>
          {#each searchSorts as option (option)}
            <button
              type="button"
              class="sort-opt"
              role="menuitemradio"
              aria-checked={$activeSortPref === option}
              on:click={() => pickSort(option)}
            >
              <span class="sort-check">
                {#if $activeSortPref === option}<IconCheck size={13} stroke={2} />{/if}
              </span>
              <span class="sort-name">{$t(`messageList.search.sort.${option}`)}</span>
            </button>
          {/each}
        </div>
      {/if}
    </div>
  {/if}

  <div class="filter-wrap">
    <button
      type="button"
      class="filter-btn"
      aria-label={$t('messageList.search.filterByDate')}
      aria-expanded={showDate}
      title={$t('messageList.search.filterByDate')}
      on:click={() => (showDate = !showDate)}
    >
      <IconCalendar size={16} stroke={1.7} />
    </button>

    {#if showDate}
      <!-- svelte-ignore a11y-click-events-have-key-events a11y-no-static-element-interactions -->
      <div class="scrim" on:click={() => (showDate = false)}></div>
      <div class="menu" role="menu">
        <span class="menu-label">{$t('messageList.search.dateRange')}</span>
        <div class="date">
          <span>{$t('messageList.search.chip.after')}</span>
          <div class="date-picker">
            <DateTimePicker mode="date" bind:value={afterDate} />
          </div>
        </div>
        <div class="date">
          <span>{$t('messageList.search.chip.before')}</span>
          <div class="date-picker">
            <DateTimePicker mode="date" bind:value={beforeDate} />
          </div>
        </div>
        <div class="menu-actions">
          <button type="button" class="primary" on:click={applyDates}>{$t('messageList.search.apply')}</button>
        </div>
      </div>
    {/if}
  </div>
</div>

<style>
  .bar {
    display: flex;
    align-items: center;
    gap: var(--space-2);
  }

  .search {
    position: relative;
    flex: 1;
    /* without this the box refuses to shrink below the chips it contains: a
       flex item's automatic minimum is its min-content, and the chips are
       nowrap. The list column is a fixed width, so the bar grew straight over
       the reading pane instead. The chips scroll inside .field. */
    min-width: 0;
    display: flex;
    align-items: center;
    gap: var(--space-2);
    padding: 0 var(--space-3);
    min-height: var(--control-height);
    border: var(--hairline) solid var(--border-default);
    border-radius: var(--radius-control);
    background: var(--surface-sunken);
    color: var(--text-tertiary);
  }

  .search:focus-within {
    border-color: var(--accent);
  }

  .field {
    flex: 1;
    min-width: 0;
    display: flex;
    /* one row, always. A before: and an after: chip together are wider than
       the field, and wrapping made the whole search bar grow a second line and
       shove the list down. Chips scroll sideways instead. */
    flex-wrap: nowrap;
    overflow-x: auto;
    scrollbar-width: none;
    align-items: center;
    gap: var(--space-1);
    padding: 3px 0;
  }

  .field::-webkit-scrollbar {
    display: none;
  }

  .chip {
    display: inline-flex;
    align-items: center;
    gap: var(--space-1);
    padding: 1px var(--space-1) 1px var(--space-2);
    border-radius: var(--radius-control);
    background: var(--selection-bg);
    color: var(--text-primary);
    font-size: var(--fz-meta);
    white-space: nowrap;
  }

  .chip-x {
    display: inline-flex;
    border: none;
    background: transparent;
    color: var(--text-tertiary);
    cursor: var(--cursor-action);
    padding: 1px;
    border-radius: var(--radius-control);
  }
  .chip-x:hover {
    color: var(--text-primary);
  }

  input[type='text'] {
    flex: 1;
    min-width: 80px;
    border: none;
    background: transparent;
    outline: none;
    font-size: var(--fz-list);
    color: var(--text-primary);
  }

  .clear {
    display: inline-flex;
    border: none;
    background: transparent;
    color: var(--text-tertiary);
    cursor: var(--cursor-action);
    padding: 2px;
    border-radius: var(--radius-control);
  }
  .clear:hover {
    color: var(--text-primary);
  }

  .hint {
    flex-shrink: 0;
    padding: 1px var(--space-2);
    border: var(--hairline) solid var(--border-default);
    border-radius: var(--radius-control);
    background: var(--surface-raised);
    color: var(--text-tertiary);
    font-family: var(--font-mono);
    font-size: var(--fz-meta);
    line-height: 1.4;
  }

  .autocomplete {
    position: absolute;
    top: calc(100% + var(--space-1));
    left: 0;
    right: 0;
    z-index: 41;
    padding: var(--space-1);
    border: var(--hairline) solid var(--border-default);
    border-radius: var(--radius-card);
    background: var(--surface-overlay);
    box-shadow: var(--shadow-overlay);
  }

  .ac-opt {
    display: flex;
    align-items: baseline;
    gap: var(--space-2);
    width: 100%;
    border: none;
    background: transparent;
    cursor: var(--cursor-action);
    text-align: start;
    padding: var(--space-2);
    border-radius: var(--radius-control);
  }
  .ac-opt:hover,
  .ac-opt.active {
    background: var(--surface-hover);
  }
  .ac-key {
    font-family: var(--font-mono);
    font-size: var(--fz-label);
    color: var(--accent);
  }
  .ac-desc {
    font-size: var(--fz-meta);
    color: var(--text-tertiary);
  }

  .filter-wrap {
    position: relative;
    flex-shrink: 0;
  }

  .filter-btn {
    display: inline-flex;
    align-items: center;
    justify-content: center;
    height: var(--control-height);
    width: var(--control-height);
    border: var(--hairline) solid var(--border-default);
    border-radius: var(--radius-control);
    background: var(--surface-sunken);
    color: var(--text-secondary);
    cursor: var(--cursor-action);
  }
  .filter-btn:hover {
    background: var(--surface-hover);
    color: var(--text-primary);
  }

  .scrim {
    position: fixed;
    inset: 0;
    z-index: 40;
  }

  .menu {
    position: absolute;
    top: calc(100% + var(--space-1));
    inset-inline-end: 0;
    z-index: 41;
    width: 232px;
    padding: var(--space-3);
    border: var(--hairline) solid var(--border-default);
    border-radius: var(--radius-card);
    background: var(--surface-overlay);
    box-shadow: var(--shadow-overlay);
  }

  .sort-menu {
    width: 208px;
    padding: var(--space-1);
  }

  .sort-menu .menu-label {
    padding: var(--space-2) var(--space-2) var(--space-1);
    margin: 0;
  }

  .sort-opt {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    width: 100%;
    border: none;
    background: transparent;
    color: var(--text-primary);
    cursor: var(--cursor-action);
    text-align: start;
    padding: var(--space-2);
    border-radius: var(--radius-control);
    font-size: var(--fz-label);
  }
  .sort-opt:hover {
    background: var(--surface-hover);
  }

  /* the tick keeps its column whether or not it is showing, so the labels do
     not shift sideways as the choice moves. */
  .sort-check {
    display: inline-flex;
    justify-content: center;
    width: 13px;
    flex-shrink: 0;
    color: var(--accent);
  }

  .sort-name {
    flex: 1;
  }

  .sort-note {
    color: var(--text-tertiary);
    font-size: var(--fz-meta);
  }

  .menu-label {
    display: block;
    font-size: var(--fz-meta);
    color: var(--text-tertiary);
    margin: 0 0 var(--space-2);
  }

  .date {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-2);
    padding-bottom: var(--space-2);
  }
  .date span {
    font-size: var(--fz-label);
    color: var(--text-secondary);
  }
  .date-picker {
    width: 132px;
  }

  .menu-actions {
    display: flex;
    justify-content: flex-end;
    padding-top: var(--space-1);
  }

  .primary {
    padding: var(--space-1) var(--space-3);
    border-radius: var(--radius-control);
    border: none;
    background: var(--accent);
    color: var(--accent-fg);
    font-size: var(--fz-label);
    cursor: var(--cursor-action);
  }
</style>
