package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"taskboard/internal/httpapi"
	"taskboard/internal/store"
)

func TestTaskAPIWorkflowAndValidation(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	dataStore, err := store.Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer dataStore.Close()
	if err := dataStore.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `TRUNCATE activity_logs, step_logs, steps, tasks, task_groups RESTART IDENTITY CASCADE`); err != nil {
		pool.Close()
		t.Fatal(err)
	}
	pool.Close()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	server := httptest.NewServer(httpapi.NewHandler(dataStore, logger))
	defer server.Close()
	client := server.Client()

	created := requestJSON(t, client, http.MethodPost, server.URL+"/api/tasks", `{
		"group_name":"api-group",
		"group_overrides":{"channel":"sms"},
		"base_parameters":{"channel":"email"},
		"steps":[{"overrides":{"template":"welcome"}}]
	}`)
	if created.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d, body = %s", created.StatusCode, created.Body)
	}

	listed := requestJSON(t, client, http.MethodGet, server.URL+"/api/tasks", "")
	if listed.StatusCode != http.StatusOK {
		t.Fatalf("list status = %d, body = %s", listed.StatusCode, listed.Body)
	}
	if listed.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("task response is cacheable: %q", listed.Header.Get("Cache-Control"))
	}
	var payload struct {
		Tasks []struct {
			ID     int64  `json:"id"`
			Status string `json:"status"`
		} `json:"tasks"`
		Summary struct {
			Total   int `json:"total"`
			Pending int `json:"pending"`
		} `json:"summary"`
	}
	if err := json.Unmarshal(listed.Body, &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Tasks) != 1 || payload.Tasks[0].Status != "pending" ||
		payload.Summary.Total != 1 || payload.Summary.Pending != 1 {
		t.Fatalf("unexpected list response: %s", listed.Body)
	}
	invalidPage := requestJSON(t, client, http.MethodGet, server.URL+"/api/tasks?limit=201", "")
	if invalidPage.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid page status = %d, body = %s", invalidPage.StatusCode, invalidPage.Body)
	}
	activity := requestJSON(t, client, http.MethodGet, server.URL+"/api/activity", "")
	if activity.StatusCode != http.StatusOK || !bytes.Contains(activity.Body, []byte(`"event_type":"task_created"`)) {
		t.Fatalf("activity status = %d, body = %s", activity.StatusCode, activity.Body)
	}

	pendingCompletion := requestJSON(t, client, http.MethodPost,
		server.URL+"/api/tasks/1/steps/1/complete", `{"success":true}`)
	if pendingCompletion.StatusCode != http.StatusConflict {
		t.Fatalf("pending completion status = %d, body = %s", pendingCompletion.StatusCode, pendingCompletion.Body)
	}

	invalid := requestJSON(t, client, http.MethodPost, server.URL+"/api/tasks", `{"group_name":"empty","steps":[]}`)
	if invalid.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid create status = %d, body = %s", invalid.StatusCode, invalid.Body)
	}
	cancelled := requestJSON(t, client, http.MethodPost, server.URL+"/api/tasks/1/cancel", "")
	if cancelled.StatusCode != http.StatusOK || !bytes.Contains(cancelled.Body, []byte(`"changed":true`)) {
		t.Fatalf("cancel status = %d, body = %s", cancelled.StatusCode, cancelled.Body)
	}
	cancelledAgain := requestJSON(t, client, http.MethodPost, server.URL+"/api/tasks/1/cancel", "")
	if cancelledAgain.StatusCode != http.StatusOK || !bytes.Contains(cancelledAgain.Body, []byte(`"changed":false`)) {
		t.Fatalf("repeat cancel status = %d, body = %s", cancelledAgain.StatusCode, cancelledAgain.Body)
	}

	claimDemo := requestJSON(t, client, http.MethodPost, server.URL+"/api/demos/claim-race", "")
	if claimDemo.StatusCode != http.StatusOK {
		t.Fatalf("claim demo status = %d, body = %s", claimDemo.StatusCode, claimDemo.Body)
	}
	var demoPayload struct {
		OwnerCount int `json:"owner_count"`
		Attempts   []struct {
			Won         bool  `json:"won"`
			DatabasePID int32 `json:"database_pid"`
		} `json:"attempts"`
	}
	if err := json.Unmarshal(claimDemo.Body, &demoPayload); err != nil {
		t.Fatal(err)
	}
	winners := 0
	databaseSessions := make(map[int32]bool)
	for _, attempt := range demoPayload.Attempts {
		if attempt.Won {
			winners++
		}
		databaseSessions[attempt.DatabasePID] = true
	}
	if demoPayload.OwnerCount != 1 || len(demoPayload.Attempts) != 5 || winners != 1 || len(databaseSessions) != 5 {
		t.Fatalf("unexpected claim demo result: %s", claimDemo.Body)
	}
}

type response struct {
	StatusCode int
	Body       []byte
	Header     http.Header
}

func requestJSON(t *testing.T, client *http.Client, method, url, body string) response {
	t.Helper()
	request, err := http.NewRequest(method, url, bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	httpResponse, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer httpResponse.Body.Close()
	data, err := io.ReadAll(httpResponse.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response{StatusCode: httpResponse.StatusCode, Body: data, Header: httpResponse.Header.Clone()}
}
