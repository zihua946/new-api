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
import { z } from 'zod'

const citation = z.object({
  title: z.string().optional(),
  url: z.string().optional(),
  reference: z.string().optional(),
})
export const consultationResult = z.object({
  structuredContent: z.object({
    answer: z.string(),
    status: z.string(),
    done: z.boolean(),
    citations: z.array(citation),
  }),
})
export const consultationUpdate = citation.extend({
  task_id: z.string(),
  type: z.enum(['answer_delta', 'citation', 'status', 'progress']),
  text: z.string().optional(),
  status: z.string().optional(),
})
export type ConsultationUpdate = z.infer<typeof consultationUpdate>
export type ConsultationCitation = z.infer<typeof citation>

export type MCPKind = 'deepseek' | 'public_knowledge' | 'remote_mcp'
export interface MCPServer {
  id: string
  slug: string
  version?: string
  icon?: string
  readme?: string
  installation?: { id: string; authorized: boolean }
  oauth_callback_url?: string
  name: string
  description: string
  kind: MCPKind
  enabled: boolean
  auth_mode: string
  allowed_user_groups: string[]
  connection: {
    command?: string
    package?: string
    package_digest?: string
    owner?: string
    kb_id?: string
    channel_id?: number
    confirmed_base_url?: string
    engine_base_url?: string
    client_id?: string
    scopes?: string
    url?: string
    transport?: string
  }
  limits: { timeout_seconds: number; max_concurrency: number }
  credentials_configured: boolean
  credential_revision: number
  connection_health?: {
    checked_at: string
    connected: boolean
    version?: string
    protocol_version?: string
    error_code?: string
  } | null
  revision: number
}
export interface MCPPrice {
  mode: 'free' | 'per_success'
  currency?: string
  unit_price?: string
  exchange_rate?: string
  unit_price_usd: string
  charge_quota: number
  effective_credits: string
  quota_per_unit: number
  price_revision: number
}
export interface MCPTool {
  id: string
  name: string
  description: string
  enabled: boolean
  removed: boolean
  review_required: boolean
  allowed_user_groups: string[] | null
  input_schema: Record<string, unknown>
  output_schema: Record<string, unknown> | null
  schema_hash: string
  revision: number
  pricing: MCPPrice | null
}
export interface MCPInvocation {
  is_test: boolean
  id: string
  server_id: string
  tool_name: string
  user_id: number
  payer_user_id: number
  channel_id: number
  status: string
  billing_status: string
  charge_quota: number
  charged_quota: number
  quota_per_unit: string
  error_code?: string
  reconcile_at?: string
  created_at: string
}
export interface MCPPage<T> {
  items: T[]
  total: number
  page: number
  page_size: number
}
export interface MCPDiscovery {
  discovery_id: string
  server_revision: number
  added: string[]
  changed: string[]
  removed: string[]
  tools: MCPTool[]
}
export interface MCPTestResult {
  invocation: MCPInvocation
  result?: unknown
}

export function testRequest(tool: MCPTool, argumentsText: string) {
  const argumentsValue: unknown = JSON.parse(argumentsText)
  if (
    !argumentsValue ||
    typeof argumentsValue !== 'object' ||
    Array.isArray(argumentsValue) ||
    !tool.pricing
  ) {
    throw new Error('Invalid test request')
  }
  return {
    tool_name: tool.name,
    arguments: argumentsValue,
    expected_tool_revision: tool.revision,
    max_charge_quota: tool.pricing.charge_quota,
  }
}

export function testPending(result?: MCPTestResult) {
  if (!result) return true
  return (
    ['accepted', 'running'].includes(result.invocation.status) ||
    !['free', 'settled', 'refunded'].includes(result.invocation.billing_status)
  )
}
