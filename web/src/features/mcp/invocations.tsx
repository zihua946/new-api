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
import { useQuery } from '@tanstack/react-query'
import type { ColumnDef } from '@tanstack/react-table'
import { useState } from 'react'
import { useTranslation } from 'react-i18next'

import {
  DataTablePage,
  StaticDataTable,
  useDataTable,
} from '@/components/data-table'
import { Dialog } from '@/components/dialog'
import { ErrorState } from '@/components/error-state'
import { LoadingState } from '@/components/loading-state'
import { Button } from '@/components/ui/button'
import { useAuthStore } from '@/stores/auth-store'

import { mcpRequest } from './api'
import { useMCPStatusLabels } from './status-labels'
import {
  testPending,
  type MCPInvocation,
  type MCPPage,
  type MCPTestResult,
} from './types'

export function InvocationList({ serverID }: { serverID?: string }) {
  const { t } = useTranslation()
  const labels = useMCPStatusLabels()
  const userID = useAuthStore((state) => state.auth.user?.id)
  const [pagination, setPagination] = useState({ pageIndex: 0, pageSize: 20 })
  const [selected, setSelected] = useState<string | null>(null)
  const suffix = serverID ? `&server_id=${encodeURIComponent(serverID)}` : ''
  const calls = useQuery({
    queryKey: ['mcp', 'invocations', serverID, pagination],
    queryFn: () =>
      mcpRequest<MCPPage<MCPInvocation>>(
        `/invocations?p=${pagination.pageIndex + 1}&page_size=${pagination.pageSize}${suffix}`
      ),
    refetchInterval: 15_000,
  })
  const usage = useQuery({
    queryKey: ['mcp', 'usage', serverID],
    queryFn: () =>
      mcpRequest<{
        items:
          | {
              server_id: string
              tool_id: string
              calls: number
              charged_quota: number
              quota_per_unit: string
            }[]
          | null
      }>(`/usage?${suffix.slice(1)}`),
    refetchInterval: 15_000,
  })
  const usageItems = usage.data?.items ?? []
  const detail = useQuery({
    queryKey: ['mcp', 'invocation', selected],
    queryFn: () =>
      mcpRequest<{
        invocation: MCPInvocation
        billing_operations: {
          id: string
          kind: string
          amount: number
          state: string
          updated_at: string
        }[]
      }>(`/invocations/${selected}`),
    enabled: !!selected,
    refetchInterval: 5000,
  })
  const ownTest =
    !!detail.data?.invocation.is_test &&
    detail.data.invocation.user_id === userID
  const result = useQuery({
    queryKey: ['mcp', 'test-result', selected],
    queryFn: () => mcpRequest<MCPTestResult>(`/test-invocations/${selected}`),
    enabled: !!selected && ownTest,
    retry: false,
    refetchInterval: (query) =>
      !query.state.error && testPending(query.state.data) ? 1500 : false,
  })
  const columns: ColumnDef<MCPInvocation>[] = [
    {
      accessorKey: 'created_at',
      header: t('Time'),
      cell: ({ row }) => new Date(row.original.created_at).toLocaleString(),
    },
    {
      accessorKey: 'tool_name',
      header: t('Tool'),
      cell: ({ row }) => (
        <Button variant='link' onClick={() => setSelected(row.original.id)}>
          {row.original.tool_name}
        </Button>
      ),
    },
    { accessorKey: 'user_id', header: t('User ID') },
    { accessorKey: 'payer_user_id', header: t('Payer ID') },
    {
      accessorKey: 'status',
      header: t('Call status'),
      cell: ({ row }) => labels[row.original.status] ?? row.original.status,
    },
    {
      accessorKey: 'billing_status',
      header: t('Billing status'),
      cell: ({ row }) =>
        labels[row.original.billing_status] ?? row.original.billing_status,
    },
    {
      accessorKey: 'charged_quota',
      header: 'Credits',
      cell: ({ row }) =>
        String(
          row.original.charged_quota / Number(row.original.quota_per_unit)
        ),
    },
    { accessorKey: 'error_code', header: t('Error code') },
    {
      accessorKey: 'reconcile_at',
      header: t('Reconciliation deadline'),
      cell: ({ row }) =>
        row.original.reconcile_at
          ? new Date(row.original.reconcile_at).toLocaleString()
          : '—',
    },
  ]
  const { table } = useDataTable({
    data: calls.data?.items ?? [],
    columns,
    totalCount: calls.data?.total ?? 0,
    manualPagination: true,
    pagination,
    onPaginationChange: setPagination,
    enableRowSelection: false,
    enableSorting: false,
  })
  return (
    <div className='flex h-full min-h-0 flex-col gap-4'>
      {usage.isError ? (
        <ErrorState onRetry={() => void usage.refetch()} />
      ) : (
        <p className='text-sm'>
          {t('Last 30 days')}:{' '}
          {usageItems.reduce((sum, item) => sum + item.calls, 0)} {t('Calls')} ·{' '}
          {usageItems
            .reduce(
              (sum, item) =>
                sum + item.charged_quota / Number(item.quota_per_unit),
              0
            )
            .toFixed(6)}{' '}
          Credits
        </p>
      )}
      {calls.isError ? (
        <ErrorState onRetry={() => void calls.refetch()} />
      ) : (
        <DataTablePage
          table={table}
          columns={columns}
          isLoading={calls.isLoading}
          isFetching={calls.isFetching}
          toolbarProps={null}
          emptyTitle={t('No invocations found')}
        />
      )}
      <Dialog
        open={!!selected}
        onOpenChange={(open) => {
          if (!open) setSelected(null)
        }}
        title={t('Invocation details')}
      >
        {detail.isError ? (
          <ErrorState onRetry={() => void detail.refetch()} />
        ) : (
          detail.data && (
            <div className='space-y-4'>
              <p className='break-all'>{detail.data.invocation.id}</p>
              <p>
                {t('Call status')}:{' '}
                {labels[detail.data.invocation.status] ??
                  detail.data.invocation.status}{' '}
                · {t('Billing status')}:{' '}
                {labels[detail.data.invocation.billing_status] ??
                  detail.data.invocation.billing_status}
              </p>
              <p>
                {t('User ID')}: {detail.data.invocation.user_id} ·{' '}
                {t('Payer ID')}: {detail.data.invocation.payer_user_id}
              </p>
              <p>
                {t('Channel')}: {detail.data.invocation.channel_id}
              </p>
              <p>
                {t('Reserved quota')}: {detail.data.invocation.charge_quota} ·{' '}
                {t('Charged quota')}: {detail.data.invocation.charged_quota}
              </p>
              <p>
                {t('Reconciliation deadline')}:{' '}
                {detail.data.invocation.reconcile_at ?? '—'}
              </p>
              {ownTest && result.isLoading && <LoadingState />}
              {ownTest && result.isError && (
                <ErrorState onRetry={() => void result.refetch()} />
              )}
              {ownTest && result.data?.result != null && (
                <pre className='max-h-96 overflow-auto text-xs break-all whitespace-pre-wrap'>
                  {JSON.stringify(result.data.result, null, 2)}
                </pre>
              )}
              <StaticDataTable
                data={detail.data.billing_operations ?? []}
                getRowKey={(row) => row.id}
                columns={[
                  {
                    id: 'kind',
                    header: t('Operation'),
                    cell: (row) => row.kind,
                  },
                  {
                    id: 'amount',
                    header: t('Amount'),
                    cell: (row) => row.amount,
                  },
                  {
                    id: 'state',
                    header: t('Status'),
                    cell: (row) => row.state,
                  },
                  {
                    id: 'time',
                    header: t('Time'),
                    cell: (row) => row.updated_at,
                  },
                ]}
              />
            </div>
          )
        )}
      </Dialog>
    </div>
  )
}
