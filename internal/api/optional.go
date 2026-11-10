package api

import "encoding/json"

// Optional is a JSON field that distinguishes three cases, which plain Go
// fields (and *T) can't:
//
//	absent           Set == false                  leave unchanged
//	"field": null    Set == true,  Value == nil    clear
//	"field": value   Set == true,  Value != nil    set
//
// It works because encoding/json only calls UnmarshalJSON for keys that are
// present. Used for partial updates (PATCH) where null means "remove".
type Optional[T any] struct {
	Set   bool
	Value *T
}

// UnmarshalJSON implements json.Unmarshaler.
func (o *Optional[T]) UnmarshalJSON(b []byte) error {
	o.Set = true
	if string(b) == "null" {
		o.Value = nil
		return nil
	}
	var v T
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	o.Value = &v
	return nil
}
