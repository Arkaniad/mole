package core

import (
	"crypto/rand"
	"encoding/base32"
	"encoding/hex"
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

// PromptFence returns an unguessable delimiter suffix for wrapping untrusted
// content in a prompt (§3.2).
//
// A fixed delimiter is not a boundary. A page whose text contains "</content>"
// closes the region that was supposed to contain it, and everything after that
// line reads as instruction. Naming the delimiter randomly per call leaves the
// content nothing to imitate, and — unlike escaping the body — keeps it
// byte-identical, which §11.5's quote verification requires.
//
// Shared rather than reimplemented per package. Three call sites was the point at
// which one of them would eventually reach for math/rand.
func PromptFence() string {
	var b [8]byte
	// crypto/rand.Read fills b completely or panics internally; it cannot return
	// a short read.
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("mole: crypto/rand failed: %v", err))
	}
	return hex.EncodeToString(b[:])
}
