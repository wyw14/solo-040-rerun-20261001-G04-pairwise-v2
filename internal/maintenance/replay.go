package maintenance

import (
	"context"
	"fmt"

	"github.com/wyw14/cry-96/internal/journal"
	"github.com/wyw14/cry-96/internal/model"
)

// RestoreFromStore rebuilds lockout state from the durable event stream.
func RestoreFromStore(ctx context.Context, registry *Registry, store journal.Store) error {
	chambers, err := store.Chambers(ctx)
	if err != nil {
		return err
	}
	for _, chamberID := range chambers {
		events, err := store.Events(ctx, chamberID, 0)
		if err != nil {
			return err
		}
		for _, event := range events {
			if event.ChamberID != chamberID {
				return fmt.Errorf("lockout replay partition mismatch chamber=%s event=%s", chamberID, event.ChamberID)
			}
			if !isLockoutEvent(event.Type) {
				continue
			}
			var lockout model.MaintenanceLockout
			if err := event.Decode(&lockout); err != nil {
				return err
			}
			if lockout.ChamberID != chamberID {
				return fmt.Errorf("lockout partition mismatch chamber=%s lockout=%s", chamberID, lockout.ChamberID)
			}
			registry.Restore(lockout)
		}
	}
	return nil
}

func isLockoutEvent(kind model.EventType) bool {
	switch kind {
	case model.EventLockoutRequested, model.EventLockoutApproved,
		model.EventLockoutReleaseRequest, model.EventLockoutReleased, model.EventLockoutRejected:
		return true
	default:
		return false
	}
}
