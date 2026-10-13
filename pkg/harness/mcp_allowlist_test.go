package harness

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"gopkg.in/yaml.v3"
)

func TestMCPToolAllowlistTemplateRoundTripAndStaging(t *testing.T) {
	dir := t.TempDir()
	data := []byte(`schema_version: "1"
mcp_servers:
  unrestricted:
    transport: streamable-http
    url: https://memory.example/mcp
  memory:
    transport: streamable-http
    url: https://memory.example/mcp
    enabled_tools: [search_notes, read_note]
  disabled:
    transport: streamable-http
    url: https://code.example/mcp
    enabled_tools: []
`)
	if err := os.WriteFile(filepath.Join(dir, "scion-agent.yaml"), data, 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := (&config.Template{Path: dir}).LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	// Re-serialized cached templates must preserve the distinction between
	// an omitted allowlist and an explicit empty allowlist.
	serialized, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var cached api.ScionConfig
	if err := yaml.Unmarshal(serialized, &cached); err != nil {
		t.Fatal(err)
	}
	h, _ := newTestContainerScriptHarness(t)
	home := t.TempDir()
	if err := h.ApplyMCPSettings(home, cached.MCPServers); err != nil {
		t.Fatal(err)
	}
	staged, err := os.ReadFile(filepath.Join(home, ".scion", "harness", "inputs", "mcp-servers.json"))
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Servers map[string]map[string]json.RawMessage `json:"mcp_servers"`
	}
	if err := json.Unmarshal(staged, &payload); err != nil {
		t.Fatal(err)
	}
	if _, present := payload.Servers["unrestricted"]["enabled_tools"]; present {
		t.Fatal("omitted allowlist became explicit during staging")
	}
	for name, want := range map[string][]string{
		"memory": {"search_notes", "read_note"}, "disabled": {},
	} {
		var got []string
		if err := json.Unmarshal(payload.Servers[name]["enabled_tools"], &got); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s enabled_tools=%#v, want %#v", name, got, want)
		}
	}
}
