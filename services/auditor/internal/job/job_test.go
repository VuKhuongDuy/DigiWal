package job

import (
	"context"
	"errors"
	"testing"
	"time"
)

type fakeRec struct {
	started  []string
	result   Result
	findings int
	msg      string
	finished bool
}

func (f *fakeRec) StartRun(_ context.Context, job string, _ time.Time) (int64, error) {
	f.started = append(f.started, job)
	return int64(len(f.started)), nil
}
func (f *fakeRec) FinishRun(_ context.Context, _ int64, _ time.Time, r Result, n int, msg string) error {
	f.result, f.findings, f.msg, f.finished = r, n, msg, true
	return nil
}

type fakeJob struct {
	out Outcome
	err error
	pan bool
}

func (fakeJob) Name() string            { return "fake" }
func (fakeJob) Interval() time.Duration { return time.Minute }
func (j fakeJob) Run(context.Context) (Outcome, error) {
	if j.pan {
		panic("boom")
	}
	return j.out, j.err
}

func TestRunOnceResults(t *testing.T) {
	cases := []struct {
		name         string
		job          fakeJob
		want         Result
		wantFindings int
	}{
		{"no findings", fakeJob{}, OK, 0},
		{"warn only", fakeJob{out: Outcome{Warn: 2}}, Warn, 2},
		{"a critical makes it fail", fakeJob{out: Outcome{Warn: 1, Critical: 1}}, Fail, 2},
		{"error is error, never ok", fakeJob{err: errors.New("rpc down")}, Error, 0},
		{"error wins over findings", fakeJob{out: Outcome{Critical: 3}, err: errors.New("store down")}, Error, 0},
		{"panic is error", fakeJob{pan: true}, Error, 0},
	}
	for _, c := range cases {
		rec := &fakeRec{}
		got, err := RunOnce(context.Background(), rec, c.job, time.Now)
		if err != nil || got != c.want || rec.result != c.want || rec.findings != c.wantFindings || !rec.finished {
			t.Errorf("%s: got %q err %v, recorded %q findings %d finished %v", c.name, got, err, rec.result, rec.findings, rec.finished)
		}
		if c.want == Error && rec.msg == "" {
			t.Errorf("%s: error message not recorded", c.name)
		}
	}
}

func TestResultIsWrittenAfterCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	rec := &fakeRec{}
	cancel()
	if _, err := RunOnce(ctx, rec, fakeJob{err: ctx.Err()}, time.Now); err != nil || !rec.finished || rec.result != Error {
		t.Fatalf("a cancelled run must still be recorded as error: err %v rec %+v", err, rec)
	}
}
