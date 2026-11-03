package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"time"

	"connectrpc.com/connect/v2"
)

// Jobs, as RunJobs names them.
const (
	JobDeliveries = "deliveries"
	JobPrune      = "prune"
)

var jobs = []string{JobDeliveries, JobPrune}

// pruneEvery is how often the prune job runs, at most.
const pruneEvery = time.Hour

// JobRun is how a job went.
type JobRun struct {
	Job string `json:"job"`
	// Ran is whether it ran: not if it wasn't due, or can't, such as
	// deliveries with sending off.
	Ran bool `json:"ran"`
	// Detail is what it did, such as "tried 3 deliveries", or why it
	// didn't run.
	Detail string `json:"detail"`
}

// RunJobs runs the jobs that are due, of only, if any: deliveries, which
// sends those due, such as ones deferred until now, each time; and prune,
// which expunges deleted messages and prunes old events, at most hourly.
// Admins only.
func (s *Service) RunJobs(ctx context.Context, only ...string) ([]JobRun, error) {
	if err := s.requireAdmin(ctx); err != nil {
		return nil, err
	}
	runs := []JobRun{}
	for _, job := range jobs {
		if len(only) > 0 && !slices.Contains(only, job) {
			continue
		}
		run, err := s.runJob(ctx, job, time.Now())
		if err != nil {
			return nil, fmt.Errorf("%s: %w", job, err)
		}
		runs = append(runs, run)
	}
	return runs, nil
}

// runJob runs job if it's due at now.
func (s *Service) runJob(ctx context.Context, job string, now time.Time) (JobRun, error) {
	run := JobRun{Job: job}
	switch job {
	case JobDeliveries:
		if s.spool == nil {
			run.Detail = "sending is off: set MAIL_TRANSPORT"
			return run, nil
		}
		n, err := s.spool.SendDue(ctx)
		if err != nil {
			return run, err
		}
		run.Ran, run.Detail = true, fmt.Sprintf("tried %d deliveries", n)
	case JobPrune:
		due, err := s.db.ClaimJob(ctx, job, now, pruneEvery)
		if err != nil {
			return run, err
		}
		if !due {
			run.Detail = fmt.Sprintf("ran within the last %s", pruneEvery)
			return run, nil
		}
		if err := s.Prune(ctx, now); err != nil {
			return run, err
		}
		run.Ran, run.Detail = true, "expunged deleted messages and pruned old events"
	}
	return run, nil
}

// tick serves POST /tick, for a scheduler, such as Cloud Scheduler on
// Cloud Run, to call every minute: it runs the jobs that are due, those of
// ?job= if given, and answers {"runs": [...]}.
func (s *Service) tick(w http.ResponseWriter, r *http.Request) {
	only := r.URL.Query()["job"]
	for _, job := range only {
		if !slices.Contains(jobs, job) {
			apiError(w, fmt.Sprintf("unknown job %q: deliveries or prune", job), http.StatusBadRequest)
			return
		}
	}
	runs, err := s.RunJobs(r.Context(), only...)
	if err != nil {
		status := http.StatusInternalServerError
		switch connect.CodeOf(err) {
		case connect.CodePermissionDenied:
			status = http.StatusForbidden
		case connect.CodeUnauthenticated:
			status = http.StatusUnauthorized
		}
		apiError(w, err.Error(), status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string][]JobRun{"runs": runs})
}
