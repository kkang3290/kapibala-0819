package httpapi

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"taskboard/internal/store"
)

//go:embed web/*
var webFiles embed.FS

type API struct {
	store  *store.Store
	logger *slog.Logger
}

func NewHandler(dataStore *store.Store, logger *slog.Logger) http.Handler {
	api := &API{store: dataStore, logger: logger}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", api.health)
	mux.HandleFunc("GET /api/tasks", api.listTasks)
	mux.HandleFunc("GET /api/activity", api.listActivity)
	mux.HandleFunc("POST /api/tasks", api.createTask)
	mux.HandleFunc("POST /api/tasks/{taskID}/cancel", api.cancelTask)
	mux.HandleFunc("POST /api/tasks/{taskID}/steps/{position}/complete", api.completeStep)
	mux.HandleFunc("POST /api/demos/claim-race", api.runClaimDemo)

	assets, err := fs.Sub(webFiles, "web")
	if err != nil {
		panic(err)
	}
	mux.Handle("GET /", http.FileServer(http.FS(assets)))
	return securityHeaders(requestLogger(logger, mux))
}

func (a *API) health(w http.ResponseWriter, r *http.Request) {
	if err := a.store.Ping(r.Context()); err != nil {
		writeError(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (a *API) listTasks(w http.ResponseWriter, r *http.Request) {
	limit, offset, err := pagination(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	page, err := a.store.ListTasks(r.Context(), limit, offset)
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func pagination(r *http.Request) (int, int, error) {
	limit := 25
	offset := 0
	if raw := r.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 200 {
			return 0, 0, errors.New("limit must be between 1 and 200")
		}
		limit = parsed
	}
	if raw := r.URL.Query().Get("offset"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 0 {
			return 0, 0, errors.New("offset must be zero or greater")
		}
		offset = parsed
	}
	return limit, offset, nil
}

func (a *API) listActivity(w http.ResponseWriter, r *http.Request) {
	limit := 60
	if rawLimit := r.URL.Query().Get("limit"); rawLimit != "" {
		parsed, err := strconv.Atoi(rawLimit)
		if err != nil || parsed <= 0 || parsed > 200 {
			writeError(w, http.StatusBadRequest, "limit must be between 1 and 200")
			return
		}
		limit = parsed
	}
	logs, err := a.store.ListActivity(r.Context(), limit)
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"logs": logs})
}

func (a *API) createTask(w http.ResponseWriter, r *http.Request) {
	var input store.CreateTaskInput
	if err := decodeJSON(w, r, &input); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	taskID, err := a.store.CreateTask(r.Context(), input)
	if err != nil {
		if errors.Is(err, store.ErrInvalidPayload) {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		a.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": taskID, "status": "pending"})
}

func (a *API) cancelTask(w http.ResponseWriter, r *http.Request) {
	taskID, err := strconv.ParseInt(r.PathValue("taskID"), 10, 64)
	if err != nil || taskID <= 0 {
		writeError(w, http.StatusBadRequest, "invalid task ID")
		return
	}
	cancelled, err := a.store.CancelTask(r.Context(), taskID, "dashboard")
	if err != nil {
		switch {
		case errors.Is(err, store.ErrNotFound):
			writeError(w, http.StatusNotFound, err.Error())
		case errors.Is(err, store.ErrTaskTerminal):
			writeError(w, http.StatusConflict, err.Error())
		default:
			a.internalError(w, r, err)
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": taskID, "status": "cancelled", "changed": cancelled})
}

func (a *API) completeStep(w http.ResponseWriter, r *http.Request) {
	receivedAt := time.Now().UTC()
	taskID, err := strconv.ParseInt(r.PathValue("taskID"), 10, 64)
	if err != nil || taskID <= 0 {
		writeError(w, http.StatusBadRequest, "invalid task ID")
		return
	}
	position, err := strconv.Atoi(r.PathValue("position"))
	if err != nil || position <= 0 {
		writeError(w, http.StatusBadRequest, "invalid step position")
		return
	}
	var input struct {
		Success      bool   `json:"success"`
		WorkerID     string `json:"worker_id"`
		FencingToken int64  `json:"fencing_token"`
	}
	if err := decodeJSON(w, r, &input); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	result, err := a.store.CompleteStep(
		r.Context(), taskID, position, input.WorkerID, input.FencingToken, input.Success,
	)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrNotFound):
			writeError(w, http.StatusNotFound, err.Error())
		case errors.Is(err, store.ErrInvalidState):
			writeError(w, http.StatusConflict, err.Error())
		case errors.Is(err, store.ErrLeaseLost), errors.Is(err, store.ErrTaskTerminal):
			writeError(w, http.StatusConflict, err.Error())
		default:
			a.internalError(w, r, err)
		}
		return
	}
	result.ReceivedAt = receivedAt
	writeJSON(w, http.StatusOK, result)
}

type claimDemoAttempt struct {
	WorkerID    string  `json:"worker_id"`
	Won         bool    `json:"won"`
	DatabasePID int32   `json:"database_pid"`
	StartMS     float64 `json:"start_ms"`
	DurationMS  float64 `json:"duration_ms"`
	Error       string  `json:"error,omitempty"`
}

func (a *API) runClaimDemo(w http.ResponseWriter, r *http.Request) {
	taskID, err := a.store.CreateClaimDemoTask(r.Context())
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	cleanupNeeded := true
	defer func() {
		if !cleanupNeeded {
			return
		}
		cleanupContext, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
		defer cancel()
		if err := a.store.FinishClaimDemo(cleanupContext, taskID); err != nil {
			a.logger.Error("claim demo cleanup failed", "task_id", taskID, "error", err)
		}
	}()

	const workerCount = 5
	attempts := make([]claimDemoAttempt, workerCount)
	startedAt := make([]time.Time, workerCount)
	startGate := make(chan struct{})
	batchStart := time.Now()
	var waitGroup sync.WaitGroup
	for index := 0; index < workerCount; index++ {
		waitGroup.Add(1)
		go func(index int) {
			defer waitGroup.Done()
			workerID := fmt.Sprintf("demo-worker-%d", index+1)
			<-startGate
			started := time.Now()
			startedAt[index] = started
			won, backendPID, claimErr := a.store.ClaimSpecificDemo(r.Context(), taskID, workerID)
			attempts[index] = claimDemoAttempt{
				WorkerID: workerID, Won: won, DatabasePID: backendPID,
				StartMS:    float64(started.Sub(batchStart).Microseconds()) / 1000,
				DurationMS: float64(time.Since(started).Microseconds()) / 1000,
			}
			if claimErr != nil {
				attempts[index].Error = claimErr.Error()
			}
		}(index)
	}
	batchStart = time.Now()
	close(startGate)
	waitGroup.Wait()

	winner := ""
	for _, attempt := range attempts {
		if attempt.Error != "" {
			a.internalError(w, r, errors.New(attempt.Error))
			return
		}
		if attempt.Won {
			if winner != "" {
				a.internalError(w, r, errors.New("claim demo produced multiple owners"))
				return
			}
			winner = attempt.WorkerID
		}
	}
	if winner == "" {
		a.internalError(w, r, errors.New("claim demo produced no owner"))
		return
	}
	for _, attempt := range attempts {
		if err := a.store.RecordClaimDemoAttempt(
			r.Context(), taskID, attempt.WorkerID, attempt.Won, winner,
			time.Duration(attempt.DurationMS*float64(time.Millisecond)),
		); err != nil {
			a.internalError(w, r, err)
			return
		}
	}
	if err := a.store.FinishClaimDemo(r.Context(), taskID); err != nil {
		a.internalError(w, r, err)
		return
	}
	cleanupNeeded = false

	minimum, maximum := startedAt[0], startedAt[0]
	for _, started := range startedAt[1:] {
		if started.Before(minimum) {
			minimum = started
		}
		if started.After(maximum) {
			maximum = started
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"task_id": taskID, "winner": winner, "owner_count": 1,
		"start_spread_ms": float64(maximum.Sub(minimum).Microseconds()) / 1000,
		"attempts":        attempts,
	})
}

func (a *API) internalError(w http.ResponseWriter, r *http.Request, err error) {
	a.logger.Error("request failed", "method", r.Method, "path", r.URL.Path, "error", err)
	writeError(w, http.StatusInternalServerError, "internal server error")
}

func decodeJSON(w http.ResponseWriter, r *http.Request, destination any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return fmt.Errorf("request body must contain exactly one JSON value")
	}
	return nil
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func requestLogger(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		next.ServeHTTP(w, r)
		if r.URL.Path != "/healthz" {
			logger.InfoContext(context.Background(), "http request",
				"method", r.Method, "path", r.URL.Path, "duration", time.Since(started))
		}
	})
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		if strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}
