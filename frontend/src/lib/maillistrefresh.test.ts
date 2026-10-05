import { describe, expect, it } from 'vitest'
import { shouldReplaceListOnMailNew } from './maillistrefresh'

describe('shouldReplaceListOnMailNew', () => {
  it('keeps the list during body sync', () => {
    expect(
      shouldReplaceListOnMailNew({ syncPhase: 'bodies', paginated: false, searching: false }),
    ).toBe(false)
    expect(
      shouldReplaceListOnMailNew({ syncPhase: 'bodies', paginated: true, searching: false }),
    ).toBe(false)
  })

  it('reloads on stub sync when only the first page is loaded', () => {
    expect(
      shouldReplaceListOnMailNew({ syncPhase: 'stubs', paginated: false, searching: false }),
    ).toBe(true)
    expect(shouldReplaceListOnMailNew({ syncPhase: '', paginated: false, searching: false })).toBe(true)
  })

  it('merges instead of replacing once the user has paged ahead', () => {
    expect(
      shouldReplaceListOnMailNew({ syncPhase: 'stubs', paginated: true, searching: false }),
    ).toBe(false)
    expect(shouldReplaceListOnMailNew({ syncPhase: '', paginated: true, searching: false })).toBe(false)
  })

  it('keeps search results whatever the sync phase or paging', () => {
    for (const syncPhase of ['stubs', '', 'bodies'] as const) {
      expect(
        shouldReplaceListOnMailNew({ syncPhase, paginated: false, searching: true }),
      ).toBe(false)
    }
  })
})
