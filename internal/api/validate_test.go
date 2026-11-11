package api

import (
	"errors"
	"strings"
	"testing"
)

func TestCreateSessionRequestValidate(t *testing.T) {
	tests := []struct {
		name  string
		req   CreateSessionRequest
		field string // "" means valid
	}{
		{"valid", CreateSessionRequest{"Shoot", "2026-11-09"}, ""},
		{"title padded", CreateSessionRequest{"  Shoot  ", "2026-11-09"}, ""},
		{"title max length", CreateSessionRequest{strings.Repeat("あ", MaxTitleLen), "2026-11-09"}, ""},
		{"title empty", CreateSessionRequest{"", "2026-11-09"}, "title"},
		{"title blank", CreateSessionRequest{"   ", "2026-11-09"}, "title"},
		{"title too long", CreateSessionRequest{strings.Repeat("a", MaxTitleLen+1), "2026-11-09"}, "title"},
		{"date missing", CreateSessionRequest{"Shoot", ""}, "date"},
		{"date wrong format", CreateSessionRequest{"Shoot", "09/11/2026"}, "date"},
		{"date impossible", CreateSessionRequest{"Shoot", "2026-02-30"}, "date"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			checkField(t, tt.req.Validate(), tt.field)
		})
	}
}

func TestCreatePhotoRequestValidate(t *testing.T) {
	hash := strings.Repeat("a1", 32)
	tests := []struct {
		name  string
		req   CreatePhotoRequest
		field string
	}{
		{"valid", CreatePhotoRequest{"DSC0001.ARW", hash}, ""},
		{"filename empty", CreatePhotoRequest{"", hash}, "filename"},
		{"filename too long", CreatePhotoRequest{strings.Repeat("a", MaxFilenameLen+1), hash}, "filename"},
		{"filename relative path", CreatePhotoRequest{"DCIM/101MSDCF/a.ARW", hash}, ""},
		{"filename backslash", CreatePhotoRequest{`dir\a.ARW`, hash}, "filename"},
		{"filename dot", CreatePhotoRequest{".", hash}, "filename"},
		{"filename dotdot", CreatePhotoRequest{"..", hash}, "filename"},
		{"filename dotdot segment", CreatePhotoRequest{"dir/../a.ARW", hash}, "filename"},
		{"filename dot segment", CreatePhotoRequest{"./a.ARW", hash}, "filename"},
		{"filename leading slash", CreatePhotoRequest{"/a.ARW", hash}, "filename"},
		{"filename trailing slash", CreatePhotoRequest{"dir/", hash}, "filename"},
		{"filename empty segment", CreatePhotoRequest{"dir//a.ARW", hash}, "filename"},
		{"hash empty", CreatePhotoRequest{"a.ARW", ""}, "hash"},
		{"hash short", CreatePhotoRequest{"a.ARW", "abc"}, "hash"},
		{"hash uppercase", CreatePhotoRequest{"a.ARW", strings.ToUpper(hash)}, "hash"},
		{"hash non-hex", CreatePhotoRequest{"a.ARW", strings.Repeat("z", 64)}, "hash"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			checkField(t, tt.req.Validate(), tt.field)
		})
	}
}

func checkField(t *testing.T, err error, wantField string) {
	t.Helper()
	if wantField == "" {
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		return
	}
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want ValidationError on %q", err, wantField)
	}
	if ve.Field != wantField {
		t.Errorf("field = %q, want %q", ve.Field, wantField)
	}
}

func TestPaths(t *testing.T) {
	if got := CompletePath("S1", "P1"); got != "/api/sessions/S1/photos/P1/complete" {
		t.Errorf("CompletePath = %q", got)
	}
	if got := PhotosPath("a/b"); got != "/api/sessions/a%2Fb/photos" {
		t.Errorf("PhotosPath escaping = %q", got)
	}
}
