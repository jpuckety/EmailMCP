package notify

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// notifyItem is the DynamoDB NotifyState row.
// PK applicationId, SK folderKey = ownerUserId#accountId#folder.
type notifyItem struct {
	ApplicationID string `dynamodbav:"applicationId"`
	FolderKey     string `dynamodbav:"folderKey"`
	LastUID       int64  `dynamodbav:"last_uid"`
	UIDValidity   int64  `dynamodbav:"uidvalidity"`
	WatermarkSet  bool   `dynamodbav:"watermark_set"`
	LeaseOwner    string `dynamodbav:"lease_owner"`
	LeaseUntil    int64  `dynamodbav:"lease_until"`
	ClaimedUID    int64  `dynamodbav:"claimed_uid"`
	ClaimedOwner  string `dynamodbav:"claimed_owner"`
	ClaimedUntil  int64  `dynamodbav:"claimed_until"`
}

type dynamoLeaseStore struct {
	client        *dynamodb.Client
	tableName     string
	applicationID string
	now           func() time.Time
}

// NewLeaseStore returns a DynamoDB-backed lease store when tableName is set.
func NewLeaseStore(ctx context.Context, tableName, applicationID string, logger *slog.Logger) (LeaseStore, error) {
	if logger == nil {
		logger = slog.Default()
	}
	if applicationID == "" {
		applicationID = "default"
	}
	if tableName == "" {
		logger.Warn("notify state table not configured; using in-memory lease store")
		return NewMemoryStore(), nil
	}
	var opts []func(*awsconfig.LoadOptions) error
	if region := os.Getenv("AWS_REGION"); region != "" {
		opts = append(opts, awsconfig.WithRegion(region))
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("load aws config for notify state: %w", err)
	}
	return &dynamoLeaseStore{
		client:        dynamodb.NewFromConfig(awsCfg),
		tableName:     tableName,
		applicationID: applicationID,
		now:           func() time.Time { return time.Now() },
	}, nil
}

func (d *dynamoLeaseStore) key(folderKey string) map[string]ddbtypes.AttributeValue {
	return map[string]ddbtypes.AttributeValue{
		"applicationId": &ddbtypes.AttributeValueMemberS{Value: d.applicationID},
		"folderKey":     &ddbtypes.AttributeValueMemberS{Value: folderKey},
	}
}

func isConditionalFail(err error) bool {
	var ccf *ddbtypes.ConditionalCheckFailedException
	return errors.As(err, &ccf)
}

func (d *dynamoLeaseStore) Acquire(ctx context.Context, key, owner string, ttl time.Duration) (bool, error) {
	if ttl <= 0 {
		ttl = DefaultLeaseTTL
	}
	now := d.now().Unix()
	until := d.now().Add(ttl).Unix()
	_, err := d.client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(d.tableName),
		Key:       d.key(key),
		UpdateExpression: aws.String(
			"SET lease_owner = :owner, lease_until = :until",
		),
		ConditionExpression: aws.String(
			"attribute_not_exists(lease_until) OR lease_until < :now OR lease_owner = :owner",
		),
		ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
			":owner": &ddbtypes.AttributeValueMemberS{Value: owner},
			":until": &ddbtypes.AttributeValueMemberN{Value: fmt.Sprintf("%d", until)},
			":now":   &ddbtypes.AttributeValueMemberN{Value: fmt.Sprintf("%d", now)},
		},
	})
	if err != nil {
		if isConditionalFail(err) {
			return false, nil
		}
		return false, fmt.Errorf("acquire lease: %w", err)
	}
	return true, nil
}

func (d *dynamoLeaseStore) Heartbeat(ctx context.Context, key, owner string, ttl time.Duration) (bool, error) {
	if ttl <= 0 {
		ttl = DefaultLeaseTTL
	}
	now := d.now().Unix()
	until := d.now().Add(ttl).Unix()
	_, err := d.client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName:        aws.String(d.tableName),
		Key:              d.key(key),
		UpdateExpression: aws.String("SET lease_until = :until"),
		ConditionExpression: aws.String(
			"lease_owner = :owner AND lease_until >= :now",
		),
		ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
			":owner": &ddbtypes.AttributeValueMemberS{Value: owner},
			":until": &ddbtypes.AttributeValueMemberN{Value: fmt.Sprintf("%d", until)},
			":now":   &ddbtypes.AttributeValueMemberN{Value: fmt.Sprintf("%d", now)},
		},
	})
	if err != nil {
		if isConditionalFail(err) {
			return false, nil
		}
		return false, fmt.Errorf("heartbeat lease: %w", err)
	}
	return true, nil
}

func (d *dynamoLeaseStore) Release(ctx context.Context, key, owner string) error {
	_, err := d.client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(d.tableName),
		Key:       d.key(key),
		UpdateExpression: aws.String(
			"SET lease_owner = :empty, lease_until = :zero, claimed_uid = :zero, claimed_owner = :empty, claimed_until = :zero",
		),
		ConditionExpression: aws.String("lease_owner = :owner"),
		ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
			":owner": &ddbtypes.AttributeValueMemberS{Value: owner},
			":empty": &ddbtypes.AttributeValueMemberS{Value: ""},
			":zero":  &ddbtypes.AttributeValueMemberN{Value: "0"},
		},
	})
	if err != nil {
		if isConditionalFail(err) {
			return nil
		}
		return fmt.Errorf("release lease: %w", err)
	}
	return nil
}

func (d *dynamoLeaseStore) GetWatermark(ctx context.Context, key string) (Watermark, error) {
	out, err := d.client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName:      aws.String(d.tableName),
		Key:            d.key(key),
		ConsistentRead: aws.Bool(true),
	})
	if err != nil {
		return Watermark{}, fmt.Errorf("get watermark: %w", err)
	}
	if len(out.Item) == 0 {
		return Watermark{}, nil
	}
	var it notifyItem
	if err := attributevalue.UnmarshalMap(out.Item, &it); err != nil {
		return Watermark{}, fmt.Errorf("unmarshal watermark: %w", err)
	}
	if !it.WatermarkSet {
		return Watermark{}, nil
	}
	return Watermark{
		LastUID:     uint32(it.LastUID),
		UIDValidity: uint32(it.UIDValidity),
		Set:         true,
	}, nil
}

func (d *dynamoLeaseStore) Snapshot(ctx context.Context, key, owner string, uidValidity, lastUID uint32) (bool, error) {
	now := d.now().Unix()
	_, err := d.client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(d.tableName),
		Key:       d.key(key),
		UpdateExpression: aws.String(
			"SET last_uid = :uid, uidvalidity = :uv, watermark_set = :true, claimed_uid = :zero, claimed_owner = :empty, claimed_until = :zero",
		),
		ConditionExpression: aws.String(
			"lease_owner = :owner AND lease_until >= :now AND (attribute_not_exists(watermark_set) OR watermark_set = :false OR uidvalidity <> :uv)",
		),
		ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
			":owner": &ddbtypes.AttributeValueMemberS{Value: owner},
			":now":   &ddbtypes.AttributeValueMemberN{Value: fmt.Sprintf("%d", now)},
			":uid":   &ddbtypes.AttributeValueMemberN{Value: fmt.Sprintf("%d", lastUID)},
			":uv":    &ddbtypes.AttributeValueMemberN{Value: fmt.Sprintf("%d", uidValidity)},
			":true":  &ddbtypes.AttributeValueMemberBOOL{Value: true},
			":false": &ddbtypes.AttributeValueMemberBOOL{Value: false},
			":zero":  &ddbtypes.AttributeValueMemberN{Value: "0"},
			":empty": &ddbtypes.AttributeValueMemberS{Value: ""},
		},
	})
	if err != nil {
		if isConditionalFail(err) {
			return false, nil
		}
		return false, fmt.Errorf("snapshot watermark: %w", err)
	}
	return true, nil
}

func (d *dynamoLeaseStore) ClaimUID(ctx context.Context, key, owner string, uidValidity, uid uint32, ttl time.Duration) (bool, error) {
	if ttl <= 0 {
		ttl = defaultClaimTTL
	}
	now := d.now().Unix()
	until := d.now().Add(ttl).Unix()
	_, err := d.client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(d.tableName),
		Key:       d.key(key),
		UpdateExpression: aws.String(
			"SET claimed_uid = :uid, claimed_owner = :owner, claimed_until = :until",
		),
		ConditionExpression: aws.String(
			"watermark_set = :true AND uidvalidity = :uv AND last_uid < :uid AND lease_owner = :owner AND lease_until >= :now AND " +
				"(attribute_not_exists(claimed_uid) OR claimed_uid = :zero OR claimed_until < :now OR (claimed_owner = :owner AND claimed_uid = :uid))",
		),
		ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
			":uid":   &ddbtypes.AttributeValueMemberN{Value: fmt.Sprintf("%d", uid)},
			":uv":    &ddbtypes.AttributeValueMemberN{Value: fmt.Sprintf("%d", uidValidity)},
			":owner": &ddbtypes.AttributeValueMemberS{Value: owner},
			":until": &ddbtypes.AttributeValueMemberN{Value: fmt.Sprintf("%d", until)},
			":now":   &ddbtypes.AttributeValueMemberN{Value: fmt.Sprintf("%d", now)},
			":true":  &ddbtypes.AttributeValueMemberBOOL{Value: true},
			":zero":  &ddbtypes.AttributeValueMemberN{Value: "0"},
		},
	})
	if err != nil {
		if isConditionalFail(err) {
			return false, nil
		}
		return false, fmt.Errorf("claim uid: %w", err)
	}
	return true, nil
}

func (d *dynamoLeaseStore) AdvanceUID(ctx context.Context, key, owner string, uidValidity, uid uint32) (bool, error) {
	now := d.now().Unix()
	_, err := d.client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(d.tableName),
		Key:       d.key(key),
		UpdateExpression: aws.String(
			"SET last_uid = :uid, claimed_uid = :zero, claimed_owner = :empty, claimed_until = :zero",
		),
		ConditionExpression: aws.String(
			"watermark_set = :true AND uidvalidity = :uv AND last_uid < :uid AND lease_owner = :owner AND lease_until >= :now AND claimed_owner = :owner AND claimed_uid = :uid",
		),
		ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
			":uid":   &ddbtypes.AttributeValueMemberN{Value: fmt.Sprintf("%d", uid)},
			":uv":    &ddbtypes.AttributeValueMemberN{Value: fmt.Sprintf("%d", uidValidity)},
			":owner": &ddbtypes.AttributeValueMemberS{Value: owner},
			":now":   &ddbtypes.AttributeValueMemberN{Value: fmt.Sprintf("%d", now)},
			":true":  &ddbtypes.AttributeValueMemberBOOL{Value: true},
			":zero":  &ddbtypes.AttributeValueMemberN{Value: "0"},
			":empty": &ddbtypes.AttributeValueMemberS{Value: ""},
		},
	})
	if err != nil {
		if isConditionalFail(err) {
			return false, nil
		}
		return false, fmt.Errorf("advance uid: %w", err)
	}
	return true, nil
}

func (d *dynamoLeaseStore) ClearClaim(ctx context.Context, key, owner string) error {
	_, err := d.client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(d.tableName),
		Key:       d.key(key),
		UpdateExpression: aws.String(
			"SET claimed_uid = :zero, claimed_owner = :empty, claimed_until = :zero",
		),
		ConditionExpression: aws.String("claimed_owner = :owner"),
		ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
			":owner": &ddbtypes.AttributeValueMemberS{Value: owner},
			":zero":  &ddbtypes.AttributeValueMemberN{Value: "0"},
			":empty": &ddbtypes.AttributeValueMemberS{Value: ""},
		},
	})
	if err != nil {
		if isConditionalFail(err) {
			return nil
		}
		return fmt.Errorf("clear claim: %w", err)
	}
	return nil
}
