import { act, fireEvent, render, screen } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { PlaygroundChat } from './PlaygroundChat'
import { clearSession, configureGateway, saveSession } from '../lib/api'
import type { CatalogModel } from '../lib/playground'
import { memoryStorage } from '../test/stubs'

/*
 * The real request builder runs here, deadline included, with the gateway
 * stubbed at `fetch`. PlaygroundPage's own suite replaces `rawRequest`
 * wholesale, so the bound a send is made under is invisible there.
 */

const models: CatalogModel[] = [{ id: 'gpt-4o-mini', owned_by: 'openai', mode: 'chat' }]

/** A fetch that answers only when the test says so, and honours an abort as a real one does. */
function heldFetch(): { answer: (response: Response) => void } {
  const held: { answer: (response: Response) => void } = { answer: () => {} }
  vi.stubGlobal('fetch', vi.fn((_url: string, init?: RequestInit) => new Promise<Response>((resolve, reject) => {
    held.answer = resolve
    init?.signal?.addEventListener('abort', () => reject(new DOMException('The operation was aborted.', 'AbortError')))
  })))
  return held
}

describe('PlaygroundChat', () => {
  beforeEach(() => {
    vi.stubGlobal('localStorage', memoryStorage())
    vi.stubGlobal('sessionStorage', memoryStorage())
    clearSession()
    configureGateway('')
    saveSession({ token: 'fgws_abc', scopes: ['admin'] })
  })

  afterEach(() => {
    vi.useRealTimers()
    vi.unstubAllGlobals()
  })

  it('waits for a streamed answer whose first chunk takes longer than thirty seconds', async () => {
    // A stream's response headers leave the gateway with its first chunk, so
    // for a stream the time to headers is the time to first token. With MCP
    // tools configured it is longer still: the agentic loop runs to completion
    // and the whole answer arrives as one chunk. Neither is a gateway that has
    // stopped answering, and the send must not be abandoned as one.
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] })
    const gateway = heldFetch()
    render(<PlaygroundChat capabilities={null} loadingModels={false} models={models} onRefreshModels={() => {}} />)

    fireEvent.change(screen.getByLabelText('Message'), { target: { value: 'Look it up' } })
    fireEvent.click(screen.getByRole('button', { name: /send/i }))
    await act(() => vi.advanceTimersByTimeAsync(45_000))

    gateway.answer(new Response(
      'data: {"choices":[{"delta":{"content":"Found it."}}]}\n\ndata: [DONE]\n\n',
      { status: 200, headers: { 'Content-Type': 'text/event-stream', 'X-Gateway-Target': 'openai' } },
    ))
    vi.useRealTimers()

    expect(await screen.findByText('Found it.')).toBeInTheDocument()
    expect(screen.queryByText(/did not respond in time/)).not.toBeInTheDocument()
  })
})
