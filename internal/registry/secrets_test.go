package registry

import (
	"path/filepath"
	"testing"
	"time"
)

func openTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestWorkspaceSecretSaveAndList(t *testing.T) {
	db := openTestDB(t)

	s := &Secret{
		ID:             "sec-1",
		AppID:          "",
		Name:           "API_KEY",
		EncryptedValue: []byte("encrypted-data"),
		Scope:          "per_workspace",
		CreatedAt:      time.Now().Truncate(time.Second),
	}

	if err := db.SaveSecret(s); err != nil {
		t.Fatal(err)
	}

	secrets, err := db.ListWorkspaceSecrets()
	if err != nil {
		t.Fatal(err)
	}
	if len(secrets) != 1 {
		t.Fatalf("expected 1 secret, got %d", len(secrets))
	}
	if secrets[0].Name != "API_KEY" {
		t.Fatalf("name: got %q, want API_KEY", secrets[0].Name)
	}
}

func TestSecretUpsert(t *testing.T) {
	db := openTestDB(t)

	s1 := &Secret{
		ID:             "sec-1",
		AppID:          "",
		Name:           "KEY",
		EncryptedValue: []byte("v1"),
		Scope:          "per_workspace",
		CreatedAt:      time.Now(),
	}
	db.SaveSecret(s1)

	s2 := &Secret{
		ID:             "sec-2",
		AppID:          "",
		Name:           "KEY",
		EncryptedValue: []byte("v2"),
		Scope:          "per_workspace",
		CreatedAt:      time.Now(),
	}
	db.SaveSecret(s2)

	secrets, _ := db.ListWorkspaceSecrets()
	if len(secrets) != 1 {
		t.Fatalf("expected 1 secret after upsert, got %d", len(secrets))
	}
	if string(secrets[0].EncryptedValue) != "v2" {
		t.Fatalf("value not updated: got %q", secrets[0].EncryptedValue)
	}
}

func TestDeleteSecret(t *testing.T) {
	db := openTestDB(t)

	db.SaveSecret(&Secret{
		ID:             "sec-1",
		AppID:          "",
		Name:           "KEY",
		EncryptedValue: []byte("val"),
		Scope:          "per_workspace",
		CreatedAt:      time.Now(),
	})

	if err := db.DeleteSecret("sec-1"); err != nil {
		t.Fatal(err)
	}

	secrets, _ := db.ListWorkspaceSecrets()
	if len(secrets) != 0 {
		t.Fatalf("expected 0 secrets after delete, got %d", len(secrets))
	}
}
