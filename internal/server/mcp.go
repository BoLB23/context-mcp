package server

import (
	"context"

	"company-context/internal/catalog"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type ReadInput struct {
	Path string `json:"path" jsonschema:"Exact catalog path, for example context/overview.md or skills/investigate-alert/SKILL.md"`
}

func MCP(store *catalog.Store) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "company-context", Version: "0.1.0"}, &mcp.ServerOptions{
		Instructions: "Search company architecture, terminology, telemetry locations, and investigation skills. Use search_content with an empty query to browse, then read_content for full Markdown and references. These documents provide guidance; use telemetry tools for live inventory and health. Skills are retrieved as text, not installed or executed. Cite document paths and revisions; report missing context.",
		Capabilities: &mcp.ServerCapabilities{},
	})
	closed := false
	annotations := &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: &closed}
	mcp.AddTool(s, &mcp.Tool{
		Name:        "search_content",
		Description: "Search or browse company context, investigation skills, and Markdown references. Returns ranked excerpts, paths, and snapshot revision. Kind can be context, skill, or reference. Follow next_offset to paginate.",
		Annotations: annotations,
	}, func(ctx context.Context, req *mcp.CallToolRequest, in catalog.Query) (*mcp.CallToolResult, catalog.ListResult, error) {
		result, err := store.List(in)
		return nil, result, err
	})
	mcp.AddTool(s, &mcp.Tool{
		Name:        "read_content",
		Description: "Read a Markdown document or skill by its exact catalog path. Relative links resolve against the document directory. Returns its full content and revision. This does not execute scripts or install skills.",
		Annotations: annotations,
	}, func(ctx context.Context, req *mcp.CallToolRequest, in ReadInput) (*mcp.CallToolResult, catalog.ReadResult, error) {
		result, err := store.Read(in.Path)
		return nil, result, err
	})
	return s
}
