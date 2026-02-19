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

func TestSecretSaveAndList(t *testing.T) {
	db := openTestDB(t)

	s := &Secret{
		ID:             "sec-1",
		Name:           "API_KEY",
		EncryptedValue: []byte("encrypted-data"),
		CreatedAt:      time.Now().Truncate(time.Second),
	}

	if err := db.SaveSecret(s); err != nil {
		t.Fatal(err)
	}

	secrets, err := db.ListSecrets()
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

	db.SaveSecret(&Secret{
		ID:             "sec-1",
		Name:           "KEY",
		EncryptedValue: []byte("v1"),
		CreatedAt:      time.Now(),
	})

	db.SaveSecret(&Secret{
		ID:             "sec-2",
		Name:           "KEY",
		EncryptedValue: []byte("v2"),
		CreatedAt:      time.Now(),
	})

	secrets, _ := db.ListSecrets()
	if len(secrets) != 1 {
		t.Fatalf("expected 1 secret after upsert, got %d", len(secrets))
	}
	if string(secrets[0].EncryptedValue) != "v2" {
		t.Fatalf("value not updated: got %q", secrets[0].EncryptedValue)
	}
}

func TestDeleteSecretByName(t *testing.T) {
	db := openTestDB(t)

	db.SaveSecret(&Secret{
		ID:             "sec-1",
		Name:           "KEY",
		EncryptedValue: []byte("val"),
		CreatedAt:      time.Now(),
	})

	if err := db.DeleteSecretByName("KEY"); err != nil {
		t.Fatal(err)
	}

	secrets, _ := db.ListSecrets()
	if len(secrets) != 0 {
		t.Fatalf("expected 0 secrets after delete, got %d", len(secrets))
	}

	if err := db.DeleteSecretByName("NONEXISTENT"); err == nil {
		t.Fatal("expected error for nonexistent secret")
	}
}

func TestMultipleSecrets(t *testing.T) {
	db := openTestDB(t)

	for _, name := range []string{"C_KEY", "A_KEY", "B_KEY"} {
		db.SaveSecret(&Secret{
			ID:             "sec-" + name,
			Name:           name,
			EncryptedValue: []byte("val"),
			CreatedAt:      time.Now(),
		})
	}

	secrets, err := db.ListSecrets()
	if err != nil {
		t.Fatal(err)
	}
	if len(secrets) != 3 {
		t.Fatalf("expected 3 secrets, got %d", len(secrets))
	}
	// Should be ordered by name
	if secrets[0].Name != "A_KEY" {
		t.Fatalf("first secret: got %q, want A_KEY", secrets[0].Name)
	}
}
