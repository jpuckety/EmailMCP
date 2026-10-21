package config

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/jpuckett/EmailMCP/emailmcp/internal/types"
)

// ErrConfigNotFound is returned when no config exists for the user/account.
var ErrConfigNotFound = errors.New("config not found")

// Store persists per-user email account configurations. It replaces the former
// HTTP Config API Lambda: the Go MCP server now reads and writes the
// EmailMCPUserConfigs DynamoDB table directly (encrypted at rest with the
// shared KMS key) via the ECS task role, including IMAP/SMTP credentials. Access is already
// authenticated upstream, so callers pass the authenticated user's Google
// subject as userID.
type Store interface {
	// GetUserConfig returns a single account for the user, or ErrConfigNotFound.
	GetUserConfig(ctx context.Context, userID, accountID string) (*types.Account, error)
	// ListUserConfigs returns all accounts owned by the user (may be empty).
	ListUserConfigs(ctx context.Context, userID string) ([]*types.Account, error)
	// ListAllConfigs returns every account in the application partition.
	// Used by the notify watcher to find accounts with a webhook URL.
	ListAllConfigs(ctx context.Context) ([]*types.Account, error)
	// PutUserConfig creates or replaces an account configuration.
	PutUserConfig(ctx context.Context, userID string, acc *types.Account) error
	// DeleteUserConfig removes an account, returning ErrConfigNotFound if absent.
	DeleteUserConfig(ctx context.Context, userID, accountID string) error
}

// NewStore returns a DynamoDB-backed config store when tableName is set and AWS
// configuration can be loaded; otherwise it falls back to an in-memory store
// (suitable for local/stdio use and tests) with a warning.
func NewStore(ctx context.Context, tableName, applicationID string, logger *slog.Logger) (Store, error) {
	if logger == nil {
		logger = slog.Default()
	}
	if applicationID == "" {
		applicationID = "default"
	}
	if tableName == "" {
		logger.Warn("user config table not configured; using in-memory config store " +
			"(email accounts will not survive a restart or span replicas). " +
			"Set EMAILMCP_USER_CONFIG_TABLE or the SSM parameter /emailmcp/user-config-table/name")
		return newMemoryStore(), nil
	}

	var opts []func(*awsconfig.LoadOptions) error
	if region := os.Getenv("AWS_REGION"); region != "" {
		opts = append(opts, awsconfig.WithRegion(region))
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("load aws config for config store: %w", err)
	}
	logger.Info("dynamodb user config store enabled",
		"table", tableName,
		"application_id", applicationID,
		"region", cfg.Region,
	)
	return &dynamoStore{
		client:        dynamodb.NewFromConfig(cfg),
		tableName:     tableName,
		applicationID: applicationID,
	}, nil
}

// sortKey builds the DynamoDB sort key for an account. The table is keyed by
// (applicationId, userId) where userId encodes the owner's subject and the
// account id as "<subject>#<accountId>".
func sortKey(userID, accountID string) string {
	return userID + "#" + accountID
}

// --- DynamoDB implementation ---

type dynamoStore struct {
	client        *dynamodb.Client
	tableName     string
	applicationID string
}

// accountItem is the DynamoDB representation of an email account. Credentials
// are stored as attributes; the table is encrypted at rest with the shared KMS
// key. Timestamps are stored as Unix seconds.
type accountItem struct {
	ApplicationID string   `dynamodbav:"applicationId"`
	UserID        string   `dynamodbav:"userId"`
	ID            string   `dynamodbav:"id"`
	Name          string   `dynamodbav:"name"`
	IMAPHost      string   `dynamodbav:"imap_host"`
	IMAPPort      int      `dynamodbav:"imap_port"`
	IMAPUsername  string   `dynamodbav:"imap_username"`
	IMAPPassword  string   `dynamodbav:"imap_password"`
	IMAPUseTLS    bool     `dynamodbav:"imap_use_tls"`
	SMTPHost      string   `dynamodbav:"smtp_host"`
	SMTPPort      int      `dynamodbav:"smtp_port"`
	SMTPUsername  string   `dynamodbav:"smtp_username"`
	SMTPPassword  string   `dynamodbav:"smtp_password"`
	SMTPUseTLS    bool     `dynamodbav:"smtp_use_tls"`
	FromAddress   string   `dynamodbav:"from_address"`
	SaveToSent    string   `dynamodbav:"save_to_sent,omitempty"`
	WebhookURL    string   `dynamodbav:"webhook_url,omitempty"`
	WebhookToken  string   `dynamodbav:"webhook_token,omitempty"`
	WatchFolders  []string `dynamodbav:"watch_folders,omitempty"`
	CreatedAt     int64    `dynamodbav:"created_at"`
	UpdatedAt     int64    `dynamodbav:"updated_at"`
}

func newAccountItem(applicationID, userID string, acc *types.Account) accountItem {
	smtpPass := acc.SMTPPassword
	if smtpPass == "" {
		smtpPass = acc.IMAPPassword
	}
	return accountItem{
		ApplicationID: applicationID,
		UserID:        sortKey(userID, acc.ID),
		ID:            acc.ID,
		Name:          acc.Name,
		IMAPHost:      acc.IMAPHost,
		IMAPPort:      acc.IMAPPort,
		IMAPUsername:  acc.IMAPUsername,
		IMAPPassword:  acc.IMAPPassword,
		IMAPUseTLS:    acc.IMAPUseTLS,
		SMTPHost:      acc.SMTPHost,
		SMTPPort:      acc.SMTPPort,
		SMTPUsername:  acc.SMTPUsername,
		SMTPPassword:  smtpPass,
		SMTPUseTLS:    acc.SMTPUseTLS,
		FromAddress:   acc.FromAddress,
		SaveToSent:    acc.SaveToSent,
		WebhookURL:    acc.WebhookURL,
		WebhookToken:  acc.WebhookToken,
		WatchFolders:  copyWatchFolders(acc.WatchFolders),
	}
}

func (it accountItem) toAccount() *types.Account {
	acc := &types.Account{
		ID:           it.ID,
		Name:         it.Name,
		IMAPHost:     it.IMAPHost,
		IMAPPort:     it.IMAPPort,
		IMAPUsername: it.IMAPUsername,
		IMAPPassword: it.IMAPPassword,
		IMAPUseTLS:   it.IMAPUseTLS,
		SMTPHost:     it.SMTPHost,
		SMTPPort:     it.SMTPPort,
		SMTPUsername: it.SMTPUsername,
		SMTPPassword: it.SMTPPassword,
		SMTPUseTLS:   it.SMTPUseTLS,
		FromAddress:  it.FromAddress,
		SaveToSent:   it.SaveToSent,
		WebhookURL:   it.WebhookURL,
		WebhookToken: it.WebhookToken,
		WatchFolders: copyWatchFolders(it.WatchFolders),
		OwnerUserID:  ownerFromSortKey(it.UserID, it.ID),
	}
	// Fall back SMTP → IMAP password when a distinct SMTP password is unset.
	if acc.SMTPPassword == "" {
		acc.SMTPPassword = acc.IMAPPassword
	}
	if it.CreatedAt > 0 {
		acc.CreatedAt = time.Unix(it.CreatedAt, 0).UTC()
	}
	if it.UpdatedAt > 0 {
		acc.UpdatedAt = time.Unix(it.UpdatedAt, 0).UTC()
	}
	return acc
}

func (d *dynamoStore) key(userID, accountID string) map[string]ddbtypes.AttributeValue {
	return map[string]ddbtypes.AttributeValue{
		"applicationId": &ddbtypes.AttributeValueMemberS{Value: d.applicationID},
		"userId":        &ddbtypes.AttributeValueMemberS{Value: sortKey(userID, accountID)},
	}
}

func (d *dynamoStore) getItem(ctx context.Context, userID, accountID string) (*accountItem, error) {
	out, err := d.client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName:      aws.String(d.tableName),
		ConsistentRead: aws.Bool(true),
		Key:            d.key(userID, accountID),
	})
	if err != nil {
		return nil, fmt.Errorf("get user config: %w", err)
	}
	if out.Item == nil {
		return nil, ErrConfigNotFound
	}
	var it accountItem
	if err := attributevalue.UnmarshalMap(out.Item, &it); err != nil {
		return nil, fmt.Errorf("unmarshal user config: %w", err)
	}
	return &it, nil
}

func (d *dynamoStore) GetUserConfig(ctx context.Context, userID, accountID string) (*types.Account, error) {
	it, err := d.getItem(ctx, userID, accountID)
	if err != nil {
		return nil, err
	}
	return it.toAccount(), nil
}

func (d *dynamoStore) ListUserConfigs(ctx context.Context, userID string) ([]*types.Account, error) {
	var accs []*types.Account
	var startKey map[string]ddbtypes.AttributeValue
	for {
		out, err := d.client.Query(ctx, &dynamodb.QueryInput{
			TableName:              aws.String(d.tableName),
			KeyConditionExpression: aws.String("applicationId = :app AND begins_with(userId, :prefix)"),
			ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
				":app":    &ddbtypes.AttributeValueMemberS{Value: d.applicationID},
				":prefix": &ddbtypes.AttributeValueMemberS{Value: userID + "#"},
			},
			ExclusiveStartKey: startKey,
		})
		if err != nil {
			return nil, fmt.Errorf("list user configs: %w", err)
		}
		for _, raw := range out.Items {
			var it accountItem
			if err := attributevalue.UnmarshalMap(raw, &it); err != nil {
				return nil, fmt.Errorf("unmarshal user config: %w", err)
			}
			accs = append(accs, it.toAccount())
		}
		if len(out.LastEvaluatedKey) == 0 {
			break
		}
		startKey = out.LastEvaluatedKey
	}
	return accs, nil
}

func (d *dynamoStore) ListAllConfigs(ctx context.Context) ([]*types.Account, error) {
	var accs []*types.Account
	var startKey map[string]ddbtypes.AttributeValue
	for {
		out, err := d.client.Query(ctx, &dynamodb.QueryInput{
			TableName:              aws.String(d.tableName),
			KeyConditionExpression: aws.String("applicationId = :app"),
			ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
				":app": &ddbtypes.AttributeValueMemberS{Value: d.applicationID},
			},
			ExclusiveStartKey: startKey,
		})
		if err != nil {
			return nil, fmt.Errorf("list all configs: %w", err)
		}
		for _, raw := range out.Items {
			var it accountItem
			if err := attributevalue.UnmarshalMap(raw, &it); err != nil {
				return nil, fmt.Errorf("unmarshal user config: %w", err)
			}
			accs = append(accs, it.toAccount())
		}
		if len(out.LastEvaluatedKey) == 0 {
			break
		}
		startKey = out.LastEvaluatedKey
	}
	return accs, nil
}

func (d *dynamoStore) PutUserConfig(ctx context.Context, userID string, acc *types.Account) error {
	now := time.Now().Unix()
	created := now
	// Preserve the original created_at across updates.
	if existing, err := d.getItem(ctx, userID, acc.ID); err == nil {
		if existing.CreatedAt > 0 {
			created = existing.CreatedAt
		}
	} else if !errors.Is(err, ErrConfigNotFound) {
		return err
	}

	it := newAccountItem(d.applicationID, userID, acc)
	it.CreatedAt = created
	it.UpdatedAt = now

	av, err := attributevalue.MarshalMap(it)
	if err != nil {
		return fmt.Errorf("marshal user config: %w", err)
	}
	if _, err := d.client.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(d.tableName),
		Item:      av,
	}); err != nil {
		return fmt.Errorf("put user config: %w", err)
	}
	return nil
}

func (d *dynamoStore) DeleteUserConfig(ctx context.Context, userID, accountID string) error {
	_, err := d.client.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName:           aws.String(d.tableName),
		Key:                 d.key(userID, accountID),
		ConditionExpression: aws.String("attribute_exists(applicationId)"),
	})
	if err != nil {
		var ccf *ddbtypes.ConditionalCheckFailedException
		if errors.As(err, &ccf) {
			return ErrConfigNotFound
		}
		return fmt.Errorf("delete user config: %w", err)
	}
	return nil
}

// --- In-memory implementation (local/stdio + tests) ---

type memoryStore struct {
	mu    sync.Mutex
	items map[string]*types.Account // sortKey -> account
}

func newMemoryStore() *memoryStore {
	return &memoryStore{items: make(map[string]*types.Account)}
}

func cloneAccount(a *types.Account) *types.Account {
	if a == nil {
		return nil
	}
	cp := *a
	cp.WatchFolders = copyWatchFolders(a.WatchFolders)
	return &cp
}

func copyWatchFolders(in []string) []string {
	if in == nil {
		return nil
	}
	return append([]string(nil), in...)
}

func ownerFromSortKey(sk, accountID string) string {
	if accountID != "" {
		suffix := "#" + accountID
		if strings.HasSuffix(sk, suffix) && len(sk) > len(suffix) {
			return sk[:len(sk)-len(suffix)]
		}
	}
	if i := strings.LastIndex(sk, "#"); i > 0 {
		return sk[:i]
	}
	return ""
}

func (m *memoryStore) GetUserConfig(_ context.Context, userID, accountID string) (*types.Account, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.items[sortKey(userID, accountID)]
	if !ok {
		return nil, ErrConfigNotFound
	}
	return cloneAccount(a), nil
}

func (m *memoryStore) ListUserConfigs(_ context.Context, userID string) ([]*types.Account, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	prefix := userID + "#"
	var accs []*types.Account
	for k, a := range m.items {
		if strings.HasPrefix(k, prefix) {
			accs = append(accs, cloneAccount(a))
		}
	}
	sort.Slice(accs, func(i, j int) bool { return accs[i].ID < accs[j].ID })
	return accs, nil
}

func (m *memoryStore) ListAllConfigs(_ context.Context) ([]*types.Account, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	accs := make([]*types.Account, 0, len(m.items))
	for _, a := range m.items {
		accs = append(accs, cloneAccount(a))
	}
	sort.Slice(accs, func(i, j int) bool {
		if accs[i].OwnerUserID != accs[j].OwnerUserID {
			return accs[i].OwnerUserID < accs[j].OwnerUserID
		}
		return accs[i].ID < accs[j].ID
	})
	return accs, nil
}

func (m *memoryStore) PutUserConfig(_ context.Context, userID string, acc *types.Account) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := cloneAccount(acc)
	cp.OwnerUserID = userID
	if cp.SMTPPassword == "" {
		cp.SMTPPassword = cp.IMAPPassword
	}
	now := time.Now().UTC()
	if existing, ok := m.items[sortKey(userID, acc.ID)]; ok && !existing.CreatedAt.IsZero() {
		cp.CreatedAt = existing.CreatedAt
	} else {
		cp.CreatedAt = now
	}
	cp.UpdatedAt = now
	m.items[sortKey(userID, acc.ID)] = cp
	return nil
}

func (m *memoryStore) DeleteUserConfig(_ context.Context, userID, accountID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := sortKey(userID, accountID)
	if _, ok := m.items[key]; !ok {
		return ErrConfigNotFound
	}
	delete(m.items, key)
	return nil
}
