package handler

import (
	"strconv"
	"testing"
)

func TestLogSlice(t *testing.T) {
	data := []byte("0123456789")
	cases := []struct {
		name, offset, limit, want string
	}{
		{"first chunk", "0", "4", "0123"},
		{"next chunk advances", "4", "4", "4567"},
		{"last partial chunk", "8", "4", "89"},
		// The case that matters: a CLI that has read everything must get an
		// empty body so it falls through to its "is the run finished?" check.
		{"offset at end is empty", "10", "32768", ""},
		{"offset past end is empty", "99", "32768", ""},
		{"no params returns everything", "", "", "0123456789"},
		{"garbage offset means 0", "abc", "3", "012"},
		{"negative offset means 0", "-5", "3", "012"},
		{"limit larger than rest", "7", "100", "789"},
		{"zero limit means to the end", "2", "0", "23456789"},
	}
	for _, c := range cases {
		if got := string(logSlice(data, c.offset, c.limit)); got != c.want {
			t.Errorf("%s: logSlice(offset=%q, limit=%q) = %q, want %q", c.name, c.offset, c.limit, got, c.want)
		}
	}
}

// Reassembling a log from successive CLI-style polls must reproduce it exactly
// (no duplicated head) and terminate.
func TestLogSlicePollingReassembles(t *testing.T) {
	data := make([]byte, 100_000)
	for i := range data {
		data[i] = byte('a' + i%26)
	}
	var got []byte
	for polls := 0; ; polls++ {
		if polls > 100 {
			t.Fatal("polling did not terminate")
		}
		chunk := logSlice(data, strconv.Itoa(len(got)), "32768")
		if len(chunk) == 0 {
			break
		}
		got = append(got, chunk...)
	}
	if string(got) != string(data) {
		t.Fatalf("reassembled log differs (got %d bytes, want %d)", len(got), len(data))
	}
}
