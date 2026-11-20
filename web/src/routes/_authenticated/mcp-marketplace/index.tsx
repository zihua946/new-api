import { createFileRoute } from '@tanstack/react-router'

import { MCPMarketplace } from '@/features/mcp/marketplace'

export const Route = createFileRoute('/_authenticated/mcp-marketplace/')({
  component: MCPMarketplace,
})
