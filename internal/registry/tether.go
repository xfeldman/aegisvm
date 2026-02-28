package registry

import (
	"encoding/json"

	"github.com/xfeldman/aegisvm/internal/tether"
)

// SaveTetherFrame persists a single tether frame to the database.
func (d *DB) SaveTetherFrame(instanceID string, seq int64, frameJSON []byte) error {
	_, err := d.db.Exec(
		`INSERT OR REPLACE INTO tether_frames (instance_id, seq, frame) VALUES (?, ?, ?)`,
		instanceID, seq, string(frameJSON),
	)
	return err
}

// LoadAllTetherFrames loads persisted tether frames grouped by instance ID.
// Returns the last limitPerInstance frames per instance (matching ring buffer size).
func (d *DB) LoadAllTetherFrames(limitPerInstance int) (map[string][]tether.Frame, error) {
	rows, err := d.db.Query(
		`SELECT instance_id, frame FROM tether_frames ORDER BY instance_id, seq`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	all := make(map[string][]tether.Frame)
	for rows.Next() {
		var instanceID, frameJSON string
		if err := rows.Scan(&instanceID, &frameJSON); err != nil {
			return nil, err
		}
		var f tether.Frame
		if err := json.Unmarshal([]byte(frameJSON), &f); err != nil {
			continue // skip corrupt frames
		}
		all[instanceID] = append(all[instanceID], f)
	}

	// Trim to last N per instance
	for id, frames := range all {
		if len(frames) > limitPerInstance {
			all[id] = frames[len(frames)-limitPerInstance:]
		}
	}

	return all, rows.Err()
}

// DeleteTetherFrames removes all persisted tether frames for an instance.
func (d *DB) DeleteTetherFrames(instanceID string) error {
	_, err := d.db.Exec(
		`DELETE FROM tether_frames WHERE instance_id = ?`,
		instanceID,
	)
	return err
}
