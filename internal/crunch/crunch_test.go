package crunch

import (
	"os"
	"testing"
)

// Standalone sanity test: point CRUNCH_TEST_FILE at a raw .crn or raw crunch-blob file to
// confirm the cgo binding actually decodes, not just compiles.
func TestDecodeSmoke(t *testing.T) {
	path := os.Getenv("CRUNCH_TEST_FILE")
	if path == "" {
		t.Skip("set CRUNCH_TEST_FILE")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	out, err := Decode(data, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("decoded %d bytes -> %d bytes", len(data), len(out))
	if len(out) == 0 {
		t.Fatal("empty output")
	}
}
