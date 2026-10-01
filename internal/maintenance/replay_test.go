package maintenance

import (
	"context"
	"testing"
	"time"

	"github.com/wyw14/cry-96/internal/journal"
	"github.com/wyw14/cry-96/internal/model"
)

func TestRestoreFromStoreRebuildsState(t *testing.T) {
	ctx := context.Background()
	store := journal.NewMemoryStore()
	clock := func() time.Time { return time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC) }
	first := NewRegistry(journal.NewRecorder(store, clock), clock)
	chamber := "chamber-9"

	active, err := first.Register(ctx, chamber, []model.DeviceKind{model.DeviceFillValve}, "valve overhaul", "crew")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, _, err := first.Approve(ctx, chamber, active.ID, "engineer"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	released, err := first.Register(ctx, chamber, []model.DeviceKind{model.DeviceDrainValve}, "drain inspection", "crew")
	if err != nil {
		t.Fatalf("register second: %v", err)
	}
	if _, _, err := first.Approve(ctx, chamber, released.ID, "engineer"); err != nil {
		t.Fatalf("approve second: %v", err)
	}
	if _, err := first.RequestRelease(ctx, chamber, released.ID, "done"); err != nil {
		t.Fatalf("release request: %v", err)
	}
	if _, err := first.Release(ctx, chamber, released.ID, "engineer", "checked"); err != nil {
		t.Fatalf("release: %v", err)
	}

	// A fresh registry must rebuild exactly the post-restore blocking state.
	second := NewRegistry(journal.NewRecorder(store, clock), clock)
	if err := RestoreFromStore(ctx, second, store); err != nil {
		t.Fatalf("restore: %v", err)
	}

	restored := second.List(chamber)
	if len(restored) != 2 {
		t.Fatalf("restored lockouts = %d, want 2", len(restored))
	}

	if _, blocked := second.Covering(chamber, model.DeviceFillValve); !blocked {
		t.Fatal("fill valve must remain blocked after restore")
	}
	if _, blocked := second.Covering(chamber, model.DeviceDrainValve); blocked {
		t.Fatal("released drain valve must not block after restore")
	}

	// Lifetime enforcement continues after restore.
	if err := second.Guard(ctx, chamber, model.DeviceFillValve, "set_opening", func(context.Context) error {
		t.Fatal("guard must block restored lockout")
		return nil
	}); err == nil {
		t.Fatal("expected LockedError after restore")
	}
}
