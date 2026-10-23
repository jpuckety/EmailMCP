package server

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Independent of skillCatalog so a catalog drift cannot hide a missing guide.
var wantSkillURIs = []string{
	"email://guides/replying",
	"email://guides/forwarding",
	"email://guides/attachments",
	"email://guides/drafts",
}

func TestSkillResources_ListAndRead(t *testing.T) {
	cs := newSkillResourceClient(t)
	ctx := context.Background()

	listed, err := cs.ListResources(ctx, nil)
	if err != nil {
		t.Fatalf("ListResources: %v", err)
	}
	if len(listed.Resources) != len(wantSkillURIs) {
		t.Fatalf("listed %d resources, want %d", len(listed.Resources), len(wantSkillURIs))
	}

	byURI := make(map[string]*mcp.Resource, len(listed.Resources))
	for _, r := range listed.Resources {
		if _, dup := byURI[r.URI]; dup {
			t.Fatalf("duplicate listed URI %q", r.URI)
		}
		byURI[r.URI] = r
	}

	wantFlags := map[string][]string{
		"email://guides/replying":    {"reply_to_uid", "quote_original"},
		"email://guides/forwarding":  {"forward_uid", "include_original_attachments"},
		"email://guides/attachments": {"get_attachment"},
		"email://guides/drafts":      {"create_draft"},
	}

	for _, uri := range wantSkillURIs {
		r, ok := byURI[uri]
		if !ok {
			t.Fatalf("resources/list missing %s", uri)
		}
		if r.Name == "" {
			t.Errorf("%s name is empty", uri)
		}
		if r.Title == "" {
			t.Errorf("%s title is empty", uri)
		}
		if r.Description == "" {
			t.Errorf("%s description is empty", uri)
		}
		if r.MIMEType != skillMIMEType {
			t.Errorf("%s MIMEType = %q, want %q", uri, r.MIMEType, skillMIMEType)
		}
		if r.Annotations == nil || len(r.Annotations.Audience) != 1 || r.Annotations.Audience[0] != "assistant" {
			t.Errorf("%s audience = %#v, want [assistant]", uri, r.Annotations)
		}

		got, err := cs.ReadResource(ctx, &mcp.ReadResourceParams{URI: uri})
		if err != nil {
			t.Fatalf("ReadResource %s: %v", uri, err)
		}
		if len(got.Contents) != 1 {
			t.Fatalf("ReadResource %s: got %d contents, want 1", uri, len(got.Contents))
		}
		c := got.Contents[0]
		if c.URI != uri {
			t.Errorf("ReadResource %s URI = %q", uri, c.URI)
		}
		if c.MIMEType != skillMIMEType {
			t.Errorf("ReadResource %s MIMEType = %q, want %q", uri, c.MIMEType, skillMIMEType)
		}
		if strings.TrimSpace(c.Text) == "" {
			t.Errorf("ReadResource %s: empty markdown", uri)
		}
		for _, flag := range wantFlags[uri] {
			if !strings.Contains(c.Text, flag) {
				t.Errorf("ReadResource %s: missing %q", uri, flag)
			}
		}
	}

	got, err := cs.ReadResource(ctx, &mcp.ReadResourceParams{URI: "email://guides/replying"})
	if err != nil {
		t.Fatalf("ReadResource replying: %v", err)
	}
	body := got.Contents[0].Text
	if strings.Contains(body, "include_original_attachments") {
		t.Error("replying guide must not mention include_original_attachments")
	}
}

func TestListResourcesTool_MatchesNativeCatalog(t *testing.T) {
	cs := newSkillResourceClient(t)
	ctx := context.Background()

	listedTools, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	found := false
	for _, tool := range listedTools.Tools {
		if tool.Name == "list_resources" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("tools/list missing list_resources")
	}

	listed, err := cs.ListResources(ctx, nil)
	if err != nil {
		t.Fatalf("ListResources: %v", err)
	}

	got, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "list_resources"})
	if err != nil {
		t.Fatalf("list_resources: %v", err)
	}

	raw, err := json.Marshal(got.StructuredContent)
	if err != nil {
		t.Fatalf("marshal structured content: %v", err)
	}
	type listedSkill struct {
		URI         string `json:"uri"`
		Name        string `json:"name"`
		Title       string `json:"title"`
		Description string `json:"description"`
		MIMEType    string `json:"mime_type"`
	}
	var out struct {
		Resources []listedSkill `json:"resources"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal structured content: %v", err)
	}
	if len(out.Resources) != len(wantSkillURIs) {
		t.Fatalf("list_resources returned %d resources, want %d", len(out.Resources), len(wantSkillURIs))
	}

	nativeByURI := make(map[string]*mcp.Resource, len(listed.Resources))
	for _, r := range listed.Resources {
		nativeByURI[r.URI] = r
	}
	toolByURI := make(map[string]listedSkill, len(out.Resources))
	for _, r := range out.Resources {
		if _, dup := toolByURI[r.URI]; dup {
			t.Fatalf("list_resources duplicate URI %q", r.URI)
		}
		toolByURI[r.URI] = r
	}

	for _, uri := range wantSkillURIs {
		native, ok := nativeByURI[uri]
		if !ok {
			t.Fatalf("resources/list missing %s", uri)
		}
		tool, ok := toolByURI[uri]
		if !ok {
			t.Fatalf("list_resources missing %s", uri)
		}
		if tool.Name != native.Name {
			t.Errorf("%s name: tool %q, resource %q", uri, tool.Name, native.Name)
		}
		if tool.Title != native.Title {
			t.Errorf("%s title: tool %q, resource %q", uri, tool.Title, native.Title)
		}
		if tool.Description != native.Description {
			t.Errorf("%s description: tool %q, resource %q", uri, tool.Description, native.Description)
		}
		if tool.MIMEType != native.MIMEType {
			t.Errorf("%s mime_type: tool %q, resource %q", uri, tool.MIMEType, native.MIMEType)
		}
	}
}

func TestReadResourceTool_MatchesNativeBodies(t *testing.T) {
	cs := newSkillResourceClient(t)
	ctx := context.Background()

	listedTools, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	var readTool *mcp.Tool
	for _, tool := range listedTools.Tools {
		if tool.Name == "read_resource" {
			readTool = tool
			break
		}
	}
	if readTool == nil {
		t.Fatal("tools/list missing read_resource")
	}
	schemaRaw, err := json.Marshal(readTool.InputSchema)
	if err != nil {
		t.Fatalf("marshal InputSchema: %v", err)
	}
	var schema struct {
		Required   []string       `json:"required"`
		Properties map[string]any `json:"properties"`
	}
	if err := json.Unmarshal(schemaRaw, &schema); err != nil {
		t.Fatalf("unmarshal InputSchema: %v", err)
	}
	if _, ok := schema.Properties["uri"]; !ok {
		t.Fatalf("read_resource InputSchema missing uri property: %s", schemaRaw)
	}
	foundURI := false
	for _, name := range schema.Required {
		if name == "uri" {
			foundURI = true
			break
		}
	}
	if !foundURI {
		t.Fatalf("read_resource uri is not required: %s", schemaRaw)
	}

	wantFlags := map[string][]string{
		"email://guides/replying":    {"reply_to_uid", "quote_original"},
		"email://guides/forwarding":  {"forward_uid", "include_original_attachments"},
		"email://guides/attachments": {"get_attachment"},
		"email://guides/drafts":      {"create_draft"},
	}

	for _, uri := range wantSkillURIs {
		native, err := cs.ReadResource(ctx, &mcp.ReadResourceParams{URI: uri})
		if err != nil {
			t.Fatalf("ReadResource %s: %v", uri, err)
		}
		if len(native.Contents) != 1 {
			t.Fatalf("ReadResource %s: got %d contents, want 1", uri, len(native.Contents))
		}

		got, err := cs.CallTool(ctx, &mcp.CallToolParams{
			Name:      "read_resource",
			Arguments: map[string]any{"uri": uri},
		})
		if err != nil {
			t.Fatalf("read_resource %s: protocol error: %v", uri, err)
		}
		if got.IsError {
			t.Fatalf("read_resource %s: IsError=true, content=%q", uri, toolText(t, got))
		}

		text := toolText(t, got)
		if text != native.Contents[0].Text {
			t.Errorf("read_resource %s text does not match resources/read", uri)
		}
		for _, flag := range wantFlags[uri] {
			if !strings.Contains(text, flag) {
				t.Errorf("read_resource %s: missing %q", uri, flag)
			}
		}

		raw, err := json.Marshal(got.StructuredContent)
		if err != nil {
			t.Fatalf("marshal structured content %s: %v", uri, err)
		}
		var out struct {
			URI      string `json:"uri"`
			MIMEType string `json:"mime_type"`
			Text     string `json:"text"`
		}
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("unmarshal structured content %s: %v", uri, err)
		}
		if out.URI != uri {
			t.Errorf("read_resource %s structured uri = %q", uri, out.URI)
		}
		if out.MIMEType != native.Contents[0].MIMEType {
			t.Errorf("read_resource %s mime_type: tool %q, resource %q", uri, out.MIMEType, native.Contents[0].MIMEType)
		}
		if out.Text != native.Contents[0].Text {
			t.Errorf("read_resource %s structured text does not match resources/read", uri)
		}
	}
}

func TestReadResourceTool_UnknownURI(t *testing.T) {
	cs := newSkillResourceClient(t)
	ctx := context.Background()

	requireToolErrorListsSkillURIs := func(t *testing.T, got *mcp.CallToolResult, err error, label string) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: protocol error %v (MCP tool errors should be IsError results)", label, err)
		}
		if got == nil {
			t.Fatalf("%s: nil result", label)
		}
		if !got.IsError {
			t.Fatalf("%s: IsError=false, want true", label)
		}
		msg := toolText(t, got)
		for _, uri := range wantSkillURIs {
			if !strings.Contains(msg, uri) {
				t.Errorf("%s: error %q missing valid URI %q", label, msg, uri)
			}
		}
	}

	got, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "read_resource",
		Arguments: map[string]any{"uri": "email://guides/nope"},
	})
	requireToolErrorListsSkillURIs(t, got, err, "unknown URI")

	got, err = cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "read_resource",
		Arguments: map[string]any{"uri": ""},
	})
	requireToolErrorListsSkillURIs(t, got, err, "empty URI")

	got, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "read_resource"})
	if err != nil {
		t.Fatalf("missing uri: protocol error %v (want IsError result)", err)
	}
	if got == nil || !got.IsError {
		t.Fatalf("missing uri: IsError=%v, want true", got != nil && got.IsError)
	}
	if msg := toolText(t, got); !strings.Contains(msg, "uri") {
		t.Errorf("missing uri: error %q does not mention required field uri", msg)
	}
}

func TestSkillResources_UnknownURI(t *testing.T) {
	const unknown = "email://guides/nope"

	s := &Server{}
	_, err := s.readSkillResource(context.Background(), &mcp.ReadResourceRequest{
		Params: &mcp.ReadResourceParams{URI: unknown},
	})
	requireResourceNotFound(t, err)

	cs := newSkillResourceClient(t)
	_, err = cs.ReadResource(context.Background(), &mcp.ReadResourceParams{URI: unknown})
	requireResourceNotFound(t, err)
}

func TestSkillInstructionsAndToolPointers(t *testing.T) {
	instr := skillInstructions()
	requireContainsAll(t, instr, wantSkillURIs)
	requireContainsAll(t, instr, []string{"list_resources", "read_resource"})

	cs := newSkillResourceClient(t)
	init := cs.InitializeResult()
	if init == nil {
		t.Fatal("nil InitializeResult")
	}
	if init.Instructions != instr {
		t.Errorf("initialize instructions = %q, want %q", init.Instructions, instr)
	}
	requireContainsAll(t, init.Instructions, wantSkillURIs)
	requireContainsAll(t, init.Instructions, []string{"list_resources", "read_resource"})

	listed, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	byName := make(map[string]*mcp.Tool, len(listed.Tools))
	for _, tool := range listed.Tools {
		byName[tool.Name] = tool
	}

	wantToolURIs := map[string][]string{
		"send_email":     wantSkillURIs,
		"create_draft":   wantSkillURIs,
		"read_email":     {"email://guides/attachments"},
		"get_attachment": {"email://guides/attachments"},
	}
	for name, uris := range wantToolURIs {
		tool, ok := byName[name]
		if !ok {
			t.Fatalf("tools/list missing %s", name)
		}
		requireContainsAll(t, tool.Description, uris)
		requireContainsAll(t, tool.Description, []string{"read_resource"})
	}

	for _, name := range []string{"send_email", "create_draft"} {
		tool, ok := byName[name]
		if !ok {
			t.Fatalf("tools/list missing %s", name)
		}
		requireReplyDoesNotReattachOriginals(t, name, tool.Description)
	}
}

func newSkillResourceClient(t *testing.T) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	mcpServer := mcp.NewServer(&mcp.Implementation{Name: "emailmcp", Version: "0.2.0"},
		&mcp.ServerOptions{Instructions: skillInstructions()})
	s := &Server{mcpServer: mcpServer}
	s.registerTools()
	s.registerResources()

	ct, st := mcp.NewInMemoryTransports()
	ss, err := mcpServer.Connect(ctx, st, nil)
	if err != nil {
		t.Fatalf("server Connect: %v", err)
	}
	t.Cleanup(func() { _ = ss.Close() })

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0.0.1"}, nil)
	cs, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatalf("client Connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func requireContainsAll(t *testing.T, got string, want []string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(got, w) {
			t.Errorf("missing %q in %q", w, got)
		}
	}
}

// requireReplyDoesNotReattachOriginals fails if a compose-tool description attributes
// original-file re-attachment to reply_to_uid (that behavior is forward-only).
func requireReplyDoesNotReattachOriginals(t *testing.T, name, desc string) {
	t.Helper()
	if strings.Contains(desc, "reply_to_uid/forward_uid") || strings.Contains(desc, "forward_uid/reply_to_uid") {
		t.Errorf("%s description joins reply_to_uid with forward_uid, so later claims apply to reply: %q", name, desc)
	}
	for _, clause := range strings.FieldsFunc(desc, func(r rune) bool {
		return r == '.' || r == ';'
	}) {
		clause = strings.TrimSpace(clause)
		if clause == "" {
			continue
		}
		hasReply := strings.Contains(clause, "reply_to_uid")
		hasForward := strings.Contains(clause, "forward_uid")
		reattach := strings.Contains(strings.ToLower(clause), "re-attach") ||
			strings.Contains(strings.ToLower(clause), "reattach") ||
			strings.Contains(clause, "include_original_attachments") ||
			strings.Contains(strings.ToLower(clause), "original files")
		if hasReply && reattach {
			t.Errorf("%s description attributes original-attachment re-use to reply_to_uid in %q", name, clause)
		}
		if reattach && !hasForward {
			t.Errorf("%s description mentions original-attachment re-use without forward_uid in %q", name, clause)
		}
	}
}

func requireResourceNotFound(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected resource-not-found error")
	}
	var rpcErr *jsonrpc.Error
	if !errors.As(err, &rpcErr) {
		t.Fatalf("got %T %v, want jsonrpc.Error", err, err)
	}
	if rpcErr.Code != mcp.CodeResourceNotFound {
		t.Fatalf("error code %d (%s), want %d", rpcErr.Code, rpcErr.Message, mcp.CodeResourceNotFound)
	}
}
