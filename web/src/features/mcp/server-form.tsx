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
import { useId, useState } from 'react'
import { useForm, useWatch } from 'react-hook-form'
import { useTranslation } from 'react-i18next'
import z from 'zod'

import { CopyButton } from '@/components/copy-button'
import { Dialog } from '@/components/dialog'
import { LobeIconField } from '@/components/lobe-icon-field'
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
import { Label } from '@/components/ui/label'
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select'
import { Textarea } from '@/components/ui/textarea'
import { encodePluginIconFile } from '@/features/task-plugins/lib/plugin-icon-file'
import { handleServerError } from '@/lib/handle-server-error'

import { mcpRequest } from './api'
import { SecretFields } from './secret-fields'
import type { MCPServer } from './types'

const schema = z
  .object({
    name: z.string().trim().min(1).max(128),
    description: z.string().max(4096),
    version: z.string().trim().max(64),
    url: z.string(),
    command: z.string().max(4096),
    image: z.string().max(256),
    icon: z.string(),
    readme: z.string().max(131072),
    transport: z.enum(['streamable_http', 'http_sse', 'stdio']),
    client_id: z.string().trim().max(256),
    client_secret: z.string().max(4096),
    timeout: z.number().int().min(1).max(600),
    concurrency: z.number().int().min(1).max(1000),
  })
  .superRefine((v, ctx) => {
    const bad = (key: string) =>
      ctx.addIssue({ code: 'custom', path: [key], message: 'Invalid value' })
    if (v.transport === 'stdio') {
      if (!v.image.trim()) bad('image')
      if (!v.command.trim()) bad('command')
    } else {
      if (!z.url({ protocol: /^https?$/ }).safeParse(v.url).success) bad('url')
    }
  })

export function ServerForm(props: {
  server?: MCPServer
  sensitive: boolean
  onClose: () => void
  onCreated?: (id: string) => void
}) {
  const { t } = useTranslation()
  const formID = useId()
  const client = useQueryClient()
  const { server } = props
  const [headers, setHeaders] = useState<Record<string, string>>()
  const [env, setEnv] = useState<Record<string, string>>()
  const publicConfig = useQuery({
    queryKey: ['mcp', 'oauth-config'],
    queryFn: () => mcpRequest<{ callback_url: string }>('/oauth/config'),
  })
  const storedTransport = server?.connection.transport
  const remoteTransport =
    storedTransport === 'http_sse' || storedTransport === 'stdio'
      ? storedTransport
      : 'streamable_http'
  const form = useForm<z.infer<typeof schema>>({
    resolver: zodResolver(schema),
    defaultValues: {
      name: server?.name ?? '',
      description: server?.description ?? '',
      version: server?.version ?? '',
      url: server?.connection.url ?? '',
      icon: server?.icon ?? '',
      readme: server?.readme ?? '',
      image: server?.connection.package ?? '',
      command: server?.connection.command ?? '',
      transport: remoteTransport,
      client_id: server?.connection.client_id ?? '',
      client_secret: '',
      timeout: server?.limits.timeout_seconds ?? 60,
      concurrency: server?.limits.max_concurrency ?? 10,
    },
  })
  const transport = useWatch({ control: form.control, name: 'transport' })
  const stdio = transport === 'stdio',
    library = server?.kind === 'public_knowledge'
  const [advanced, setAdvanced] = useState(false)
  const clientID = useWatch({ control: form.control, name: 'client_id' })
  const oauth = !stdio && !library && !!clientID.trim()
  const transportLabels = {
    stdio: 'stdio',
    http_sse: 'SSE',
    streamable_http: 'Streamable HTTP',
  }
  const preview = useMutation({
    mutationFn: () =>
      mcpRequest<string[]>('/command-preview', 'POST', {
        package: form.getValues('image'),
        command: form.getValues('command'),
      }),
    onError: (e) => handleServerError(e),
  })
  const icon = useWatch({ control: form.control, name: 'icon' })

  const save = useMutation({
    mutationFn: (value: z.infer<typeof schema>) => {
      let connection: MCPServer['connection'] = {
        transport: value.transport,
        url: value.url,
      }
      if (stdio) {
        connection = {
          transport: 'stdio',
          package: value.image,
          command: value.command,
        }
      } else if (oauth) {
        connection.client_id = value.client_id
        connection.scopes = ''
      }
      return mcpRequest<MCPServer>(
        server ? `/servers/${server.id}` : '/servers',
        server ? 'PATCH' : 'POST',
        {
          name: value.name,
          ...(value.icon || server?.icon ? { icon: value.icon } : {}),
          ...(value.readme || server?.readme ? { readme: value.readme } : {}),
          version: value.version,
          description: value.description,
          expected_revision: server?.revision,
          ...(props.sensitive
            ? {
                ...(stdio && env !== undefined ? { env } : {}),
                ...(!stdio && !library && headers !== undefined
                  ? { headers }
                  : {}),
                connection,
                ...(oauth && value.client_secret
                  ? { client_secret: value.client_secret }
                  : {}),
                limits: {
                  timeout_seconds: value.timeout,
                  max_concurrency: value.concurrency,
                },
              }
            : {}),
        }
      )
    },
    onSuccess: (saved) => {
      void client.invalidateQueries({ queryKey: ['mcp'] })
      props.onClose()
      if (!server) props.onCreated?.(saved.id)
    },
    onError: (error) => handleServerError(error),
  })
  const fields: {
    name: keyof z.infer<typeof schema>
    label: string
    disabled?: boolean
  }[] = [
    { name: 'name', label: t('Display name') },
    { name: 'description', label: t('Description') },
    { name: 'version', label: t('Version') },
  ]
  if (stdio) {
    fields.push(
      {
        name: 'image',
        label: t('Versioned package'),
        disabled: !props.sensitive,
      },
      { name: 'command', label: t('Command'), disabled: !props.sensitive }
    )
  } else {
    fields.push({
      name: 'url',
      label: t('Upstream URL'),
      disabled: !props.sensitive || library,
    })
  }
  return (
    <Dialog
      open
      onOpenChange={(open) => {
        if (!open) props.onClose()
      }}
      title={server ? t('Edit configuration') : t('Create MCP server')}
      contentClassName='sm:max-w-xl'
      footer={
        <Button type='submit' form={formID} disabled={save.isPending}>
          {t('Save')}
        </Button>
      }
    >
      <Form {...form}>
        <form
          id={formID}
          className='space-y-4'
          onSubmit={form.handleSubmit(
            (value) => save.mutate(value),
            () => handleServerError(new Error(t('Invalid value')))
          )}
        >
          {fields.map(({ name, label, disabled }) => (
            <FormField
              key={name}
              name={name}
              control={form.control}
              render={({ field }) => (
                <FormItem>
                  <FormLabel>{label}</FormLabel>
                  <FormControl>
                    {name === 'description' ? (
                      <Textarea {...field} rows={3} />
                    ) : (
                      <Input {...field} disabled={disabled} />
                    )}
                  </FormControl>
                  <FormMessage />
                </FormItem>
              )}
            />
          ))}
          <div className='grid gap-4 sm:grid-cols-2'>
            <FormField
              control={form.control}
              name='transport'
              render={({ field }) => (
                <FormItem>
                  <FormLabel>{t('Transport')}</FormLabel>
                  <Select
                    value={field.value}
                    disabled={
                      !props.sensitive || server?.kind === 'public_knowledge'
                    }
                    onValueChange={field.onChange}
                  >
                    <FormControl>
                      <SelectTrigger>
                        <SelectValue>{transportLabels[transport]}</SelectValue>
                      </SelectTrigger>
                    </FormControl>
                    <SelectContent>
                      <SelectItem value='streamable_http'>
                        Streamable HTTP
                      </SelectItem>
                      <SelectItem value='http_sse'>SSE</SelectItem>
                      <SelectItem value='stdio'>stdio</SelectItem>
                    </SelectContent>
                  </Select>
                  <FormMessage />
                </FormItem>
              )}
            />
          </div>
          {stdio && (
            <div className='space-y-2'>
              <Button
                type='button'
                variant='outline'
                disabled={preview.isPending}
                onClick={() => preview.mutate()}
              >
                {t('Preview command')}
              </Button>
              {preview.data && (
                <pre className='whitespace-pre-wrap'>
                  {JSON.stringify(preview.data, null, 2)}
                </pre>
              )}
            </div>
          )}
          {props.sensitive && !library && (
            <SecretFields
              key={stdio ? 'env' : 'headers'}
              label={stdio ? t('Environment variables') : t('HTTP headers')}
              configured={!!server?.credentials_configured}
              onChange={stdio ? setEnv : setHeaders}
            />
          )}
          <div className='space-y-2'>
            <Label htmlFor='mcp-icon'>{t('Icon')}</Label>
            {!icon.startsWith('data:') ? (
              <LobeIconField
                id='mcp-icon'
                value={icon}
                onChange={(v) => form.setValue('icon', v)}
              />
            ) : (
              <div className='flex items-center gap-2'>
                <img alt='' src={icon} className='size-10 object-contain' />
                <Button
                  type='button'
                  variant='outline'
                  onClick={() => form.setValue('icon', '')}
                >
                  {t('Clear')}
                </Button>
              </div>
            )}
            <Input
              aria-label={t('Upload PNG icon')}
              type='file'
              accept='image/png,.png'
              onChange={async (e) => {
                const file = e.target.files?.[0]
                if (!file) return
                try {
                  if (
                    file.size > 131072 ||
                    !file.name.toLowerCase().endsWith('.png')
                  ) {
                    throw new Error(t('Invalid value'))
                  }
                  form.setValue('icon', await encodePluginIconFile(file))
                  form.clearErrors('icon')
                } catch {
                  form.setError('icon', { message: t('Invalid value') })
                }
              }}
            />
            {form.formState.errors.icon && (
              <p role='alert'>{t('Invalid value')}</p>
            )}
          </div>
          <FormField
            control={form.control}
            name='readme'
            render={({ field }) => (
              <FormItem>
                <FormLabel>README</FormLabel>
                <FormControl>
                  <Textarea {...field} rows={6} />
                </FormControl>
                <FormMessage />
              </FormItem>
            )}
          />
          <details
            onToggle={(event) => setAdvanced(event.currentTarget.open)}
            className='rounded-lg border px-4 py-3'
          >
            <summary className='cursor-pointer text-sm font-medium'>
              {t('Advanced settings')}
            </summary>
            {(!server
              ? advanced && !stdio && !library
              : advanced && !stdio && !library) && (
              <div className='space-y-4 rounded-lg border p-4'>
                {(
                  [
                    ['client_id', t('Client ID')],
                    ['client_secret', t('Client Secret')],
                  ] as const
                ).map(([name, label]) => (
                  <FormField
                    key={name}
                    name={name}
                    control={form.control}
                    render={({ field }) => (
                      <FormItem>
                        <FormLabel>{label}</FormLabel>
                        <FormControl>
                          <Input
                            {...field}
                            disabled={!props.sensitive}
                            type={
                              name === 'client_secret' ? 'password' : 'text'
                            }
                            autoComplete='off'
                          />
                        </FormControl>
                        <FormMessage />
                      </FormItem>
                    )}
                  />
                ))}
                <p className='text-muted-foreground text-sm'>
                  {t('Redirect URI')}
                </p>
                <code className='text-xs break-all'>
                  {publicConfig.data?.callback_url ??
                    server?.oauth_callback_url}
                </code>
                {publicConfig.data?.callback_url && (
                  <CopyButton value={publicConfig.data.callback_url} />
                )}
              </div>
            )}
            <div className='mt-4 grid gap-4 sm:grid-cols-2'>
              {(
                [
                  ['timeout', t('Timeout (seconds)')],
                  ['concurrency', t('Max concurrency')],
                ] as const
              ).map(([name, label]) => (
                <FormField
                  key={name}
                  control={form.control}
                  name={name}
                  render={({ field }) => (
                    <FormItem>
                      <FormLabel>{label}</FormLabel>
                      <FormControl>
                        <Input
                          {...field}
                          type='number'
                          disabled={!props.sensitive}
                          onChange={(e) =>
                            field.onChange(
                              e.target.value === '' && name === 'timeout'
                                ? 60
                                : e.target.valueAsNumber
                            )
                          }
                        />
                      </FormControl>
                      <FormMessage />
                    </FormItem>
                  )}
                />
              ))}
            </div>
          </details>
        </form>
      </Form>
    </Dialog>
  )
}
