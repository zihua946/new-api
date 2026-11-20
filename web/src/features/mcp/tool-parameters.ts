type Parameter = {
  type?: unknown
  description?: string
  default?: unknown
  enum?: unknown[]
  minimum?: number
  maximum?: number
}

export function toolParameters(schema?: Record<string, unknown>) {
  if (
    !schema?.properties ||
    typeof schema.properties !== 'object' ||
    Array.isArray(schema.properties)
  ) {
    return null
  }
  const entries = Object.entries(schema.properties)
  if (
    (!entries.length && schema.additionalProperties !== false) ||
    schema.oneOf ||
    schema.anyOf ||
    schema.allOf ||
    schema.patternProperties
  ) {
    return null
  }
  return entries.map(([name, property]) => {
    const spec: Parameter =
      property && typeof property === 'object' ? property : {}
    const types = Array.isArray(spec.type)
      ? spec.type.filter((type) => type !== 'null')
      : []
    return {
      name,
      spec: types.length === 1 ? { ...spec, type: types[0] } : spec,
      required:
        Array.isArray(schema.required) && schema.required.includes(name),
    }
  })
}

export type Fields = NonNullable<ReturnType<typeof toolParameters>>

export function readArguments(fields: Fields, form: FormData) {
  const args: Record<string, unknown> = Object.create(null)
  for (const { name, spec } of fields) {
    const raw = form.get(`arg:${name}`)
    if (typeof raw !== 'string' || raw === '') continue
    args[name] = spec.type === 'string' && !spec.enum ? raw : JSON.parse(raw)
  }
  return JSON.stringify(args)
}
