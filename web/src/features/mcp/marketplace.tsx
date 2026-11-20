import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useNavigate } from '@tanstack/react-router'
import { useEffect, useRef, useState } from 'react'
import { useTranslation } from 'react-i18next'

import { ConfirmDialog } from '@/components/confirm-dialog'
import { CopyButton } from '@/components/copy-button'
import { EmptyState } from '@/components/empty-state'
import { ErrorState } from '@/components/error-state'
import { LoadingState } from '@/components/loading-state'
import { Badge } from '@/components/ui/badge'
import { Button, buttonVariants } from '@/components/ui/button'
import {
  Card,
  CardContent,
  CardFooter,
  CardHeader,
  CardTitle,
} from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { Tabs, TabsList, TabsTrigger } from '@/components/ui/tabs'
import { handleServerError } from '@/lib/handle-server-error'

import { mcpRequest } from './api'
import { InstallationConnection } from './installation-connection'
import { ServiceIcon } from './service-page'
import type { MCPPage } from './types'

type Installation = { id: string; authorized: boolean }
type CatalogItem = {
  id: string
  name: string
  description: string
  version: string
  auth_mode: 'oauth2' | 'none' | 'engine_app'
  enabled: boolean
  url?: string
  icon?: string
  installation?: Installation
}

export function MCPMarketplace() {
  const { t } = useTranslation()
  const client = useQueryClient()
  const navigate = useNavigate()
  const [name, setName] = useState('')
  const [tab, setTab] = useState('all')
  const [page, setPage] = useState(1)
  const [removing, setRemoving] = useState<CatalogItem | null>(null)
  const callbackStarted = useRef(false)
  const catalog = useQuery({
    queryKey: ['mcp', 'catalog', tab, name, page],
    queryFn: () =>
      mcpRequest<MCPPage<CatalogItem>>(
        `${tab === 'installed' ? '/self/installations' : '/catalog'}?${new URLSearchParams({ p: String(page), page_size: '24', name })}`
      ),
  })
  const change = useMutation({
    mutationFn: async (action: { item: CatalogItem; remove?: boolean }) => {
      const { item } = action
      if (action.remove && item.installation) {
        await mcpRequest(
          `/self/installations/${item.installation.id}`,
          'DELETE'
        )
        return
      }
      if (!item.installation) {
        await mcpRequest('/self/installations', 'POST', { server_id: item.id })
        return
      }
    },
    onSettled: () => client.invalidateQueries({ queryKey: ['mcp', 'catalog'] }),
    onSuccess: () => setRemoving(null),
    onError: (error) => handleServerError(error),
  })

  useEffect(() => {
    if (callbackStarted.current) return
    const params = new URLSearchParams(window.location.hash.slice(1))
    const state = params.get('mcp_state')
    if (!state) return
    callbackStarted.current = true
    window.history.replaceState(null, '', window.location.pathname)
    void mcpRequest<{ admin: boolean }>('/oauth/callback', 'POST', {
      state,
      code: params.get('mcp_code') ?? '',
      issuer: params.get('mcp_iss') ?? '',
    })
      .then(async ({ admin }) => {
        await client.invalidateQueries({ queryKey: ['mcp'] })
        if (admin) await navigate({ to: '/mcp' })
      })
      .catch((error: unknown) => handleServerError(error))
  }, [client, navigate])

  return (
    <main className='mx-auto w-full max-w-7xl space-y-6 px-4 py-6 sm:px-6'>
      <h1 className='text-2xl font-semibold tracking-tight'>
        {t('MCP Marketplace')}
      </h1>
      <div className='flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-between'>
        <Tabs
          value={tab}
          onValueChange={(next) => {
            setTab(next)
            setPage(1)
          }}
        >
          <TabsList>
            <TabsTrigger value='all'>{t('All')}</TabsTrigger>
            <TabsTrigger value='installed'>{t('Installed')}</TabsTrigger>
          </TabsList>
        </Tabs>
        <Input
          className='sm:max-w-72'
          aria-label={t('Search servers...')}
          placeholder={t('Search servers...')}
          value={name}
          onChange={(event) => {
            setName(event.target.value)
            setPage(1)
          }}
        />
      </div>
      {catalog.isPending && <LoadingState />}
      {catalog.isError && <ErrorState onRetry={() => void catalog.refetch()} />}
      {catalog.data?.items.length === 0 && (
        <EmptyState title={t('No MCP servers found')} />
      )}
      <div className='grid gap-4 sm:grid-cols-2 xl:grid-cols-3'>
        {catalog.data?.items.map((item) => (
          <Card key={item.id} className='flex h-full min-w-0 flex-col'>
            <a
              href={`/mcp/servers/${item.id}`}
              aria-label={item.name}
              className='hover:bg-muted/30 flex flex-1 flex-col gap-4 focus-visible:outline-offset-[-2px]'
            >
              <CardHeader>
                <div className='flex items-start gap-3'>
                  <span className='bg-muted rounded-lg p-2'>
                    <ServiceIcon icon={item.icon} />
                  </span>
                  <div className='min-w-0 flex-1 space-y-1'>
                    <CardTitle className='break-words'>{item.name}</CardTitle>
                    <p className='text-muted-foreground text-xs'>
                      {[
                        item.version,
                        item.auth_mode !== 'none'
                          ? 'OAuth2'
                          : t('No authentication'),
                      ]
                        .filter(Boolean)
                        .join(' · ')}
                    </p>
                  </div>
                  {item.installation && (
                    <Badge variant='secondary'>
                      {!item.installation.authorized && t('Disabled')}
                      {item.installation.authorized &&
                        item.auth_mode === 'none' &&
                        t('Installed')}
                      {item.installation.authorized &&
                        item.auth_mode !== 'none' &&
                        t('Connected')}
                    </Badge>
                  )}
                </div>
              </CardHeader>
              <CardContent className='flex-1'>
                <p className='text-muted-foreground line-clamp-3 text-sm break-words'>
                  {item.description}
                </p>
              </CardContent>
            </a>
            <CardFooter className='flex flex-wrap gap-2 border-t'>
              {!item.enabled && (
                <Badge variant='outline'>{t('Unavailable')}</Badge>
              )}
              {!item.installation && item.enabled && (
                <Button
                  size='sm'
                  disabled={change.isPending}
                  onClick={() => change.mutate({ item })}
                >
                  {t('Install')}
                </Button>
              )}
              {item.installation && (
                <InstallationConnection
                  item={item}
                  installation={item.installation}
                />
              )}
              {item.url && (
                <a
                  className={buttonVariants({ size: 'sm' })}
                  href={`/mcp/servers/${item.id}?tab=tools`}
                >
                  {t('Test')}
                </a>
              )}
              {item.url && (
                <CopyButton
                  value={JSON.stringify(
                    {
                      url: item.url,
                      transport: 'streamable_http',
                      headers: { Authorization: 'Bearer <NEW_API_KEY>' },
                    },
                    null,
                    2
                  )}
                  tooltip={t('Copy connection configuration')}
                />
              )}
              {item.installation && (
                <Button
                  size='sm'
                  variant='ghost'
                  onClick={() => setRemoving(item)}
                >
                  {t('Uninstall')}
                </Button>
              )}
            </CardFooter>
          </Card>
        ))}
      </div>
      {catalog.data && catalog.data.total > 24 && (
        <div className='flex items-center justify-end gap-2'>
          <Button
            variant='outline'
            disabled={page === 1}
            onClick={() => setPage(page - 1)}
          >
            {t('Previous page')}
          </Button>
          <Button
            variant='outline'
            disabled={page * 24 >= catalog.data.total}
            onClick={() => setPage(page + 1)}
          >
            {t('Next page')}
          </Button>
        </div>
      )}
      {removing && (
        <ConfirmDialog
          open
          onOpenChange={(open) => {
            if (!open) setRemoving(null)
          }}
          title={t('Uninstall')}
          desc={removing.name}
          confirmText={t('Uninstall')}
          destructive
          isLoading={change.isPending}
          handleConfirm={() => change.mutate({ item: removing, remove: true })}
        />
      )}
    </main>
  )
}
