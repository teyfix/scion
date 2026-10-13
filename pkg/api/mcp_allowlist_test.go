package api

import "testing"

func TestMCPToolAllowlistValidation(t *testing.T) {
	for _, tools := range [][]string{{}, {"search_notes", "read_note"}, {`quote"tool`, `slash\tool`}} {
		if err := ValidateMCPServers(map[string]MCPServerConfig{"memory": {
			Transport: MCPTransportStreamableHTTP, URL: "https://memory.example/mcp", EnabledTools: &tools,
		}}); err != nil {
			t.Errorf("valid allowlist %#v rejected: %v", tools, err)
		}
	}
	for _, tool := range []string{"", " ", "read\nnote", "read\x7fnote"} {
		tools := []string{tool}
		if err := ValidateMCPServers(map[string]MCPServerConfig{"memory": {
			Transport: MCPTransportStreamableHTTP, URL: "https://memory.example/mcp", EnabledTools: &tools,
		}}); err == nil {
			t.Errorf("invalid allowlist %#v accepted", tools)
		}
	}
}
