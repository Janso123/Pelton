import { describe, expect, it } from 'vitest'
import { looksLikeHtml, textToHtml } from './richtext'

describe('textToHtml', () => {
  it('keeps line breaks and paragraphs', () => {
    expect(textToHtml('one\ntwo\n\nthree')).toBe('<p>one<br>two</p><p>three</p>')
  })

  it('escapes markup', () => {
    expect(textToHtml('a <b> & "c"')).toBe('<p>a &lt;b&gt; &amp; "c"</p>')
  })

  it('turns "> " quoting into nested blockquotes', () => {
    expect(textToHtml('top\n> one\n> > two\n> back')).toBe(
      '<p>top</p><blockquote><p>one</p><blockquote><p>two</p></blockquote><p>back</p></blockquote>',
    )
  })

  it('reads ">>" and a bare ">" as quote levels', () => {
    expect(textToHtml('>> deep\n>\n> next')).toBe(
      '<blockquote><blockquote><p>deep</p></blockquote><p>next</p></blockquote>',
    )
  })
})

describe('looksLikeHtml', () => {
  it('tells the rich editor html from text', () => {
    expect(looksLikeHtml('<p>hi</p>')).toBe(true)
    expect(looksLikeHtml('<blockquote><p>x</p></blockquote>')).toBe(true)
    expect(looksLikeHtml('hi\n> quoted')).toBe(false)
    expect(looksLikeHtml('<3 you')).toBe(false)
  })
})
