package interlock

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/wyw14/cry-96/internal/gate"
	"github.com/wyw14/cry-96/internal/model"
)

// CommandGuard refuses command issue while maintenance lockouts cover a device.
// Implemented by the maintenance registry; nil means no maintenance blocking.
type CommandGuard interface {
	Guard(ctx context.Context, chamberID string, device model.DeviceKind, action string, issue func(context.Context) error) error
}

// Coordinator keeps policy evaluation and command reservation in one flow.
type Coordinator struct {
	policy    Policy
	arbiter   *Arbiter
	gates     *gate.Controller
	guard     CommandGuard
	level     func(string) float64
	emergency func(string) bool
}

func NewCoordinator(policy Policy, arbiter *Arbiter, gates *gate.Controller, guard CommandGuard, level func(string) float64, emergency func(string) bool) *Coordinator {
	return &Coordinator{
		policy: policy, arbiter: arbiter, gates: gates, guard: guard,
		level: level, emergency: emergency,
	}
}

func gateDevice(direction model.Direction) model.DeviceKind {
	if direction == model.DirectionDownstream {
		return model.DeviceDownstreamGate
	}
	return model.DeviceUpstreamGate
}

func (c *Coordinator) runGuarded(ctx context.Context, chamberID string, device model.DeviceKind, action string, issue func(context.Context) error) error {
	if c.guard == nil {
		return issue(ctx)
	}
	return c.guard.Guard(ctx, chamberID, device, action, issue)
}

func (c *Coordinator) Snapshot(chamberID string) EquipmentSnapshot {
	level := 0.0
	if c.level != nil {
		level = c.level(chamberID)
	}
	emergency := false
	if c.emergency != nil {
		emergency = c.emergency(chamberID)
	}
	return EquipmentSnapshot{
		ChamberID: chamberID, Gates: c.gates.State().Snapshot(chamberID),
		LevelM: level, Emergency: emergency,
	}
}

// RequestOpen validates interlocks and issues the open command inside the
// maintenance guard, so an approving lockout can never race the issue.
func (c *Coordinator) RequestOpen(ctx context.Context, cycle model.TransitCycle, direction model.Direction, owner string) (model.DeviceCommand, error) {
	if cycle.ID == uuid.Nil {
		return model.DeviceCommand{}, errors.New("active transit not found")
	}
	var command model.DeviceCommand
	err := c.runGuarded(ctx, cycle.ChamberID, gateDevice(direction), "open", func(ctx context.Context) error {
		if err := c.policy.ValidateOpen(c.Snapshot(cycle.ChamberID), direction); err != nil {
			return err
		}
		_, reserveErr := c.arbiter.Reserve(cycle.ChamberID, direction, owner, func() error {
			issued, issueErr := c.gates.Request(ctx, cycle, direction, true)
			command = issued
			return issueErr
		})
		return reserveErr
	})
	return command, err
}

func (c *Coordinator) RequestClose(ctx context.Context, cycle model.TransitCycle, direction model.Direction) (model.DeviceCommand, error) {
	var command model.DeviceCommand
	err := c.runGuarded(ctx, cycle.ChamberID, gateDevice(direction), "close", func(ctx context.Context) error {
		if err := c.policy.ValidateClose(c.Snapshot(cycle.ChamberID), direction); err != nil {
			return err
		}
		issued, issueErr := c.gates.Request(ctx, cycle, direction, false)
		command = issued
		return issueErr
	})
	return command, err
}

func (c *Coordinator) Release(chamberID, owner string) error {
	if !c.arbiter.Release(chamberID, owner) {
		return fmt.Errorf("reservation owner %q does not control chamber %s", owner, chamberID)
	}
	return nil
}

func (c *Coordinator) Reservation(chamberID string) (Reservation, bool) {
	return c.arbiter.Current(chamberID)
}

func (c *Coordinator) Observe(chamberID string, direction model.Direction, position model.GatePosition, _ time.Time) error {
	return c.gates.Observe(chamberID, direction, position)
}
