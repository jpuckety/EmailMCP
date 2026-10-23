package server

import (
	"context"
	"embed"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const skillMIMEType = "text/markdown"

//go:embed guides/*.md
var skillFS embed.FS

type skillDoc struct {
	URI, Name, Title, Description, File string
}

var skillCatalog = []skillDoc{
	{
		URI:         "email://guides/replying",
		Name:        "replying",
		Title:       "Replying",
		Description: "Threading headers, reply_to_uid / reply_all / reply_folder, and quote_original.",
		File:        "guides/replying.md",
	},
	{
		URI:         "email://guides/forwarding",
		Name:        "forwarding",
		Title:       "Forwarding",
		Description: "forward_uid, quote_original, and server-side include_original_attachments.",
		File:        "guides/forwarding.md",
	},
	{
		URI:         "email://guides/attachments",
		Name:        "attachments",
		Title:       "Attachments",
		Description: "get_attachment vs send-time data / url / path, including inline cid: images.",
		File:        "guides/attachments.md",
	},
	{
		URI:         "email://guides/drafts",
		Name:        "drafts",
		Title:       "Drafts",
		Description: "When to create_draft instead of send_email; drafts share the same compose options.",
		File:        "guides/drafts.md",
	},
}

var skillBodies = mustLoadSkillBodies()

func mustLoadSkillBodies() map[string]string {
	bodies := make(map[string]string, len(skillCatalog))
	for _, doc := range skillCatalog {
		b, err := skillFS.ReadFile(doc.File)
		if err != nil {
			panic(fmt.Sprintf("skill resource %s: %v", doc.URI, err))
		}
		bodies[doc.URI] = string(b)
	}
	return bodies
}

func skillInstructions() string {
	return "If the user has no email account, use add_email_account and have them set passwords in the portal. " +
		"Use list_resources then read_resource to load guides. " +
		"Before replying, call read_resource with uri email://guides/replying. " +
		"Before forwarding, call read_resource with uri email://guides/forwarding. " +
		"For attachments or cid: images, call read_resource with uri email://guides/attachments. " +
		"When the user should review before send, call read_resource with uri email://guides/drafts."
}

func (s *Server) registerResources() {
	for _, doc := range skillCatalog {
		s.mcpServer.AddResource(&mcp.Resource{
			URI:         doc.URI,
			Name:        doc.Name,
			Title:       doc.Title,
			Description: doc.Description,
			MIMEType:    skillMIMEType,
			Annotations: &mcp.Annotations{
				Audience: []mcp.Role{"assistant"},
			},
		}, s.readSkillResource)
	}
}

func (s *Server) readSkillResource(_ context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
	body, ok := skillBodies[req.Params.URI]
	if !ok {
		return nil, mcp.ResourceNotFoundError(req.Params.URI)
	}
	return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{
		URI:      req.Params.URI,
		MIMEType: skillMIMEType,
		Text:     body,
	}}}, nil
}

func (s *Server) listResources(_ context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
	resources := make([]map[string]any, 0, len(skillCatalog))
	for _, doc := range skillCatalog {
		resources = append(resources, map[string]any{
			"uri":         doc.URI,
			"name":        doc.Name,
			"title":       doc.Title,
			"description": doc.Description,
			"mime_type":   skillMIMEType,
		})
	}
	return nil, map[string]any{"resources": resources}, nil
}

// ReadResourceInput is the schema for the read_resource tool.
type ReadResourceInput struct {
	URI string `json:"uri" jsonschema:"Resource URI, e.g. email://guides/replying"`
}

func (s *Server) readResource(_ context.Context, _ *mcp.CallToolRequest, in ReadResourceInput) (*mcp.CallToolResult, any, error) {
	body, ok := skillBodies[in.URI]
	if !ok {
		return nil, nil, fmt.Errorf("unknown resource URI %q; valid URIs: %s", in.URI, skillCatalogURIList())
	}
	return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: body}},
		}, map[string]any{
			"uri":       in.URI,
			"mime_type": skillMIMEType,
			"text":      body,
		}, nil
}

func skillCatalogURIList() string {
	uris := make([]string, 0, len(skillCatalog))
	for _, doc := range skillCatalog {
		uris = append(uris, doc.URI)
	}
	return strings.Join(uris, ", ")
}
