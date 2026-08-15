import { afterEach, describe, expect, it, vi } from 'vitest'
import { copyText } from './clipboard'

const writeText = vi.fn()

// jsdom 29 no longer ships document.execCommand, so the legacy fallback path
// is backed by an explicitly defined mock per test.
function stubExecCommand(impl: () => boolean) {
  const mock = vi.fn(impl)
  Object.defineProperty(document, 'execCommand', {
    configurable: true,
    writable: true,
    value: mock,
  })
  return mock
}

afterEach(() => {
  vi.unstubAllGlobals()
  vi.restoreAllMocks()
  delete (document as unknown as { execCommand?: unknown }).execCommand
})

describe('copyText', () => {
  it('returns true when the async clipboard API succeeds', async () => {
    writeText.mockResolvedValue(undefined)
    vi.stubGlobal('navigator', { clipboard: { writeText } })
    await expect(copyText('secret')).resolves.toBe(true)
    expect(writeText).toHaveBeenCalledWith('secret')
  })

  it('falls back to a hidden textarea + execCommand when the clipboard API fails', async () => {
    writeText.mockRejectedValue(new Error('denied'))
    vi.stubGlobal('navigator', { clipboard: { writeText } })
    const exec = stubExecCommand(() => true)
    await expect(copyText('secret')).resolves.toBe(true)
    expect(exec).toHaveBeenCalledWith('copy')
    expect(document.querySelector('textarea')).toBeNull()
  })

  it('returns false when both the clipboard API and the fallback fail', async () => {
    writeText.mockRejectedValue(new Error('denied'))
    vi.stubGlobal('navigator', { clipboard: { writeText } })
    stubExecCommand(() => {
      throw new Error('unsupported')
    })
    await expect(copyText('secret')).resolves.toBe(false)
  })

  it('returns false when execCommand reports failure', async () => {
    writeText.mockRejectedValue(new Error('denied'))
    vi.stubGlobal('navigator', { clipboard: { writeText } })
    stubExecCommand(() => false)
    await expect(copyText('secret')).resolves.toBe(false)
  })
})
