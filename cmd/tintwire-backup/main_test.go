package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kilo666mj/tintwire/internal/store"
)

func TestRunCreatesAndVerifiesBackup(t *testing.T) {
	ctx := context.Background()
	sourcePath := filepath.Join(t.TempDir(), "source.db")
	snapshotPath := filepath.Join(t.TempDir(), "snapshot.db")
	data, err := store.Open(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := data.BootstrapUser(ctx, "admin", "a sufficiently long password"); err != nil {
		t.Fatal(err)
	}
	if err := data.Close(); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	if code := run([]string{"-db", sourcePath, "-out", snapshotPath}, &stdout, &stderr); code != 0 {
		t.Fatalf("create code=%d stderr=%q", code, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"-verify", snapshotPath}, &stdout, &stderr); code != 0 {
		t.Fatalf("verify code=%d stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "verified ") {
		t.Fatalf("stdout=%q", stdout.String())
	}
}

func TestRunRequiresDestination(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run(nil, &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), "-out is required") {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
}
