package isureal

import (
	"encoding/hex"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestNewTimeID_ShapeAndTimestamp(t *testing.T) {
	before := time.Now().UnixMilli()
	id := newTimeID()
	after := time.Now().UnixMilli()

	if !imageIDPattern.MatchString(id) {
		t.Fatalf("id %q does not match 8-4-4-4-12 lowercase hex", id)
	}
	parsed, err := uuid.Parse(id)
	if err != nil {
		t.Fatalf("uuid.Parse(%q): %v", id, err)
	}
	if parsed.Version() != 4 {
		t.Errorf("version nibble = %d, want 4 (kept for compatibility)", parsed.Version())
	}
	if parsed.Variant() != uuid.RFC4122 {
		t.Errorf("variant = %v, want RFC4122", parsed.Variant())
	}
	tsHex := strings.ReplaceAll(id[:13], "-", "")
	raw, err := hex.DecodeString(tsHex)
	if err != nil || len(raw) != 6 {
		t.Fatalf("timestamp prefix %q is not 12 hex digits", tsHex)
	}
	var ms int64
	for _, b := range raw {
		ms = ms<<8 | int64(b)
	}
	if ms < before || ms > after {
		t.Errorf("timestamp prefix %d not within [%d, %d]", ms, before, after)
	}
}

func TestNewTimeID_UniqueAndOrdered(t *testing.T) {
	const n = 20000
	seen := make(map[string]struct{}, n)
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		id := newTimeID()
		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate id %q after %d ids", id, i)
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	// Generation order and lexicographic order agree at millisecond granularity:
	// an id generated at least 2 ms later must sort after.
	early := newTimeID()
	time.Sleep(3 * time.Millisecond)
	late := newTimeID()
	if !(early < late) {
		t.Errorf("later id %q does not sort after earlier id %q", late, early)
	}
	// Prefixes are non-decreasing in generation order.
	prefixes := make([]string, len(ids))
	for i, id := range ids {
		prefixes[i] = id[:13]
	}
	if !sort.StringsAreSorted(prefixes) {
		t.Errorf("timestamp prefixes are not non-decreasing in generation order")
	}
}

func BenchmarkNewTimeID(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = newTimeID()
	}
}

func BenchmarkUUIDNewString(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = uuid.NewString()
	}
}
