package types

import (
	"errors"
	"fmt"
	"strings"
)

// Error codes returned to MCP clients. Stable identifiers for structured errors.
const (
	CodeAuthFailed           = "authentication_failed"
	CodeFolderNotFound       = "folder_not_found"
	CodeAttachmentFetch      = "attachment_fetch_failed"
	CodeSizeLimit            = "size_limit_exceeded"
	CodeRejectedRecipients   = "rejected_recipients"
	CodeValidation           = "invalid_input"
	CodeURLNotAllowed        = "url_not_allowed"
	CodePathNotAllowed       = "path_not_allowed"
	CodeAttachmentTooLarge   = "attachment_too_large"
	CodeMessageNotFound      = "message_not_found"
)

// Error is a structured, readable error suitable for MCP tool responses.
type Error struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	if len(e.Details) == 0 {
		return e.Message
	}
	parts := make([]string, 0, len(e.Details))
	for k, v := range e.Details {
		parts = append(parts, fmt.Sprintf("%s=%v", k, v))
	}
	return e.Message + " (" + strings.Join(parts, ", ") + ")"
}

func (e *Error) Is(target error) bool {
	var other *Error
	if !errors.As(target, &other) {
		return false
	}
	return e.Code == other.Code
}

func NewError(code, message string, details map[string]any) *Error {
	return &Error{Code: code, Message: message, Details: details}
}

func AuthFailed(proto string, err error) *Error {
	msg := proto + " authentication failed"
	if err != nil {
		msg = msg + ": " + err.Error()
	}
	return &Error{Code: CodeAuthFailed, Message: msg}
}

func FolderNotFound(folder string, err error) *Error {
	msg := fmt.Sprintf("folder %q not found", folder)
	if err != nil {
		msg = msg + ": " + err.Error()
	}
	return &Error{
		Code:    CodeFolderNotFound,
		Message: msg,
		Details: map[string]any{"folder": folder},
	}
}

func SizeLimitExceeded(actual, limit int64, limitName string) *Error {
	return &Error{
		Code:    CodeSizeLimit,
		Message: fmt.Sprintf("encoded message size %d bytes exceeds %s limit of %d bytes", actual, limitName, limit),
		Details: map[string]any{
			"actual_bytes": actual,
			"limit_bytes":  limit,
			"limit_name":   limitName,
		},
	}
}

func AttachmentFetchFailed(name string, err error) *Error {
	msg := "attachment fetch failed"
	if name != "" {
		msg = fmt.Sprintf("attachment %q fetch failed", name)
	}
	if err != nil {
		msg = msg + ": " + err.Error()
	}
	return &Error{
		Code:    CodeAttachmentFetch,
		Message: msg,
		Details: map[string]any{"filename": name},
	}
}

func RejectedRecipients(accepted, rejected []string) *Error {
	return &Error{
		Code:    CodeRejectedRecipients,
		Message: fmt.Sprintf("smtp server rejected %d recipient(s)", len(rejected)),
		Details: map[string]any{
			"accepted": accepted,
			"rejected": rejected,
		},
	}
}

func Validation(message string) *Error {
	return &Error{Code: CodeValidation, Message: message}
}
