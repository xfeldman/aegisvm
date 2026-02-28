package registry

import (
	"encoding/json"
	"testing"

	"github.com/xfeldman/aegisvm/internal/tether"
)

func TestSaveTetherFrame(t *testing.T) {
	db := openTestDB(t)

	frame := tether.Frame{V: 1, Type: "user.message", Seq: 1, Session: tether.SessionID{Channel: "host", ID: "default"}}
	data, _ := json.Marshal(frame)

	if err := db.SaveTetherFrame("inst-1", 1, data); err != nil {
		t.Fatal(err)
	}

	// Verify it's there
	frames, err := db.LoadAllTetherFrames(100)
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 1 {
		t.Fatalf("expected 1 instance, got %d", len(frames))
	}
	if len(frames["inst-1"]) != 1 {
		t.Fatalf("expected 1 frame, got %d", len(frames["inst-1"]))
	}
	if frames["inst-1"][0].Type != "user.message" {
		t.Errorf("type = %q, want %q", frames["inst-1"][0].Type, "user.message")
	}
	if frames["inst-1"][0].Seq != 1 {
		t.Errorf("seq = %d, want 1", frames["inst-1"][0].Seq)
	}
}

func TestSaveTetherFrame_Upsert(t *testing.T) {
	db := openTestDB(t)

	f1 := tether.Frame{V: 1, Type: "user.message", Seq: 1}
	d1, _ := json.Marshal(f1)
	db.SaveTetherFrame("inst-1", 1, d1)

	// Overwrite same (instance_id, seq) with different type
	f2 := tether.Frame{V: 1, Type: "assistant.done", Seq: 1}
	d2, _ := json.Marshal(f2)
	if err := db.SaveTetherFrame("inst-1", 1, d2); err != nil {
		t.Fatal(err)
	}

	frames, _ := db.LoadAllTetherFrames(100)
	if len(frames["inst-1"]) != 1 {
		t.Fatalf("expected 1 frame after upsert, got %d", len(frames["inst-1"]))
	}
	if frames["inst-1"][0].Type != "assistant.done" {
		t.Errorf("type = %q, want %q after upsert", frames["inst-1"][0].Type, "assistant.done")
	}
}

func TestLoadAllTetherFrames_MultipleInstances(t *testing.T) {
	db := openTestDB(t)

	for i := int64(1); i <= 3; i++ {
		f := tether.Frame{V: 1, Type: "user.message", Seq: i}
		data, _ := json.Marshal(f)
		db.SaveTetherFrame("inst-a", i, data)
	}
	for i := int64(1); i <= 2; i++ {
		f := tether.Frame{V: 1, Type: "assistant.done", Seq: i}
		data, _ := json.Marshal(f)
		db.SaveTetherFrame("inst-b", i, data)
	}

	frames, err := db.LoadAllTetherFrames(100)
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 2 {
		t.Fatalf("expected 2 instances, got %d", len(frames))
	}
	if len(frames["inst-a"]) != 3 {
		t.Errorf("inst-a: expected 3 frames, got %d", len(frames["inst-a"]))
	}
	if len(frames["inst-b"]) != 2 {
		t.Errorf("inst-b: expected 2 frames, got %d", len(frames["inst-b"]))
	}
}

func TestLoadAllTetherFrames_LimitPerInstance(t *testing.T) {
	db := openTestDB(t)

	for i := int64(1); i <= 10; i++ {
		f := tether.Frame{V: 1, Type: "user.message", Seq: i}
		data, _ := json.Marshal(f)
		db.SaveTetherFrame("inst-1", i, data)
	}

	frames, err := db.LoadAllTetherFrames(3)
	if err != nil {
		t.Fatal(err)
	}
	if len(frames["inst-1"]) != 3 {
		t.Fatalf("expected 3 frames (limit), got %d", len(frames["inst-1"]))
	}
	// Should keep the last 3 (seq 8, 9, 10)
	if frames["inst-1"][0].Seq != 8 {
		t.Errorf("first frame seq = %d, want 8", frames["inst-1"][0].Seq)
	}
	if frames["inst-1"][2].Seq != 10 {
		t.Errorf("last frame seq = %d, want 10", frames["inst-1"][2].Seq)
	}
}

func TestLoadAllTetherFrames_OrderedBySeq(t *testing.T) {
	db := openTestDB(t)

	// Insert out of order
	for _, seq := range []int64{3, 1, 2} {
		f := tether.Frame{V: 1, Type: "user.message", Seq: seq}
		data, _ := json.Marshal(f)
		db.SaveTetherFrame("inst-1", seq, data)
	}

	frames, err := db.LoadAllTetherFrames(100)
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i < len(frames["inst-1"]); i++ {
		if frames["inst-1"][i].Seq <= frames["inst-1"][i-1].Seq {
			t.Errorf("frames not ordered: seq[%d]=%d <= seq[%d]=%d",
				i, frames["inst-1"][i].Seq, i-1, frames["inst-1"][i-1].Seq)
		}
	}
}

func TestLoadAllTetherFrames_SkipsCorruptFrames(t *testing.T) {
	db := openTestDB(t)

	// Insert a valid frame
	f := tether.Frame{V: 1, Type: "user.message", Seq: 1}
	data, _ := json.Marshal(f)
	db.SaveTetherFrame("inst-1", 1, data)

	// Insert corrupt JSON directly
	db.db.Exec(`INSERT INTO tether_frames (instance_id, seq, frame) VALUES (?, ?, ?)`,
		"inst-1", 2, "not-json{{{")

	// Insert another valid frame
	f2 := tether.Frame{V: 1, Type: "assistant.done", Seq: 3}
	data2, _ := json.Marshal(f2)
	db.SaveTetherFrame("inst-1", 3, data2)

	frames, err := db.LoadAllTetherFrames(100)
	if err != nil {
		t.Fatal(err)
	}
	// Should have 2 valid frames, corrupt one skipped
	if len(frames["inst-1"]) != 2 {
		t.Fatalf("expected 2 frames (corrupt skipped), got %d", len(frames["inst-1"]))
	}
}

func TestDeleteTetherFrames(t *testing.T) {
	db := openTestDB(t)

	for i := int64(1); i <= 3; i++ {
		f := tether.Frame{V: 1, Type: "user.message", Seq: i}
		data, _ := json.Marshal(f)
		db.SaveTetherFrame("inst-1", i, data)
	}
	// Another instance — should not be affected
	f := tether.Frame{V: 1, Type: "user.message", Seq: 1}
	data, _ := json.Marshal(f)
	db.SaveTetherFrame("inst-2", 1, data)

	if err := db.DeleteTetherFrames("inst-1"); err != nil {
		t.Fatal(err)
	}

	frames, err := db.LoadAllTetherFrames(100)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := frames["inst-1"]; ok {
		t.Error("inst-1 frames should be deleted")
	}
	if len(frames["inst-2"]) != 1 {
		t.Errorf("inst-2 should still have 1 frame, got %d", len(frames["inst-2"]))
	}
}

func TestDeleteTetherFrames_NonExistent(t *testing.T) {
	db := openTestDB(t)

	// Should not error on non-existent instance
	if err := db.DeleteTetherFrames("no-such-instance"); err != nil {
		t.Fatal(err)
	}
}

func TestLoadAllTetherFrames_Empty(t *testing.T) {
	db := openTestDB(t)

	frames, err := db.LoadAllTetherFrames(100)
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 0 {
		t.Errorf("expected empty map, got %d entries", len(frames))
	}
}
