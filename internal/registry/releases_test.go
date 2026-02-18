package registry

import (
	"testing"
	"time"
)

func TestSaveAndGetRelease(t *testing.T) {
	db := testDB(t)

	app := &App{ID: "app-1", Name: "myapp", Image: "alpine", CreatedAt: time.Now()}
	if err := db.SaveApp(app); err != nil {
		t.Fatalf("save app: %v", err)
	}

	rel := &Release{
		ID:          "rel-1",
		AppID:       "app-1",
		ImageDigest: "sha256:abc123",
		RootfsPath:  "/data/releases/rel-1",
		Label:       "v1",
		CreatedAt:   time.Now().Truncate(time.Second),
	}
	if err := db.SaveRelease(rel); err != nil {
		t.Fatalf("save release: %v", err)
	}

	got, err := db.GetRelease("rel-1")
	if err != nil {
		t.Fatalf("get release: %v", err)
	}
	if got == nil {
		t.Fatal("expected release, got nil")
	}
	if got.AppID != "app-1" {
		t.Errorf("app_id = %q, want %q", got.AppID, "app-1")
	}
	if got.ImageDigest != "sha256:abc123" {
		t.Errorf("image_digest = %q, want %q", got.ImageDigest, "sha256:abc123")
	}
	if got.Label != "v1" {
		t.Errorf("label = %q, want %q", got.Label, "v1")
	}
}

func TestGetLatestRelease(t *testing.T) {
	db := testDB(t)

	app := &App{ID: "app-1", Name: "myapp", Image: "alpine", CreatedAt: time.Now()}
	if err := db.SaveApp(app); err != nil {
		t.Fatalf("save app: %v", err)
	}

	now := time.Now()
	releases := []Release{
		{ID: "rel-1", AppID: "app-1", ImageDigest: "sha256:aaa", RootfsPath: "/r/1", Label: "v1", CreatedAt: now.Add(-2 * time.Second)},
		{ID: "rel-2", AppID: "app-1", ImageDigest: "sha256:bbb", RootfsPath: "/r/2", Label: "v2", CreatedAt: now.Add(-1 * time.Second)},
		{ID: "rel-3", AppID: "app-1", ImageDigest: "sha256:ccc", RootfsPath: "/r/3", Label: "v3", CreatedAt: now},
	}
	for _, rel := range releases {
		r := rel
		if err := db.SaveRelease(&r); err != nil {
			t.Fatalf("save release %s: %v", rel.ID, err)
		}
	}

	latest, err := db.GetLatestRelease("app-1")
	if err != nil {
		t.Fatalf("get latest: %v", err)
	}
	if latest == nil {
		t.Fatal("expected release, got nil")
	}
	if latest.ID != "rel-3" {
		t.Errorf("latest id = %q, want %q", latest.ID, "rel-3")
	}
	if latest.Label != "v3" {
		t.Errorf("latest label = %q, want %q", latest.Label, "v3")
	}
}

func TestListReleases(t *testing.T) {
	db := testDB(t)

	app := &App{ID: "app-1", Name: "myapp", Image: "alpine", CreatedAt: time.Now()}
	if err := db.SaveApp(app); err != nil {
		t.Fatalf("save app: %v", err)
	}

	for i, id := range []string{"rel-1", "rel-2"} {
		rel := &Release{
			ID:          id,
			AppID:       "app-1",
			ImageDigest: "sha256:abc",
			RootfsPath:  "/r/" + id,
			CreatedAt:   time.Now().Add(time.Duration(i) * time.Second),
		}
		if err := db.SaveRelease(rel); err != nil {
			t.Fatalf("save release: %v", err)
		}
	}

	releases, err := db.ListReleases("app-1")
	if err != nil {
		t.Fatalf("list releases: %v", err)
	}
	if len(releases) != 2 {
		t.Errorf("len = %d, want 2", len(releases))
	}
}

func TestGetLatestReleaseNoReleases(t *testing.T) {
	db := testDB(t)

	got, err := db.GetLatestRelease("nonexistent")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != nil {
		t.Error("expected nil for app with no releases")
	}
}

func TestDeleteRelease(t *testing.T) {
	db := testDB(t)

	app := &App{ID: "app-1", Name: "myapp", Image: "alpine", CreatedAt: time.Now()}
	if err := db.SaveApp(app); err != nil {
		t.Fatalf("save app: %v", err)
	}

	rel := &Release{
		ID:          "rel-1",
		AppID:       "app-1",
		ImageDigest: "sha256:abc",
		RootfsPath:  "/r/1",
		CreatedAt:   time.Now(),
	}
	if err := db.SaveRelease(rel); err != nil {
		t.Fatalf("save release: %v", err)
	}

	if err := db.DeleteRelease("rel-1"); err != nil {
		t.Fatalf("delete release: %v", err)
	}

	got, err := db.GetRelease("rel-1")
	if err != nil {
		t.Fatalf("get release: %v", err)
	}
	if got != nil {
		t.Error("expected nil after delete")
	}
}

func TestReleaseNullLabel(t *testing.T) {
	db := testDB(t)

	app := &App{ID: "app-1", Name: "myapp", Image: "alpine", CreatedAt: time.Now()}
	if err := db.SaveApp(app); err != nil {
		t.Fatalf("save app: %v", err)
	}

	rel := &Release{
		ID:          "rel-1",
		AppID:       "app-1",
		ImageDigest: "sha256:abc",
		RootfsPath:  "/r/1",
		CreatedAt:   time.Now(),
	}
	if err := db.SaveRelease(rel); err != nil {
		t.Fatalf("save release: %v", err)
	}

	got, err := db.GetRelease("rel-1")
	if err != nil {
		t.Fatalf("get release: %v", err)
	}
	if got.Label != "" {
		t.Errorf("label = %q, want empty", got.Label)
	}
}
