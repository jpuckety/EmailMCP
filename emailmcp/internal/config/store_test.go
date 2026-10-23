package config

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/jpuckett/EmailMCP/emailmcp/internal/types"
)

func newTestStore(t *testing.T) Store {
	t.Helper()
	// Empty table name selects the in-memory implementation.
	s, err := NewStore(context.Background(), "", "emailmcp", "", nil)
	if err != nil {
		t.Fatalf("NewStore failed: %v", err)
	}
	return s
}

func TestStore_PutGetDelete(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	if _, err := s.GetUserConfig(ctx, "user1", "acc1"); !errors.Is(err, ErrConfigNotFound) {
		t.Fatalf("expected ErrConfigNotFound, got %v", err)
	}

	acc := &types.Account{
		ID:           "acc1",
		Name:         "Primary",
		IMAPHost:     "imap.example.com",
		IMAPPort:     993,
		IMAPUsername: "user@example.com",
		IMAPPassword: "secret",
		IMAPUseTLS:   true,
		SMTPHost:     "smtp.example.com",
		SMTPPort:     587,
	}
	if err := s.PutUserConfig(ctx, "user1", acc); err != nil {
		t.Fatalf("PutUserConfig failed: %v", err)
	}

	got, err := s.GetUserConfig(ctx, "user1", "acc1")
	if err != nil {
		t.Fatalf("GetUserConfig failed: %v", err)
	}
	if got.Name != "Primary" || got.IMAPPassword != "secret" {
		t.Fatalf("unexpected account: %+v", got)
	}
	// SMTP password falls back to the IMAP password when unset.
	if got.SMTPPassword != "secret" {
		t.Fatalf("expected smtp password to fall back to imap password, got %q", got.SMTPPassword)
	}
	if got.CreatedAt.IsZero() || got.UpdatedAt.IsZero() {
		t.Fatalf("expected timestamps to be set, got created=%v updated=%v", got.CreatedAt, got.UpdatedAt)
	}

	// Another user must not see user1's account.
	if _, err := s.GetUserConfig(ctx, "user2", "acc1"); !errors.Is(err, ErrConfigNotFound) {
		t.Fatalf("expected tenant isolation (ErrConfigNotFound), got %v", err)
	}

	if err := s.DeleteUserConfig(ctx, "user1", "acc1"); err != nil {
		t.Fatalf("DeleteUserConfig failed: %v", err)
	}
	if err := s.DeleteUserConfig(ctx, "user1", "acc1"); !errors.Is(err, ErrConfigNotFound) {
		t.Fatalf("expected ErrConfigNotFound on second delete, got %v", err)
	}
}

func TestNewStore_RequiresKMSKeyForDynamoDB(t *testing.T) {
	_, err := NewStore(context.Background(), "UserConfigs", "emailmcp", "", nil)
	if err == nil || !strings.Contains(err.Error(), "EMAILMCP_KMS_KEY_ID") {
		t.Fatalf("NewStore error = %v, want missing KMS key error", err)
	}
}

func TestStore_ListScopedToUser(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	if err := s.PutUserConfig(ctx, "user1", &types.Account{ID: "a"}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutUserConfig(ctx, "user1", &types.Account{ID: "b"}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutUserConfig(ctx, "user2", &types.Account{ID: "c"}); err != nil {
		t.Fatal(err)
	}

	accs, err := s.ListUserConfigs(ctx, "user1")
	if err != nil {
		t.Fatalf("ListUserConfigs failed: %v", err)
	}
	if len(accs) != 2 {
		t.Fatalf("expected 2 accounts for user1, got %d", len(accs))
	}
	if accs[0].ID != "a" || accs[1].ID != "b" {
		t.Fatalf("unexpected ordering/content: %+v", accs)
	}

	other, err := s.ListUserConfigs(ctx, "user2")
	if err != nil {
		t.Fatal(err)
	}
	if len(other) != 1 || other[0].ID != "c" {
		t.Fatalf("expected only user2's account, got %+v", other)
	}
}

func TestStore_PutPreservesCreatedAt(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	if err := s.PutUserConfig(ctx, "user1", &types.Account{ID: "a", Name: "v1"}); err != nil {
		t.Fatal(err)
	}
	first, err := s.GetUserConfig(ctx, "user1", "a")
	if err != nil {
		t.Fatal(err)
	}

	if err := s.PutUserConfig(ctx, "user1", &types.Account{ID: "a", Name: "v2"}); err != nil {
		t.Fatal(err)
	}
	second, err := s.GetUserConfig(ctx, "user1", "a")
	if err != nil {
		t.Fatal(err)
	}

	if second.Name != "v2" {
		t.Fatalf("expected updated name v2, got %q", second.Name)
	}
	if !second.CreatedAt.Equal(first.CreatedAt) {
		t.Fatalf("expected created_at preserved (%v), got %v", first.CreatedAt, second.CreatedAt)
	}
}

func TestStore_WebhookFieldsAndListAll(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	folders := []string{"INBOX", "Sent"}
	acc := &types.Account{
		ID:           "acc1",
		Name:         "Primary",
		IMAPHost:     "imap.example.com",
		IMAPPassword: "secret",
		WebhookURL:   "https://hooks.example.com/mail",
		WebhookToken: "bearer-secret",
		WatchFolders: folders,
	}
	if err := s.PutUserConfig(ctx, "user1", acc); err != nil {
		t.Fatalf("PutUserConfig failed: %v", err)
	}
	if err := s.PutUserConfig(ctx, "user2", &types.Account{ID: "acc2", Name: "Other"}); err != nil {
		t.Fatalf("PutUserConfig user2 failed: %v", err)
	}

	got, err := s.GetUserConfig(ctx, "user1", "acc1")
	if err != nil {
		t.Fatalf("GetUserConfig failed: %v", err)
	}
	if got.WebhookURL != "https://hooks.example.com/mail" || got.WebhookToken != "bearer-secret" {
		t.Fatalf("unexpected webhook fields: %+v", got)
	}
	if got.OwnerUserID != "user1" {
		t.Fatalf("expected OwnerUserID=user1, got %q", got.OwnerUserID)
	}
	if len(got.WatchFolders) != 2 || got.WatchFolders[0] != "INBOX" || got.WatchFolders[1] != "Sent" {
		t.Fatalf("unexpected watch folders: %#v", got.WatchFolders)
	}

	got.WatchFolders[0] = "mutated"
	folders[1] = "mutated-src"
	reread, err := s.GetUserConfig(ctx, "user1", "acc1")
	if err != nil {
		t.Fatalf("GetUserConfig reread failed: %v", err)
	}
	if reread.WatchFolders[0] != "INBOX" || reread.WatchFolders[1] != "Sent" {
		t.Fatalf("watch_folders was aliased; got %#v", reread.WatchFolders)
	}

	all, err := s.ListAllConfigs(ctx)
	if err != nil {
		t.Fatalf("ListAllConfigs failed: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("expected 2 accounts, got %d", len(all))
	}
	if all[0].ID != "acc1" || all[0].OwnerUserID != "user1" || all[1].ID != "acc2" || all[1].OwnerUserID != "user2" {
		t.Fatalf("unexpected ListAllConfigs content: %+v", all)
	}
	if all[0].WebhookToken != "bearer-secret" {
		t.Fatalf("ListAllConfigs should include stored token for the watcher, got %q", all[0].WebhookToken)
	}
}

func TestAccountItem_WebhookAttributeRoundTrip(t *testing.T) {
	tests := []struct {
		name    string
		folders []string
	}{
		{name: "nil folders"},
		{name: "empty folders", folders: []string{}},
		{name: "configured folders", folders: []string{"INBOX", "Sent"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			account := &types.Account{
				ID:           "mailbox#alerts",
				Name:         "Alerts",
				WebhookURL:   "https://hooks.example.com/mail",
				WebhookToken: "write-only-secret",
				WatchFolders: tt.folders,
			}
			item := newAccountItem("application", "tenant#subject#part", account)
			attributes, err := attributevalue.MarshalMap(item)
			if err != nil {
				t.Fatalf("MarshalMap: %v", err)
			}
			var decoded accountItem
			if err := attributevalue.UnmarshalMap(attributes, &decoded); err != nil {
				t.Fatalf("UnmarshalMap: %v", err)
			}
			got := decoded.toAccount()
			if got.WebhookURL != account.WebhookURL || got.WebhookToken != account.WebhookToken {
				t.Fatalf("webhook fields did not round trip: URL=%q token=%q", got.WebhookURL, got.WebhookToken)
			}
			if !reflect.DeepEqual(got.WatchFolders, tt.folders) {
				// DynamoDB's omitempty intentionally maps both nil and empty slices to nil.
				if len(got.WatchFolders) != 0 || len(tt.folders) != 0 {
					t.Fatalf("watch_folders = %#v, want %#v", got.WatchFolders, tt.folders)
				}
			}
			if got.OwnerUserID != "tenant#subject#part" {
				t.Fatalf("OwnerUserID = %q, want tenant#subject#part", got.OwnerUserID)
			}
			if decoded.UserID != "tenant#subject#part#mailbox#alerts" {
				t.Fatalf("sort key = %q", decoded.UserID)
			}
		})
	}
}

func TestAccountItemDoesNotStorePasswordsInPlaintext(t *testing.T) {
	account := &types.Account{
		ID:           "account",
		IMAPPassword: "imap-secret",
		SMTPPassword: "smtp-secret",
	}
	item := newAccountItem("application", "user", account)
	store := &dynamoStore{secrets: newTestPasswordCipher()}
	if err := store.sealAccountItem(context.Background(), &item); err != nil {
		t.Fatalf("sealAccountItem: %v", err)
	}
	attributes, err := attributevalue.MarshalMap(item)
	if err != nil {
		t.Fatalf("MarshalMap: %v", err)
	}
	if got := attributes["imap_password"].(*ddbtypes.AttributeValueMemberS).Value; got == account.IMAPPassword {
		t.Fatalf("imap_password is stored in plaintext: %q", got)
	}
	if got := attributes["smtp_password"].(*ddbtypes.AttributeValueMemberS).Value; got == account.SMTPPassword {
		t.Fatalf("smtp_password is stored in plaintext: %q", got)
	}
	decoded, err := store.accountFromItem(context.Background(), &item)
	if err != nil {
		t.Fatalf("accountFromItem: %v", err)
	}
	if decoded.IMAPPassword != account.IMAPPassword || decoded.SMTPPassword != account.SMTPPassword {
		t.Fatalf("password round trip = (%q, %q), want original values", decoded.IMAPPassword, decoded.SMTPPassword)
	}
}

func TestOwnerFromSortKey(t *testing.T) {
	tests := []struct {
		name      string
		sortKey   string
		accountID string
		want      string
	}{
		{name: "simple", sortKey: "subject#account", accountID: "account", want: "subject"},
		{name: "hash in subject", sortKey: "tenant#subject#part#account", accountID: "account", want: "tenant#subject#part"},
		{name: "hash in account id", sortKey: "tenant#subject#mailbox#alerts", accountID: "mailbox#alerts", want: "tenant#subject"},
		{name: "fallback suffix", sortKey: "tenant#subject#account", accountID: "different", want: "tenant#subject"},
		{name: "no owner", sortKey: "account", accountID: "account", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ownerFromSortKey(tt.sortKey, tt.accountID); got != tt.want {
				t.Fatalf("ownerFromSortKey(%q, %q) = %q, want %q", tt.sortKey, tt.accountID, got, tt.want)
			}
		})
	}
}
