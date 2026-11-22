package authz

const ResourceMCP = "mcp"

var (
	MCPRead           = Permission{Resource: ResourceMCP, Action: ActionRead}
	MCPWrite          = Permission{Resource: ResourceMCP, Action: ActionWrite}
	MCPOperate        = Permission{Resource: ResourceMCP, Action: ActionOperate}
	MCPSensitiveWrite = Permission{Resource: ResourceMCP, Action: ActionSensitiveWrite}
)

func init() {
	RegisterResource(ResourceDefinition{
		Resource: ResourceMCP, LabelKey: "MCP Services",
		Actions: []ActionDefinition{
			{Action: ActionRead, LabelKey: "View MCP services and usage", DefaultRoles: []string{BuiltInRoleAdmin}},
			{Action: ActionWrite, LabelKey: "Edit MCP service descriptions and prices", DefaultRoles: []string{BuiltInRoleAdmin}},
			{Action: ActionOperate, LabelKey: "Operate and test MCP services", DefaultRoles: []string{BuiltInRoleAdmin}},
			{Action: ActionSensitiveWrite, LabelKey: "Edit MCP connections, credentials and prices"},
		},
	})
}
