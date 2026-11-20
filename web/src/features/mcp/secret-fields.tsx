import { useState } from 'react'
import { useTranslation } from 'react-i18next'

import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Textarea } from '@/components/ui/textarea'

type Entry = { id: string; name: string; value: string }
export function SecretFields(props: {
  label: string
  configured: boolean
  onChange: (entries: Record<string, string> | undefined) => void
}) {
  const { t } = useTranslation()
  const [entries, setEntries] = useState<Entry[]>([])
  const [editing, setEditing] = useState(false)
  const [bulk, setBulk] = useState('')
  const [error, setError] = useState(false)
  function update(rows: Entry[]) {
    setEntries(rows)
    props.onChange(
      Object.fromEntries(
        rows.filter((item) => item.name).map((item) => [item.name, item.value])
      )
    )
  }
  function paste() {
    try {
      const value: unknown = JSON.parse(bulk)
      if (!value || typeof value !== 'object' || Array.isArray(value)) {
        throw new Error()
      }
      const rows = Object.entries(value).map(([name, item]) => {
        if (!name || typeof item !== 'string') throw new Error()
        return { id: crypto.randomUUID(), name, value: item }
      })
      update(rows)
      setBulk('')
      setError(false)
    } catch {
      setError(true)
    }
  }
  return (
    <fieldset className='space-y-2'>
      <legend>{props.label}</legend>
      {!editing ? (
        <Button
          type='button'
          variant='outline'
          onClick={() => {
            setEditing(true)
            update([])
          }}
        >
          {props.configured ? t('Replace') : t('Add')}
        </Button>
      ) : (
        <>
          {entries.map((row) => (
            <div key={row.id} className='flex gap-2'>
              <Input
                aria-label={t('Name')}
                value={row.name}
                onChange={(e) =>
                  update(
                    entries.map((v) =>
                      v.id === row.id ? { ...v, name: e.target.value } : v
                    )
                  )
                }
              />
              <Input
                type='password'
                autoComplete='off'
                aria-label={t('Value')}
                value={row.value}
                onChange={(e) =>
                  update(
                    entries.map((v) =>
                      v.id === row.id ? { ...v, value: e.target.value } : v
                    )
                  )
                }
              />
              <Button
                type='button'
                variant='outline'
                onClick={() => update(entries.filter((v) => v.id !== row.id))}
              >
                {t('Remove')}
              </Button>
            </div>
          ))}
          <Button
            type='button'
            variant='outline'
            onClick={() =>
              update([
                ...entries,
                { id: crypto.randomUUID(), name: '', value: '' },
              ])
            }
          >
            {t('Add')}
          </Button>
          <Textarea
            aria-label={t('Paste JSON')}
            placeholder='{"key":"value"}'
            autoComplete='off'
            value={bulk}
            onChange={(e) => setBulk(e.target.value)}
          />
          {error && <p role='alert'>{t('Invalid value')}</p>}
          <div className='flex gap-2'>
            <Button
              type='button'
              variant='outline'
              disabled={!bulk}
              onClick={paste}
            >
              {t('Apply')}
            </Button>
            <Button
              type='button'
              variant='outline'
              onClick={() => {
                setEditing(false)
                setEntries([])
                setBulk('')
                props.onChange(undefined)
              }}
            >
              {t('Cancel')}
            </Button>
          </div>
        </>
      )}
    </fieldset>
  )
}
