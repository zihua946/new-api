/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program. If not, see <https://www.gnu.org/licenses/>.

For commercial licensing, please contact support@quantumnous.com
*/
import { afterEach, describe, expect, it, vi } from 'vitest'
import { streamMCPTest } from '../test-stream'

vi.mock('@/lib/auth-session', () => ({ getFreshAuthHeaders: async () => ({ Authorization: 'Bearer fixture' }) }))

class StreamXHR extends EventTarget {
  static instances: StreamXHR[] = []
  responseText = ''
  response = ''
  status = 200
  readyState = 3
  withCredentials = false
  headers: Record<string, string> = {}
  open = vi.fn()
  send = vi.fn()
  abort = vi.fn()
  constructor() { super(); StreamXHR.instances.push(this) }
  setRequestHeader(name: string, value: string) { this.headers[name] = value }
  getAllResponseHeaders() { return 'content-type: text/event-stream' }
  push(text: string) {
    this.responseText += text
    this.dispatchEvent(new Event('progress'))
  }
}

afterEach(() => { vi.unstubAllGlobals(); StreamXHR.instances = [] })

describe('MCP test SSE transport', () => {
  it('parses fragmented SSE, ignores duplicate progress, and completes without resubmitting', async () => {
    vi.stubGlobal('XMLHttpRequest', StreamXHR)
    const accepted = vi.fn()
    const update = vi.fn()
    const pending = streamMCPTest('/self/servers/library/test-invocations', {tool_name:'get_consultation_result'}, 'once', new AbortController().signal, accepted, update)
    await vi.waitFor(() => expect(StreamXHR.instances).toHaveLength(1))
    const xhr = StreamXHR.instances[0]
    expect(xhr.headers).toMatchObject({Authorization:'Bearer fixture','X-MCP-Request':'1','Idempotency-Key':'once'})
    expect(xhr.withCredentials).toBe(true)
    xhr.push('event: invocation\r\ndata: {"invocation_id":"call-1"}\r\n\r\n')
    const frame = 'event: message\ndata: {"jsonrpc":"2.0","method":"notifications/progress","params":{"progressToken":"call-1","progress":1,"_meta":{"knowyet.com/consultation":{"task_id":"task-1","type":"answer_delta","text":"你好"}}}}\n\n'
    xhr.push(frame.slice(0, 60))
    expect(update).not.toHaveBeenCalled()
    xhr.push(frame.slice(60))
    xhr.push(frame)
    expect(accepted).toHaveBeenCalledWith('call-1')
    expect(update).toHaveBeenCalledExactlyOnceWith({task_id:'task-1',type:'answer_delta',text:'你好'})
    xhr.push('event: complete\ndata: {"invocation_id":"call-1"}\n\n')
    await expect(pending).resolves.toEqual({invocation_id:'call-1',interrupted:false})
    expect(xhr.send).toHaveBeenCalledOnce()
    expect(xhr.abort).toHaveBeenCalledOnce()
  })

  it.each(['disconnect', 'invalid-token', 'abort'])('handles %s after acceptance without replaying the POST', async (failure) => {
    vi.stubGlobal('XMLHttpRequest', StreamXHR)
    const controller = new AbortController()
    const update = vi.fn()
    const pending = streamMCPTest('/servers/library/test-invocations', {}, 'once', controller.signal, vi.fn(), update)
    const outcome = pending.catch((error: unknown) => error)
    await vi.waitFor(() => expect(StreamXHR.instances).toHaveLength(1))
    const xhr = StreamXHR.instances[0]
    xhr.push('event: invocation\ndata: {"invocation_id":"call-1"}\n\n')
    if (failure === 'abort') controller.abort()
    else if (failure === 'disconnect') xhr.dispatchEvent(new Event('error'))
    else xhr.push('data: {"method":"notifications/progress","params":{"progressToken":"other","progress":1}}\n\n')
    if (failure === 'abort') expect(await outcome).toMatchObject({name:'AbortError'})
    else expect(await outcome).toEqual({invocation_id:'call-1',interrupted:true})
    expect(update).not.toHaveBeenCalled()
    expect(xhr.send).toHaveBeenCalledOnce()
  })

  it('preserves server error codes before acceptance for price refresh handling', async () => {
    vi.stubGlobal('XMLHttpRequest', StreamXHR)
    const pending = streamMCPTest('/servers/library/test-invocations', {}, 'once', new AbortController().signal, vi.fn(), vi.fn())
    const rejected = expect(pending).rejects.toMatchObject({message:'Price changed'})
    await vi.waitFor(() => expect(StreamXHR.instances).toHaveLength(1))
    const xhr = StreamXHR.instances[0]
    xhr.status = 409
    xhr.response = '{"success":false,"code":"PRICE_CONFIRMATION_REQUIRED","message":"Price changed"}'
    xhr.dispatchEvent(new Event('error'))
    await rejected
  })
})
