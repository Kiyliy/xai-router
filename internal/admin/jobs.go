package admin

import (
	"context"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// Job is a long-running admin operation (log purge, key tests, compaction)
// that runs in the background; clients poll GET /api/v1/jobs/{id}.
type Job struct {
	ID         int64      `json:"id"`
	Kind       string     `json:"kind"`
	State      string     `json:"state"` // running, done, failed
	Progress   int64      `json:"progress"`
	Total      int64      `json:"total"`           // 0 = unknown
	Phase      string     `json:"phase,omitempty"` // purge_logs: delete (rows), then reclaim (bytes)
	Message    string     `json:"message,omitempty"`
	Result     any        `json:"result,omitempty"`
	Error      string     `json:"error,omitempty"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at"`
}

type jobs struct {
	mu   sync.Mutex
	seq  atomic.Int64
	list []*Job // newest last, bounded
}

// update applies fn to the job under the lock.
func (js *jobs) update(j *Job, fn func(*Job)) {
	js.mu.Lock()
	fn(j)
	js.mu.Unlock()
}

// start runs fn in the background and returns the job. Only one job of a
// kind runs at a time: a second start returns the running one.
func (js *jobs) start(kind string, fn func(ctx context.Context, j *Job) (any, error)) (*Job, bool) {
	js.mu.Lock()
	for _, j := range js.list {
		if j.Kind == kind && j.State == "running" {
			c := *j
			js.mu.Unlock()
			return &c, false
		}
	}
	j := &Job{ID: js.seq.Add(1), Kind: kind, State: "running", StartedAt: time.Now().UTC()}
	js.list = append(js.list, j)
	if len(js.list) > 50 {
		js.list = js.list[len(js.list)-50:]
	}
	c := *j
	js.mu.Unlock()
	go func() {
		res, err := fn(context.Background(), j)
		js.update(j, func(j *Job) {
			now := time.Now().UTC()
			j.FinishedAt, j.Result = &now, res
			if err != nil {
				j.State, j.Error = "failed", err.Error()
			} else {
				j.State = "done"
			}
		})
	}()
	return &c, true
}

func (js *jobs) get(id int64) (*Job, bool) {
	js.mu.Lock()
	defer js.mu.Unlock()
	for _, j := range js.list {
		if j.ID == id {
			c := *j
			return &c, true
		}
	}
	return nil, false
}

func (js *jobs) all() []Job {
	js.mu.Lock()
	defer js.mu.Unlock()
	out := make([]Job, 0, len(js.list))
	for i := len(js.list) - 1; i >= 0; i-- {
		out = append(out, *js.list[i])
	}
	return out
}

func (s *Server) listJobs(w http.ResponseWriter, r *http.Request) {
	all := s.jobs.all()
	writeJSON(w, http.StatusOK, map[string]any{"data": all, "total": len(all)})
}

func (s *Server) getJob(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_id", "invalid id")
		return
	}
	j, ok := s.jobs.get(id)
	if !ok {
		writeErr(w, http.StatusNotFound, "not_found", "job not found (jobs are kept in memory until restart)")
		return
	}
	writeJSON(w, http.StatusOK, j)
}

// compact reclaims free pages of the database file in the background.
func (s *Server) compact(w http.ResponseWriter, r *http.Request) {
	j, _ := s.jobs.start("compact", func(ctx context.Context, j *Job) (any, error) {
		before := s.DB.Size()
		freed, err := s.DB.IncrementalVacuum(ctx, func(done, total int64) {
			s.jobs.update(j, func(j *Job) { j.Progress, j.Total = done, total })
		})
		return map[string]any{"freed_bytes": freed, "size_before": before, "size_after": s.DB.Size()}, err
	})
	writeJSON(w, http.StatusAccepted, j)
}
