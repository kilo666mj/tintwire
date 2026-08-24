package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

func TestRunValidatesArgumentsAndReportsMigration(t *testing.T) {
	var stdout, stderr bytes.Buffer
	called := false
	migrate := func(_ context.Context, sqlite, postgres string) error {
		called = true
		if sqlite != "source.db" || postgres != "postgres://destination" {
			t.Fatalf("arguments = %q, %q", sqlite, postgres)
		}
		return nil
	}
	if code := run(nil, &stdout, &stderr, migrate); code != 2 || called {
		t.Fatalf("missing arguments: code=%d called=%t", code, called)
	}
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"-sqlite", "source.db", "-postgres", "postgres://destination"}, &stdout, &stderr, migrate); code != 0 || !called {
		t.Fatalf("success: code=%d called=%t stderr=%q", code, called, stderr.String())
	}
	if !strings.Contains(stdout.String(), "migrated and verified") {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestRunReportsMigrationFailure(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"-sqlite", "source.db", "-postgres", "postgres://destination"}, &stdout, &stderr, func(context.Context, string, string) error {
		return errors.New("copy failed")
	})
	if code != 1 || !strings.Contains(stderr.String(), "migration failed: copy failed") {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
}
