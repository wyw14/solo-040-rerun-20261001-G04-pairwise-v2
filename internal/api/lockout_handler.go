package api

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/wyw14/cry-96/internal/maintenance"
	"github.com/wyw14/cry-96/internal/model"
)

type lockoutInput struct {
	Devices      []model.DeviceKind `json:"devices"`
	Reason       string             `json:"reason"`
	RegisteredBy string             `json:"registered_by"`
}

type lockoutDecisionInput struct {
	ApprovedBy string `json:"approved_by"`
	ReleasedBy string `json:"released_by"`
	RejectedBy string `json:"rejected_by"`
	Note       string `json:"note"`
}

func lockoutIDParam(r *http.Request) (uuid.UUID, error) {
	return uuid.Parse(chi.URLParam(r, "lockoutID"))
}

func registerLockoutHandler(runtime *Runtime) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var input lockoutInput
		if err := decodeJSON(r, &input); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		chamberID := chi.URLParam(r, "chamberID")
		lockout, err := runtime.Lockouts.Register(r.Context(), chamberID, input.Devices, input.Reason, input.RegisteredBy)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, http.StatusCreated, lockout)
	}
}

func listLockoutsHandler(runtime *Runtime) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		chamberID := chi.URLParam(r, "chamberID")
		writeJSON(w, http.StatusOK, runtime.Lockouts.List(chamberID))
	}
}

func approveLockoutHandler(runtime *Runtime) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var input lockoutDecisionInput
		if err := decodeJSON(r, &input); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		lockoutID, err := lockoutIDParam(r)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		lockout, cancelled, err := runtime.Lockouts.Approve(r.Context(), chi.URLParam(r, "chamberID"), lockoutID, input.ApprovedBy)
		if err != nil {
			writeLockoutError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"lockout": lockout, "cancelled_commands": cancelled})
	}
}

func rejectLockoutHandler(runtime *Runtime) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var input lockoutDecisionInput
		if err := decodeJSON(r, &input); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		lockoutID, err := lockoutIDParam(r)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		lockout, err := runtime.Lockouts.Reject(r.Context(), chi.URLParam(r, "chamberID"), lockoutID, input.RejectedBy, input.Note)
		if err != nil {
			writeLockoutError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, lockout)
	}
}

func requestReleaseLockoutHandler(runtime *Runtime) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var input lockoutDecisionInput
		if err := decodeJSON(r, &input); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		lockoutID, err := lockoutIDParam(r)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		lockout, err := runtime.Lockouts.RequestRelease(r.Context(), chi.URLParam(r, "chamberID"), lockoutID, input.Note)
		if err != nil {
			writeLockoutError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, lockout)
	}
}

func releaseLockoutHandler(runtime *Runtime) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var input lockoutDecisionInput
		if err := decodeJSON(r, &input); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		lockoutID, err := lockoutIDParam(r)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		lockout, err := runtime.Lockouts.Release(r.Context(), chi.URLParam(r, "chamberID"), lockoutID, input.ReleasedBy, input.Note)
		if err != nil {
			writeLockoutError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, lockout)
	}
}

// writeLockoutError maps lifecycle conflicts to 409 and unknown lockouts to 404.
func writeLockoutError(w http.ResponseWriter, err error) {
	if errors.Is(err, maintenance.ErrNotFound) {
		writeError(w, http.StatusNotFound, err)
		return
	}
	writeError(w, http.StatusConflict, err)
}
