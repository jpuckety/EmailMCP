package server

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jpuckett/EmailMCP/emailmcp/internal/config"
	"github.com/jpuckett/EmailMCP/emailmcp/internal/types"
)

func TestGetAccount_RequiredAccountID(t *testing.T) {
	s := &Server{
		logger: slog.Default(),
	}

	// Mock authenticated context
	ctx := context.WithValue(context.Background(), userContextKey, &UserInfo{Subject: "user1", Email: "user1@example.com"})

	// Call with empty accountID
	_, err := s.getAccount(ctx, "")
	if err == nil {
		t.Fatal("expected error when accountID is empty, got nil")
	}
	if err.Error() != "account_id is required" {
		t.Errorf("expected 'account_id is required' error, got %q", err.Error())
	}
}

func TestListEmailAccounts_ReturnsMap(t *testing.T) {
	// In-memory config store (no DynamoDB table configured).
	store, err := config.NewStore(context.Background(), "", "emailmcp", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("NewStore failed: %v", err)
	}
	if err := store.PutUserConfig(context.Background(), "user1", &types.Account{ID: "acc1", Name: "Account 1"}); err != nil {
		t.Fatalf("PutUserConfig failed: %v", err)
	}

	s := &Server{
		configStore: store,
		logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	ctx := context.WithValue(context.Background(), userContextKey, &UserInfo{Subject: "user1", Email: "user1@example.com"})

	_, result, err := s.listEmailAccounts(ctx, nil, struct{}{})
	if err != nil {
		t.Fatalf("listEmailAccounts failed: %v", err)
	}

	resMap, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("expected map[string]any, got %T", result)
	}

	accs, ok := resMap["accounts"]
	if !ok {
		t.Fatal("expected 'accounts' key in result map")
	}

	summaries, ok := accs.([]types.AccountSummary)
	if !ok {
		t.Fatalf("expected []types.AccountSummary, got %T", accs)
	}

	if len(summaries) != 1 || summaries[0].ID != "acc1" {
		t.Errorf("unexpected summaries: %v", summaries)
	}
}

func TestListEmailAccounts_EmptyWhenUserHasNone(t *testing.T) {
	s, _ := newAddAccountTestServer(t, "https://emailmcp.ecg.co")
	ctx := context.WithValue(context.Background(), userContextKey, &UserInfo{Subject: "user1", Email: "user1@example.com"})

	result, out, err := s.listEmailAccounts(ctx, nil, struct{}{})
	if err != nil {
		t.Fatalf("listEmailAccounts failed: %v", err)
	}

	resMap, ok := out.(map[string]any)
	if !ok {
		t.Fatalf("expected map[string]any, got %T", out)
	}
	accs, ok := resMap["accounts"].([]types.AccountSummary)
	if !ok {
		t.Fatalf("expected []types.AccountSummary, got %T", resMap["accounts"])
	}
	if accs == nil {
		t.Fatal("expected non-nil empty accounts slice, got nil")
	}
	if len(accs) != 0 {
		t.Fatalf("expected 0 accounts, got %d", len(accs))
	}

	msg, _ := resMap["message"].(string)
	if msg == "" {
		t.Fatal("expected a message when the user has no accounts")
	}
	portalURL, _ := resMap["portal_url"].(string)
	if portalURL != "https://emailmcp.ecg.co/portal" {
		t.Errorf("expected portal_url https://emailmcp.ecg.co/portal, got %q", portalURL)
	}

	body := toolText(t, result)
	if !strings.Contains(body, "No email accounts") {
		t.Fatalf("expected empty-state guidance in result, got %q", body)
	}
	if !strings.Contains(body, "https://emailmcp.ecg.co/portal") {
		t.Fatalf("expected portal URL in result, got %q", body)
	}
}

func TestListEmailAccounts_EmptyWithoutBaseURL(t *testing.T) {
	s, _ := newAddAccountTestServer(t, "")
	ctx := context.WithValue(context.Background(), userContextKey, &UserInfo{Subject: "user1", Email: "user1@example.com"})

	result, out, err := s.listEmailAccounts(ctx, nil, struct{}{})
	if err != nil {
		t.Fatalf("listEmailAccounts failed: %v", err)
	}
	resMap, ok := out.(map[string]any)
	if !ok {
		t.Fatalf("expected map[string]any, got %T", out)
	}
	if _, ok := resMap["portal_url"]; ok {
		t.Fatalf("expected no portal_url when PublicBaseURL is empty, got %v", resMap["portal_url"])
	}
	accs, ok := resMap["accounts"].([]types.AccountSummary)
	if !ok {
		t.Fatalf("expected []types.AccountSummary, got %T", resMap["accounts"])
	}
	if len(accs) != 0 {
		t.Fatalf("expected 0 accounts, got %d", len(accs))
	}
	if !strings.Contains(toolText(t, result), "No email accounts") {
		t.Fatalf("expected empty-state guidance even without a base URL, got %#v", result.Content)
	}
}

func newAddAccountTestServer(t *testing.T, publicBaseURL string) (*Server, config.Store) {
	t.Helper()
	store, err := config.NewStore(context.Background(), "", "emailmcp", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("NewStore failed: %v", err)
	}
	s := &Server{
		configStore: store,
		logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		cfg:         &config.Config{PublicBaseURL: publicBaseURL},
	}
	return s, store
}

func TestAddEmailAccount_DoesNotRequirePasswords(t *testing.T) {
	s, store := newAddAccountTestServer(t, "https://emailmcp.ecg.co")
	ctx := context.WithValue(context.Background(), userContextKey, &UserInfo{Subject: "user1", Email: "user1@example.com"})

	result, out, err := s.addEmailAccount(ctx, nil, AddAccountInput{
		Name:         "Work",
		IMAPHost:     "8.8.8.8",
		IMAPUsername: "worker@example.com",
		SMTPHost:     "1.1.1.1",
	})
	if err != nil {
		t.Fatalf("addEmailAccount failed: %v", err)
	}

	acc, err := store.GetUserConfig(ctx, "user1", "work")
	if err != nil {
		t.Fatalf("GetUserConfig failed: %v", err)
	}
	if acc.IMAPPassword != "" || acc.SMTPPassword != "" {
		t.Fatalf("expected no passwords to be stored, got imap=%q smtp=%q", acc.IMAPPassword, acc.SMTPPassword)
	}

	resMap, ok := out.(map[string]any)
	if !ok {
		t.Fatalf("expected map[string]any, got %T", out)
	}
	if resMap["id"] != "work" {
		t.Errorf("expected id=work, got %v", resMap["id"])
	}
	portalURL, _ := resMap["portal_url"].(string)
	if portalURL != "https://emailmcp.ecg.co/portal" {
		t.Errorf("expected portal_url https://emailmcp.ecg.co/portal, got %q", portalURL)
	}

	body := toolText(t, result)
	if !strings.Contains(body, "portal") {
		t.Fatalf("expected portal guidance in result, got %q", body)
	}
	if !strings.Contains(body, "https://emailmcp.ecg.co/portal") {
		t.Fatalf("expected portal URL in result, got %q", body)
	}
}

func TestAddEmailAccount_PreservesExistingPasswords(t *testing.T) {
	s, store := newAddAccountTestServer(t, "https://emailmcp.ecg.co")
	ctx := context.WithValue(context.Background(), userContextKey, &UserInfo{Subject: "user1", Email: "user1@example.com"})

	if err := store.PutUserConfig(ctx, "user1", &types.Account{
		ID:           "work",
		Name:         "Work",
		IMAPHost:     "8.8.8.8",
		IMAPUsername: "old@example.com",
		IMAPPassword: "keep-imap",
		SMTPHost:     "1.1.1.1",
		SMTPPassword: "keep-smtp",
		WebhookURL:   "https://8.8.8.8/hook",
		WebhookToken: "keep-webhook",
		WatchFolders: []string{"INBOX"},
	}); err != nil {
		t.Fatalf("PutUserConfig failed: %v", err)
	}

	_, _, err := s.addEmailAccount(ctx, nil, AddAccountInput{
		ID:           "work",
		Name:         "Work Updated",
		IMAPHost:     "8.8.8.8",
		IMAPUsername: "new@example.com",
		SMTPHost:     "1.1.1.1",
	})
	if err != nil {
		t.Fatalf("addEmailAccount failed: %v", err)
	}

	acc, err := store.GetUserConfig(ctx, "user1", "work")
	if err != nil {
		t.Fatalf("GetUserConfig failed: %v", err)
	}
	if acc.Name != "Work Updated" || acc.IMAPUsername != "new@example.com" {
		t.Fatalf("expected updated non-secret fields, got %+v", acc)
	}
	if acc.IMAPPassword != "keep-imap" || acc.SMTPPassword != "keep-smtp" {
		t.Fatalf("expected existing passwords to be preserved, got imap=%q smtp=%q", acc.IMAPPassword, acc.SMTPPassword)
	}
	if acc.WebhookURL != "https://8.8.8.8/hook" || acc.WebhookToken != "keep-webhook" {
		t.Fatalf("expected existing webhook secrets to be preserved, got url=%q token=%q", acc.WebhookURL, acc.WebhookToken)
	}
	if len(acc.WatchFolders) != 1 || acc.WatchFolders[0] != "INBOX" {
		t.Fatalf("expected existing watch folders to be preserved, got %#v", acc.WatchFolders)
	}
}

func TestAddEmailAccount_PortalMessageWithoutBaseURL(t *testing.T) {
	s, _ := newAddAccountTestServer(t, "")
	ctx := context.WithValue(context.Background(), userContextKey, &UserInfo{Subject: "user1", Email: "user1@example.com"})

	result, out, err := s.addEmailAccount(ctx, nil, AddAccountInput{
		Name:         "Home",
		IMAPHost:     "8.8.8.8",
		IMAPUsername: "home@example.com",
		SMTPHost:     "1.1.1.1",
	})
	if err != nil {
		t.Fatalf("addEmailAccount failed: %v", err)
	}
	resMap := out.(map[string]any)
	if _, ok := resMap["portal_url"]; ok {
		t.Fatalf("expected no portal_url when PublicBaseURL is empty, got %v", resMap["portal_url"])
	}
	if !strings.Contains(toolText(t, result), "portal") {
		t.Fatalf("expected portal guidance even without a base URL, got %#v", result.Content)
	}
}

func toolText(t *testing.T, result *mcp.CallToolResult) string {
	t.Helper()
	if result == nil || len(result.Content) == 0 {
		t.Fatal("expected tool result content")
	}
	tc, ok := result.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("expected *mcp.TextContent, got %T", result.Content[0])
	}
	return tc.Text
}
