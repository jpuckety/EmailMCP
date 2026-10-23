package server

import (
	"context"
	"embed"
	"fmt"

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
		"Before replying, read email://guides/replying. " +
		"Before forwarding, read email://guides/forwarding. " +
		"For attachments or cid: images, read email://guides/attachments. " +
		"When the user should review before send, read email://guides/drafts."
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
