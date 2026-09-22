package web

import (
	"os"
	"testing"
)

func TestWritableProbeLeavesDirectoryClean(t *testing.T) {
	dir := t.TempDir()
	if err := writableProbe(dir); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("probe left %d file(s) behind", len(entries))
	}
}
