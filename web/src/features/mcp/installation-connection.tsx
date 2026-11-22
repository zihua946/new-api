import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useEffect, useRef, useState } from 'react'
import { useTranslation } from 'react-i18next'

import { Button } from '@/components/ui/button'
import { Switch } from '@/components/ui/switch'
import { handleServerError } from '@/lib/handle-server-error'

import { mcpRequest } from './api'

type Item = {
  id: string
  name: string
  description: string
  auth_mode: string
  enabled: boolean
}
type Installation = { id: string; authorized: boolean }
export function InstallationConnection({
  item,
  installation,
  compact = false,
  returnToMarketplace = false,
}: {
  item: Item
  installation?: Installation
  compact?: boolean
  returnToMarketplace?: boolean
}) {
  const { t } = useTranslation()
  const client = useQueryClient()
  const [waiting, setWaiting] = useState(false)
  const [failure, setFailure] = useState('')
  const browser = useRef<Window | null>(null)
  const status = useQuery({
    queryKey: ['mcp', 'connection', installation?.id ?? item.id],
    queryFn: () =>
      mcpRequest<{ installation?: Installation }>(`/catalog/${item.id}`),
    enabled: waiting,
    refetchInterval: waiting ? 1200 : false,
    refetchIntervalInBackground: true,
    staleTime: 0,
  })
  useEffect(() => {
    if (waiting && status.data?.installation?.authorized) {
      setWaiting(false)
      void client.invalidateQueries({ queryKey: ['mcp', 'catalog'] })
      void client.invalidateQueries({ queryKey: ['mcp', 'detail'] })
    }
  }, [waiting, status.data, client])
  useEffect(() => {
    if (!waiting) return
    const timer = window.setTimeout(() => {
      setWaiting(false)
      setFailure(t('Authorization required'))
    }, 600000)
    const closed = window.setInterval(() => {
      if (!browser.current?.closed) return
      setWaiting(false)
      void mcpRequest<{ installation?: Installation }>(`/catalog/${item.id}`)
        .then((result) => {
          if (!result.installation?.authorized) {
            setFailure(t('Authorization required'))
          }
          return client.invalidateQueries({ queryKey: ['mcp'] })
        })
        .catch(handleServerError)
    }, 1200)
    const channel =
      typeof BroadcastChannel === 'undefined'
        ? null
        : new BroadcastChannel('mcp-authorization')
    if (channel) {
      channel.addEventListener('message', (event: MessageEvent<unknown>) => {
        const result = event.data
        if (
          !result ||
          typeof result !== 'object' ||
          !('server_id' in result) ||
          result.server_id !== item.id ||
          !('success' in result)
        ) {
          return
        }
        if (result.success === false) {
          setWaiting(false)
          setFailure(
            'error' in result && typeof result.error === 'string'
              ? result.error
              : t('Authorization required')
          )
        }
        void client.invalidateQueries({ queryKey: ['mcp'] })
      })
    }
    return () => {
      window.clearTimeout(timer)
      window.clearInterval(closed)
      channel?.close()
    }
  }, [waiting, item.id, client, t])
  const change = useMutation({
    mutationFn: async (action: 'enable' | 'reset' | 'disconnect') => {
      if (!installation && action !== 'enable') return
      const installed =
        installation ??
        (await mcpRequest<Installation>('/self/installations', 'POST', {
          server_id: item.id,
        }))
      const path = `/self/installations/${installed.id}`
      if (action === 'disconnect') {
        await mcpRequest(`${path}/disable`, 'POST')
        return
      }
      if (action === 'reset') {
        await mcpRequest(`${path}/authorization`, 'DELETE')
        return
      }
      const response = await mcpRequest<{
        authorization_url?: string
        connected?: boolean
      }>(`${path}/enable`, 'POST')
      if (response.authorization_url) {
        if (browser.current && !browser.current.closed) {
          browser.current.location.replace(response.authorization_url)
          setWaiting(true)
        } else {
          if (returnToMarketplace) {
            sessionStorage.setItem(
              'mcp-authorization-return',
              `marketplace:${item.id}`
            )
          }
          window.location.assign(response.authorization_url)
        }
      } else browser.current?.close()
    },
    onError: (error) => {
      setWaiting(false)
      browser.current?.close()
      handleServerError(error)
    },
    onSettled: () => client.invalidateQueries({ queryKey: ['mcp'] }),
  })
  const act = (action: 'enable' | 'reset' | 'disconnect') => {
    setFailure('')
    if (action !== 'enable') {
      setWaiting(false)
      browser.current?.close()
      browser.current = null
    } else if (returnToMarketplace && item.auth_mode !== 'none') {
      client.removeQueries({
        queryKey: ['mcp', 'connection', installation?.id ?? item.id],
      })
      browser.current?.close()
      const popup = window.open('about:blank', '_blank')
      browser.current = popup
      if (popup) {
        popup.opener = null
        popup.sessionStorage.setItem(
          'mcp-authorization-return',
          `popup:${item.id}`
        )
      }
    } else {
      sessionStorage.removeItem('mcp-authorization-return')
    }
    change.mutate(action)
  }
  return (
    <>
      {(compact ||
        (!waiting &&
          installation?.authorized &&
          item.auth_mode !== 'none')) && (
        <Button
          size='sm'
          variant={compact ? 'secondary' : 'ghost'}
          className={compact ? 'rounded-full px-4' : undefined}
          disabled={change.isPending || !item.enabled || !installation}
          onClick={() => act('reset')}
        >
          {compact ? t('Reset') : t('Reset authentication')}
        </Button>
      )}
      <Switch
        aria-label={item.name}
        checked={waiting || !!installation?.authorized}
        disabled={
          change.isPending ||
          (!item.enabled && !installation?.authorized && !waiting)
        }
        onCheckedChange={(checked) => act(checked ? 'enable' : 'disconnect')}
      />
      {waiting && (
        <span role='status'>{t('Waiting for browser authorization')}</span>
      )}
      {failure && <span role='alert'>{failure}</span>}
    </>
  )
}
