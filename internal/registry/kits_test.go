package registry

import (
	"testing"
	"time"
)

func TestKitSaveAndGet(t *testing.T) {
	db := openTestDB(t)

	k := &Kit{
		Name:        "famiglia",
		Version:     "0.1.0",
		Description: "Famiglia AI agent kit",
		Config: KitConfig{
			Secrets: KitSecrets{
				Required: []KitSecretDef{
					{Name: "ANTHROPIC_API_KEY", Description: "API key"},
				},
			},
			Resources: KitResources{MemoryMB: 1024, VCPUs: 2},
		},
		ImageRef:    "ghcr.io/famiglia/agent:latest",
		InstalledAt: time.Now().Truncate(time.Second),
	}

	if err := db.SaveKit(k); err != nil {
		t.Fatal(err)
	}

	got, err := db.GetKit("famiglia")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("expected kit, got nil")
	}
	if got.Version != "0.1.0" {
		t.Fatalf("version: got %q, want %q", got.Version, "0.1.0")
	}
	if len(got.Config.Secrets.Required) != 1 {
		t.Fatalf("expected 1 required secret, got %d", len(got.Config.Secrets.Required))
	}
	if got.Config.Resources.MemoryMB != 1024 {
		t.Fatalf("memory: got %d, want 1024", got.Config.Resources.MemoryMB)
	}
}

func TestKitUpsert(t *testing.T) {
	db := openTestDB(t)

	k1 := &Kit{
		Name:        "test-kit",
		Version:     "0.1.0",
		ImageRef:    "image:v1",
		InstalledAt: time.Now(),
	}
	db.SaveKit(k1)

	k2 := &Kit{
		Name:        "test-kit",
		Version:     "0.2.0",
		ImageRef:    "image:v2",
		InstalledAt: time.Now(),
	}
	db.SaveKit(k2)

	got, _ := db.GetKit("test-kit")
	if got.Version != "0.2.0" {
		t.Fatalf("version not updated: got %q", got.Version)
	}
}

func TestKitList(t *testing.T) {
	db := openTestDB(t)

	for _, name := range []string{"beta", "alpha", "gamma"} {
		db.SaveKit(&Kit{
			Name:        name,
			Version:     "1.0.0",
			ImageRef:    "img:" + name,
			InstalledAt: time.Now(),
		})
	}

	kits, err := db.ListKits()
	if err != nil {
		t.Fatal(err)
	}
	if len(kits) != 3 {
		t.Fatalf("expected 3 kits, got %d", len(kits))
	}
	// Should be ordered by name
	if kits[0].Name != "alpha" {
		t.Fatalf("first kit: got %q, want alpha", kits[0].Name)
	}
}

func TestKitDelete(t *testing.T) {
	db := openTestDB(t)

	db.SaveKit(&Kit{
		Name:        "del-kit",
		Version:     "1.0.0",
		ImageRef:    "img:del",
		InstalledAt: time.Now(),
	})

	if err := db.DeleteKit("del-kit"); err != nil {
		t.Fatal(err)
	}

	got, _ := db.GetKit("del-kit")
	if got != nil {
		t.Fatal("expected nil after delete")
	}

	// Delete non-existent should error
	if err := db.DeleteKit("nope"); err == nil {
		t.Fatal("expected error for non-existent kit")
	}
}

func TestKitGetNotFound(t *testing.T) {
	db := openTestDB(t)

	got, err := db.GetKit("nonexistent")
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatal("expected nil for non-existent kit")
	}
}
