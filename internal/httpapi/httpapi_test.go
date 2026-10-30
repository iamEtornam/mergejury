package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/iamEtornam/mergejury/internal/config"
	"github.com/iamEtornam/mergejury/internal/run"
	"github.com/iamEtornam/mergejury/internal/store"
	"github.com/iamEtornam/mergejury/prompts"
)

func TestReplayReturnsSeparateCompletedRun(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "console.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	id, err := st.CreateRun("o/r", 1, "", "", "test", "{}")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.FinishRun(id, "completed", "APPROVE", "clean", 0); err != nil {
		t.Fatal(err)
	}
	ps := prompts.New("")
	s := New(&run.Orchestrator{Cfg: config.Default(), Store: st, Prompts: ps}, st, ps)
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/runs/1/replay", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("replay response=%d: %s", w.Code, w.Body.String())
	}
	var sum run.Summary
	if err := json.NewDecoder(w.Body).Decode(&sum); err != nil {
		t.Fatal(err)
	}
	if sum.RunID == 0 || sum.RunID == id || sum.Status != "completed" {
		t.Fatalf("missing new completed run: %+v", sum)
	}
	source, err := st.GetRun(id)
	if err != nil || source.ReviewEventReason != "clean" {
		t.Fatalf("source changed: %+v %v", source, err)
	}
}

func TestReplayMissingRunReturnsNotFound(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "console.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ps := prompts.New("")
	s := New(&run.Orchestrator{Cfg: config.Default(), Store: st, Prompts: ps}, st, ps)
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/runs/999/replay", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("missing run response=%d: %s", w.Code, w.Body.String())
	}
}
