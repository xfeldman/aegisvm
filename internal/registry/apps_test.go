package registry

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func testDB(t *testing.T) *DB {
	t.Helper()
	dir := t.TempDir()
	db, err := Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestSaveAndGetApp(t *testing.T) {
	db := testDB(t)

	app := &App{
		ID:          "app-1",
		Name:        "myapp",
		Image:       "python:3.12",
		Command:     []string{"python", "-m", "http.server", "80"},
		ExposePorts: []int{80},
		Config:      map[string]string{"key": "value"},
		CreatedAt:   time.Now().Truncate(time.Second),
	}
	if err := db.SaveApp(app); err != nil {
		t.Fatalf("save app: %v", err)
	}

	got, err := db.GetApp("app-1")
	if err != nil {
		t.Fatalf("get app: %v", err)
	}
	if got == nil {
		t.Fatal("expected app, got nil")
	}
	if got.Name != "myapp" {
		t.Errorf("name = %q, want %q", got.Name, "myapp")
	}
	if got.Image != "python:3.12" {
		t.Errorf("image = %q, want %q", got.Image, "python:3.12")
	}
	if len(got.Command) != 4 {
		t.Errorf("command len = %d, want 4", len(got.Command))
	}
	if len(got.ExposePorts) != 1 || got.ExposePorts[0] != 80 {
		t.Errorf("expose_ports = %v, want [80]", got.ExposePorts)
	}
}

func TestGetAppByName(t *testing.T) {
	db := testDB(t)

	app := &App{
		ID:        "app-1",
		Name:      "myapp",
		Image:     "alpine:3.21",
		Command:   []string{"echo", "hello"},
		CreatedAt: time.Now(),
	}
	if err := db.SaveApp(app); err != nil {
		t.Fatalf("save app: %v", err)
	}

	got, err := db.GetAppByName("myapp")
	if err != nil {
		t.Fatalf("get app by name: %v", err)
	}
	if got == nil {
		t.Fatal("expected app, got nil")
	}
	if got.ID != "app-1" {
		t.Errorf("id = %q, want %q", got.ID, "app-1")
	}
}

func TestAppUniqueName(t *testing.T) {
	db := testDB(t)

	app1 := &App{ID: "app-1", Name: "myapp", Image: "alpine", CreatedAt: time.Now()}
	app2 := &App{ID: "app-2", Name: "myapp", Image: "ubuntu", CreatedAt: time.Now()}

	if err := db.SaveApp(app1); err != nil {
		t.Fatalf("save app1: %v", err)
	}
	if err := db.SaveApp(app2); err == nil {
		t.Fatal("expected unique constraint error, got nil")
	}
}

func TestListApps(t *testing.T) {
	db := testDB(t)

	for i, name := range []string{"app-a", "app-b", "app-c"} {
		app := &App{
			ID:        name,
			Name:      name,
			Image:     "alpine",
			Command:   []string{"echo"},
			CreatedAt: time.Now().Add(time.Duration(i) * time.Second),
		}
		if err := db.SaveApp(app); err != nil {
			t.Fatalf("save app %s: %v", name, err)
		}
	}

	apps, err := db.ListApps()
	if err != nil {
		t.Fatalf("list apps: %v", err)
	}
	if len(apps) != 3 {
		t.Errorf("len = %d, want 3", len(apps))
	}
}

func TestDeleteApp(t *testing.T) {
	db := testDB(t)

	app := &App{ID: "app-1", Name: "myapp", Image: "alpine", CreatedAt: time.Now()}
	if err := db.SaveApp(app); err != nil {
		t.Fatalf("save app: %v", err)
	}

	// Add a release too
	rel := &Release{
		ID:          "rel-1",
		AppID:       "app-1",
		ImageDigest: "sha256:abc",
		RootfsPath:  "/tmp/rootfs",
		CreatedAt:   time.Now(),
	}
	if err := db.SaveRelease(rel); err != nil {
		t.Fatalf("save release: %v", err)
	}

	if err := db.DeleteApp("app-1"); err != nil {
		t.Fatalf("delete app: %v", err)
	}

	got, err := db.GetApp("app-1")
	if err != nil {
		t.Fatalf("get app: %v", err)
	}
	if got != nil {
		t.Error("expected nil after delete")
	}

	// Releases should be deleted too
	releases, err := db.ListReleases("app-1")
	if err != nil {
		t.Fatalf("list releases: %v", err)
	}
	if len(releases) != 0 {
		t.Errorf("expected 0 releases after delete, got %d", len(releases))
	}
}

func TestGetAppNotFound(t *testing.T) {
	db := testDB(t)

	got, err := db.GetApp("nonexistent")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != nil {
		t.Error("expected nil for nonexistent app")
	}
}

func TestMigrationIdempotency(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	// Open and close twice — migration should be idempotent
	db1, err := Open(dbPath)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	db1.Close()

	db2, err := Open(dbPath)
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	db2.Close()

	// Clean up
	os.Remove(dbPath)
}
