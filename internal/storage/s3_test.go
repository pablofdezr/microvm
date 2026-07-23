package storage

import "testing"

// HTTP byte ranges are inclusive at both ends, which is the kind of detail that
// is obviously right until it is quietly wrong: an off-by-one here fetches one
// byte too many on every read, forever, and nothing ever complains -- the data
// is a superset of what was asked for, so it looks correct and costs a little
// extra on every single request.
func TestByteRange(t *testing.T) {
	tests := []struct {
		name           string
		offset, length int64
		want           string
	}{
		{"the first 10 bytes", 0, 10, "bytes=0-9"},
		{"10 bytes from 100", 100, 10, "bytes=100-109"},
		{"one byte", 5, 1, "bytes=5-5"},
		{"to the end", 100, -1, "bytes=100-"},
		{"everything from the start", 0, -1, "bytes=0-"},
		{"a 4KB page, as a filesystem would ask", 4096, 4096, "bytes=4096-8191"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := byteRange(tc.offset, tc.length); got != tc.want {
				t.Errorf("byteRange(%d, %d) = %q, want %q", tc.offset, tc.length, got, tc.want)
			}
		})
	}
}
