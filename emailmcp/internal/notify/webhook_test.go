package notify

import (
	"strings"
	"testing"
)

func TestValidateWebhookURL(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"valid", "https://example.com/hook", ""},
		{"invalid url", "https://hooks.example.com/%zz", "webhook_url is not a valid URL"},
		{"missing scheme", "example.com", "webhook_url must be https"},
		{"http forbidden", "http://example.com", "webhook_url must be https"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateWebhookURL(tt.raw)
			if tt.want == "" {
				if err != nil {
					t.Errorf("got error %q, want nil", err)
				}
			} else {
				if err == nil {
					t.Fatalf("got nil error, want %q", tt.want)
				}
				if err.Error() != tt.want {
					t.Errorf("got error %q, want %q", err.Error(), tt.want)
				}
				if strings.Contains(err.Error(), "https://") || strings.Contains(err.Error(), "%zz") {
					t.Errorf("error leaked webhook URL: %q", err.Error())
				}
			}
		})
	}
}
