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
import { api, type ApiRequestConfig } from '@/lib/api'
import { createServerError } from '@/lib/server-error-message'

export async function mcpRequest<T>(
  path: string,
  method: 'GET' | 'POST' | 'PATCH' | 'PUT' | 'DELETE' = 'GET',
  data?: unknown,
  idempotencyKey?: string
): Promise<T> {
  const config: ApiRequestConfig = {
    url: `/api/mcp${path}`,
    method,
    data,
    headers: {
      'X-MCP-Request': '1',
      ...(idempotencyKey ? { 'Idempotency-Key': idempotencyKey } : {}),
    },
    singleUseAuthorization: path === '/oauth/callback',
    skipBusinessError: true,
    skipErrorHandler: true,
  }
  const response = await api.request<{
    success: boolean
    message: string
    data: T
  }>(config)
  if (!response.data.success) throw createServerError(response.data)
  return response.data.data
}
