package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestManagedUserSafetyAndAccess(t *testing.T) {
	ctx := context.Background()
	data, err := Open(filepath.Join(t.TempDir(), "users.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer data.Close()
	admin, err := data.CreateUser(ctx, "admin", "secure admin password", true)
	if err != nil {
		t.Fatal(err)
	}
	viewer, err := data.CreateUser(ctx, "viewer", "secure viewer password", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := data.SetManagedUserAdmin(ctx, admin.ID, admin.ID, false); err == nil {
		t.Fatal("demoted final administrator")
	}
	if err := data.SetManagedUserDisabled(ctx, admin.ID, admin.ID, true); err == nil {
		t.Fatal("administrator disabled own account")
	}
	if err := data.SetManagedUserAdmin(ctx, admin.ID, viewer.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := data.SetManagedUserAdmin(ctx, admin.ID, admin.ID, false); err != nil {
		t.Fatal(err)
	}
	if err := data.SetManagedUserAdmin(ctx, viewer.ID, admin.ID, true); err != nil {
		t.Fatal(err)
	}
	token, _, err := data.CreateSession(ctx, viewer.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := data.SetManagedUserDisabled(ctx, admin.ID, viewer.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := data.UserForSession(ctx, token); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("disabled session error = %v", err)
	}
	if _, err := data.AuthenticateUser(ctx, "viewer", "secure viewer password"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("disabled login error = %v", err)
	}
	if err := data.SetManagedUserDisabled(ctx, admin.ID, viewer.ID, false); err != nil {
		t.Fatal(err)
	}
	if err := data.ResetManagedUserPassword(ctx, admin.ID, viewer.ID, "replacement secure password"); err != nil {
		t.Fatal(err)
	}
	if _, err := data.AuthenticateUser(ctx, "viewer", "replacement secure password"); err != nil {
		t.Fatal(err)
	}
	if err := data.SetManagedUserDisabled(ctx, admin.ID, localInboxUserID, true); !errors.Is(err, ErrForbidden) {
		t.Fatalf("system disable error = %v", err)
	}

	channel, _, err := data.CreateChannel(ctx, CreateChannelInput{Name: "private", DisplayName: "Private", Visibility: "private"})
	if err != nil {
		t.Fatal(err)
	}
	if err := data.SetChannelMemberByID(ctx, admin.ID, channel.ID, viewer.ID, "operator"); err != nil {
		t.Fatal(err)
	}
	users, err := data.ListManagedUsers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var found *ManagedUser
	for i := range users {
		if users[i].ID == viewer.ID {
			found = &users[i]
		}
	}
	if found == nil || len(found.Memberships) != 1 || found.Memberships[0].Role != "operator" {
		t.Fatalf("managed viewer = %#v", found)
	}
	if err := data.RemoveChannelMember(ctx, admin.ID, channel.ID, viewer.ID); err != nil {
		t.Fatal(err)
	}
	var auditCount int
	if err := data.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM admin_audit_events WHERE actor_user_id=? AND target_user_id=?`, admin.ID, viewer.ID).Scan(&auditCount); err != nil {
		t.Fatal(err)
	}
	if auditCount != 6 {
		t.Fatalf("admin audit events = %d, want 6", auditCount)
	}
}
