package registry

import (
	"fmt"
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

func TestSecretSaveAndGet(t *testing.T) {
	db := openTestDB(t)

	s := &Secret{
		ID:             "sec-1",
		AppID:          "app-1",
		Name:           "API_KEY",
		EncryptedValue: []byte("encrypted-data"),
		Scope:          "per_app",
		CreatedAt:      time.Now().Truncate(time.Second),
	}

	if err := db.SaveSecret(s); err != nil {
		t.Fatal(err)
	}

	got, err := db.GetSecretByName("app-1", "API_KEY")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("expected secret, got nil")
	}
	if got.ID != "sec-1" {
		t.Fatalf("ID: got %q, want %q", got.ID, "sec-1")
	}
	if string(got.EncryptedValue) != "encrypted-data" {
		t.Fatalf("value mismatch")
	}
}

func TestSecretUpsert(t *testing.T) {
	db := openTestDB(t)

	s1 := &Secret{
		ID:             "sec-1",
		AppID:          "app-1",
		Name:           "KEY",
		EncryptedValue: []byte("v1"),
		Scope:          "per_app",
		CreatedAt:      time.Now(),
	}
	db.SaveSecret(s1)

	// Upsert with same app_id + name
	s2 := &Secret{
		ID:             "sec-2",
		AppID:          "app-1",
		Name:           "KEY",
		EncryptedValue: []byte("v2"),
		Scope:          "per_app",
		CreatedAt:      time.Now(),
	}
	db.SaveSecret(s2)

	got, _ := db.GetSecretByName("app-1", "KEY")
	if string(got.EncryptedValue) != "v2" {
		t.Fatalf("value not updated: got %q", got.EncryptedValue)
	}
	if got.ID != "sec-2" {
		t.Fatalf("ID not updated: got %q", got.ID)
	}
}

func TestSecretList(t *testing.T) {
	db := openTestDB(t)

	for _, name := range []string{"B_KEY", "A_KEY", "C_KEY"} {
		db.SaveSecret(&Secret{
			ID:             "sec-" + name,
			AppID:          "app-1",
			Name:           name,
			EncryptedValue: []byte("val"),
			Scope:          "per_app",
			CreatedAt:      time.Now(),
		})
	}

	secrets, err := db.ListSecrets("app-1")
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

func TestWorkspaceSecrets(t *testing.T) {
	db := openTestDB(t)

	db.SaveSecret(&Secret{
		ID:             "sec-ws-1",
		AppID:          "",
		Name:           "GLOBAL_KEY",
		EncryptedValue: []byte("global"),
		Scope:          "per_workspace",
		CreatedAt:      time.Now(),
	})

	db.SaveSecret(&Secret{
		ID:             "sec-app-1",
		AppID:          "app-1",
		Name:           "APP_KEY",
		EncryptedValue: []byte("app"),
		Scope:          "per_app",
		CreatedAt:      time.Now(),
	})

	ws, err := db.ListWorkspaceSecrets()
	if err != nil {
		t.Fatal(err)
	}
	if len(ws) != 1 {
		t.Fatalf("expected 1 workspace secret, got %d", len(ws))
	}
	if ws[0].Name != "GLOBAL_KEY" {
		t.Fatalf("got %q, want GLOBAL_KEY", ws[0].Name)
	}
}

func TestDeleteSecretByName(t *testing.T) {
	db := openTestDB(t)

	db.SaveSecret(&Secret{
		ID:             "sec-1",
		AppID:          "app-1",
		Name:           "KEY",
		EncryptedValue: []byte("val"),
		Scope:          "per_app",
		CreatedAt:      time.Now(),
	})

	if err := db.DeleteSecretByName("app-1", "KEY"); err != nil {
		t.Fatal(err)
	}

	got, _ := db.GetSecretByName("app-1", "KEY")
	if got != nil {
		t.Fatal("expected nil after delete")
	}

	// Delete non-existent should error
	if err := db.DeleteSecretByName("app-1", "NOPE"); err == nil {
		t.Fatal("expected error for non-existent secret")
	}
}

func TestDeleteAppSecrets(t *testing.T) {
	db := openTestDB(t)

	for i, name := range []string{"A", "B"} {
		db.SaveSecret(&Secret{
			ID:             fmt.Sprintf("sec-%d", i),
			AppID:          "app-1",
			Name:           name,
			EncryptedValue: []byte("val"),
			Scope:          "per_app",
			CreatedAt:      time.Now(),
		})
	}

	if err := db.DeleteAppSecrets("app-1"); err != nil {
		t.Fatal(err)
	}

	list, _ := db.ListSecrets("app-1")
	if len(list) != 0 {
		t.Fatalf("expected 0 secrets after delete, got %d", len(list))
	}
}

func TestDeleteAppCascadesSecrets(t *testing.T) {
	db := openTestDB(t)

	app := &App{
		ID:        "app-cascade",
		Name:      "cascade-test",
		Image:     "alpine:3.21",
		CreatedAt: time.Now(),
	}
	db.SaveApp(app)

	db.SaveSecret(&Secret{
		ID:             "sec-cascade",
		AppID:          "app-cascade",
		Name:           "KEY",
		EncryptedValue: []byte("val"),
		Scope:          "per_app",
		CreatedAt:      time.Now(),
	})

	if err := db.DeleteApp("app-cascade"); err != nil {
		t.Fatal(err)
	}

	got, _ := db.GetSecretByName("app-cascade", "KEY")
	if got != nil {
		t.Fatal("expected secret to be cascade-deleted")
	}
}
