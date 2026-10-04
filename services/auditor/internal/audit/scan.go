package audit

import (
	"context"
	"fmt"
	"strings"
	"time"

	"digiwal/services/auditor/internal/source"
	"digiwal/services/auditor/internal/store"
)

type lister func(ctx context.Context, after source.Ref, notAfter time.Time, limit int) ([]source.Ref, error)

// scanRefs walks a lister from its stored cursor to notAfter, calling each on every reference, and saves
// the cursor after every batch so a failure part-way re-reads at most one batch.
func scanRefs(ctx context.Context, st *store.Store, name string, list lister, notAfter time.Time, batch int, now func() time.Time, each func(ref string) error) error {
	cur, err := loadCursor(ctx, st, name)
	if err != nil {
		return err
	}
	for {
		refs, err := list(ctx, cur, notAfter, batch)
		if err != nil {
			return err
		}
		for _, r := range refs {
			if err := each(r.ID); err != nil {
				return err
			}
			cur = r
		}
		if len(refs) > 0 {
			if err := st.SetCursor(ctx, name, cur.At.Format(time.RFC3339Nano)+"|"+cur.ID, now()); err != nil {
				return err
			}
		}
		if len(refs) < batch {
			return nil
		}
	}
}

func loadCursor(ctx context.Context, st *store.Store, name string) (source.Ref, error) {
	s, err := st.Cursor(ctx, name)
	if err != nil || s == "" {
		return source.Ref{At: time.Unix(0, 0).UTC()}, err
	}
	ts, id, ok := strings.Cut(s, "|")
	if !ok {
		return source.Ref{}, fmt.Errorf("corrupt cursor %q for %s", s, name)
	}
	at, err := time.Parse(time.RFC3339Nano, ts)
	return source.Ref{ID: id, At: at}, err
}

type blockTimer interface {
	BlockTime(ctx context.Context, n int64) (time.Time, error)
}

// graceEnd shrinks [next, end] until block `end` is no newer than notAfter, so a batch never reaches into
// the Grace Period. It returns end < next when even block `next` is too new.
func graceEnd(ctx context.Context, c blockTimer, next, end int64, notAfter time.Time) (int64, error) {
	for end >= next {
		t, err := c.BlockTime(ctx, end)
		if err != nil {
			return 0, err
		}
		if !t.After(notAfter) {
			return end, nil
		}
		if end == next {
			return next - 1, nil
		}
		end = next + (end-next)/2
	}
	return end, nil
}
