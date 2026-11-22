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
import { SSE } from 'sse.js'
import { z } from 'zod'

import { getFreshAuthHeaders } from '@/lib/auth-session'
import { createServerError } from '@/lib/server-error-message'

import { consultationUpdate, type ConsultationUpdate } from './types'

// Reuse the project's authenticated SSE transport; the playground hook itself
// only understands chat completion chunks, not MCP progress notifications.
export async function streamMCPTest(
  path: string,
  body: unknown,
  key: string,
  signal: AbortSignal,
  onAccepted: (id: string) => void,
  onUpdate: (update: ConsultationUpdate) => void
): Promise<{ invocation_id: string; interrupted: boolean }> {
  const headers = await getFreshAuthHeaders()
  signal.throwIfAborted()
  return new Promise((resolve, reject) => {
    const source = new SSE(`/api/mcp${path}`, {
      method: 'POST',
      payload: JSON.stringify(body),
      headers: {
        ...headers,
        Accept: 'text/event-stream',
        'X-MCP-Request': '1',
        'Idempotency-Key': key,
      },
      withCredentials: true,
      start: false,
      autoReconnect: false,
    })
    let invocationID = ''
    let settled = false
    let sequence = 0
    const finish = (error?: unknown, aborted = false) => {
      if (settled) return
      settled = true
      signal.removeEventListener('abort', abort)
      source.close()
      if (aborted || !invocationID) {
        reject(error ?? new Error('Invalid test response'))
      } else {
        resolve({ invocation_id: invocationID, interrupted: !!error })
      }
    }
    const abort = () => finish(signal.reason, true)
    signal.addEventListener('abort', abort, { once: true })
    source.addEventListener(
      'invocation',
      (event: Event & { data?: string; readyState?: number }) => {
        try {
          const accepted = z
            .object({ invocation_id: z.string().min(1) })
            .parse(JSON.parse(event.data ?? ''))
          invocationID = accepted.invocation_id
          onAccepted(invocationID)
        } catch (error) {
          finish(error)
        }
      }
    )
    source.addEventListener(
      'message',
      (event: Event & { data?: string; readyState?: number }) => {
        if (settled) return
        try {
          const message = z
            .object({
              method: z.literal('notifications/progress'),
              params: z.object({
                progressToken: z.string(),
                progress: z.number(),
                _meta: z.record(z.string(), z.unknown()).optional(),
              }),
            })
            .parse(JSON.parse(event.data ?? ''))
          if (!invocationID || message.params.progressToken !== invocationID) {
            throw new Error('Invalid progress token')
          }
          if (message.params.progress <= sequence) return
          sequence = message.params.progress
          const update = consultationUpdate.safeParse(
            message.params._meta?.['knowyet.com/consultation']
          )
          if (update.success) onUpdate(update.data)
        } catch (error) {
          finish(error)
        }
      }
    )
    source.addEventListener('complete', () => finish())
    source.addEventListener(
      'error',
      (event: Event & { data?: string; readyState?: number }) => {
        let error: unknown = new Error('Stream interrupted')
        if (event.data) {
          try {
            error = createServerError(JSON.parse(event.data))
          } catch {
            /* Fall back to the transport error. */
          }
        }
        finish(error)
      }
    )
    source.addEventListener(
      'readystatechange',
      (event: Event & { data?: string; readyState?: number }) => {
        if (event.readyState === 2) finish(new Error('Stream interrupted'))
      }
    )
    source.stream()
  })
}
