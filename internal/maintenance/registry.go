// Package maintenance implements the formal register/approve/release lockout
// mechanism used to secure chamber equipment during field maintenance.
package maintenance

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/wyw14/cry-96/internal/journal"
	"github.com/wyw14/cry-96/internal/model"
)

// ErrNotFound is returned when a lockout id is unknown in a chamber.
var ErrNotFound = errors.New("lockout not found")

// LockedError reports that a command was refused because a lockout covers it.
type LockedError struct {
	Lockout model.MaintenanceLockout
	Device  model.DeviceKind
	Action  string
}

func (e *LockedError) Error() string {
	return fmt.Sprintf("device %s locked for maintenance: %s (lockout %s)", e.Device, e.Lockout.Reason, e.Lockout.ID)
}

// IssueFunc issues the blocked command exactly once, only while no lockout is held.
type IssueFunc = func(context.Context) error

// Registry owns lockout lifecycle and serializes it against command issuing.
//
// A single mutex covers both lockout state transitions and Guard's check-then-
// issue critical section. Concurrent register/approve/release requests can
// therefore never overlap a command issue in a way that briefly clears the
// block: a command is either refused under the held lock or issued while the
// lockout was durably absent.
type Registry struct {
	mu       sync.Mutex
	records  map[string][]model.MaintenanceLockout
	recorder *journal.Recorder
	now      func() time.Time

	// Injected to avoid a package cycle with gate/safety.
	emergencyActive   func(chamberID string) bool
	pendingForDevices func(chamberID string, devices []model.DeviceKind) []model.DeviceCommand
	cancelDevices     func(chamberID string, devices []model.DeviceKind, now time.Time) []model.DeviceCommand
}

func NewRegistry(recorder *journal.Recorder, now func() time.Time) *Registry {
	if now == nil {
		now = time.Now
	}
	return &Registry{records: make(map[string][]model.MaintenanceLockout), recorder: recorder, now: now}
}

// BindSafetyChecks supplies the release-time safety predicates.
func (r *Registry) BindSafetyChecks(
	emergencyActive func(chamberID string) bool,
	pendingForDevices func(chamberID string, devices []model.DeviceKind) []model.DeviceCommand,
	cancelDevices func(chamberID string, devices []model.DeviceKind, now time.Time) []model.DeviceCommand,
) {
	r.mu.Lock()
	r.emergencyActive = emergencyActive
	r.pendingForDevices = pendingForDevices
	r.cancelDevices = cancelDevices
	r.mu.Unlock()
}

// liveEntry returns the index of an unterminated lockout overlapping devices.
func liveIndex(entries []model.MaintenanceLockout, devices []model.DeviceKind) int {
	for i := range entries {
		candidate := &entries[i]
		if candidate.Status.Terminal() {
			continue
		}
		for _, wanted := range devices {
			if candidate.Covers(wanted) || candidate.Status == model.LockoutPending && containsDevice(candidate.Devices, wanted) {
				return i
			}
		}
	}
	return -1
}

func containsDevice(devices []model.DeviceKind, wanted model.DeviceKind) bool {
	for _, device := range devices {
		if device == wanted {
			return true
		}
	}
	return false
}

func (r *Registry) findLocked(chamberID string, lockoutID uuid.UUID) (int, error) {
	for i := range r.records[chamberID] {
		if r.records[chamberID][i].ID == lockoutID {
			return i, nil
		}
	}
	return -1, fmt.Errorf("%w: %s in chamber %s", ErrNotFound, lockoutID, chamberID)
}

// Find locates a lockout in any chamber.
func (r *Registry) Find(lockoutID uuid.UUID) (model.MaintenanceLockout, string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for chamberID, entries := range r.records {
		for i := range entries {
			if entries[i].ID == lockoutID {
				return entries[i].Clone(), chamberID, true
			}
		}
	}
	return model.MaintenanceLockout{}, "", false
}

// Register records a pending lockout request awaiting formal approval.
func (r *Registry) Register(ctx context.Context, chamberID string, devices []model.DeviceKind, reason, registeredBy string) (model.MaintenanceLockout, error) {
	normalized, err := model.NormalizeDevices(devices)
	if err != nil {
		return model.MaintenanceLockout{}, err
	}
	lockout, err := model.NewMaintenanceLockout(chamberID, normalized, reason, registeredBy, r.now())
	if err != nil {
		return model.MaintenanceLockout{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if existing := liveIndex(r.records[chamberID], normalized); existing >= 0 {
		return model.MaintenanceLockout{}, fmt.Errorf("lockout %s already covers requested equipment", r.records[chamberID][existing].ID)
	}
	if _, err := r.recorder.Record(ctx, chamberID, model.EventLockoutRequested, uuid.Nil, 0, lockout); err != nil {
		return model.MaintenanceLockout{}, err
	}
	r.records[chamberID] = append(r.records[chamberID], lockout)
	return lockout.Clone(), nil
}

// Approve activates a registered lockout and cancels queued commands for its devices.
func (r *Registry) Approve(ctx context.Context, chamberID string, lockoutID uuid.UUID, approvedBy string) (model.MaintenanceLockout, []model.DeviceCommand, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	index, err := r.findLocked(chamberID, lockoutID)
	if err != nil {
		return model.MaintenanceLockout{}, nil, err
	}
	lockout := r.records[chamberID][index].Clone()
	if err := lockout.Approve(approvedBy, r.now()); err != nil {
		return model.MaintenanceLockout{}, nil, err
	}
	if _, err := r.recorder.Record(ctx, chamberID, model.EventLockoutApproved, uuid.Nil, 0, lockout); err != nil {
		return model.MaintenanceLockout{}, nil, err
	}
	r.records[chamberID][index] = lockout
	var cancelled []model.DeviceCommand
	if r.cancelDevices != nil {
		cancelled = r.cancelDevices(chamberID, lockout.Devices, r.now())
	}
	return lockout.Clone(), cancelled, nil
}

// Reject terminates a never-approved registration.
func (r *Registry) Reject(ctx context.Context, chamberID string, lockoutID uuid.UUID, rejectedBy, note string) (model.MaintenanceLockout, error) {
	return r.transition(ctx, chamberID, lockoutID, model.EventLockoutRejected, func(lockout *model.MaintenanceLockout) error {
		return lockout.Reject(rejectedBy, note, r.now())
	})
}

// RequestRelease marks an active lockout as awaiting the releasing authority.
func (r *Registry) RequestRelease(ctx context.Context, chamberID string, lockoutID uuid.UUID, note string) (model.MaintenanceLockout, error) {
	return r.transition(ctx, chamberID, lockoutID, model.EventLockoutReleaseRequest, func(lockout *model.MaintenanceLockout) error {
		return lockout.RequestRelease(note, r.now())
	})
}

func (r *Registry) transition(ctx context.Context, chamberID string, lockoutID uuid.UUID, kind model.EventType, apply func(*model.MaintenanceLockout) error) (model.MaintenanceLockout, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	index, err := r.findLocked(chamberID, lockoutID)
	if err != nil {
		return model.MaintenanceLockout{}, err
	}
	lockout := r.records[chamberID][index].Clone()
	if err := apply(&lockout); err != nil {
		return model.MaintenanceLockout{}, err
	}
	if _, err := r.recorder.Record(ctx, chamberID, kind, uuid.Nil, 0, lockout); err != nil {
		return model.MaintenanceLockout{}, err
	}
	r.records[chamberID][index] = lockout
	return lockout.Clone(), nil
}

// Release clears a lockout after verifying emergency and actuator safety state.
// Clearing the lockout does not bypass the regular water-level, opposite-gate
// or emergency interlocks: subsequent commands are still validated as before.
func (r *Registry) Release(ctx context.Context, chamberID string, lockoutID uuid.UUID, releasedBy, note string) (model.MaintenanceLockout, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	index, err := r.findLocked(chamberID, lockoutID)
	if err != nil {
		return model.MaintenanceLockout{}, err
	}
	current := r.records[chamberID][index].Clone()
	if r.emergencyActive != nil && r.emergencyActive(chamberID) {
		return model.MaintenanceLockout{}, errors.New("cannot release lockout while emergency stop is active")
	}
	if r.pendingForDevices != nil {
		pending := r.pendingForDevices(chamberID, current.Devices)
		if len(pending) > 0 {
			return model.MaintenanceLockout{}, fmt.Errorf("cannot release lockout while %d actuator command(s) are pending", len(pending))
		}
	}
	if err := current.Release(releasedBy, note, r.now()); err != nil {
		return model.MaintenanceLockout{}, err
	}
	if _, err := r.recorder.Record(ctx, chamberID, model.EventLockoutReleased, uuid.Nil, 0, current); err != nil {
		return model.MaintenanceLockout{}, err
	}
	r.records[chamberID][index] = current
	return current.Clone(), nil
}

// Guard refuses issue while any lockout covers the device, recording the reason
// in the event stream; otherwise it runs issue atomically under the registry lock.
func (r *Registry) Guard(ctx context.Context, chamberID string, device model.DeviceKind, action string, issue IssueFunc) error {
	if issue == nil {
		return errors.New("lockout guard requires an issue function")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if lockout, blocked := r.coveringLocked(chamberID, device); blocked {
		denial := model.LockoutDenial{
			LockoutID: lockout.ID, ChamberID: chamberID, Device: device,
			Action: action, Reason: lockout.Reason, DeniedAt: r.now().UTC(),
		}
		if _, err := r.recorder.Record(ctx, chamberID, model.EventLockoutDenied, uuid.Nil, 0, denial); err != nil {
			return err
		}
		return &LockedError{Lockout: lockout.Clone(), Device: device, Action: action}
	}
	return issue(ctx)
}

// coveringLocked reports the blocking lockout; caller must hold r.mu.
func (r *Registry) coveringLocked(chamberID string, device model.DeviceKind) (model.MaintenanceLockout, bool) {
	for i := range r.records[chamberID] {
		if r.records[chamberID][i].Covers(device) {
			return r.records[chamberID][i], true
		}
	}
	return model.MaintenanceLockout{}, false
}

// Covering returns the active lockout that blocks device, if any.
func (r *Registry) Covering(chamberID string, device model.DeviceKind) (model.MaintenanceLockout, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	lockout, ok := r.coveringLocked(chamberID, device)
	return lockout.Clone(), ok
}

// Active returns unterminated lockouts for a chamber.
func (r *Registry) Active(chamberID string) []model.MaintenanceLockout {
	r.mu.Lock()
	defer r.mu.Unlock()
	var result []model.MaintenanceLockout
	for _, lockout := range r.records[chamberID] {
		if !lockout.Status.Terminal() {
			result = append(result, lockout.Clone())
		}
	}
	return result
}

// List returns all lockouts known for a chamber, including terminated history.
func (r *Registry) List(chamberID string) []model.MaintenanceLockout {
	r.mu.Lock()
	defer r.mu.Unlock()
	result := append([]model.MaintenanceLockout(nil), r.records[chamberID]...)
	for i := range result {
		result[i] = result[i].Clone()
	}
	return result
}

// Restore rebuilds lockout state from replayed events; used at startup only.
func (r *Registry) Restore(lockout model.MaintenanceLockout) {
	r.mu.Lock()
	defer r.mu.Unlock()
	chamberID := lockout.ChamberID
	for i := range r.records[chamberID] {
		if r.records[chamberID][i].ID == lockout.ID {
			r.records[chamberID][i] = lockout.Clone()
			return
		}
	}
	r.records[chamberID] = append(r.records[chamberID], lockout.Clone())
}
