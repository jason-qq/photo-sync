package api

import (
	"encoding/json"
	"testing"
)

func TestOptional(t *testing.T) {
	type body struct {
		Stars Optional[int]    `json:"stars"`
		Note  Optional[string] `json:"note"`
	}
	tests := []struct {
		in        string
		starsSet  bool
		stars     *int
		noteSet   bool
		noteIsNil bool
	}{
		{`{}`, false, nil, false, true},
		{`{"stars":null}`, true, nil, false, true},
		{`{"stars":4,"note":"メモ"}`, true, ptrTo(4), true, false},
		{`{"note":null}`, false, nil, true, true},
	}
	for _, tt := range tests {
		var b body
		if err := json.Unmarshal([]byte(tt.in), &b); err != nil {
			t.Fatalf("%s: %v", tt.in, err)
		}
		if b.Stars.Set != tt.starsSet || (tt.stars == nil) != (b.Stars.Value == nil) ||
			(tt.stars != nil && *b.Stars.Value != *tt.stars) {
			t.Errorf("%s: stars = %+v", tt.in, b.Stars)
		}
		if b.Note.Set != tt.noteSet || (b.Note.Value == nil) != tt.noteIsNil {
			t.Errorf("%s: note = %+v", tt.in, b.Note)
		}
	}
	var b body
	if err := json.Unmarshal([]byte(`{"stars":"five"}`), &b); err == nil {
		t.Error("wrong type accepted")
	}
}

func ptrTo[T any](v T) *T { return &v }
