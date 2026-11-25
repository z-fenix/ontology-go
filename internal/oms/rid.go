package oms

import (
	"crypto/rand"
	"fmt"
)

// newRID mints a resource ID "ri.<kind>.<uuid>" for a type registry entry.
func newRID(kind string) string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("ri.%s.%x-%x-%x-%x-%x", kind, b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}
