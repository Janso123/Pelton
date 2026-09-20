import { describe, it, expect, afterEach, vi } from 'vitest'
import { flagFor } from './flags'
import { detectOSLocale, locales, localeNames } from './i18n'

// the flag module resolves urls from an eager glob of the bundled flag-icons
// set, so these assert on which country file was chosen rather than on the
// hashed asset name.
function countryOf(url: string | undefined): string | undefined {
  return url?.match(/flags\/4x3\/([a-z]{2})\.svg/)?.[1] ?? url?.split('/').pop()?.slice(0, 2)
}

function withSystemLanguage(tag: string) {
  vi.stubGlobal('navigator', { language: tag })
}

afterEach(() => {
  vi.unstubAllGlobals()
})

describe('flagFor', () => {
  it('follows the system region for a bare language', () => {
    withSystemLanguage('en-US')
    expect(countryOf(flagFor('en'))).toBe('us')
  })

  it('falls back to the language default when the region does not belong to it', () => {
    withSystemLanguage('de-DE')
    expect(countryOf(flagFor('en'))).toBe('gb')
  })

  it('keeps its own region for a region-qualified locale regardless of the system', () => {
    withSystemLanguage('zh-TW')
    expect(countryOf(flagFor('zh-CN'))).toBe('cn')
  })

  it('resolves zh-CN on a matching system too', () => {
    withSystemLanguage('zh-CN')
    expect(countryOf(flagFor('zh-CN'))).toBe('cn')
  })
})

describe('detectOSLocale', () => {
  it('recommends a region-qualified locale only for its own region', () => {
    withSystemLanguage('zh-CN')
    expect(detectOSLocale()).toBe('zh-CN')
    withSystemLanguage('zh-TW')
    expect(detectOSLocale()).toBe('en')
  })

  it('matches a bare language tag', () => {
    withSystemLanguage('de')
    expect(detectOSLocale()).toBe('de')
  })

  // the three webviews do not spell Chinese the same way, and only the first of
  // these is the exact locale id. Matching on the whole tag or its first two
  // letters recognizes that one and leaves the rest on English.
  it('recognizes simplified chinese however the system spells it', () => {
    for (const tag of ['zh-CN', 'zh-Hans-CN', 'zh-Hans', 'zh', 'zh-SG']) {
      withSystemLanguage(tag)
      expect(detectOSLocale(), tag).toBe('zh-CN')
    }
  })

  // traditional is a different script to read, so it is left on English rather
  // than pointed at a catalogue written in simplified characters.
  it('does not recommend simplified chinese to traditional systems', () => {
    for (const tag of ['zh-TW', 'zh-HK', 'zh-MO', 'zh-Hant', 'zh-Hant-TW']) {
      withSystemLanguage(tag)
      expect(detectOSLocale(), tag).toBe('en')
    }
  })

  it('falls back to english for an unsupported language', () => {
    withSystemLanguage('ja-JP')
    expect(detectOSLocale()).toBe('en')
  })
})

describe('zh-CN registration', () => {
  it('is offered in the picker under its own spelling', () => {
    expect(locales).toContain('zh-CN')
    expect(localeNames['zh-CN']).toBe('简体中文')
  })
})
