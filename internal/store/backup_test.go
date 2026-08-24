package store

import (
	"context"
	"path/filepath"
	"testing"
)

func TestBackupAndVerify(t *testing.T) {
	ctx := context.Background()
	data, err := Open(filepath.Join(t.TempDir(), "live.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = data.Close() })
	if err := data.BootstrapWebhook(ctx, "backup-token", "operations"); err != nil {
		t.Fatal(err)
	}

	backup := filepath.Join(t.TempDir(), "snapshot.db")
	if err := data.Backup(ctx, backup); err != nil {
		t.Fatal(err)
	}
	if err := VerifyDatabase(ctx, backup); err != nil {
		t.Fatal(err)
	}
	if err := data.Backup(ctx, backup); err == nil {
		t.Fatal("second backup unexpectedly overwrote the existing snapshot")
	}
}
