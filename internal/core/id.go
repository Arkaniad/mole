package core

import (
	"crypto/rand"
	"encoding/base32"
	"fmt"
	"time"
)

// IDs are time-prefixed so they sort chronologically in a database index and
// are readable in a terminal: "s_01JQ8F3K7X2M4P".
var idAlphabet = base32.NewEncoding("0123456789ABCDEFGHJKMNPQRSTVWXYZ").WithPadding(base32.NoPadding)

func newID(prefix string) string {
	var b [10]byte
	ms := uint64(time.Now().UnixMilli())
	// 48-bit big-endian timestamp, then 32 bits of randomness.
	b[0] = byte(ms >> 40)
	b[1] = byte(ms >> 32)
	b[2] = byte(ms >> 24)
	b[3] = byte(ms >> 16)
	b[4] = byte(ms >> 8)
	b[5] = byte(ms)
	if _, err := rand.Read(b[6:]); err != nil {
		panic(fmt.Sprintf("mole: crypto/rand failed: %v", err))
	}
	return prefix + "_" + idAlphabet.EncodeToString(b[:])
}

func NewSessionID() string     { return newID("s") }
func NewLeadID() string        { return newID("l") }
func NewClaimID() string       { return newID("c") }
func NewEdgeID() string        { return newID("e") }
func NewToolCallID() string    { return newID("tc") }
func NewReservationID() string { return newID("rsv") }
func NewSpanID() string        { return newID("sp") }
