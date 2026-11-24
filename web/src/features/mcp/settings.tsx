import { zodResolver } from '@hookform/resolvers/zod'
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
import { useState } from 'react'
import { useForm } from 'react-hook-form'
import { useTranslation } from 'react-i18next'
import z from 'zod'

import { ConfirmDialog } from '@/components/confirm-dialog'
import { ErrorState } from '@/components/error-state'
import { LoadingState } from '@/components/loading-state'
import { PasswordInput } from '@/components/password-input'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select'
import {
  SecureVerificationDialog,
  useSecureVerification,
} from '@/features/auth/secure-verification'
import { SettingsCard } from '@/features/system-settings/components/settings-card'
import { api } from '@/lib/api'
import { handleServerError } from '@/lib/handle-server-error'

import { mcpRequest } from './api'

const settingsSchema = z.object({
  values: z.record(z.string(), z.string().max(4096)),
})
const applicationSchema = z.object({
  id: z.string().trim().min(1).max(256),
  serverIDs: z.string().trim().min(1),
  quota: z.number().int().min(1).max(2147483647),
  enabled: z.boolean(),
  exchange: z.string().max(4096).optional(),
  casdoor: z.string().max(4096).optional(),
})
interface Settings {
  revision: number
  values: Record<string, string>
  secrets: Record<string, boolean>
  effective: Record<string, string>
}
interface Application {
  id: string
  revision: number
  enabled: boolean
  server_ids: string[]
  max_charge_quota: number
  exchange_configured: boolean
  casdoor_configured: boolean
}
const fields = [
  ['public_base_url', 'Public URL'],
  ['dashboard_base_url', 'Dashboard URL'],
  ['casdoor_organization', 'Organization'],
  ['oauth_client_id', 'Resource verifier client ID'],
  ['oauth_allowed_client_ids', 'Allowed client IDs'],
  ['knowledge_engine_url', 'Knowledge engine URL'],
  ['knowledge_client_id', 'Knowledge application ID'],
  ['knowledge_scopes', 'Scopes'],
] as const
const secrets = [
  ['oauth_client_secret', 'Resource verifier secret'],
  ['knowledge_client_secret', 'Engine exchange secret'],
  ['knowledge_oauth_client_secret', 'Knowledge OAuth secret'],
] as const

export function MCPConfiguration(props: {
  sensitive: boolean
  apps?: boolean
}) {
  const query = useQuery<Settings | Application[]>({
    queryKey: ['mcp', props.apps ? 'trusted-apps' : 'settings'],
    queryFn: () =>
      props.apps
        ? mcpRequest<Application[]>('/trusted-apps')
        : mcpRequest<Settings>('/settings'),
  })
  if (query.isPending) return <LoadingState />
  if (query.isError) return <ErrorState onRetry={() => void query.refetch()} />
  return props.apps ? (
    <TrustedApps
      key={(query.data as Application[])
        .map((row) => `${row.id}:${row.revision}`)
        .join(',')}
      rows={query.data as Application[]}
      sensitive={props.sensitive}
    />
  ) : (
    <SettingsEditor
      key={(query.data as Settings).revision}
      settings={query.data as Settings}
      sensitive={props.sensitive}
    />
  )
}
function SettingsEditor(props: { settings: Settings; sensitive: boolean }) {
  const { t } = useTranslation()
  const form = useForm<z.infer<typeof settingsSchema>>({
    resolver: zodResolver(settingsSchema),
    defaultValues: { values: props.settings.values },
  })
  const values = form.watch('values')
  const verification = useSecureVerification()
  const client = useQueryClient()
  const providers = useQuery({
    queryKey: ['mcp', 'identity-providers'],
    queryFn: () =>
      mcpRequest<{ id: number; name: string }[]>('/settings/providers'),
  })
  const check = useMutation({
    mutationFn: () => mcpRequest<Record<string, boolean>>('/settings/check'),
    onError: (error) => handleServerError(error),
  })

  const save = useMutation({
    mutationFn: async () => {
      const proof = await verification.requestVerification({
        scope: 'mcp.config.write',
        context: { target: 'settings', revision: props.settings.revision },
        title: t('Settings'),
      })
      if (!proof) return
      await saveConfiguration(
        '/settings',
        { expected_revision: props.settings.revision, values },
        proof.proof_token
      )
      await client.invalidateQueries({ queryKey: ['mcp'] })
    },
    onError: (error) => handleServerError(error),
  })
  return (
    <SettingsCard
      title={t('Settings')}
      description={t(
        'Business settings are stored in the database. Deployment policies remain in environment variables.'
      )}
    >
      <form
        className='grid gap-4 sm:grid-cols-2'
        onSubmit={form.handleSubmit(
          () => save.mutate(),
          () => handleServerError(new Error(t('Invalid value')))
        )}
      >
        <div className='space-y-2'>
          <Label>{t('OAuth provider')}</Label>
          <Select
            value={values.casdoor_provider_id || 'none'}
            disabled={!props.sensitive}
            onValueChange={(value) =>
              form.setValue('values', {
                ...values,
                casdoor_provider_id: value === 'none' ? '' : (value ?? ''),
              })
            }
          >
            <SelectTrigger>
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value='none'>{t('None')}</SelectItem>
              {providers.data?.map((p) => (
                <SelectItem value={String(p.id)} key={p.id}>
                  {p.name}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
          {providers.isError && (
            <ErrorState onRetry={() => void providers.refetch()} />
          )}
        </div>
        {fields.map(([key, label]) => (
          <div className='space-y-2' key={key}>
            <Label htmlFor={key}>{t(label)}</Label>
            <Input
              id={key}
              disabled={!props.sensitive}
              value={values[key] ?? ''}
              placeholder={props.settings.effective[key]}
              onChange={(e) =>
                form.setValue('values', { ...values, [key]: e.target.value })
              }
            />
          </div>
        ))}
        {secrets.map(([key, label]) => (
          <div className='space-y-2' key={key}>
            <Label htmlFor={key}>{t(label)}</Label>
            <PasswordInput
              id={key}
              value={values[key] ?? ''}
              autoComplete='new-password'
              disabled={!props.sensitive}
              placeholder={
                !Object.hasOwn(values, key) && props.settings.secrets[key]
                  ? t('Configured')
                  : t('Not configured')
              }
              onChange={(e) =>
                form.setValue('values', { ...values, [key]: e.target.value })
              }
            />
            <Button
              type='button'
              variant='outline'
              disabled={!props.sensitive}
              onClick={() => form.setValue('values', { ...values, [key]: '' })}
            >
              {t('Clear')}
            </Button>
          </div>
        ))}
        <Button disabled={!props.sensitive || save.isPending} type='submit'>
          {t('Save')}
        </Button>
      </form>
      <Button
        variant='outline'
        disabled={check.isPending}
        onClick={() => check.mutate()}
      >
        {t('Check configuration')}
      </Button>
      {check.data && (
        <ul>
          {Object.entries(check.data).map(([key, ready]) => (
            <li key={key}>
              {t(
                fields.find(([field]) => field === key)?.[1] ??
                  (key === 'casdoor_provider_id'
                    ? 'OAuth provider'
                    : 'Settings')
              )}
              : {ready ? t('Configured') : t('Not configured')}
            </li>
          ))}
        </ul>
      )}
      <SecureVerificationDialog {...verification.dialogProps} />
    </SettingsCard>
  )
}
async function saveConfiguration(
  path: string,
  data: unknown,
  proof: string,
  method: 'PUT' | 'DELETE' = 'PUT'
) {
  const response = await api.request<{ success: boolean; message: string }>({
    url: `/api/mcp${path}`,
    method,
    data,
    headers: { 'X-MCP-Request': '1', 'X-Security-Proof': proof },
    singleUseAuthorization: true,
  })
  if (!response.data.success) throw new Error(response.data.message)
}
function TrustedApps(props: { rows: Application[]; sensitive: boolean }) {
  const { t } = useTranslation()
  const [selected, setSelected] = useState<Application | null>(null)
  const form = useForm<z.infer<typeof applicationSchema>>({
    resolver: zodResolver(applicationSchema),
    defaultValues: { id: '', serverIDs: '', quota: 1, enabled: false },
  })
  const { id, serverIDs, quota, enabled, exchange, casdoor } = form.watch()
  const [deleting, setDeleting] = useState(false)
  const verification = useSecureVerification()
  const client = useQueryClient()
  const save = useMutation({
    mutationFn: async (remove: boolean) => {
      const revision = selected?.revision ?? 0
      const proof = await verification.requestVerification({
        scope: 'mcp.config.write',
        context: { target: `app:${id}`, revision },
        title: t('Trusted applications'),
      })
      if (!proof) return
      await saveConfiguration(
        `/trusted-apps/${encodeURIComponent(id)}`,
        {
          expected_revision: revision,
          enabled,
          server_ids: serverIDs
            .split(',')
            .map((s) => s.trim())
            .filter(Boolean),
          max_charge_quota: quota,
          ...(exchange !== undefined ? { exchange_secret: exchange } : {}),
          ...(casdoor !== undefined ? { casdoor_client_secret: casdoor } : {}),
        },
        proof.proof_token,
        remove ? 'DELETE' : 'PUT'
      )
      await client.invalidateQueries({ queryKey: ['mcp'] })
    },
    onError: (error) => handleServerError(error),
  })
  return (
    <SettingsCard title={t('Trusted applications')}>
      <div className='mb-4 flex flex-wrap gap-2'>
        {props.rows.map((row) => (
          <Button
            key={row.id}
            variant='outline'
            onClick={() => {
              setSelected(row)
              form.setValue('id', row.id)
              form.setValue('serverIDs', row.server_ids.join(','))
              form.setValue('quota', row.max_charge_quota)
              form.setValue('enabled', row.enabled)
              form.setValue('exchange', undefined)
              form.setValue('casdoor', undefined)
            }}
          >
            {row.id}
          </Button>
        ))}
        <Button
          variant='outline'
          onClick={() => {
            setSelected(null)
            form.setValue('id', '')
            form.setValue('serverIDs', '')
            form.setValue('quota', 1)
            form.setValue('enabled', false)
            form.setValue('exchange', undefined)
            form.setValue('casdoor', undefined)
          }}
        >
          {t('Add')}
        </Button>
      </div>
      <form
        className='grid gap-4 sm:grid-cols-2'
        onSubmit={form.handleSubmit(
          () => save.mutate(false),
          () => handleServerError(new Error(t('Invalid value')))
        )}
      >
        <div>
          <Label htmlFor='app-id'>{t('Client ID')}</Label>
          <Input
            id='app-id'
            value={id}
            disabled={!props.sensitive || !!selected}
            onChange={(e) => form.setValue('id', e.target.value)}
          />
        </div>
        <div>
          <Label htmlFor='app-services'>{t('Server IDs')}</Label>
          <Input
            id='app-services'
            value={serverIDs}
            disabled={!props.sensitive}
            onChange={(e) => form.setValue('serverIDs', e.target.value)}
          />
        </div>
        <div>
          <Label htmlFor='app-quota'>{t('Maximum charge quota')}</Label>
          <Input
            id='app-quota'
            type='number'
            min={1}
            max={2147483647}
            value={quota}
            disabled={!props.sensitive}
            onChange={(e) => form.setValue('quota', e.target.valueAsNumber)}
          />
        </div>
        <div>
          <Label>{t('Status')}</Label>
          <Select
            value={enabled ? 'true' : 'false'}
            disabled={!props.sensitive}
            onValueChange={(v) => form.setValue('enabled', v === 'true')}
          >
            <SelectTrigger>
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value='true'>{t('Enabled')}</SelectItem>
              <SelectItem value='false'>{t('Disabled')}</SelectItem>
            </SelectContent>
          </Select>
        </div>
        <div>
          <Label htmlFor='app-exchange'>
            {t('Application exchange secret')}
          </Label>
          <PasswordInput
            key={`${selected?.id}-exchange`}
            value={exchange ?? ''}
            id='app-exchange'
            autoComplete='new-password'
            disabled={!props.sensitive}
            placeholder={
              selected?.exchange_configured
                ? t('Configured')
                : t('Not configured')
            }
            onChange={(e) => form.setValue('exchange', e.target.value)}
          />
        </div>
        <div>
          <Label htmlFor='app-casdoor'>{t('Client Secret')}</Label>
          <PasswordInput
            key={`${selected?.id}-casdoor`}
            value={casdoor ?? ''}
            id='app-casdoor'
            autoComplete='new-password'
            disabled={!props.sensitive}
            placeholder={
              selected?.casdoor_configured
                ? t('Configured')
                : t('Not configured')
            }
            onChange={(e) => form.setValue('casdoor', e.target.value)}
          />
        </div>
        <Button
          type='submit'
          disabled={!props.sensitive || !id || save.isPending}
        >
          {t('Save')}
        </Button>
        <Button
          type='button'
          variant='destructive'
          disabled={!props.sensitive || !selected || save.isPending}
          onClick={() => setDeleting(true)}
        >
          {t('Delete')}
        </Button>
      </form>
      <ConfirmDialog
        open={deleting}
        onOpenChange={setDeleting}
        title={t('Delete')}
        desc={id}
        destructive
        isLoading={save.isPending}
        handleConfirm={() => save.mutate(true)}
      />
      <SecureVerificationDialog {...verification.dialogProps} />
    </SettingsCard>
  )
}
