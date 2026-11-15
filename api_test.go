package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWindowsDeviceID(t *testing.T) {
	for _, id := range []string{"CON", "con", "PRN", "AUX", "NUL", "COM1", "com9", "LPT9"} {
		if !isWindowsDeviceID(id) {
			t.Fatalf("%q should be a reserved device name", id)
		}
	}
	for _, id := range []string{"console", "com10", "lpt0", "svc-con", "my-service"} {
		if isWindowsDeviceID(id) {
			t.Fatalf("%q should not be a reserved device name", id)
		}
	}
}

func TestReadTailLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "svc.log")
	content := "one\ntwo\nthree\nfour\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	lines, size, err := readTailLines(path, 3)
	if err != nil {
		t.Fatal(err)
	}
	if size != int64(len(content)) {
		t.Fatalf("size = %d, want %d", size, len(content))
	}
	if got := strings.Join(lines, ","); got != "two,three,four" {
		t.Fatalf("lines = %q", got)
	}

	missing := filepath.Join(t.TempDir(), "missing.log")
	lines, size, err = readTailLines(missing, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 0 || size != 0 {
		t.Fatalf("missing file should be empty: lines=%v size=%d", lines, size)
	}
}

func TestNormalizeServiceIDRules(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "panel.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	a := &API{store: store}

	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}

	if _, err := a.normalizeService(Service{ID: "CON", Name: "x", Exe: exe}, true); err == nil || !strings.Contains(err.Error(), "保留设备名") {
		t.Fatalf("CON should be rejected, got %v", err)
	}

	first, err := a.normalizeService(Service{ID: "foo", Name: "foo", Exe: exe}, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.InsertService(first); err != nil {
		t.Fatal(err)
	}
	if _, err := a.normalizeService(Service{ID: "FOO", Name: "foo2", Exe: exe}, true); err == nil || !strings.Contains(err.Error(), "大小写") {
		t.Fatalf("case-only duplicate should be rejected, got %v", err)
	}

	// 更新自身不应被大小写检查误伤。
	if _, err := a.normalizeService(Service{ID: "foo", Name: "foo3", Exe: exe}, false); err != nil {
		t.Fatalf("updating same id should be allowed: %v", err)
	}
}
