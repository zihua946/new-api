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
import { useRef, useState } from 'react'
import { useTranslation } from 'react-i18next'

import { ErrorState } from '@/components/error-state'
import { JsonCodeEditor } from '@/components/json-code-editor'
import { LoadingState } from '@/components/loading-state'
import {
  Accordion,
  AccordionItem,
  AccordionTrigger,
  AccordionContent,
} from '@/components/ui/accordion'
import { Button } from '@/components/ui/button'
import { Spinner } from '@/components/ui/spinner'
import { handleServerError } from '@/lib/handle-server-error'
import { getServerErrorSources } from '@/lib/server-error-message'

import { mcpRequest } from './api'
import { ArgumentFields } from './argument-fields'
import { useMCPStatusLabels } from './status-labels'
import { readArguments, toolParameters } from './tool-parameters'
import {
  testPending,
  testRequest,
  type MCPServer,
  type MCPTestResult,
  type MCPTool,
} from './types'

export function TestPanel({
  server,
  catalogMode,
}: {
  server: MCPServer
  catalogMode?: 'admin' | 'self'
}) {
  const prefix = catalogMode === 'self' ? '/self' : ''
  const serverPath = `${prefix}/servers/${server.id}`
  const { t } = useTranslation()
  const client = useQueryClient()
  const tools = useQuery({
    retry: false,
    queryKey: ['mcp', 'test-tools', catalogMode, server.id],
    queryFn: () =>
      mcpRequest<MCPTool[]>(
        serverPath + (catalogMode ? '/test-tools' : '/tools')
      ),
  })
  const auth = useQuery({
    queryKey: ['mcp', 'test-auth', server.id],
    queryFn: () =>
      mcpRequest<{
        authorized: boolean
        authorization?: { subject: string; expires_at: string }
      }>(`/servers/${server.id}/test-authorization`),
    enabled: !catalogMode && server.auth_mode === 'casdoor_oauth',
    refetchInterval: 30_000,
  })
  let authorizationPath = `/servers/${server.id}/test-authorizations`
  if (catalogMode === 'admin') {
    authorizationPath = `${serverPath}/discovery-authorization`
  } else if (catalogMode === 'self') {
    authorizationPath = `/self/installations/${server.installation?.id}/authorization`
  }
  const authorize = useMutation({
    mutationFn: () =>
      mcpRequest<{ authorization_url: string }>(authorizationPath, 'POST'),
    onSuccess: (value) => {
      sessionStorage.removeItem('mcp-authorization-return')
      window.location.assign(value.authorization_url)
    },
    onError: (error) => handleServerError(error),
  })
  const clearAuth = useMutation({
    mutationFn: () =>
      mcpRequest(`/servers/${server.id}/test-authorization`, 'DELETE'),
    onSuccess: () =>
      client.invalidateQueries({ queryKey: ['mcp', 'test-auth', server.id] }),
    onError: (error) => handleServerError(error),
  })
  const connection = useMutation({
    mutationFn: () =>
      mcpRequest(`/servers/${server.id}/connection-tests`, 'POST'),
    onSettled: () =>
      client.invalidateQueries({ queryKey: ['mcp', 'server', server.id] }),
    onError: (error) => handleServerError(error),
  })
  const requiresAuthorization =
    tools.isError &&
    getServerErrorSources(tools.error).some(
      (source) =>
        source.code === 'ENGINE_AUTHORIZATION_REQUIRED' ||
        source.code === 'REAUTHORIZATION_REQUIRED'
    )
  const ready =
    server.auth_mode !== 'casdoor_oauth' || auth.data?.authorized === true
  return (
    <div className='space-y-4'>
      {server.auth_mode === 'casdoor_oauth' && (
        <div className='flex flex-wrap items-center gap-2'>
          <p className='text-sm'>
            {auth.data?.authorized
              ? `${auth.data.authorization?.subject} · ${auth.data.authorization?.expires_at}`
              : t('Authorize your own Casdoor account before testing.')}
          </p>
          <Button
            variant='outline'
            disabled={authorize.isPending}
            onClick={() => authorize.mutate()}
          >
            {t('Authorize test')}
          </Button>
          {auth.data?.authorized && (
            <Button
              variant='outline'
              disabled={clearAuth.isPending}
              onClick={() => clearAuth.mutate()}
            >
              {t('Clear authorization')}
            </Button>
          )}
          {auth.isError && <ErrorState onRetry={() => void auth.refetch()} />}
        </div>
      )}
      {catalogMode && tools.isError && (
        <div role='alert' className='space-y-2'>
          <p>
            {requiresAuthorization
              ? t('Authorize your own Casdoor account before testing.')
              : tools.error.message}
          </p>
          {requiresAuthorization && (
            <Button
              variant='outline'
              disabled={authorize.isPending}
              onClick={() => authorize.mutate()}
            >
              {t('Authorize')}
            </Button>
          )}
        </div>
      )}
      {!catalogMode && (
        <Button
          variant='outline'
          disabled={connection.isPending}
          onClick={() => connection.mutate()}
        >
          {t('Check connection')}
        </Button>
      )}
      {connection.isSuccess && (
        <p role='status' className='text-sm'>
          {t('Connection verified. Tool execution has not been tested.')}
        </p>
      )}
      {tools.isLoading && <LoadingState />}
      {tools.isError && !catalogMode && (
        <ErrorState onRetry={() => void tools.refetch()} />
      )}
      <Accordion className='gap-3'>
        {tools.data
          ?.filter((tool) => !tool.removed && tool.pricing)
          .map((tool) => (
            <AccordionItem
              key={tool.id}
              value={tool.id}
              className='rounded-lg border px-4'
            >
              <AccordionTrigger aria-label={tool.name}>
                <span className='grid gap-1'>
                  <strong>{tool.name}</strong>
                  <span className='text-muted-foreground font-normal'>
                    {tool.description}
                  </span>
                </span>
              </AccordionTrigger>
              <AccordionContent keepMounted>
                <ToolTest
                  tool={tool}
                  serverPath={serverPath}
                  prefix={prefix}
                  ready={ready}
                  refresh={() => void tools.refetch()}
                />
              </AccordionContent>
            </AccordionItem>
          ))}
      </Accordion>
    </div>
  )
}

function ToolTest({
  tool,
  serverPath,
  prefix,
  ready,
  refresh,
}: {
  tool: MCPTool
  serverPath: string
  prefix: string
  ready: boolean
  refresh: () => void
}) {
  const { t } = useTranslation()
  const labels = useMCPStatusLabels()
  const parameters = toolParameters(tool.input_schema)
  const [raw, setRaw] = useState('{}')
  const [invalid, setInvalid] = useState(false)
  const [invocationID, setInvocationID] = useState('')
  const [expanded, setExpanded] = useState(true)
  const attempt = useRef<{ body: string; key: string } | null>(null)
  const run = useMutation({
    mutationFn: ({
      body,
      key,
    }: {
      body: ReturnType<typeof testRequest>
      key: string
    }) =>
      mcpRequest<{ invocation_id: string }>(
        `${serverPath}/test-invocations`,
        'POST',
        body,
        key
      ),
    onSuccess: (data) => {
      attempt.current = null
      setInvocationID(data.invocation_id)
    },
    onError: (error) => {
      if (
        getServerErrorSources(error).some((source) =>
          [
            'PRICE_CONFIRMATION_REQUIRED',
            'REVISION_CONFLICT',
            'TOOL_CHANGED',
          ].includes(String(source.code))
        )
      ) {
        attempt.current = null
        refresh()
      }
    },
  })
  const result = useQuery({
    queryKey: ['mcp', 'test-result', invocationID],
    queryFn: () =>
      mcpRequest<MCPTestResult>(`${prefix}/test-invocations/${invocationID}`),
    enabled: !!invocationID,
    retry: false,
    refetchInterval: (query) =>
      !query.state.error && testPending(query.state.data) ? 1500 : false,
  })
  const busy =
    run.isPending ||
    (!!invocationID && !result.isError && testPending(result.data))
  const error = run.error || result.error
  const invocation = result.data?.invocation
  return (
    <form
      className='grid gap-4 py-2'
      onSubmit={(event) => {
        event.preventDefault()
        if (busy || !ready) return
        try {
          const args = parameters
            ? readArguments(parameters, new FormData(event.currentTarget))
            : raw
          if (args.length > 1048576) throw new Error()
          const body = testRequest(tool, args)
          const encoded = JSON.stringify(body)
          if (attempt.current?.body !== encoded) {
            attempt.current = { body: encoded, key: crypto.randomUUID() }
          }
          setInvalid(false)
          setInvocationID('')
          setExpanded(true)
          run.mutate({ body, key: attempt.current.key })
        } catch {
          setInvalid(true)
        }
      }}
    >
      {parameters ? (
        <ArgumentFields fields={parameters} disabled={busy} />
      ) : (
        <JsonCodeEditor
          value={raw}
          onChange={setRaw}
          ariaLabel={t('Arguments')}
          disabled={busy}
        />
      )}
      {invalid && <p role='alert'>{t('Invalid value')}</p>}
      <div className='text-muted-foreground text-sm'>
        <div>
          {t('Maximum MCP charge')} · {tool.pricing?.effective_credits} Credits
          · {tool.pricing?.charge_quota} quota
        </div>
        {t(
          'Real tests use your payment identity and normal billing. Knowledge tasks can incur model and search charges inside the engine.'
        )}
      </div>
      <Button
        className='justify-self-start rounded-full'
        size={busy ? 'icon' : 'default'}
        aria-label={t('Execute')}
        aria-busy={busy}
        type='submit'
        disabled={!ready || busy}
      >
        {busy ? <Spinner aria-label={t('Running')} /> : t('Execute')}
      </Button>
      {!run.isIdle && !busy && (
        <details
          open={expanded}
          onToggle={(event) => setExpanded(event.currentTarget.open)}
          className='min-w-0 rounded-md border p-3'
        >
          <summary className='cursor-pointer font-medium'>
            {t('Result')} · JSON
          </summary>
          {invocation && (
            <p className='mt-2 text-sm'>
              {labels[invocation.status] ?? invocation.status} ·{' '}
              {labels[invocation.billing_status] ?? invocation.billing_status} ·{' '}
              {t('Charged quota')} {invocation.charged_quota}
            </p>
          )}
          {invocation?.error_code === 'INSUFFICIENT_CREDITS' && (
            <p role='alert'>
              {t('Insufficient balance')} · <a href='/wallet'>{t('Wallet')}</a>
            </p>
          )}
          {error || result.data ? (
            <pre className='bg-muted mt-2 max-h-96 overflow-auto rounded p-3 text-xs'>
              {JSON.stringify(
                error
                  ? { error: error.message }
                  : (result.data?.result ?? invocation),
                null,
                2
              )}
            </pre>
          ) : (
            <LoadingState />
          )}
        </details>
      )}
    </form>
  )
}
