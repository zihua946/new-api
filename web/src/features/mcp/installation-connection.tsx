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
}: {
  item: Item
  installation: Installation
  compact?: boolean
}) {
  const { t } = useTranslation()
  const client = useQueryClient()
  const [waiting, setWaiting] = useState(false)
  const browser = useRef<Window | null>(null)
  const path = `/self/installations/${installation.id}`
  const status = useQuery({
    queryKey: ['mcp', 'connection', installation.id],
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
    const timer = window.setTimeout(() => setWaiting(false), 600000)
    return () => window.clearTimeout(timer)
  }, [waiting])
  const change = useMutation({
    mutationFn: async (action: 'enable' | 'reset' | 'disconnect') => {
      if (action === 'disconnect') {
        await mcpRequest(`${path}/disable`, 'POST')
        return
      }
      if (action === 'reset') {
        await mcpRequest(`${path}/authorization`, 'DELETE')
      }
      const response = await mcpRequest<{
        authorization_url?: string
        connected?: boolean
      }>(path + (action === 'reset' ? '/authorization' : '/enable'), 'POST')
      if (response.authorization_url) {
        if (browser.current && !browser.current.closed) {
          browser.current.location.replace(response.authorization_url)
          setWaiting(true)
        } else window.location.assign(response.authorization_url)
      } else browser.current?.close()
    },
    onError: (error) => {
      browser.current?.close()
      handleServerError(error)
    },
    onSettled: () => client.invalidateQueries({ queryKey: ['mcp'] }),
  })
  const act = (action: 'enable' | 'reset' | 'disconnect') => {
    if (action === 'disconnect') {
      setWaiting(false)
      browser.current?.close()
      browser.current = null
    } else if (item.auth_mode !== 'none') {
      client.removeQueries({ queryKey: ['mcp', 'connection', installation.id] })
      const popup = window.open('about:blank', '_blank')
      browser.current = popup
      if (popup) {
        popup.opener = null
        popup.sessionStorage.setItem('mcp-return-installation', item.id)
      }
    }
    change.mutate(action)
  }
  return (
    <>
      {!waiting &&
        (compact || installation.authorized) &&
        item.auth_mode !== 'none' && (
          <Button
            size='sm'
            variant={compact ? 'secondary' : 'ghost'}
            className={compact ? 'rounded-full px-4' : undefined}
            disabled={change.isPending || !item.enabled}
            onClick={() => act('reset')}
          >
            {compact ? t('Reset') : t('Reset authentication')}
          </Button>
        )}
      <Switch
        aria-label={item.name}
        checked={waiting || installation.authorized}
        disabled={
          change.isPending ||
          (!item.enabled && !installation.authorized && !waiting)
        }
        onCheckedChange={(checked) => act(checked ? 'enable' : 'disconnect')}
      />
      {waiting && (
        <span role='status'>{t('Waiting for browser authorization')}</span>
      )}
    </>
  )
}
