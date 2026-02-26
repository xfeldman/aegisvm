package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/xfeldman/aegisvm/internal/cron"
)

const maxCronEntries = 20

// CronEntry is a single scheduled task.
type CronEntry struct {
	ID         string `json:"id"`
	Schedule   string `json:"schedule"`
	Message    string `json:"message"`
	Session    string `json:"session"`
	OnConflict string `json:"on_conflict,omitempty"`
	Enabled    bool   `json:"enabled"`
	CreatedAt  string `json:"created_at"`
}

// CronFile is the on-disk format of /workspace/.aegis/cron.json.
type CronFile struct {
	Entries []CronEntry `json:"entries"`
	NextID  int         `json:"next_id"`
}

// CronStore manages cron entries backed by a JSON file.
type CronStore struct {
	mu   sync.Mutex
	path string
}

// NewCronStore creates a cron store pointing at dir/cron.json.
func NewCronStore(dir string) *CronStore {
	return &CronStore{path: filepath.Join(dir, "cron.json")}
}

// Load reads the cron file. Returns an empty CronFile if the file doesn't exist.
func (cs *CronStore) Load() (*CronFile, error) {
	data, err := os.ReadFile(cs.path)
	if err != nil {
		if os.IsNotExist(err) {
			return &CronFile{}, nil
		}
		return nil, err
	}
	var cf CronFile
	if err := json.Unmarshal(data, &cf); err != nil {
		return nil, fmt.Errorf("parse cron.json: %w", err)
	}
	return &cf, nil
}

// Save writes the cron file atomically (tmp + rename).
func (cs *CronStore) Save(cf *CronFile) error {
	os.MkdirAll(filepath.Dir(cs.path), 0755)
	tmp := cs.path + ".tmp"
	data, _ := json.MarshalIndent(cf, "", "  ")
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, cs.path)
}

// Create adds a new cron entry. Validates the schedule expression.
func (cs *CronStore) Create(schedule, message, session, onConflict string) (string, error) {
	if schedule == "" {
		return "", fmt.Errorf("schedule is required")
	}
	if message == "" {
		return "", fmt.Errorf("message is required")
	}
	if len(message) > 1000 {
		return "", fmt.Errorf("message exceeds 1000 character limit")
	}

	// Validate cron expression
	if _, err := cron.Parse(schedule); err != nil {
		return "", fmt.Errorf("invalid schedule: %w", err)
	}

	if onConflict == "" {
		onConflict = "skip"
	}
	if onConflict != "skip" && onConflict != "queue" {
		return "", fmt.Errorf("on_conflict must be 'skip' or 'queue'")
	}

	cs.mu.Lock()
	defer cs.mu.Unlock()

	cf, err := cs.Load()
	if err != nil {
		return "", err
	}

	if len(cf.Entries) >= maxCronEntries {
		return "", fmt.Errorf("maximum %d cron entries reached", maxCronEntries)
	}

	id := fmt.Sprintf("cron-%d", cf.NextID)
	cf.NextID++

	if session == "" {
		session = id
	}

	cf.Entries = append(cf.Entries, CronEntry{
		ID:         id,
		Schedule:   schedule,
		Message:    message,
		Session:    session,
		OnConflict: onConflict,
		Enabled:    true,
		CreatedAt:  time.Now().UTC().Format(time.RFC3339),
	})

	if err := cs.Save(cf); err != nil {
		return "", err
	}
	return id, nil
}

// Delete removes a cron entry by ID.
func (cs *CronStore) Delete(id string) error {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	cf, err := cs.Load()
	if err != nil {
		return err
	}

	found := false
	var remaining []CronEntry
	for _, e := range cf.Entries {
		if e.ID == id {
			found = true
			continue
		}
		remaining = append(remaining, e)
	}
	if !found {
		return fmt.Errorf("cron entry %s not found", id)
	}

	cf.Entries = remaining
	return cs.Save(cf)
}

// SetEnabled toggles the enabled state of a cron entry.
func (cs *CronStore) SetEnabled(id string, enabled bool) error {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	cf, err := cs.Load()
	if err != nil {
		return err
	}

	for i := range cf.Entries {
		if cf.Entries[i].ID == id {
			cf.Entries[i].Enabled = enabled
			return cs.Save(cf)
		}
	}
	return fmt.Errorf("cron entry %s not found", id)
}

// List returns all cron entries.
func (cs *CronStore) List() ([]CronEntry, error) {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	cf, err := cs.Load()
	if err != nil {
		return nil, err
	}
	return cf.Entries, nil
}
