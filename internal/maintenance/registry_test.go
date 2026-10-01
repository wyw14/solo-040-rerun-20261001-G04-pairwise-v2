package maintenance

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wyw14/cry-96/internal/journal"
	"github.com/wyw14/cry-96/internal/model"
)

type fixture struct {
	ctx      context.Context
	store    *journal.MemoryStore
	registry *Registry
	now      time.Time
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	clock := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	now := func() time.Time { return clock }
	store := journal.NewMemoryStore()
	recorder := journal.NewRecorder(store, now)
	return &fixture{
		ctx: context.Background(), store: store,
		registry: NewRegistry(recorder, now), now: clock,
	}
}

func eventTypes(t *testing.T, f *fixture, chamber string) []model.EventType {
	t.Helper()
	events, err := f.store.Events(f.ctx, chamber, 0)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	kinds := make([]model.EventType, 0, len(events))
	for _, event := range events {
		kinds = append(kinds, event.Type)
	}
	return kinds
}

func TestLockoutLifecycleAndEvents(t *testing.T) {
	f := newFixture(t)
	chamber := "chamber-1"

	lockout, err := f.registry.Register(f.ctx, chamber, []model.DeviceKind{model.DeviceUpstreamGate}, "seal replacement", "crew-lead")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if lockout.Status != model.LockoutPending {
		t.Fatalf("new lockout status = %s, want pending", lockout.Status)
	}

	// A second registration covering the same equipment must be refused.
	if _, err := f.registry.Register(f.ctx, chamber, []model.DeviceKind{model.DeviceUpstreamGate, model.DeviceFillValve}, "other work", "crew-2"); err == nil {
		t.Fatal("overlapping registration must be rejected")
	}

	approved, cancelled, err := f.registry.Approve(f.ctx, chamber, lockout.ID, "duty-engineer")
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if approved.Status != model.LockoutActive || approved.ApprovedBy != "duty-engineer" || approved.ApprovedAt == nil {
		t.Fatalf("approval not recorded: %+v", approved)
	}
	if len(cancelled) != 0 {
		t.Fatalf("no bound canceller yet, got %d cancelled", len(cancelled))
	}

	requested, err := f.registry.RequestRelease(f.ctx, chamber, lockout.ID, "work complete")
	if err != nil {
		t.Fatalf("request release: %v", err)
	}
	if requested.Status != model.LockoutReleasePending {
		t.Fatalf("status = %s, want release_pending", requested.Status)
	}

	released, err := f.registry.Release(f.ctx, chamber, lockout.ID, "duty-engineer", "checked")
	if err != nil {
		t.Fatalf("release: %v", err)
	}
	if released.Status != model.LockoutReleased || released.ReleasedAt == nil {
		t.Fatalf("release not recorded: %+v", released)
	}

	kinds := eventTypes(t, f, chamber)
	want := []model.EventType{
		model.EventLockoutRequested, model.EventLockoutApproved,
		model.EventLockoutReleaseRequest, model.EventLockoutReleased,
	}
	if strings.Join(toStrings(kinds), ",") != strings.Join(toStrings(want), ",") {
		t.Fatalf("event stream = %v, want %v", kinds, want)
	}
}

func toStrings(kinds []model.EventType) []string {
	out := make([]string, len(kinds))
	for i, kind := range kinds {
		out[i] = string(kind)
	}
	return out
}

func TestGuardBlocksWhileActiveAndRecordsDenial(t *testing.T) {
	f := newFixture(t)
	chamber := "chamber-1"
	issued := 0
	issue := func(context.Context) error { issued++; return nil }

	// Pending registration does not yet block: formal approval is the block.
	lockout, err := f.registry.Register(f.ctx, chamber, []model.DeviceKind{model.DeviceFillValve}, "valve overhaul", "crew")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := f.registry.Guard(f.ctx, chamber, model.DeviceFillValve, "set_opening", issue); err != nil {
		t.Fatalf("pending lockout must not block: %v", err)
	}
	if issued != 1 {
		t.Fatalf("issue count = %d, want 1", issued)
	}

	if _, _, err := f.registry.Approve(f.ctx, chamber, lockout.ID, "engineer"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	err = f.registry.Guard(f.ctx, chamber, model.DeviceFillValve, "set_opening", issue)
	var locked *LockedError
	if !errors.As(err, &locked) {
		t.Fatalf("active lockout error = %v, want *LockedError", err)
	}
	if locked.Lockout.ID != lockout.ID || locked.Reason() == "" {
		t.Fatalf("locked error missing identity/reason: %+v", locked)
	}
	if issued != 1 {
		t.Fatalf("command must not be issued while locked, count = %d", issued)
	}

	// An unrelated device is unaffected.
	if err := f.registry.Guard(f.ctx, chamber, model.DeviceDrainValve, "set_opening", issue); err != nil {
		t.Fatalf("unrelated device blocked: %v", err)
	}
	if issued != 2 {
		t.Fatalf("issue count = %d, want 2", issued)
	}

	events, _ := f.store.Events(f.ctx, chamber, 0)
	denials := 0
	for _, event := range events {
		if event.Type == model.EventLockoutDenied {
			var denial model.LockoutDenial
			if err := event.Decode(&denial); err != nil {
				t.Fatalf("decode denial: %v", err)
			}
			if denial.Reason != "valve overhaul" || denial.Device != model.DeviceFillValve {
				t.Fatalf("denial payload wrong: %+v", denial)
			}
			denials++
		}
	}
	if denials != 1 {
		t.Fatalf("denial events = %d, want 1", denials)
	}
}

func (e *LockedError) Reason() string { return e.Lockout.Reason }

func TestReleaseSafetyChecks(t *testing.T) {
	f := newFixture(t)
	chamber := "chamber-1"
	lockout, _ := f.registry.Register(f.ctx, chamber, model.AllDevices, "annual service", "crew")

	emergency := false
	f.registry.BindSafetyChecks(
		func(string) bool { return emergency },
		func(string, []model.DeviceKind) []model.DeviceCommand { return nil },
		nil,
	)
	if _, _, err := f.registry.Approve(f.ctx, chamber, lockout.ID, "engineer"); err != nil {
		t.Fatalf("approve: %v", err)
	}

	emergency = true
	if _, err := f.registry.Release(f.ctx, chamber, lockout.ID, "engineer", ""); err == nil {
		t.Fatal("release must be refused during emergency stop")
	}
	emergency = false

	// Pending actuator commands also prevent release.
	f.registry.BindSafetyChecks(
		func(string) bool { return false },
		func(string, []model.DeviceKind) []model.DeviceCommand {
			return []model.DeviceCommand{{ID: uuid.New()}}
		},
		nil,
	)
	if _, err := f.registry.Release(f.ctx, chamber, lockout.ID, "engineer", ""); err == nil {
		t.Fatal("release must be refused with pending actuator commands")
	}
}

func TestApproveCancelsQueuedCommandsForDevices(t *testing.T) {
	f := newFixture(t)
	chamber := "chamber-1"
	cancelled := []model.DeviceKind{}
	var mu sync.Mutex
	f.registry.BindSafetyChecks(nil, nil, func(_ string, devices []model.DeviceKind, _ time.Time) []model.DeviceCommand {
		mu.Lock()
		cancelled = append(cancelled, devices...)
		mu.Unlock()
		return []model.DeviceCommand{{ID: uuid.New()}}
	})
	lockout, _ := f.registry.Register(f.ctx, chamber, []model.DeviceKind{model.DeviceUpstreamGate}, "gate work", "crew")
	_, returned, err := f.registry.Approve(f.ctx, chamber, lockout.ID, "engineer")
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if len(cancelled) != 1 || cancelled[0] != model.DeviceUpstreamGate {
		t.Fatalf("canceller devices = %v, want [upstream_gate]", cancelled)
	}
	if len(returned) != 1 {
		t.Fatalf("cancelled commands returned = %d, want 1", len(returned))
	}
}

// TestGuardAtomicAgainstApprove proves an in-flight command issue holds the
// registry lock: an approval cannot land in the middle of the issue and create
// a brief false-clear window.
func TestGuardAtomicAgainstApprove(t *testing.T) {
	f := newFixture(t)
	chamber := "chamber-1"
	lockout, _ := f.registry.Register(f.ctx, chamber, []model.DeviceKind{model.DeviceUpstreamGate}, "gate work", "crew")

	insideIssue := make(chan struct{})
	proceed := make(chan struct{})
	issueDone := make(chan error, 1)
	go func() {
		issueDone <- f.registry.Guard(f.ctx, chamber, model.DeviceUpstreamGate, "open", func(context.Context) error {
			close(insideIssue)
			<-proceed
			return nil
		})
	}()
	<-insideIssue

	approveDone := make(chan error, 1)
	go func() {
		_, _, err := f.registry.Approve(f.ctx, chamber, lockout.ID, "engineer")
		approveDone <- err
	}()
	select {
	case err := <-approveDone:
		t.Fatalf("approve completed while issue still in flight: %v", err)
	case <-time.After(20 * time.Millisecond):
	}

	close(proceed)
	if err := <-issueDone; err != nil {
		t.Fatalf("in-flight issue: %v", err)
	}
	if err := <-approveDone; err != nil {
		t.Fatalf("approve after issue: %v", err)
	}

	// Every subsequent command must be refused.
	err := f.registry.Guard(f.ctx, chamber, model.DeviceUpstreamGate, "open", func(context.Context) error {
		t.Fatal("command issued after lockout approval")
		return nil
	})
	var locked *LockedError
	if !errors.As(err, &locked) {
		t.Fatalf("post-approval guard error = %v, want LockedError", err)
	}
}

// TestConcurrentReleaseAndCommands runs command attempts concurrently with a
// release: no command may be issued while the lockout is still active.
func TestConcurrentReleaseAndCommands(t *testing.T) {
	f := newFixture(t)
	chamber := "chamber-1"
	lockout, _ := f.registry.Register(f.ctx, chamber, []model.DeviceKind{model.DeviceFillValve}, "valve work", "crew")
	if _, _, err := f.registry.Approve(f.ctx, chamber, lockout.ID, "engineer"); err != nil {
		t.Fatalf("approve: %v", err)
	}

	released := make(chan struct{})
	const attempts = 200
	var wg sync.WaitGroup
	var mu sync.Mutex
	issuedWhileActive, issuedAfter := 0, 0
	start := make(chan struct{})
	wg.Add(attempts)
	for range attempts {
		go func() {
			defer wg.Done()
			<-start
			err := f.registry.Guard(f.ctx, chamber, model.DeviceFillValve, "set_opening", func(context.Context) error {
				mu.Lock()
				defer mu.Unlock()
				select {
				case <-released:
					issuedAfter++
				default:
					issuedWhileActive++
				}
				return nil
			})
			if err != nil {
				var locked *LockedError
				if !errors.As(err, &locked) {
					t.Errorf("unexpected guard error: %v", err)
				}
			}
		}()
	}
	close(start)
	if _, err := f.registry.Release(f.ctx, chamber, lockout.ID, "engineer", "all clear"); err != nil {
		t.Fatalf("release: %v", err)
	}
	close(released)
	wg.Wait()

	if issuedWhileActive != 0 {
		t.Fatalf("%d commands issued while lockout was active", issuedWhileActive)
	}
	if issuedAfter == 0 {
		t.Fatal("expected at least one command to be issued after release")
	}
}
