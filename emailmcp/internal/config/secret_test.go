package config

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/kms"
)

type testKMSClient struct {
	keyID      string
	context    map[string]string
	encryptHit int
	decryptHit int
}

func (c *testKMSClient) Encrypt(_ context.Context, input *kms.EncryptInput, _ ...func(*kms.Options)) (*kms.EncryptOutput, error) {
	c.encryptHit++
	if input.KeyId != nil {
		c.keyID = *input.KeyId
	}
	c.context = input.EncryptionContext
	encoded := base64.StdEncoding.EncodeToString(input.Plaintext)
	return &kms.EncryptOutput{CiphertextBlob: []byte("test:" + encoded)}, nil
}

func (c *testKMSClient) Decrypt(_ context.Context, input *kms.DecryptInput, _ ...func(*kms.Options)) (*kms.DecryptOutput, error) {
	c.decryptHit++
	c.context = input.EncryptionContext
	encoded := strings.TrimPrefix(string(input.CiphertextBlob), "test:")
	plaintext, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, err
	}
	return &kms.DecryptOutput{Plaintext: plaintext}, nil
}

func newTestPasswordCipher() *kmsPasswordCipher {
	return &kmsPasswordCipher{
		client:        &testKMSClient{},
		keyID:         "test-key",
		applicationID: "emailmcp-test",
	}
}

func TestKMSPasswordCipherEncryptDecrypt(t *testing.T) {
	client := &testKMSClient{}
	cipher := &kmsPasswordCipher{client: client, keyID: "test-key", applicationID: "emailmcp-test"}

	encoded, err := cipher.Encrypt(context.Background(), "mail-password")
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if !strings.HasPrefix(encoded, kmsPasswordPrefix) || strings.Contains(encoded, "mail-password") {
		t.Fatalf("encrypted value is not safely prefixed: %q", encoded)
	}
	if client.keyID != "test-key" {
		t.Fatalf("encryption key = %q", client.keyID)
	}
	if client.context["applicationId"] != "emailmcp-test" || client.context["purpose"] != "email-password" {
		t.Fatalf("encryption context = %#v", client.context)
	}

	decoded, err := cipher.Decrypt(context.Background(), encoded)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if decoded != "mail-password" {
		t.Fatalf("decrypted value = %q", decoded)
	}
}

func TestKMSPasswordCipherLegacyAndEmptyValues(t *testing.T) {
	client := &testKMSClient{}
	cipher := &kmsPasswordCipher{client: client, keyID: "test-key", applicationID: "emailmcp-test"}

	legacy, err := cipher.Decrypt(context.Background(), "legacy-password")
	if err != nil || legacy != "legacy-password" {
		t.Fatalf("legacy decrypt = %q, %v", legacy, err)
	}
	empty, err := cipher.Encrypt(context.Background(), "")
	if err != nil || empty != "" {
		t.Fatalf("empty encrypt = %q, %v", empty, err)
	}
	if client.encryptHit != 0 || client.decryptHit != 0 {
		t.Fatalf("KMS calls for legacy/empty values: encrypt=%d decrypt=%d", client.encryptHit, client.decryptHit)
	}
}

func TestKMSPasswordCipherRejectsMalformedCiphertext(t *testing.T) {
	cipher := newTestPasswordCipher()
	if _, err := cipher.Decrypt(context.Background(), kmsPasswordPrefix+"%%%"); err == nil {
		t.Fatal("Decrypt accepted malformed prefixed ciphertext")
	}
}
