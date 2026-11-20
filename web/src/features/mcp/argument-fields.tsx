import { useTranslation } from 'react-i18next'

import { Input } from '@/components/ui/input'
import { NativeSelect, NativeSelectOption } from '@/components/ui/native-select'
import { Textarea } from '@/components/ui/textarea'

import type { Fields } from './tool-parameters'

export function ArgumentFields({
  fields,
  disabled,
}: {
  fields: Fields
  disabled: boolean
}) {
  const { t } = useTranslation()
  return (
    <fieldset disabled={disabled} className='grid gap-4'>
      {fields.map(({ name, spec, required }) => {
        const options =
          spec.enum ?? (spec.type === 'boolean' ? [true, false] : undefined)
        const numeric = spec.type === 'integer' || spec.type === 'number'
        const initial =
          spec.default === undefined ? '' : JSON.stringify(spec.default)
        const props = { name: `arg:${name}`, required, 'aria-label': name }
        let control
        if (options) {
          control = (
            <NativeSelect {...props} defaultValue={initial}>
              <NativeSelectOption value=''>—</NativeSelectOption>
              {options.map((option) => (
                <NativeSelectOption
                  key={JSON.stringify(option)}
                  value={JSON.stringify(option)}
                >
                  {typeof option === 'string' ? option : JSON.stringify(option)}
                </NativeSelectOption>
              ))}
            </NativeSelect>
          )
        } else if (numeric) {
          control = (
            <Input
              {...props}
              type='number'
              step={spec.type === 'integer' ? 1 : 'any'}
              min={spec.minimum}
              max={spec.maximum}
              defaultValue={initial}
            />
          )
        } else if (spec.type === 'string') {
          control = (
            <Input
              {...props}
              defaultValue={
                typeof spec.default === 'string' ? spec.default : ''
              }
            />
          )
        } else {
          control = (
            <Textarea {...props} defaultValue={initial} placeholder='JSON' />
          )
        }
        return (
          <label key={name} className='grid gap-1.5'>
            <span className='flex flex-wrap items-baseline gap-2 text-sm'>
              <strong>{name}</strong>
              <span className='text-muted-foreground'>
                {typeof spec.type === 'string' ? spec.type : 'JSON'} ·{' '}
                {required ? t('Required') : t('Optional')}
              </span>
            </span>
            {typeof spec.description === 'string' && (
              <span className='text-muted-foreground text-sm'>
                {spec.description}
              </span>
            )}
            {control}
          </label>
        )
      })}
    </fieldset>
  )
}
