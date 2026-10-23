package config

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/kms"
)

const kmsPasswordPrefix = "kms:v1:"

type passwordCipher interface {
	Encrypt(ctx context.Context, plaintext string) (string, error)
	Decrypt(ctx context.Context, ciphertext string) (string, error)
}

type kmsAPI interface {
	Encrypt(ctx context.Context, params *kms.EncryptInput, optFns ...func(*kms.Options)) (*kms.EncryptOutput, error)
	Decrypt(ctx context.Context, params *kms.DecryptInput, optFns ...func(*kms.Options)) (*kms.DecryptOutput, error)
}

type kmsPasswordCipher struct {
	client        kmsAPI
	keyID         string
	applicationID string
}

func newKMSPasswordCipher(cfg aws.Config, keyID, applicationID string) *kmsPasswordCipher {
	return &kmsPasswordCipher{
		client:        kms.NewFromConfig(cfg),
		keyID:         keyID,
		applicationID: applicationID,
	}
}

func (c *kmsPasswordCipher) Encrypt(ctx context.Context, plaintext string) (string, error) {
	if plaintext == "" {
		return "", nil
	}
	out, err := c.client.Encrypt(ctx, &kms.EncryptInput{
		KeyId:             aws.String(c.keyID),
		Plaintext:         []byte(plaintext),
		EncryptionContext: c.encryptionContext(),
	})
	if err != nil {
		return "", fmt.Errorf("KMS encrypt failed: %w", err)
	}
	if out == nil || len(out.CiphertextBlob) == 0 {
		return "", errors.New("KMS encrypt returned empty ciphertext")
	}
	return kmsPasswordPrefix + base64.StdEncoding.EncodeToString(out.CiphertextBlob), nil
}

func (c *kmsPasswordCipher) Decrypt(ctx context.Context, ciphertext string) (string, error) {
	if ciphertext == "" || !strings.HasPrefix(ciphertext, kmsPasswordPrefix) {
		// Rows written before application-level encryption contained plaintext.
		// They remain readable and are encrypted the next time they are updated.
		return ciphertext, nil
	}
	blob, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(ciphertext, kmsPasswordPrefix))
	if err != nil {
		return "", fmt.Errorf("decode KMS ciphertext: %w", err)
	}
	out, err := c.client.Decrypt(ctx, &kms.DecryptInput{
		CiphertextBlob:    blob,
		EncryptionContext: c.encryptionContext(),
	})
	if err != nil {
		return "", fmt.Errorf("KMS decrypt failed: %w", err)
	}
	if out == nil {
		return "", errors.New("KMS decrypt returned no result")
	}
	return string(out.Plaintext), nil
}

func (c *kmsPasswordCipher) encryptionContext() map[string]string {
	return map[string]string{
		"applicationId": c.applicationID,
		"purpose":       "email-password",
	}
}
