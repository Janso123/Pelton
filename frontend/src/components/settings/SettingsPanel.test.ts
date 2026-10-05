import { describe, expect, it } from 'vitest'
import en from '../../lib/locales/en'

describe('SettingsPanel sync settings copy', () => {
  it('explains message limit as local bodies per folder with JMAP stub note', () => {
    const hint = en['settingsPanel.hint.syncMessageLimit']
    expect(hint).toMatch(/bodies/i)
    expect(hint).toMatch(/JMAP/i)
    expect(hint).toMatch(/0 = all/i)
  })

  it('explains parallel pool as IMAP sessions vs JMAP sync HTTP', () => {
    const hint = en['settingsPanel.hint.syncMaxParallel']
    expect(hint).toMatch(/IMAP/i)
    expect(hint).toMatch(/JMAP/i)
    expect(hint).toMatch(/HTTP/i)
    expect(hint).toMatch(/push/i)
  })
})
