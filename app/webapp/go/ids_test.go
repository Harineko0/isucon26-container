package isureal

import (
	"encoding/hex"
	"fmt"
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

// timeIDAt は ms の時刻を先頭に持つ、newTimeID と同じ形の固定 id を作る (テスト用)。
func timeIDAt(at time.Time, suffix string) string {
	ms := at.UnixMilli()
	return fmt.Sprintf("%08x-%04x-4000-8000-0000000000%s", ms>>16, ms&0xffff, suffix)
}

func TestTimeIDTime_RoundTrip(t *testing.T) {
	for _, gen := range []struct {
		name string
		id   func() string
	}{
		{"newTimeID", newTimeID},
		{"newImageID(europe)", func() string { return newImageID("europe") }},
	} {
		before := time.Now().Truncate(time.Millisecond)
		id := gen.id()
		after := time.Now()
		got, ok := timeIDTime(id)
		if !ok {
			t.Fatalf("%s: timeIDTime(%q) = !ok", gen.name, id)
		}
		if got.Before(before) || got.After(after) {
			t.Errorf("%s: timeIDTime(%q) = %s, want within [%s, %s]", gen.name, id, got, before, after)
		}
	}
	fixed := time.Date(2026, 10, 31, 0, 30, 0, 0, time.UTC)
	got, ok := timeIDTime(timeIDAt(fixed, "f1"))
	if !ok || !got.Equal(fixed) {
		t.Errorf("timeIDTime(fixed) = %s, %v; want %s, true", got, ok, fixed)
	}
}

func TestTimeIDTime_RejectsOtherIDs(t *testing.T) {
	cases := map[string]string{
		"empty":            "",
		"short":            "b",
		"not uuid":         "unused-image",
		"zero":             "00000000-0000-4000-8000-000000000000",
		"random v4 (2122)": "3f2504e0-4f89-41d3-9a0c-0305e82c3301",
		"far future":       "ffffffff-ffff-4fff-8fff-ffffffffffff",
		"upper hex":        strings.ToUpper(newTimeID()),
		"bad hex":          "zzzzzzzz-zzzz-4000-8000-000000000000",
		"before since":     timeIDAt(timeIDSince.Add(-time.Millisecond), "00"),
		"2 hours ahead":    timeIDAt(time.Now().Add(2*time.Hour), "00"),
		"truncated":        newTimeID()[:35],
		"no dashes":        strings.ReplaceAll(newTimeID(), "-", "0"),
	}
	for name, id := range cases {
		if got, ok := timeIDTime(id); ok {
			t.Errorf("%s: timeIDTime(%q) = %s, true; want !ok", name, id, got)
		}
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
