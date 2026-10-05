import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { render, waitFor } from '@testing-library/svelte'
import type { Attachment } from '../../lib/types'

const api = vi.hoisted(() => ({
  readAttachment: vi.fn(),
}))

vi.mock('../../lib/api', async (importOriginal) => ({
  ...(await importOriginal<typeof import('../../lib/api')>()),
  readAttachment: api.readAttachment,
}))

import AttachmentPreview from './AttachmentPreview.svelte'
import { closePreview, openPreview } from '../../stores/preview'

// the preview closes with a transition, and jsdom has no web animations. A
// finished stub lets svelte run the transition to its end.
Element.prototype.animate ??= function () {
  const animation = { onfinish: null as (() => void) | null, cancel() {}, finished: Promise.resolve() }
  queueMicrotask(() => animation.onfinish?.())
  return animation as unknown as Animation
}

function attachment(filename: string, contentType: string): Attachment {
  return { id: 14, filename, contentType, sizeBytes: 9, inline: false }
}

describe('AttachmentPreview', () => {
  let blobs: Blob[]

  beforeEach(() => {
    blobs = []
    URL.createObjectURL = vi.fn((b: Blob) => {
      blobs.push(b)
      return 'blob:preview'
    })
    URL.revokeObjectURL = vi.fn()
  })

  afterEach(() => {
    closePreview()
  })

  // senders often attach a pdf as application/octet-stream; the frame only
  // renders it when the blob says application/pdf, otherwise it stays blank.
  it('hands a pdf declared as octet-stream to the frame as a pdf', async () => {
    const file = attachment('invoice-0042.Pdf', 'application/octet-stream')
    api.readAttachment.mockResolvedValue({
      filename: file.filename,
      contentType: 'application/octet-stream',
      sizeBytes: 9,
      data: btoa('%PDF-1.3\n'),
      tooLarge: false,
    })
    render(AttachmentPreview)
    openPreview(7897, file)
    await waitFor(() => expect(blobs).toHaveLength(1))
    expect(blobs[0].type).toBe('application/pdf')
  })

  it('keeps the declared type of an image', async () => {
    const file = attachment('photo.png', 'image/png')
    api.readAttachment.mockResolvedValue({
      filename: file.filename,
      contentType: 'image/png',
      sizeBytes: 4,
      data: btoa('\x89PNG'),
      tooLarge: false,
    })
    render(AttachmentPreview)
    openPreview(1, file)
    await waitFor(() => expect(blobs).toHaveLength(1))
    expect(blobs[0].type).toBe('image/png')
  })

  // the reader moved on to B before A's bytes arrived; A must not replace B.
  it('drops a read that finishes after another attachment was opened', async () => {
    api.readAttachment.mockReset()
    let finishA!: (value: unknown) => void
    const a = attachment('a.png', 'image/png')
    const b = { ...attachment('b.png', 'image/png'), id: 15 }
    api.readAttachment.mockImplementationOnce(() => new Promise((resolve) => (finishA = resolve)))
    api.readAttachment.mockResolvedValueOnce({
      filename: 'b.png',
      contentType: 'image/png',
      sizeBytes: 1,
      data: btoa('B'),
      tooLarge: false,
    })
    render(AttachmentPreview)
    openPreview(1, a)
    await waitFor(() => expect(api.readAttachment).toHaveBeenCalledTimes(1))
    openPreview(1, b)
    await waitFor(() => expect(blobs).toHaveLength(1))
    finishA({ filename: 'a.png', contentType: 'image/png', sizeBytes: 2, data: btoa('AA'), tooLarge: false })
    await new Promise((resolve) => setTimeout(resolve, 0))
    expect(blobs).toHaveLength(1)
    expect(blobs[0].size).toBe(1)
  })
})
