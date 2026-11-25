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
import { zodResolver } from '@hookform/resolvers/zod'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useEffect, useRef, useState } from 'react'
import { useForm } from 'react-hook-form'
import { useTranslation } from 'react-i18next'
import z from 'zod'

import { ConfirmDialog } from '@/components/confirm-dialog'
import { StaticDataTable } from '@/components/data-table'
import { Dialog } from '@/components/dialog'
import { ErrorState } from '@/components/error-state'
import { LoadingState } from '@/components/loading-state'
import { Button } from '@/components/ui/button'
import {
  Form,
  FormControl,
  FormField,
  FormItem,
  FormLabel,
  FormMessage,
} from '@/components/ui/form'
import { Input } from '@/components/ui/input'
import {
  Select,
  SelectTrigger,
  SelectValue,
  SelectContent,
  SelectItem,
} from '@/components/ui/select'
import { getCurrencyDisplay } from '@/lib/currency'
import { handleServerError } from '@/lib/handle-server-error'

import { mcpRequest } from './api'
import type { MCPDiscovery, MCPPrice, MCPServer, MCPTool } from './types'

function formatToolPrice(price: MCPPrice): string {
  return new Intl.NumberFormat('en-US', {
    style: 'currency',
    currency: price.currency || 'USD',
    currencyDisplay: 'narrowSymbol',
    minimumFractionDigits: 0,
    maximumFractionDigits: 12,
  }).format(Number(price.unit_price ?? price.unit_price_usd))
}

export function ToolPanel({
  server,
  sensitive,
  operate,
  write,
}: {
  server: MCPServer
  sensitive: boolean
  operate: boolean
  write: boolean
}) {
  const { t } = useTranslation()
  const canEditPrice = write || sensitive
  const client = useQueryClient()
  const [editing, setEditing] = useState<MCPTool | null>(null)
  const [enabling, setEnabling] = useState<MCPTool | null>(null)
  const [discovery, setDiscovery] = useState<MCPDiscovery | null>(null)
  const tools = useQuery({
    queryKey: ['mcp', 'tools', server.id],
    queryFn: () => mcpRequest<MCPTool[]>(`/servers/${server.id}/tools`),
  })
  const discover = useMutation({
    mutationFn: () =>
      mcpRequest<MCPDiscovery>(`/servers/${server.id}/discoveries`, 'POST'),
    onSuccess: setDiscovery,
    onError: async (error) => {
      const code = (error as { response?: { data?: { code?: string } } })
        .response?.data?.code
      if (
        code === 'REAUTHORIZATION_REQUIRED' ||
        code === 'ENGINE_AUTHORIZATION_REQUIRED'
      ) {
        try {
          const result = await mcpRequest<{ authorization_url: string }>(
            `/servers/${server.id}/discovery-authorization`,
            'POST'
          )
          window.location.assign(result.authorization_url)
        } catch (authorizationError) {
          handleServerError(authorizationError)
        }
        return
      }
      handleServerError(error)
    },
  })
  const resumed = useRef(false)
  useEffect(() => {
    const query = new URLSearchParams(window.location.search)
    if (
      resumed.current ||
      query.get('discover') !== 'true' ||
      query.get('server_id') !== server.id ||
      window.location.hash
    ) {
      return
    }
    resumed.current = true
    query.delete('discover')
    window.history.replaceState(
      null,
      '',
      `${window.location.pathname}?${query.toString()}`
    )
    void discover
      .mutateAsync()
      .then(async (result) => {
        await mcpRequest(`/servers/${server.id}/tools/sync`, 'POST', {
          discovery_id: result.discovery_id,
          expected_revision: result.server_revision,
        })
        setDiscovery(null)
        await client.invalidateQueries({ queryKey: ['mcp'] })
      })
      .catch((error: unknown) => handleServerError(error))
  }, [server.id, discover, client])
  const sync = useMutation({
    mutationFn: () =>
      mcpRequest(`/servers/${server.id}/tools/sync`, 'POST', {
        discovery_id: discovery?.discovery_id,
        expected_revision: discovery?.server_revision,
      }),
    onSuccess: async () => {
      setDiscovery(null)
      await client.invalidateQueries({ queryKey: ['mcp'] })
    },
    onError: (error) => handleServerError(error),
  })
  const toggle = useMutation({
    mutationFn: (tool: MCPTool) =>
      mcpRequest(`/servers/${server.id}/tools/${tool.id}`, 'PATCH', {
        expected_revision: tool.revision,
        enabled: !tool.enabled,
        ...(!tool.enabled && tool.review_required
          ? { accepted_schema_hash: tool.schema_hash }
          : {}),
      }),
    onSuccess: async () => {
      setEnabling(null)
      await client.invalidateQueries({ queryKey: ['mcp', 'tools', server.id] })
    },
    onError: (error) => handleServerError(error),
  })
  if (tools.isError) return <ErrorState onRetry={() => void tools.refetch()} />
  if (tools.isLoading) return <LoadingState />
  return (
    <div className='space-y-4'>
      {operate && (
        <Button
          variant='outline'
          disabled={discover.isPending}
          onClick={() => discover.mutate()}
        >
          {t('Discover tools')}
        </Button>
      )}
      {!canEditPrice && (
        <p className='text-muted-foreground text-sm'>
          {t('Price editing requires MCP service editing permission.')}
        </p>
      )}
      <StaticDataTable
        data={tools.data ?? []}
        getRowKey={(tool) => tool.id}
        emptyContent={t('No tools found')}
        columns={[
          {
            id: 'name',
            header: t('Tool'),
            cell: (tool) => tool.name,
          },
          {
            id: 'status',
            header: t('Status'),
            cell: (tool) => {
              if (tool.removed) return t('Removed')
              if (tool.review_required) return t('Review required')
              return tool.enabled ? t('Enabled') : t('Disabled')
            },
          },
          {
            id: 'price',
            header: t('Price'),
            cell: (tool) => {
              const price = tool.pricing
                ? formatToolPrice(tool.pricing)
                : t('Not configured')
              return (
                <Button
                  variant='link'
                  className='px-0'
                  disabled={!canEditPrice || tool.removed}
                  onClick={() => setEditing(tool)}
                >
                  {price}
                </Button>
              )
            },
          },
          {
            id: 'actions',
            header: t('Actions'),
            cell: (tool) => (
              <div className='flex flex-wrap gap-2'>
                {operate && (
                  <Button
                    variant='outline'
                    disabled={tool.removed || toggle.isPending}
                    onClick={() => setEnabling(tool)}
                  >
                    {tool.enabled ? t('Disable') : t('Enable')}
                  </Button>
                )}
              </div>
            ),
          },
        ]}
      />
      {editing && (
        <ToolForm
          key={`${editing.id}:${editing.revision}`}
          tool={editing}
          server={server}
          canEditPrice={canEditPrice}
          write={write}
          onClose={() => setEditing(null)}
        />
      )}
      <ConfirmDialog
        open={!!enabling}
        onOpenChange={(open) => {
          if (!open) setEnabling(null)
        }}
        title={enabling?.enabled ? t('Disable tool?') : t('Enable tool?')}
        desc={
          <div className='space-y-2'>
            <p>{enabling?.name}</p>
            {enabling?.review_required && (
              <>
                <p>
                  {t('Review the changed schema before enabling this tool.')}
                </p>
                <pre className='max-h-72 overflow-auto text-xs break-all whitespace-pre-wrap'>
                  {JSON.stringify(
                    {
                      input: enabling.input_schema,
                      output: enabling.output_schema,
                    },
                    null,
                    2
                  )}
                </pre>
              </>
            )}
          </div>
        }
        isLoading={toggle.isPending}
        handleConfirm={() => {
          if (enabling) toggle.mutate(enabling)
        }}
        confirmText={enabling?.enabled ? t('Disable') : t('Enable')}
      />
      <Dialog
        open={!!discovery}
        onOpenChange={(open) => {
          if (!open) setDiscovery(null)
        }}
        title={t('Review tool discovery')}
        footer={
          <Button disabled={sync.isPending} onClick={() => sync.mutate()}>
            {t('Sync tools')}
          </Button>
        }
      >
        <p className='mb-4 text-sm'>
          {t(
            'New and changed tools stay disabled until reviewed. Existing prices are preserved.'
          )}
        </p>
        {discovery && (
          <div className='space-y-4'>
            <p>
              {t('Added')}: {discovery.added.join(', ') || '—'}
            </p>
            <p>
              {t('Changed')}: {discovery.changed.join(', ') || '—'}
            </p>
            <p>
              {t('Removed')}: {discovery.removed.join(', ') || '—'}
            </p>
            {discovery.tools
              .filter(
                (tool) =>
                  discovery.added.includes(tool.name) ||
                  discovery.changed.includes(tool.name)
              )
              .map((tool) => (
                <div key={tool.name}>
                  <p className='font-medium'>{tool.name}</p>
                  <div className='grid gap-2 sm:grid-cols-2'>
                    <pre className='max-h-80 overflow-auto text-xs break-all whitespace-pre-wrap'>
                      {JSON.stringify(
                        {
                          previous: tools.data?.find(
                            (old) => old.name === tool.name
                          )?.input_schema,
                          output: tools.data?.find(
                            (old) => old.name === tool.name
                          )?.output_schema,
                        },
                        null,
                        2
                      )}
                    </pre>
                    <pre className='max-h-80 overflow-auto text-xs break-all whitespace-pre-wrap'>
                      {JSON.stringify(
                        {
                          proposed: tool.input_schema,
                          output: tool.output_schema,
                        },
                        null,
                        2
                      )}
                    </pre>
                  </div>
                </div>
              ))}
          </div>
        )}
      </Dialog>
    </div>
  )
}

const toolSchema = z.object({
  description: z.string().max(4096),
  price: z.string().regex(/^(0|[1-9]\d{0,9})(\.\d{1,12})?$/),
})

function ToolForm({
  tool,
  server,
  canEditPrice,
  write,
  onClose,
}: {
  tool: MCPTool
  server: MCPServer
  canEditPrice: boolean
  write: boolean
  onClose: () => void
}) {
  const { t } = useTranslation()
  const client = useQueryClient()
  const config = getCurrencyDisplay().config
  const preferred = ['CNY', 'USD'].includes(config.quotaDisplayType)
    ? config.quotaDisplayType
    : tool.pricing?.currency || 'USD'
  let displayedPrice = tool.pricing?.unit_price_usd ?? '0'
  if (tool.pricing && preferred === tool.pricing.currency) {
    displayedPrice = tool.pricing.unit_price ?? tool.pricing.unit_price_usd
  } else if (tool.pricing && preferred === 'CNY') {
    displayedPrice = String(
      Number(
        (Number(tool.pricing.unit_price_usd) * config.usdExchangeRate).toFixed(
          12
        )
      )
    )
  }
  const [currency, setCurrency] = useState(preferred)
  const [revision, setRevision] = useState(tool.revision)
  const form = useForm<z.infer<typeof toolSchema>>({
    resolver: zodResolver(toolSchema),
    defaultValues: {
      description: tool.description,
      price: displayedPrice,
    },
  })
  const save = useMutation({
    mutationFn: async (values: z.infer<typeof toolSchema>) => {
      let expectedRevision = revision
      if (write && values.description !== tool.description) {
        const updated = await mcpRequest<MCPTool>(
          `/servers/${server.id}/tools/${tool.id}`,
          'PATCH',
          { expected_revision: revision, description: values.description }
        )
        expectedRevision = updated.revision
        setRevision(expectedRevision)
      }
      if (
        canEditPrice &&
        (!tool.pricing ||
          values.price !== displayedPrice ||
          currency !== preferred)
      ) {
        await mcpRequest(
          `/servers/${server.id}/tools/${tool.id}/pricing`,
          'PUT',
          {
            expected_revision: expectedRevision,
            mode: Number(values.price) === 0 ? 'free' : 'per_success',
            unit_price: values.price,
            currency,
          }
        )
      }
    },
    onSuccess: onClose,
    onSettled: () =>
      client.invalidateQueries({ queryKey: ['mcp', 'tools', server.id] }),
    onError: (error) => handleServerError(error),
  })
  return (
    <Dialog
      open
      onOpenChange={(open) => {
        if (!open) onClose()
      }}
      title={tool.name}
      footer={
        (canEditPrice || write) && (
          <Button
            form='mcp-tool-form'
            type='submit'
            disabled={save.isPending || (canEditPrice && !currency)}
          >
            {t('Save')}
          </Button>
        )
      }
    >
      <Form {...form}>
        <form
          id='mcp-tool-form'
          className='space-y-4'
          onSubmit={form.handleSubmit((values) => save.mutate(values))}
        >
          <Select
            value={currency}
            disabled={!canEditPrice}
            onValueChange={(v) => setCurrency(v ?? '')}
          >
            <SelectTrigger aria-label={t('Currency')}>
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value='USD'>USD</SelectItem>
              <SelectItem value='CNY'>CNY</SelectItem>
            </SelectContent>
          </Select>
          {(
            [
              {
                name: 'description',
                label: t('Description'),
                disabled: !write,
              },
              {
                name: 'price',
                label:
                  currency === 'CNY'
                    ? `${t('Price')} (CNY)`
                    : t('USD per successful call (0 is free)'),
                disabled: !canEditPrice,
              },
            ] as const
          ).map((item) => (
            <FormField
              key={item.name}
              control={form.control}
              name={item.name}
              render={({ field, fieldState }) => (
                <FormItem>
                  <FormLabel>{item.label}</FormLabel>
                  <FormControl>
                    <Input {...field} disabled={item.disabled} />
                  </FormControl>
                  {fieldState.error && (
                    <FormMessage>{t('Invalid value')}</FormMessage>
                  )}
                </FormItem>
              )}
            />
          ))}
          <p className='text-muted-foreground text-sm'>
            {tool.pricing ? formatToolPrice(tool.pricing) : t('Not configured')}
          </p>
          <pre className='max-h-96 overflow-auto text-xs break-all whitespace-pre-wrap'>
            {JSON.stringify(
              {
                input_schema: tool.input_schema,
                output_schema: tool.output_schema,
              },
              null,
              2
            )}
          </pre>
        </form>
      </Form>
    </Dialog>
  )
}
