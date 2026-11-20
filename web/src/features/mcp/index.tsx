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
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import type { ColumnDef } from '@tanstack/react-table'
import { useEffect, useRef, useState } from 'react'
import { useTranslation } from 'react-i18next'

import { ConfirmDialog } from '@/components/confirm-dialog'
import { CopyButton } from '@/components/copy-button'
import { DataTablePage, useDataTable } from '@/components/data-table'
import { Dialog } from '@/components/dialog'
import { ErrorState } from '@/components/error-state'
import { SectionPageLayout } from '@/components/layout'
import { LoadingState } from '@/components/loading-state'
import { Button } from '@/components/ui/button'
import { Tabs, TabsContent, TabsList, TabsTrigger } from '@/components/ui/tabs'
import { hasPermission } from '@/lib/admin-permissions'
import { handleServerError } from '@/lib/handle-server-error'
import { statusQueryOptions } from '@/lib/status-query'
import { useAuthStore } from '@/stores/auth-store'

import { mcpRequest } from './api'
import { InvocationList } from './invocations'
import { ServerForm } from './server-form'
import { ServiceIcon } from './service-page'
import { ToolPanel } from './tool-panel'
import type { MCPPage, MCPServer } from './types'

export function MCPManagement() {
  const { t } = useTranslation()
  const user = useAuthStore((state) => state.auth.user)
  const sensitive = hasPermission(user, 'mcp', 'sensitive_write')
  const operate = hasPermission(user, 'mcp', 'operate')
  const write = hasPermission(user, 'mcp', 'write')
  const client = useQueryClient()
  const status = useQuery(statusQueryOptions)
  const enabled = status.data?.mcp_enabled === true
  const [creating, setCreating] = useState(false)
  const [selected, setSelected] = useState<string | null>(() =>
    window.location.hash.includes('mcp_state=')
      ? null
      : new URLSearchParams(window.location.search).get('server_id')
  )
  const callbackStarted = useRef(false)
  useEffect(() => {
    if (callbackStarted.current) return
    const fragment = new URLSearchParams(window.location.hash.slice(1))
    const state = fragment.get('mcp_state')
    const code = fragment.get('mcp_code')
    if (!state || !code) return
    callbackStarted.current = true
    window.history.replaceState(
      null,
      '',
      window.location.pathname + window.location.search
    )
    void mcpRequest<{ admin: boolean }>('/oauth/callback', 'POST', {
      state,
      code,
      issuer: fragment.get('mcp_iss') ?? '',
    })
      .then(async () => {
        setSelected(
          new URLSearchParams(window.location.search).get('server_id')
        )
        await client.invalidateQueries({ queryKey: ['mcp'] })
      })
      .catch((error: unknown) => handleServerError(error))
  }, [client])
  const [action, setAction] = useState<{
    server: MCPServer
    kind: 'status' | 'archive'
  } | null>(null)
  const [pagination, setPagination] = useState({ pageIndex: 0, pageSize: 20 })
  const [filter, setFilter] = useState('')
  const servers = useQuery({
    queryKey: ['mcp', 'servers', pagination, filter],
    queryFn: () =>
      mcpRequest<MCPPage<MCPServer>>(
        `/servers?p=${pagination.pageIndex + 1}&page_size=${pagination.pageSize}&name=${encodeURIComponent(filter)}`
      ),
    enabled,
  })
  const change = useMutation({
    mutationFn: () => {
      if (!action) throw new Error('No action selected')
      return mcpRequest(
        `/servers/${action.server.id}${action.kind === 'status' ? '/status' : ''}`,
        action.kind === 'status' ? 'POST' : 'DELETE',
        {
          expected_revision: action.server.revision,
          ...(action.kind === 'status'
            ? { enabled: !action.server.enabled }
            : {}),
        }
      )
    },
    onSuccess: async () => {
      setAction(null)
      await client.invalidateQueries({ queryKey: ['mcp'] })
    },
    onError: (error) => handleServerError(error),
  })
  const columns: ColumnDef<MCPServer>[] = [
    {
      accessorKey: 'name',
      header: t('Name'),
      cell: ({ row }) => (
        <span className='inline-flex items-center gap-2'>
          <ServiceIcon icon={row.original.icon} />
          {row.original.name}
        </span>
      ),
    },
    { accessorKey: 'version', header: t('Version') },
    {
      accessorKey: 'description',
      header: t('Description'),
      cell: ({ row }) => (
        <span className='line-clamp-2 max-w-60'>
          {row.original.description}
        </span>
      ),
    },
    {
      id: 'transport',
      header: t('Transport'),
      cell: ({ row }) => {
        if (row.original.connection.transport === 'stdio') return 'stdio'
        return row.original.connection.transport === 'http_sse'
          ? 'SSE'
          : 'Streamable HTTP'
      },
    },
    {
      accessorKey: 'enabled',
      header: t('Status'),
      cell: ({ row }) => (row.original.enabled ? t('Enabled') : t('Disabled')),
    },
    {
      accessorKey: 'auth_mode',
      header: t('Authentication'),
      cell: ({ row }) =>
        row.original.auth_mode === 'oauth2' ? 'OAuth2' : t('No authentication'),
    },
    {
      id: 'actions',
      header: t('Actions'),
      cell: ({ row }) => (
        <div className='flex flex-wrap gap-2'>
          <Button
            size='sm'
            variant='outline'
            onClick={() => setSelected(row.original.id)}
          >
            {t('Manage')}
          </Button>
          {operate && (
            <Button
              size='sm'
              variant='outline'
              onClick={() =>
                setAction({ server: row.original, kind: 'status' })
              }
            >
              {row.original.enabled ? t('Disable') : t('Enable')}
            </Button>
          )}
          {sensitive && (
            <Button
              size='sm'
              variant='outline'
              disabled={row.original.enabled}
              onClick={() =>
                setAction({ server: row.original, kind: 'archive' })
              }
            >
              {t('Archive')}
            </Button>
          )}
        </div>
      ),
    },
  ]
  const { table } = useDataTable({
    data: servers.data?.items ?? [],
    columns,
    totalCount: servers.data?.total ?? 0,
    pagination,
    onPaginationChange: setPagination,
    manualPagination: true,
    manualFiltering: true,
    columnFilters: [],
    globalFilter: filter,
    onGlobalFilterChange: (value) => {
      setFilter(value)
      setPagination((p) => ({ ...p, pageIndex: 0 }))
    },
    enableRowSelection: false,
    enableSorting: false,
  })
  const statusTitle = action?.server.enabled
    ? t('Disable MCP server?')
    : t('Enable MCP server?')
  return (
    <>
      <SectionPageLayout fixedContent>
        <SectionPageLayout.Title>{t('MCP Services')}</SectionPageLayout.Title>
        <SectionPageLayout.Actions>
          {enabled && sensitive && (
            <Button onClick={() => setCreating(true)}>
              {t('Create MCP server')}
            </Button>
          )}
        </SectionPageLayout.Actions>
        <SectionPageLayout.Content>
          {status.isLoading && <LoadingState />}
          {status.isError && (
            <ErrorState onRetry={() => void status.refetch()} />
          )}
          {status.isSuccess && !enabled && (
            <p className='text-muted-foreground'>
              {t('MCP is disabled for this deployment.')}
            </p>
          )}
          {enabled && (
            <Tabs
              defaultValue='servers'
              className='flex h-full min-h-0 flex-col'
            >
              <TabsList>
                <TabsTrigger value='servers'>{t('Servers')}</TabsTrigger>
                <TabsTrigger value='invocations'>
                  {t('Invocations')}
                </TabsTrigger>
              </TabsList>
              <TabsContent value='servers' className='min-h-0 flex-1'>
                {servers.isError ? (
                  <ErrorState onRetry={() => void servers.refetch()} />
                ) : (
                  <DataTablePage
                    table={table}
                    columns={columns}
                    isLoading={servers.isLoading}
                    isFetching={servers.isFetching}
                    emptyTitle={t('No MCP servers found')}
                    toolbarProps={{ searchPlaceholder: t('Search servers...') }}
                  />
                )}
              </TabsContent>
              <TabsContent value='invocations' className='min-h-0 flex-1'>
                <InvocationList />
              </TabsContent>
            </Tabs>
          )}
        </SectionPageLayout.Content>
      </SectionPageLayout>
      {creating && (
        <ServerForm
          sensitive={sensitive}
          onClose={() => setCreating(false)}
          onCreated={setSelected}
        />
      )}
      {selected && enabled && (
        <ServerDetail
          id={selected}
          sensitive={sensitive}
          operate={operate}
          write={write}
          onClose={() => setSelected(null)}
        />
      )}
      <ConfirmDialog
        open={!!action}
        onOpenChange={(open) => {
          if (!open) setAction(null)
        }}
        title={
          action?.kind === 'archive' ? t('Archive MCP server?') : statusTitle
        }
        desc={
          <div>
            <p>{action?.server.name}</p>
            <p>
              {t(
                'Disabling stops new calls. Existing tasks and billing recovery continue.'
              )}
            </p>
          </div>
        }
        destructive={action?.kind === 'archive' || action?.server.enabled}
        isLoading={change.isPending}
        confirmText={t('Confirm')}
        handleConfirm={() => change.mutate()}
      />
    </>
  )
}

function ServerDetail({
  id,
  sensitive,
  operate,
  write,
  onClose,
}: {
  id: string
  sensitive: boolean
  operate: boolean
  write: boolean
  onClose: () => void
}) {
  const { t } = useTranslation()
  const [editing, setEditing] = useState(false)
  const server = useQuery({
    queryKey: ['mcp', 'server', id],
    queryFn: () => mcpRequest<MCPServer>(`/servers/${id}`),
  })
  const client = useQueryClient()
  const authorize = useMutation({
    mutationFn: () =>
      mcpRequest<{ authorization_url: string }>(
        `/servers/${id}/discovery-authorization`,
        'POST'
      ),
    onSuccess: ({ authorization_url }) =>
      window.location.assign(authorization_url),
    onError: (error) => handleServerError(error),
  })
  const test = useMutation({
    mutationFn: () =>
      mcpRequest<{ version?: string; protocol_version?: string }>(
        `/servers/${id}/connection-tests`,
        'POST'
      ),
    onSettled: () =>
      client.invalidateQueries({ queryKey: ['mcp', 'server', id] }),
    onError: (error) => handleServerError(error),
  })
  const download = useMutation({
    mutationFn: () =>
      mcpRequest(`/servers/${id}/package`, 'POST', {
        expected_revision: server.data?.revision,
      }),
    onSuccess: () => client.invalidateQueries({ queryKey: ['mcp'] }),
    onError: (e) => handleServerError(e),
  })
  const detectedVersion =
    test.data?.version ?? server.data?.connection_health?.version
  const useVersion = useMutation({
    mutationFn: (version: string) =>
      mcpRequest(`/servers/${id}`, 'PATCH', {
        version,
        expected_revision: server.data?.revision,
      }),
    onSuccess: () => client.invalidateQueries({ queryKey: ['mcp'] }),
    onError: (error) => handleServerError(error),
  })
  return (
    <Dialog
      open
      onOpenChange={(open) => {
        if (!open) onClose()
      }}
      title={server.data?.name ?? t('MCP Services')}
      contentClassName='sm:max-w-5xl'
      contentHeight='min(76vh, 900px)'
    >
      {server.isError && <ErrorState onRetry={() => void server.refetch()} />}
      {server.isLoading && <LoadingState />}
      {server.data && (
        <div className='space-y-4'>
          <Tabs
            defaultValue={
              new URLSearchParams(window.location.search).get('discover') ===
              'true'
                ? 'tools'
                : 'connection'
            }
          >
            <TabsList className='h-auto flex-wrap'>
              <TabsTrigger value='connection'>{t('Connection')}</TabsTrigger>
              <TabsTrigger value='tools'>{t('Tools and pricing')}</TabsTrigger>
              <TabsTrigger value='invocations'>{t('Invocations')}</TabsTrigger>
            </TabsList>
            <TabsContent value='connection' className='space-y-4 pt-3'>
              <p className='text-sm'>{server.data.description}</p>
              {server.data.connection.transport === 'stdio' && (
                <div className='space-y-2'>
                  <code>{server.data.connection.command}</code>
                  <p>{server.data.connection.package}</p>
                  <code className='break-all'>
                    {server.data.connection.package_digest}
                  </code>
                  {sensitive && (
                    <Button
                      disabled={download.isPending}
                      onClick={() => download.mutate()}
                    >
                      {t('Download package')}
                    </Button>
                  )}
                </div>
              )}
              <div className='flex items-center gap-2 rounded-md border px-3 py-2'>
                <code className='min-w-0 text-sm break-all'>
                  {server.data.connection.url}
                </code>
                <CopyButton value={server.data.connection.url ?? ''} />
              </div>
              <div className='flex flex-wrap gap-2'>
                {write && (
                  <Button variant='outline' onClick={() => setEditing(true)}>
                    {t('Edit configuration')}
                  </Button>
                )}
                {operate && server.data.auth_mode === 'oauth2' && (
                  <Button
                    variant='outline'
                    disabled={authorize.isPending}
                    onClick={() => authorize.mutate()}
                  >
                    {t('Authorize')}
                  </Button>
                )}
                {operate && (
                  <Button
                    variant='outline'
                    disabled={test.isPending}
                    onClick={() => test.mutate()}
                  >
                    {t('Test connection')}
                  </Button>
                )}
              </div>
              {server.data.connection_health && (
                <p className='text-muted-foreground text-sm'>
                  {server.data.connection_health.connected
                    ? t('Connected')
                    : t('Failed')}
                </p>
              )}
              {detectedVersion && (
                <div className='flex items-center gap-2 text-sm'>
                  <span>
                    {t('Version')} {detectedVersion}
                  </span>
                  {write && (
                    <Button
                      size='sm'
                      variant='outline'
                      disabled={useVersion.isPending}
                      onClick={() => useVersion.mutate(detectedVersion)}
                    >
                      {t('Use detected version')}
                    </Button>
                  )}
                </div>
              )}
            </TabsContent>
            <TabsContent value='tools'>
              <ToolPanel
                server={server.data}
                sensitive={sensitive}
                operate={operate}
                write={write}
              />
            </TabsContent>
            <TabsContent value='invocations'>
              <div className='h-[32rem]'>
                <InvocationList serverID={id} />
              </div>
            </TabsContent>
          </Tabs>
          {editing && (
            <ServerForm
              server={server.data}
              sensitive={sensitive}
              onClose={() => setEditing(false)}
            />
          )}
        </div>
      )}
    </Dialog>
  )
}
