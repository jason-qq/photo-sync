package store

import "testing"

func TestMemory(t *testing.T) {
	testContract(t, func(*testing.T) Store { return NewMemory() })
}
