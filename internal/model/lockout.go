package model

import (
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// LockoutStatus is the formal lifecycle state of a maintenance lockout.
type LockoutStatus string

const (
	LockoutPending        LockoutStatus = "pending"         // registered, awaiting approval
	LockoutActive         LockoutStatus = "active"          // approved, commands blocked
	LockoutReleasePending LockoutStatus = "release_pending" // field requests release, safety checks pending
	LockoutReleased       LockoutStatus = "released"        // terminal
	LockoutRejected       LockoutStatus = "rejected"        // terminal
)

func (s LockoutStatus) Terminal() bool { return s == LockoutReleased || s == LockoutRejected }

// AllDevices is the canonical set of controllable chamber equipment.
var AllDevices = []DeviceKind{
	DeviceUpstreamGate, DeviceDownstreamGate, DeviceFillValve, DeviceDrainValve,
}

// MaintenanceLockout formally blocks chamber equipment while it is serviced.
type MaintenanceLockout struct {
	ID           uuid.UUID     `json:"id"`
	ChamberID    string        `json:"chamber_id"`
	Devices      []DeviceKind  `json:"devices"`
	Status       LockoutStatus `json:"status"`
	Reason       string        `json:"reason"`
	RegisteredBy string        `json:"registered_by"`
	ApprovedBy   string        `json:"approved_by,omitempty"`
	ReleasedBy   string        `json:"released_by,omitempty"`
	ReleaseNote  string        `json:"release_note,omitempty"`
	RejectedBy   string        `json:"rejected_by,omitempty"`
	RejectNote   string        `json:"reject_note,omitempty"`
	RegisteredAt time.Time     `json:"registered_at"`
	ApprovedAt   *time.Time    `json:"approved_at,omitempty"`
	ReleasedAt   *time.Time    `json:"released_at,omitempty"`
	UpdatedAt    time.Time     `json:"updated_at"`
}

// NewMaintenanceLockout registers a pending lockout for named devices.
func NewMaintenanceLockout(chamberID string, devices []DeviceKind, reason, registeredBy string, now time.Time) (MaintenanceLockout, error) {
	if chamberID == "" {
		return MaintenanceLockout{}, errors.New("lockout chamber is required")
	}
	normalized, err := NormalizeDevices(devices)
	if err != nil {
		return MaintenanceLockout{}, err
	}
	if reason == "" {
		return MaintenanceLockout{}, errors.New("lockout reason is required")
	}
	if registeredBy == "" {
		return MaintenanceLockout{}, errors.New("lockout registrant is required")
	}
	stamp := now.UTC()
	return MaintenanceLockout{
		ID:           uuid.New(),
		ChamberID:    chamberID,
		Devices:      normalized,
		Status:       LockoutPending,
		Reason:       reason,
		RegisteredBy: registeredBy,
		RegisteredAt: stamp,
		UpdatedAt:    stamp,
	}, nil
}

// NormalizeDevices validates, de-duplicates and orders the device set.
func NormalizeDevices(devices []DeviceKind) ([]DeviceKind, error) {
	if len(devices) == 0 {
		devices = AllDevices
	}
	seen := make(map[DeviceKind]struct{}, len(devices))
	result := make([]DeviceKind, 0, len(devices))
	for _, wanted := range devices {
		if !ValidDevice(wanted) {
			return nil, fmt.Errorf("unknown lockout device %q", wanted)
		}
		if _, ok := seen[wanted]; ok {
			continue
		}
		seen[wanted] = struct{}{}
		result = append(result, wanted)
	}
	return result, nil
}

// ValidDevice reports whether kind is controllable chamber equipment.
func ValidDevice(kind DeviceKind) bool {
	for _, known := range AllDevices {
		if kind == known {
			return true
		}
	}
	return false
}

// Covers reports whether the lockout currently blocks the given device.
func (l MaintenanceLockout) Covers(device DeviceKind) bool {
	if l.Status != LockoutActive && l.Status != LockoutReleasePending {
		return false
	}
	for _, blocked := range l.Devices {
		if blocked == device {
			return true
		}
	}
	return false
}

func (l *MaintenanceLockout) touch(now time.Time) { l.UpdatedAt = now.UTC() }

// Approve moves a registered lockout into the active blocking state.
func (l *MaintenanceLockout) Approve(approvedBy string, now time.Time) error {
	if l.Status != LockoutPending {
		return fmt.Errorf("lockout %s is %s, not awaiting approval", l.ID, l.Status)
	}
	if approvedBy == "" {
		return errors.New("approver is required")
	}
	l.Status = LockoutActive
	l.ApprovedBy = approvedBy
	stamp := now.UTC()
	l.ApprovedAt = &stamp
	l.touch(now)
	return nil
}

// Reject terminates a lockout that was registered but never approved.
func (l *MaintenanceLockout) Reject(rejectedBy, note string, now time.Time) error {
	if l.Status != LockoutPending {
		return fmt.Errorf("lockout %s is %s and cannot be rejected", l.ID, l.Status)
	}
	if rejectedBy == "" {
		return errors.New("rejecting approver is required")
	}
	l.Status = LockoutRejected
	l.RejectedBy = rejectedBy
	l.RejectNote = note
	l.touch(now)
	return nil
}

// RequestRelease records that maintenance finished and the lockout awaits release.
func (l *MaintenanceLockout) RequestRelease(note string, now time.Time) error {
	if l.Status != LockoutActive {
		return fmt.Errorf("lockout %s is %s, not active", l.ID, l.Status)
	}
	l.Status = LockoutReleasePending
	l.ReleaseNote = note
	l.touch(now)
	return nil
}

// Release formally clears the lockout after all safety checks pass.
func (l *MaintenanceLockout) Release(releasedBy, note string, now time.Time) error {
	if l.Status != LockoutActive && l.Status != LockoutReleasePending {
		return fmt.Errorf("lockout %s is %s and cannot be released", l.ID, l.Status)
	}
	if releasedBy == "" {
		return errors.New("releasing approver is required")
	}
	l.Status = LockoutReleased
	l.ReleasedBy = releasedBy
	if note != "" {
		l.ReleaseNote = note
	}
	stamp := now.UTC()
	l.ReleasedAt = &stamp
	l.touch(now)
	return nil
}

// Clone returns a copy with an independent device slice.
func (l MaintenanceLockout) Clone() MaintenanceLockout {
	l.Devices = append([]DeviceKind(nil), l.Devices...)
	return l
}

// LockoutDenial is the payload recorded when a command is rejected by a lockout.
type LockoutDenial struct {
	LockoutID uuid.UUID  `json:"lockout_id"`
	ChamberID string     `json:"chamber_id"`
	Device    DeviceKind `json:"device"`
	Action    string     `json:"action"`
	Reason    string     `json:"reason"`
	DeniedAt  time.Time  `json:"denied_at"`
}
