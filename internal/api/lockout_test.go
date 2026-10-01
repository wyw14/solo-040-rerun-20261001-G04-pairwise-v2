package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newTestServer(t *testing.T) (*httptest.Server, *Runtime) {
	t.Helper()
	runtime, err := NewRuntime(context.Background(), "")
	if err != nil {
		t.Fatalf("runtime: %v", err)
	}
	server := httptest.NewServer(Router(runtime, t.TempDir()))
	t.Cleanup(server.Close)
	return server, runtime
}

func postJSON(t *testing.T, url string, body any) (int, map[string]any) {
	t.Helper()
	data, _ := json.Marshal(body)
	resp, err := http.Post(url, "application/json", bytes.NewReader(data))
	if err != nil {
		t.Fatalf("post %s: %v", url, err)
	}
	defer resp.Body.Close()
	var decoded map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&decoded)
	return resp.StatusCode, decoded
}

func startCycle(t *testing.T, server *httptest.Server, chamber string) {
	t.Helper()
	status, body := postJSON(t, server.URL+"/api/plans", map[string]any{
		"chamber_id": chamber,
		"direction":  "downstream",
		"vessels":    []map[string]any{{"name": "MV Tern", "call_sign": "V-TN1", "length_m": 80}},
	})
	if status != http.StatusCreated {
		t.Fatalf("create plan status=%d body=%v", status, body)
	}
}

func lockoutAction(t *testing.T, server *httptest.Server, chamber, path string, body map[string]any) map[string]any {
	t.Helper()
	status, decoded := postJSON(t, server.URL+"/api/chambers/"+chamber+"/lockouts"+path, body)
	if status != http.StatusOK && status != http.StatusCreated {
		t.Fatalf("lockout %s status=%d body=%v", path, status, decoded)
	}
	if lockout, ok := decoded["lockout"].(map[string]any); ok {
		return lockout
	}
	return decoded
}

func TestLockoutBlocksCommandsAndRestoresInterlocks(t *testing.T) {
	server, _ := newTestServer(t)
	chamber := "chamber-1"
	startCycle(t, server, chamber)

	// Register + approve an upstream-gate lockout.
	pending := lockoutAction(t, server, chamber, "", map[string]any{
		"devices":       []string{"upstream_gate"},
		"reason":        "seal replacement",
		"registered_by": "crew-lead",
	})
	if pending["status"] != "pending" {
		t.Fatalf("new lockout status=%v", pending["status"])
	}
	id, _ := pending["id"].(string)
	approved := lockoutAction(t, server, chamber, "/"+id+"/approve", map[string]any{"approved_by": "duty-engineer"})
	if approved["status"] != "active" {
		t.Fatalf("approved status=%v", approved["status"])
	}

	// Opening the covered gate is refused with 423.
	status, body := postJSON(t, server.URL+"/api/chambers/"+chamber+"/gates/upstream/open", map[string]any{})
	if status != http.StatusLocked {
		t.Fatalf("locked open status=%d body=%v, want 423", status, body)
	}
	if !strings.Contains(body["error"].(string), "seal replacement") {
		t.Fatalf("423 error must carry the reason: %v", body["error"])
	}

	// The opposite gate is not covered and its normal interlocks pass.
	status, body = postJSON(t, server.URL+"/api/chambers/"+chamber+"/gates/downstream/open", map[string]any{})
	if status != http.StatusAccepted {
		t.Fatalf("unrelated gate open status=%d body=%v", status, body)
	}

	// Fill valve is not covered: ramp succeeds.
	status, body = postJSON(t, server.URL+"/api/chambers/"+chamber+"/valves/ramp", map[string]any{"targets": []float64{0.2}})
	if status != http.StatusOK {
		t.Fatalf("valve ramp while gate-locked status=%d body=%v", status, body)
	}

	// Approve a valve lockout: ramps are now refused.
	valveLockout := lockoutAction(t, server, chamber, "", map[string]any{
		"devices":       []string{"fill_valve"},
		"reason":        "actuator service",
		"registered_by": "crew-lead",
	})
	valveID, _ := valveLockout["id"].(string)
	lockoutAction(t, server, chamber, "/"+valveID+"/approve", map[string]any{"approved_by": "duty-engineer"})
	status, body = postJSON(t, server.URL+"/api/chambers/"+chamber+"/valves/ramp", map[string]any{"targets": []float64{0.3}})
	if status != http.StatusLocked {
		t.Fatalf("locked ramp status=%d body=%v, want 423", status, body)
	}

	// Rejecting a pending lockout leaves the drain valve usable.
	rejected := lockoutAction(t, server, chamber, "", map[string]any{
		"devices":       []string{"drain_valve"},
		"reason":        "inspection only",
		"registered_by": "crew-lead",
	})
	drainID, _ := rejected["id"].(string)
	lockoutAction(t, server, chamber, "/"+drainID+"/reject", map[string]any{"rejected_by": "duty-engineer", "note": "not needed"})
	status, body = postJSON(t, server.URL+"/api/chambers/"+chamber+"/valves/ramp", map[string]any{"targets": []float64{-0.1}})
	if status != http.StatusOK {
		t.Fatalf("drain ramp after rejection status=%d body=%v", status, body)
	}

	// Release the upstream lockout (its device has no pending command).
	lockoutAction(t, server, chamber, "/"+id+"/release-request", map[string]any{"note": "work complete"})
	released := lockoutAction(t, server, chamber, "/"+id+"/release", map[string]any{"released_by": "duty-engineer", "note": "all clear"})
	if released["status"] != "released" {
		t.Fatalf("released status=%v", released["status"])
	}

	// Post-release, the ORIGINAL interlocks still apply: the downstream gate was
	// observed opening, so opening the (now unlocked) upstream gate must fail on
	// the opposite-gate rule rather than slipping through.
	status, body = postJSON(t, server.URL+"/api/chambers/"+chamber+"/gates/upstream/open", map[string]any{})
	if status != http.StatusConflict || !strings.Contains(body["error"].(string), "opposite gate") {
		t.Fatalf("post-release open must hit opposite-gate interlock, status=%d body=%v", status, body)
	}

	// Release the valve lockout and confirm fill ramps work again.
	lockoutAction(t, server, chamber, "/"+valveID+"/release", map[string]any{"released_by": "duty-engineer"})
	status, body = postJSON(t, server.URL+"/api/chambers/"+chamber+"/valves/ramp", map[string]any{"targets": []float64{0.4}})
	if status != http.StatusOK {
		t.Fatalf("valve ramp after release status=%d body=%v", status, body)
	}
}

func TestReleaseRefusedDuringEmergency(t *testing.T) {
	server, _ := newTestServer(t)
	chamber := "chamber-2"
	startCycle(t, server, chamber)
	lockout := lockoutAction(t, server, chamber, "", map[string]any{
		"devices":       []string{"upstream_gate", "downstream_gate", "fill_valve", "drain_valve"},
		"reason":        "annual inspection",
		"registered_by": "crew-lead",
	})
	id, _ := lockout["id"].(string)
	lockoutAction(t, server, chamber, "/"+id+"/approve", map[string]any{"approved_by": "duty-engineer"})

	// Keep one actuator outstanding so emergency stop remains in "draining".
	if status, body := postJSON(t, server.URL+"/api/chambers/"+chamber+"/equipment/pending", map[string]any{"count": 1}); status != http.StatusAccepted {
		t.Fatalf("equipment pending status=%d body=%v", status, body)
	}
	emergencyResult := make(chan int, 1)
	go func() {
		status, _ := postJSON(t, server.URL+"/api/chambers/"+chamber+"/emergency", map[string]any{})
		emergencyResult <- status
	}()

	// While the emergency stop is still draining, release must be refused.
	deadline := time.Now().Add(3 * time.Second)
	for {
		resp, err := http.Get(server.URL + "/api/interlocks")
		if err != nil {
			t.Fatalf("interlocks: %v", err)
		}
		var interlocks map[string]map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&interlocks)
		resp.Body.Close()
		if interlocks[chamber]["emergency"].(map[string]any)["status"] == "draining" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("emergency never reached draining state")
		}
		time.Sleep(5 * time.Millisecond)
	}
	status, body := postJSON(t, server.URL+"/api/chambers/"+chamber+"/lockouts/"+id+"/release", map[string]any{"released_by": "duty-engineer"})
	if status != http.StatusConflict || !strings.Contains(body["error"].(string), "emergency") {
		t.Fatalf("release during emergency status=%d body=%v, want 409 emergency", status, body)
	}

	// Once equipment settles and emergency completes, release is accepted.
	if status, _ := postJSON(t, server.URL+"/api/chambers/"+chamber+"/equipment/settled", map[string]any{}); status != http.StatusOK {
		t.Fatalf("equipment settled status=%d", status)
	}
	if status := <-emergencyResult; status != http.StatusOK {
		t.Fatalf("emergency completion status=%d", status)
	}
	status, body = postJSON(t, server.URL+"/api/chambers/"+chamber+"/lockouts/"+id+"/release", map[string]any{"released_by": "duty-engineer"})
	if status != http.StatusOK {
		t.Fatalf("release after emergency cleared status=%d body=%v", status, body)
	}
}

func TestLockoutsVisibleInInterlocksAndIncidents(t *testing.T) {
	server, _ := newTestServer(t)
	chamber := "chamber-1"
	startCycle(t, server, chamber)
	lockout := lockoutAction(t, server, chamber, "", map[string]any{
		"devices":       []string{"upstream_gate"},
		"reason":        "seal replacement",
		"registered_by": "crew-lead",
	})
	id, _ := lockout["id"].(string)
	lockoutAction(t, server, chamber, "/"+id+"/approve", map[string]any{"approved_by": "duty-engineer"})
	if status, _ := postJSON(t, server.URL+"/api/chambers/"+chamber+"/gates/upstream/open", map[string]any{}); status != http.StatusLocked {
		t.Fatal("expected 423 for blocked gate open")
	}

	resp, err := http.Get(server.URL + "/api/interlocks")
	if err != nil {
		t.Fatalf("interlocks: %v", err)
	}
	defer resp.Body.Close()
	var interlocks map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&interlocks); err != nil {
		t.Fatalf("decode: %v", err)
	}
	view := interlocks[chamber].(map[string]any)
	lockouts := view["lockouts"].([]any)
	if len(lockouts) != 1 || lockouts[0].(map[string]any)["reason"] != "seal replacement" {
		t.Fatalf("interlocks view lockouts = %v", lockouts)
	}

	resp, err = http.Get(server.URL + "/api/incidents")
	if err != nil {
		t.Fatalf("incidents: %v", err)
	}
	defer resp.Body.Close()
	var incidents []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&incidents); err != nil {
		t.Fatalf("decode: %v", err)
	}
	summaries := ""
	for _, incident := range incidents {
		summaries += incident["summary"].(string) + "\n"
	}
	if !strings.Contains(summaries, "Maintenance lockout approved") ||
		!strings.Contains(summaries, "command refused by maintenance lockout") {
		t.Fatalf("incident stream missing lockout entries:\n%s", summaries)
	}
}
