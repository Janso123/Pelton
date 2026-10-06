// richtext.ts turns plain text into html for the rich (wysiwyg) editor, which
// reads its content as html: plain text handed to it loses every line break.

// escapeHtml escapes the characters that would start markup in a text node.
export function escapeHtml(text: string): string {
  return text.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
}

// looksLikeHtml reports whether a body is the rich editor's html rather than
// plain text: it opens with one of the block tags the editor writes.
export function looksLikeHtml(body: string): boolean {
  return /^\s*<(p|blockquote|h[1-6]|ul|ol|pre|hr)[\s/>]/i.test(body)
}

// quotePrefix matches the "> " quote markers that open a line, "> > " and ">>"
// alike.
const quotePrefix = /^(?:>[ \t]?)+/

// textToHtml renders plain text as paragraphs: a blank line ends one, other
// line breaks become <br>. Lines quoted with "> " go into blockquotes, nested
// one level per marker.
export function textToHtml(text: string): string {
  let out = ''
  let open = 0
  let depth = 0
  let lines: string[] = []

  const flush = (): void => {
    if (lines.length === 0) {
      return
    }
    for (; open > depth; open--) out += '</blockquote>'
    for (; open < depth; open++) out += '<blockquote>'
    out += `<p>${lines.join('<br>')}</p>`
    lines = []
  }

  for (const raw of text.replace(/\r\n?/g, '\n').split('\n')) {
    const marker = raw.match(quotePrefix)?.[0] ?? ''
    const level = (marker.match(/>/g) ?? []).length
    const line = raw.slice(marker.length).trimEnd()
    if (level !== depth) {
      flush()
      depth = level
    }
    if (line === '') {
      flush()
      continue
    }
    lines.push(escapeHtml(line))
  }
  flush()
  for (; open > 0; open--) out += '</blockquote>'
  return out
}
