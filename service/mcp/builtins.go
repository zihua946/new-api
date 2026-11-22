package mcp

import (
	"maps"
	"net"
	"net/url"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/google/jsonschema-go/jsonschema"
)

// These schemas are shared by discovery, gateway execution and admin tests.
// Additional properties are denied so caller-supplied actors or task execution
// options cannot accidentally reach the engine's wider task API.
const builtinDefinitions = `[
 {"name":"web_search","description":"Search public web pages. A successful request with results is charged once.","input_schema":{"type":"object","additionalProperties":false,"required":["query"],"properties":{"query":{"type":"string","minLength":1,"maxLength":128},"count":{"type":"integer","minimum":1,"maximum":50},"ori_question":{"type":["string","null"],"minLength":1,"maxLength":128},"localization":{"type":"string","maxLength":45},"need_content":{"type":"boolean"}}}},
 {"name":"web_fetch","description":"Read a public web page as Markdown. Each successful request is charged at the configured tool price.","input_schema":{"type":"object","additionalProperties":false,"required":["url"],"properties":{"url":{"type":"string","minLength":1,"maxLength":8192}}}},
 {"name":"list_knowledge_bases","description":"List knowledge bases that the signed-in user may consult.","input_schema":{"type":"object","additionalProperties":false,"properties":{"cursor":{"type":["string","null"],"maxLength":2048},"limit":{"type":"integer","minimum":1,"maximum":100}}}},
 {"name":"create_consultation","description":"Create a read-only knowledge consultation. Reuse request_id when retrying. Engine usage is billed separately.","input_schema":{"type":"object","additionalProperties":false,"required":["knowledge_base","question","request_id"],"properties":{"knowledge_base":{"type":"object","additionalProperties":false,"required":["kb_id","owner"],"properties":{"kb_id":{"type":"string","minLength":1,"maxLength":256},"owner":{"type":"string","minLength":1,"maxLength":256}}},"question":{"type":"string","minLength":1,"maxLength":65536},"request_id":{"type":"string","pattern":"^[A-Za-z0-9_.:-]{1,128}$"},"session_id":{"type":["string","null"],"maxLength":128}}}},
 {"name":"get_consultation_result","description":"Read incremental public results of a consultation created by this user through this server. Continue with next_cursor until done is true.","input_schema":{"type":"object","additionalProperties":false,"required":["task_id"],"properties":{"task_id":{"type":"string","pattern":"^[A-Za-z0-9_-]{1,128}$"},"cursor":{"type":["string","null"],"maxLength":2048},"wait_seconds":{"type":"integer","minimum":0,"maximum":25},"limit":{"type":"integer","minimum":1,"maximum":200}}}},
 {"name":"cancel_consultation","description":"Request cancellation of your consultation created through this server. Already incurred usage is not refunded.","input_schema":{"type":"object","additionalProperties":false,"required":["task_id"],"properties":{"task_id":{"type":"string","pattern":"^[A-Za-z0-9_-]{1,128}$"}}}}
]`

func BuiltinTools(kind string) []model.MCPTool {
	if kind != "deepseek" && kind != "public_knowledge" {
		return nil
	}
	return builtinDefinitionsFor(kind)
}

func builtinDefinitionsFor(kind string) []model.MCPTool {
	var definitions []model.MCPTool
	if err := common.UnmarshalJsonStr(builtinDefinitions, &definitions); err != nil {
		panic("invalid built-in MCP schemas")
	}
	var tools []model.MCPTool
	for _, tool := range definitions {
		if kind == "public_knowledge" {
			switch tool.Name {
			case "create_consultation":
				delete(tool.InputSchema["properties"].(map[string]any), "knowledge_base")
				delete(tool.InputSchema["properties"].(map[string]any), "request_id")
				tool.InputSchema["required"] = []string{"question"}
				tool.Description = "Ask this public library a question. Model usage is billed separately."
			case "get_consultation_result":
				props := tool.InputSchema["properties"].(map[string]any)
				delete(props, "cursor")
				delete(props, "limit")
				delete(props, "wait_seconds")
				tool.Description = "Read the current answer and status of your consultation in this library."
			default:
				continue
			}
		}
		isWeb := strings.HasPrefix(tool.Name, "web_")
		if kind == "deepseek" && !isWeb || kind == "knowledge" && isWeb || kind == "remote_mcp" {
			continue
		}
		tool.OutputSchema = model.MCPJSON{"type": "object"}
		tool.SchemaHash, _ = SchemaHash(tool.InputSchema, tool.OutputSchema)
		tool.Revision, tool.PriceRevision = 1, 1
		if kind == "knowledge" || kind == "public_knowledge" {
			tool.PriceMode, tool.UnitPriceUSD = "free", "0"
		}
		tools = append(tools, tool)
	}
	return tools
}

func SchemaHash(input, output model.MCPJSON) (string, error) {
	b, err := common.Marshal([]model.MCPJSON{input, output})
	if err != nil {
		return "", err
	}
	return Digest(string(b)), nil
}

func ValidateToolSchema(input model.MCPJSON, arguments map[string]any) error {
	b, err := common.Marshal(input)
	if err != nil || len(b) > 48<<10 {
		return Failure(422, "INVALID_TOOL_SCHEMA", "Tool schema is invalid or too large")
	}
	var schema jsonschema.Schema
	if err = common.Unmarshal(b, &schema); err != nil {
		return Failure(422, "INVALID_TOOL_SCHEMA", "Tool schema is invalid")
	}
	// A nil loader rejects external references instead of making network calls.
	resolved, err := schema.Resolve(nil)
	if err != nil {
		return Failure(422, "INVALID_TOOL_SCHEMA", "Tool schema cannot be resolved locally")
	}
	if arguments != nil {
		if err = resolved.Validate(arguments); err != nil {
			return Failure(422, "INVALID_ARGUMENTS", "Tool arguments do not match the input schema")
		}
	}
	return nil
}

func ValidateBuiltinArguments(name string, arguments map[string]any) (map[string]any, error) {
	kind := "knowledge"
	if strings.HasPrefix(name, "web_") {
		kind = "deepseek"
	}
	var found *model.MCPTool
	for _, tool := range builtinDefinitionsFor(kind) {
		if tool.Name == name {
			found = &tool
			break
		}
	}
	if found == nil {
		return nil, Failure(404, "TOOL_NOT_FOUND", "Tool is unavailable")
	}
	if arguments == nil {
		arguments = map[string]any{}
	}
	if err := ValidateToolSchema(found.InputSchema, arguments); err != nil {
		return nil, err
	}
	args := maps.Clone(arguments)
	switch name {
	case "web_search":
		if strings.TrimSpace(args["query"].(string)) == "" {
			return nil, Failure(422, "INVALID_ARGUMENTS", "Search query cannot be blank")
		}
		if ip, ok := args["localization"].(string); ok && ip != "" && net.ParseIP(ip) == nil {
			return nil, Failure(422, "INVALID_ARGUMENTS", "Localization must be an IP address")
		}
		if args["ori_question"] == nil {
			delete(args, "ori_question")
		}
		if _, exists := args["count"]; !exists {
			args["count"] = float64(10)
		}
		if _, exists := args["need_content"]; !exists {
			args["need_content"] = false
		}
	case "web_fetch":
		u, err := url.Parse(args["url"].(string))
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil {
			return nil, Failure(422, "INVALID_ARGUMENTS", "A valid HTTP or HTTPS URL without credentials is required")
		}
	case "create_consultation":
		if strings.TrimSpace(args["question"].(string)) == "" {
			return nil, Failure(422, "INVALID_ARGUMENTS", "Question cannot be blank")
		}
	case "list_knowledge_bases", "get_consultation_result":
		if _, exists := args["limit"]; !exists {
			args["limit"] = float64(100)
		}
		if name == "get_consultation_result" {
			if _, exists := args["wait_seconds"]; !exists {
				args["wait_seconds"] = float64(25)
			}
		}
	}
	return args, nil
}
