// Package job runs the auditor's periodic checks and records every run in job_runs
// (docs/design/reconciliation-audit.md invariants 2 and 7).
package job

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"
)

// Result of an Audit Run. Error is its own value: a job that could not complete is never ok.
type Result string

const (
	OK    Result = "ok"
	Warn  Result = "warn"  // only warn findings
	Fail  Result = "fail"  // at least one critical finding
	Error Result = "error" // the job could not complete (RPC or a store down, panic)
)

// Outcome is what a job reports when it completed.
type Outcome struct {
	Warn     int // warn findings raised or still open
	Critical int // critical findings raised or still open
}

func (o Outcome) Findings() int { return o.Warn + o.Critical }

// Result derives ok/warn/fail from the findings.
func (o Outcome) Result() Result {
	switch {
	case o.Critical > 0:
		return Fail
	case o.Warn > 0:
		return Warn
	default:
		return OK
	}
}

type runIDKey struct{}

// RunID is the job_runs id of the Audit Run in progress; findings record it as first/last run.
func RunID(ctx context.Context) int64 {
	id, _ := ctx.Value(runIDKey{}).(int64)
	return id
}

// Job is one periodic check. Run returns an error only when it could not complete; findings are an
// Outcome, not an error.
type Job interface {
	Name() string
	Interval() time.Duration
	Run(ctx context.Context) (Outcome, error)
}

// Recorder persists Audit Runs. *store.Store implements it.
type Recorder interface {
	StartRun(ctx context.Context, job string, at time.Time) (int64, error)
	FinishRun(ctx context.Context, id int64, at time.Time, result Result, findings int, errMsg string) error
}

// RunOnce executes one Audit Run of j and records it. A panic or a returned error is recorded as Error.
// It returns an error only if the run could not be recorded.
func RunOnce(ctx context.Context, rec Recorder, j Job, now func() time.Time) (Result, error) {
	id, err := rec.StartRun(ctx, j.Name(), now())
	if err != nil {
		return "", fmt.Errorf("recording start of %s: %w", j.Name(), err)
	}
	out, runErr := safeRun(context.WithValue(ctx, runIDKey{}, id), j)

	result, findings, msg := out.Result(), out.Findings(), ""
	if runErr != nil {
		result, findings, msg = Error, 0, runErr.Error()
	}
	// The run may have ended because ctx was cancelled; the result must still be written.
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := rec.FinishRun(wctx, id, now(), result, findings, msg); err != nil {
		return "", fmt.Errorf("recording end of %s: %w", j.Name(), err)
	}
	return result, nil
}

func safeRun(ctx context.Context, j Job) (out Outcome, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	return j.Run(ctx)
}

// Run drives every job on its own interval until ctx is done. Each job runs once straight away, then
// every Interval; a run never overlaps the previous run of the same job.
func Run(ctx context.Context, rec Recorder, jobs []Job) {
	done := make(chan struct{})
	for _, j := range jobs {
		go func(j Job) {
			defer func() { done <- struct{}{} }()
			loop(ctx, rec, j)
		}(j)
	}
	for range jobs {
		<-done
	}
}

func loop(ctx context.Context, rec Recorder, j Job) {
	t := time.NewTicker(j.Interval())
	defer t.Stop()
	for {
		if res, err := RunOnce(ctx, rec, j, time.Now); err != nil {
			if !errors.Is(err, context.Canceled) {
				log.Printf("job %s: %v", j.Name(), err)
			}
		} else {
			log.Printf("job %s: %s", j.Name(), res)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
