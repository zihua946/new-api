import { createFileRoute } from '@tanstack/react-router'
import { z } from 'zod'

import { ServicePage } from '@/features/mcp/service-page'

export const Route = createFileRoute('/_authenticated/mcp/servers/$id')({
  validateSearch: z.object({
    tab: z.enum(['readme', 'tools']).catch('readme'),
    manage: z.boolean().catch(false),
  }),
  component: DetailRoute,
})

function DetailRoute() {
  const { id } = Route.useParams()
  const { tab, manage } = Route.useSearch()
  const navigate = Route.useNavigate()
  return (
    <ServicePage
      id={id}
      tab={tab}
      manage={manage}
      onTabChange={(value) =>
        void navigate({ search: { tab: value, manage }, replace: true })
      }
    />
  )
}
