package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseByteSize(t *testing.T) {
	tests := map[string]int64{
		"10MB": 10 * 1024 * 1024,
		"2m":   2 * 1024 * 1024,
		"512K": 512 * 1024,
		"42":   42,
	}
	for input, want := range tests {
		got, err := parseByteSize(input)
		if err != nil {
			t.Fatalf("parseByteSize(%q) returned error: %v", input, err)
		}
		if got != want {
			t.Fatalf("parseByteSize(%q) = %d, want %d", input, got, want)
		}
	}
}

func TestRotatingFileRotatesAndKeepsBackups(t *testing.T) {
	path := filepath.Join(t.TempDir(), "scheduler.log")
	file, err := newRotatingFile(path, 10, 2)
	if err != nil {
		t.Fatalf("newRotatingFile returned error: %v", err)
	}
	if _, err := file.Write([]byte("1234567890")); err != nil {
		t.Fatalf("write first chunk: %v", err)
	}
	if _, err := file.Write([]byte("abc")); err != nil {
		t.Fatalf("write second chunk: %v", err)
	}
	if _, err := file.Write([]byte("defghijklm")); err != nil {
		t.Fatalf("write third chunk: %v", err)
	}
	if _, err := file.Write([]byte("xyz")); err != nil {
		t.Fatalf("write fourth chunk: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close returned error: %v", err)
	}

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("current log missing: %v", err)
	}
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Fatalf("first backup missing: %v", err)
	}
	if _, err := os.Stat(path + ".2"); err != nil {
		t.Fatalf("second backup missing: %v", err)
	}
	if _, err := os.Stat(path + ".3"); !os.IsNotExist(err) {
		t.Fatalf("unexpected third backup stat error = %v", err)
	}
}
