package api

import (
	"strings"
	"time"
	"unicode/utf8"
)

// Field limits.
const (
	MaxTitleLen    = 200
	MaxFilenameLen = 255
)

// ValidationError reports an invalid request field. Its message is safe to
// return to clients.
type ValidationError struct {
	Field   string
	Message string
}

func (e *ValidationError) Error() string { return e.Field + ": " + e.Message }

func invalid(field, msg string) error { return &ValidationError{field, msg} }

// Validate checks r. Title is checked after trimming whitespace; callers
// should store strings.TrimSpace(r.Title).
func (r CreateSessionRequest) Validate() error {
	if n := utf8.RuneCountInString(strings.TrimSpace(r.Title)); n == 0 || n > MaxTitleLen {
		return invalid("title", "required, at most 200 characters")
	}
	if _, err := time.Parse(time.DateOnly, r.Date); err != nil {
		return invalid("date", "must be YYYY-MM-DD")
	}
	return nil
}

// Validate checks r.
func (r CreatePhotoRequest) Validate() error {
	if n := utf8.RuneCountInString(r.Filename); n == 0 || n > MaxFilenameLen {
		return invalid("filename", "required, at most 255 characters")
	}
	if strings.ContainsAny(r.Filename, `/\`) || r.Filename == "." || r.Filename == ".." {
		return invalid("filename", "must be a base name without path separators")
	}
	if !isSHA256Hex(r.Hash) {
		return invalid("hash", "must be a lowercase hex SHA-256")
	}
	return nil
}

func isSHA256Hex(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
