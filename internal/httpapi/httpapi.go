// Package httpapi exposes the orchestrator over REST and SSE for the web
// console. It binds to localhost by default: an operator console, not a
// product.
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"strconv"
	"strings"
	"time"

	"revu/internal/run"
	"revu/internal/store"
	"revu/prompts"
	"revu/web"
)

type Server struct {
	orch *run.Orchestrator
	st   *store.Store
	ps   *prompts.Set
	mux  *http.ServeMux
}

func New(orch *run.Orchestrator, st *store.Store, ps *prompts.Set) *Server {
	if orch.Events == nil {
		orch.Events = run.NewBroker()
	}
	s := &Server{orch: orch, st: st, ps: ps, mux: http.NewServeMux()}
	s.routes()
	return s
}

func (s *Server) routes() {
	s.mux.HandleFunc("POST /api/runs", s.startRun)
	s.mux.HandleFunc("GET /api/runs", s.listRuns)
	s.mux.HandleFunc("GET /api/runs/{id}", s.runDetail)
	s.mux.HandleFunc("GET /api/runs/{id}/events", s.runEvents)
	s.mux.HandleFunc("GET /api/events", s.allEvents)
	s.mux.HandleFunc("POST /api/runs/{id}/replay", s.replay)
	s.mux.HandleFunc("GET /api/adapters/probe", s.probe)
	s.mux.HandleFunc("GET /api/stats", s.stats)
	s.mux.HandleFunc("GET /api/prompts", s.listPrompts)
	s.mux.HandleFunc("GET /api/prompts/{name}", s.getPrompt)
	s.mux.HandleFunc("PUT /api/prompts/{name}", s.putPrompt)

	dist, err := fs.Sub(web.Dist, "dist")
	if err == nil {
		fileServer := http.FileServer(http.FS(dist))
		s.mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			// SPA fallback: serve index.html for app routes.
			if r.URL.Path != "/" && !strings.Contains(r.URL.Path, ".") {
				r.URL.Path = "/"
			}
			fileServer.ServeHTTP(w, r)
		})
	}
}

func (s *Server) ListenAndServe(ctx context.Context, addr string) error {
	srv := &http.Server{Addr: addr, Handler: s.mux}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx)
	}()
	err := srv.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

func (s *Server) pathID(r *http.Request) (int64, error) {
	return strconv.ParseInt(r.PathValue("id"), 10, 64)
}

// ---- runs ----

type startRunRequest struct {
	Repo        string   `json:"repo"`
	PRNumber    int      `json:"pr_number"`
	Adapters    []string `json:"adapters"`
	DryRun      bool     `json:"dry_run"`
	NoChallenge bool     `json:"no_challenger"`
	NoVerdict   bool     `json:"no_verdict"`
}

func (s *Server) startRun(w http.ResponseWriter, r *http.Request) {
	var req startRunRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if req.Repo == "" || req.PRNumber == 0 {
		writeErr(w, http.StatusBadRequest, errors.New("repo and pr_number are required"))
		return
	}
	if s.orch.Forge == nil {
		writeErr(w, http.StatusPreconditionFailed, errors.New("no GitHub token configured on the server"))
		return
	}
	opts := run.Options{
		Adapters: req.Adapters, DryRun: req.DryRun,
		NoChallenger: req.NoChallenge, NoVerdict: req.NoVerdict,
		Trigger: "web",
	}
	go func() {
		// Long-running; progress streams over /api/events.
		_, err := s.orch.ReviewPR(context.Background(), req.Repo, req.PRNumber, opts)
		if err != nil {
			s.orch.Events.Publish(run.Event{Type: "run_error", Payload: err.Error(), At: time.Now().UTC()})
		}
	}()
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "started"})
}

func (s *Server) listRuns(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	runs, err := s.st.ListRuns(r.URL.Query().Get("repo"), limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if runs == nil {
		runs = []store.Run{}
	}
	writeJSON(w, http.StatusOK, runs)
}

type runDetail struct {
	Run           *store.Run                 `json:"run"`
	AdapterRuns   []store.AdapterRun         `json:"adapter_runs"`
	Findings      []store.StoredFinding      `json:"findings"`
	Clusters      []store.StoredCluster      `json:"clusters"`
	Verifications []store.StoredVerification `json:"verifications"`
	Challenges    []store.StoredChallenge    `json:"challenges"`
	Verdicts      []store.StoredVerdict      `json:"verdicts"`
}

func (s *Server) runDetail(w http.ResponseWriter, r *http.Request) {
	id, err := s.pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	rn, err := s.st.GetRun(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, fmt.Errorf("run %d not found", id))
		return
	}
	d := runDetail{Run: rn}
	if d.AdapterRuns, err = s.st.AdapterRunsForRun(id); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if d.Findings, err = s.st.FindingsForRun(id, false); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	d.Clusters, _ = s.st.ClustersForRun(id)
	d.Verifications, _ = s.st.VerificationsForRun(id)
	d.Challenges, _ = s.st.ChallengesForRun(id)
	d.Verdicts, _ = s.st.VerdictsForRun(id)
	// Nil slices marshal to JSON null; the client expects arrays.
	if d.AdapterRuns == nil {
		d.AdapterRuns = []store.AdapterRun{}
	}
	if d.Findings == nil {
		d.Findings = []store.StoredFinding{}
	}
	if d.Clusters == nil {
		d.Clusters = []store.StoredCluster{}
	}
	if d.Verifications == nil {
		d.Verifications = []store.StoredVerification{}
	}
	if d.Challenges == nil {
		d.Challenges = []store.StoredChallenge{}
	}
	if d.Verdicts == nil {
		d.Verdicts = []store.StoredVerdict{}
	}
	writeJSON(w, http.StatusOK, d)
}

func (s *Server) replay(w http.ResponseWriter, r *http.Request) {
	id, err := s.pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	go func() {
		if _, err := s.orch.Replay(context.Background(), id, run.Options{Trigger: "replay"}); err != nil {
			s.orch.Events.Publish(run.Event{RunID: id, Type: "run_error", Payload: err.Error(), At: time.Now().UTC()})
		}
	}()
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "replaying"})
}

// ---- SSE ----

func (s *Server) runEvents(w http.ResponseWriter, r *http.Request) {
	id, err := s.pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	s.serveSSE(w, r, id)
}

func (s *Server) allEvents(w http.ResponseWriter, r *http.Request) {
	s.serveSSE(w, r, 0)
}

func (s *Server) serveSSE(w http.ResponseWriter, r *http.Request, runID int64) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, errors.New("streaming unsupported"))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	events, unsub := s.orch.Events.Subscribe(runID)
	defer unsub()
	keepalive := time.NewTicker(25 * time.Second)
	defer keepalive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-keepalive.C:
			fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		case e, ok := <-events:
			if !ok {
				return
			}
			b, _ := json.Marshal(e)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", e.Type, b)
			flusher.Flush()
		}
	}
}

// ---- adapters, stats, prompts ----

func (s *Server) probe(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.orch.ProbeAdapters(r.Context()))
}

func (s *Server) stats(w http.ResponseWriter, r *http.Request) {
	stats, err := s.st.Stats()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if stats == nil {
		stats = []store.AdapterStats{}
	}
	writeJSON(w, http.StatusOK, stats)
}

func (s *Server) listPrompts(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.ps.List())
}

func (s *Server) getPrompt(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	current, err := s.ps.Get(name)
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	committed, _ := s.ps.Embedded(name)
	writeJSON(w, http.StatusOK, map[string]string{"name": name, "content": current, "committed": committed})
}

func (s *Server) putPrompt(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var req struct {
		Content string `json:"content"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := s.ps.Save(name, req.Content); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "saved"})
}
