package testutil

import (
	"os"
	"testing"
)

func ShortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "rp-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}
