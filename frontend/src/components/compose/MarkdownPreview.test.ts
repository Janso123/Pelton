import { describe, expect, it } from 'vitest'
import { render } from '@testing-library/svelte'
import MarkdownPreview from './MarkdownPreview.svelte'

describe('MarkdownPreview', () => {
  const hostile = '\n\nOn x wrote:\n> hi\n> <img src=x onerror="window.__pwned=1">\n'

  it('never puts rendered markdown into the app document', () => {
    const { container } = render(MarkdownPreview, { markdown: hostile, font: '' })
    expect(container.querySelector('img')).toBeNull()
    const frame = container.querySelector('iframe')
    expect(frame).not.toBeNull()
  })

  it('renders into a frame that cannot run script', () => {
    const { container } = render(MarkdownPreview, { markdown: hostile, font: '' })
    const frame = container.querySelector('iframe') as HTMLIFrameElement
    expect(frame.getAttribute('sandbox')).toBe('')
    const srcdoc = frame.getAttribute('srcdoc') ?? ''
    expect(srcdoc).toContain("script-src 'none'")
    expect(srcdoc).toContain('<blockquote>')
  })

  it('sends link clicks to a new window instead of navigating the frame', () => {
    const { container } = render(MarkdownPreview, { markdown: '[x](https://example.com)', font: '' })
    const srcdoc = container.querySelector('iframe')?.getAttribute('srcdoc') ?? ''
    const base = srcdoc.indexOf('<base target="_blank">')
    expect(base).toBeGreaterThan(-1)
    expect(base).toBeLessThan(srcdoc.indexOf('<body>'))
  })

  it('drops a quoted link or form target that would override the base target', () => {
    const markdown = '> <a target="_self" href="https://x">x</a> <form target="_top"><button formtarget="_self">go</button></form>'
    const { container } = render(MarkdownPreview, { markdown, font: '' })
    const srcdoc = container.querySelector('iframe')?.getAttribute('srcdoc') ?? ''
    expect(srcdoc).toContain('href="https://x"')
    expect(srcdoc).not.toMatch(/\btarget="_self"|target="_top"|formtarget=/)
  })
})
