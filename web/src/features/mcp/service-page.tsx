import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useEffect, useRef, useState } from 'react'
import { useTranslation } from 'react-i18next'

import { EmptyState } from '@/components/empty-state'
import { ErrorState } from '@/components/error-state'
import { SectionPageLayout } from '@/components/layout'
import { LoadingState } from '@/components/loading-state'
import { Button } from '@/components/ui/button'
import { Markdown } from '@/components/ui/markdown'
import { Tabs, TabsContent, TabsList, TabsTrigger } from '@/components/ui/tabs'
import { hasPermission } from '@/lib/admin-permissions'
import { getLobeIcon } from '@/lib/lobe-icon'
import { useAuthStore } from '@/stores/auth-store'

import { mcpRequest } from './api'
import { InstallationConnection } from './installation-connection'
import { TestPanel } from './test-panel'
import type { MCPServer } from './types'

export function ServiceIcon(props: { icon?: string }) {
  if (props.icon?.startsWith('data:image/png;base64,')) {
    return (
      <img
        alt=''
        className='size-8 shrink-0 rounded object-contain'
        src={props.icon}
      />
    )
  }
  return getLobeIcon(props.icon || 'MCP', 28)
}

export function ServicePage(props: {
  id: string
  manage: boolean
  tab: 'readme' | 'tools'
  onTabChange: (tab: 'readme' | 'tools') => void
}) {
  const { t } = useTranslation()
  const client = useQueryClient()
  const user = useAuthStore((s) => s.auth.user)
  const endpoint = props.manage ? '/servers/' : '/catalog/'
  const detail = useQuery({
    queryKey: ['mcp', 'detail', props.manage, props.id],
    queryFn: () => mcpRequest<MCPServer>(endpoint + props.id),
  })
  const [callback, setCallback] = useState<'pending' | 'failed' | 'done'>(() =>
    new URLSearchParams(window.location.hash.slice(1)).has('mcp_state')
      ? 'pending'
      : 'done'
  )
  const [reason, setReason] = useState('')
  const [returnToClient] = useState(
    () =>
      callback === 'pending' &&
      sessionStorage.getItem('mcp-return-installation') === props.id
  )
  const redeemed = useRef(false)
  useEffect(() => {
    if (redeemed.current || callback !== 'pending') return
    redeemed.current = true
    sessionStorage.removeItem('mcp-return-installation')
    const hash = new URLSearchParams(window.location.hash.slice(1))
    window.history.replaceState(
      window.history.state,
      '',
      window.location.pathname + window.location.search
    )
    void mcpRequest('/oauth/callback', 'POST', {
      state: hash.get('mcp_state'),
      code: hash.get('mcp_code') ?? '',
      issuer: hash.get('mcp_iss') ?? '',
    })
      .then(async () => {
        await client.invalidateQueries({ queryKey: ['mcp'] })
        setCallback('done')
      })
      .catch((e: unknown) => {
        setReason(e instanceof Error ? e.message : t('Authorization required'))
        setCallback('failed')
      })
  }, [client, callback, t])
  const install = useMutation({
    mutationFn: () =>
      mcpRequest('/self/installations', 'POST', { server_id: props.id }),
    onSuccess: () => detail.refetch(),
  })
  const authenticationLabels: Record<string, string> = {
    none: t('No authentication'),
    engine_app: 'Casdoor',
    oauth2: 'OAuth2',
  }
  const server = detail.data
  return (
    <SectionPageLayout>
      <SectionPageLayout.Title>
        <span className='flex items-center gap-3'>
          <ServiceIcon icon={server?.icon} />
          {server?.name ?? t('MCP Services')}
        </span>
      </SectionPageLayout.Title>
      {props.manage && (
        <SectionPageLayout.Actions>
          <Button
            variant='outline'
            render={
              <a href={`/mcp?server_id=${encodeURIComponent(props.id)}`} />
            }
          >
            {t('Manage')}
          </Button>
        </SectionPageLayout.Actions>
      )}
      <SectionPageLayout.Content>
        {detail.isLoading && <LoadingState />}
        {detail.isError && <ErrorState onRetry={() => void detail.refetch()} />}
        {server && (
          <Tabs
            value={props.tab}
            onValueChange={(v) =>
              props.onTabChange(v === 'tools' ? 'tools' : 'readme')
            }
          >
            <TabsList>
              <TabsTrigger value='readme'>
                {t('Service configuration')}
              </TabsTrigger>
              <TabsTrigger value='tools'>{t('Tool testing')}</TabsTrigger>
            </TabsList>
            <TabsContent value='readme' className='space-y-4'>
              <dl className='grid grid-cols-[auto_1fr] gap-x-4 gap-y-2 text-sm'>
                {server.version && (
                  <>
                    <dt>{t('Version')}</dt>
                    <dd>{server.version}</dd>
                  </>
                )}
                <dt>{t('Authentication')}</dt>
                <dd>
                  {authenticationLabels[server.auth_mode] ?? server.auth_mode}
                </dd>
              </dl>
              {server.readme ? (
                <Markdown>{server.readme}</Markdown>
              ) : (
                <EmptyState title={t('No service README')} />
              )}
            </TabsContent>
            <TabsContent value='tools'>
              {callback === 'pending' ? (
                <LoadingState />
              ) : (
                <>
                  {returnToClient && (
                    <Button onClick={() => window.close()}>
                      {t('Return to MCP client')}
                    </Button>
                  )}
                  {!props.manage &&
                    server.installation &&
                    !server.installation.authorized && (
                      <InstallationConnection
                        item={server}
                        installation={server.installation}
                      />
                    )}
                  {callback === 'failed' && <p role='alert'>{reason}</p>}
                  {props.manage && !hasPermission(user, 'mcp', 'operate') && (
                    <p>{t('Permission denied')}</p>
                  )}
                  {!props.manage && !server.installation && (
                    <Button
                      disabled={install.isPending}
                      onClick={() => install.mutate()}
                    >
                      {t('Install')}
                    </Button>
                  )}
                  {((props.manage && hasPermission(user, 'mcp', 'operate')) ||
                    (!props.manage && server.installation?.authorized)) && (
                    <TestPanel
                      key={props.id}
                      server={server}
                      catalogMode={props.manage ? 'admin' : 'self'}
                    />
                  )}
                </>
              )}
            </TabsContent>
          </Tabs>
        )}
      </SectionPageLayout.Content>
    </SectionPageLayout>
  )
}
