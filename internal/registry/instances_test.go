package registry

import (
	"testing"
	"time"
)

func TestSaveAndGetInstance(t *testing.T) {
	db := openTestDB(t)

	inst := &Instance{
		ID:          "inst-1",
		State:       "stopped",
		Command:     []string{"python", "-m", "http.server"},
		ExposePorts: []int{80, 443},
		Handle:      "web",
		ImageRef:    "python:3.12",
		Workspace:   "/home/user/myapp",
		Env:         map[string]string{"API_KEY": "sk-123", "DEBUG": "1"},
		SecretKeys:  []string{"API_KEY"},
		CreatedAt:   time.Now().Truncate(time.Second),
	}

	if err := db.SaveInstance(inst); err != nil {
		t.Fatal(err)
	}

	got, err := db.GetInstance("inst-1")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("expected instance, got nil")
	}
	if got.ID != "inst-1" {
		t.Errorf("ID = %q, want %q", got.ID, "inst-1")
	}
	if got.State != "stopped" {
		t.Errorf("State = %q, want %q", got.State, "stopped")
	}
	if len(got.Command) != 3 || got.Command[0] != "python" {
		t.Errorf("Command = %v, want [python -m http.server]", got.Command)
	}
	if len(got.ExposePorts) != 2 || got.ExposePorts[0] != 80 {
		t.Errorf("ExposePorts = %v, want [80 443]", got.ExposePorts)
	}
	if got.Handle != "web" {
		t.Errorf("Handle = %q, want %q", got.Handle, "web")
	}
	if got.ImageRef != "python:3.12" {
		t.Errorf("ImageRef = %q, want %q", got.ImageRef, "python:3.12")
	}
	if got.Workspace != "/home/user/myapp" {
		t.Errorf("Workspace = %q, want %q", got.Workspace, "/home/user/myapp")
	}
	if got.Env["API_KEY"] != "sk-123" {
		t.Errorf("Env[API_KEY] = %q, want %q", got.Env["API_KEY"], "sk-123")
	}
	if got.Env["DEBUG"] != "1" {
		t.Errorf("Env[DEBUG] = %q, want %q", got.Env["DEBUG"], "1")
	}
	if len(got.SecretKeys) != 1 || got.SecretKeys[0] != "API_KEY" {
		t.Errorf("SecretKeys = %v, want [API_KEY]", got.SecretKeys)
	}
}

func TestGetInstance_NotFound(t *testing.T) {
	db := openTestDB(t)

	got, err := db.GetInstance("nonexistent")
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Errorf("expected nil for nonexistent instance, got %+v", got)
	}
}

func TestGetInstanceByHandle(t *testing.T) {
	db := openTestDB(t)

	db.SaveInstance(&Instance{
		ID:      "inst-1",
		State:   "running",
		Command: []string{"echo"},
		Handle:  "alpha",
	})
	db.SaveInstance(&Instance{
		ID:      "inst-2",
		State:   "stopped",
		Command: []string{"echo"},
		Handle:  "beta",
	})

	got, err := db.GetInstanceByHandle("beta")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("expected instance for handle beta, got nil")
	}
	if got.ID != "inst-2" {
		t.Errorf("ID = %q, want %q", got.ID, "inst-2")
	}
}

func TestGetInstanceByHandle_NotFound(t *testing.T) {
	db := openTestDB(t)

	got, err := db.GetInstanceByHandle("nonexistent")
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Errorf("expected nil, got %+v", got)
	}
}

func TestListInstances(t *testing.T) {
	db := openTestDB(t)

	db.SaveInstance(&Instance{
		ID:        "inst-1",
		State:     "running",
		Command:   []string{"echo"},
		Handle:    "web",
		CreatedAt: time.Now().Add(-2 * time.Hour),
	})
	db.SaveInstance(&Instance{
		ID:        "inst-2",
		State:     "stopped",
		Command:   []string{"echo"},
		Handle:    "worker",
		CreatedAt: time.Now().Add(-1 * time.Hour),
	})

	list, err := db.ListInstances()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("expected 2 instances, got %d", len(list))
	}
	// Ordered by created_at DESC
	if list[0].ID != "inst-2" {
		t.Errorf("first instance ID = %q, want inst-2 (most recent)", list[0].ID)
	}
}

func TestUpdateState(t *testing.T) {
	db := openTestDB(t)

	db.SaveInstance(&Instance{
		ID:      "inst-1",
		State:   "stopped",
		Command: []string{"echo"},
	})

	if err := db.UpdateState("inst-1", "running"); err != nil {
		t.Fatal(err)
	}

	got, _ := db.GetInstance("inst-1")
	if got.State != "running" {
		t.Errorf("State = %q, want %q", got.State, "running")
	}
}

func TestUpdateState_NotFound(t *testing.T) {
	db := openTestDB(t)

	err := db.UpdateState("nonexistent", "running")
	if err == nil {
		t.Fatal("expected error for nonexistent instance")
	}
}

func TestDeleteInstance(t *testing.T) {
	db := openTestDB(t)

	db.SaveInstance(&Instance{
		ID:      "inst-1",
		State:   "stopped",
		Command: []string{"echo"},
	})

	if err := db.DeleteInstance("inst-1"); err != nil {
		t.Fatal(err)
	}

	got, _ := db.GetInstance("inst-1")
	if got != nil {
		t.Errorf("expected nil after delete, got %+v", got)
	}
}

func TestSaveInstance_Upsert(t *testing.T) {
	db := openTestDB(t)

	db.SaveInstance(&Instance{
		ID:      "inst-1",
		State:   "stopped",
		Command: []string{"echo"},
		Handle:  "web",
	})

	// Update state
	db.SaveInstance(&Instance{
		ID:      "inst-1",
		State:   "running",
		Command: []string{"echo"},
		Handle:  "web",
	})

	got, _ := db.GetInstance("inst-1")
	if got.State != "running" {
		t.Errorf("State after upsert = %q, want %q", got.State, "running")
	}
}

func TestSaveInstance_EmptyOptionalFields(t *testing.T) {
	db := openTestDB(t)

	db.SaveInstance(&Instance{
		ID:      "inst-1",
		State:   "stopped",
		Command: []string{"echo"},
	})

	got, _ := db.GetInstance("inst-1")
	if got.Workspace != "" {
		t.Errorf("Workspace = %q, want empty", got.Workspace)
	}
	if len(got.Env) != 0 {
		t.Errorf("Env = %v, want empty", got.Env)
	}
	if len(got.SecretKeys) != 0 {
		t.Errorf("SecretKeys = %v, want empty", got.SecretKeys)
	}
}
