package migrate

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The tests below need no database: sys.jobs is a fake.

// fakeJobs is a database with a sys.jobs view: the jobs not yet in a final
// state, how each one ends, and the details of the failed ones.
type fakeJobs struct {
	pending   []job
	queryErr  error // returned by the query of the pending jobs
	rowsErr   error // returned by its rows, after the last one
	succeeded map[string]bool
	details   map[string]string

	waited []string
}

func (f *fakeJobs) Query(_ context.Context, sql string, _ ...any) (pgx.Rows, error) {
	if sql != pendingJobsQuery {
		return nil, errors.New("unexpected query: " + sql)
	}
	if f.queryErr != nil {
		return nil, f.queryErr
	}
	return &fakeRows{jobs: f.pending, err: f.rowsErr}, nil
}

func (f *fakeJobs) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	id, ok := args[0].(string)
	if !ok {
		return fakeRow{err: errors.New("the job id is not a string")}
	}
	switch sql {
	case waitForJobQuery:
		f.waited = append(f.waited, id)
		return fakeRow{value: f.succeeded[id]}
	case jobDetailsQuery:
		return fakeRow{value: f.details[id]}
	default:
		return fakeRow{err: errors.New("unexpected query: " + sql)}
	}
}

// fakeRows are the rows of the pending jobs. The methods the job wait does
// not use panic.
type fakeRows struct {
	pgx.Rows

	jobs []job
	next int
	err  error
}

func (r *fakeRows) Close()     {}
func (r *fakeRows) Err() error { return r.err }

func (r *fakeRows) Next() bool {
	if r.next == len(r.jobs) {
		return false
	}
	r.next++
	return true
}

func (r *fakeRows) Scan(dest ...any) error {
	j := r.jobs[r.next-1]
	for i, value := range []string{j.id, j.kind, j.object} {
		d, ok := dest[i].(*string)
		if !ok {
			return errors.New("unexpected destination")
		}
		*d = value
	}
	return nil
}

// fakeRow is a row of one column.
type fakeRow struct {
	value any
	err   error
}

func (r fakeRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	var ok bool
	switch d := dest[0].(type) {
	case *bool:
		*d, ok = r.value.(bool)
	case *string:
		*d, ok = r.value.(string)
	}
	if !ok {
		return errors.New("unexpected destination")
	}
	return nil
}

const testMigration = "2 (00002_orders_total.sql)"

func TestWaitForJobs_WaitsForEveryJobInProgress(t *testing.T) {
	t.Parallel()
	db := &fakeJobs{
		pending: []job{
			{id: "wqhu6ewifze5xitg3umt24h5ua", kind: "INDEX_BUILD", object: "public.orders_total"},
			{id: "kkngzf33dndl3daacxehpx5eba", kind: "ANALYZE", object: "public.orders"},
		},
		succeeded: map[string]bool{"wqhu6ewifze5xitg3umt24h5ua": true, "kkngzf33dndl3daacxehpx5eba": true},
	}

	require.NoError(t, waitForJobs(t.Context(), db, testMigration))
	assert.Equal(t, []string{"wqhu6ewifze5xitg3umt24h5ua", "kkngzf33dndl3daacxehpx5eba"}, db.waited)
}

func TestWaitForJobs_NothingInProgress(t *testing.T) {
	t.Parallel()
	db := &fakeJobs{}

	require.NoError(t, waitForJobs(t.Context(), db, testMigration))
	assert.Empty(t, db.waited)
}

func TestWaitForJobs_AFailedJobFailsNamingTheJobAndTheMigration(t *testing.T) {
	t.Parallel()
	db := &fakeJobs{
		pending: []job{
			{id: "wqhu6ewifze5xitg3umt24h5ua", kind: "INDEX_BUILD", object: "public.orders_total"},
			{id: "kkngzf33dndl3daacxehpx5eba", kind: "ANALYZE", object: "public.orders"},
		},
		succeeded: map[string]bool{"wqhu6ewifze5xitg3umt24h5ua": false},
		details:   map[string]string{"wqhu6ewifze5xitg3umt24h5ua": "duplicate key value violates unique constraint"},
	}

	err := waitForJobs(t.Context(), db, testMigration)

	require.ErrorIs(t, err, errJobFailed)
	for _, part := range []string{"wqhu6ewifze5xitg3umt24h5ua", "INDEX_BUILD", "public.orders_total", testMigration, "duplicate key value"} {
		require.ErrorContains(t, err, part)
	}
	assert.Equal(t, []string{"wqhu6ewifze5xitg3umt24h5ua"}, db.waited, "nothing runs after a failed job")
}

// TestWaitForJobs_SkippedWithoutSysJobs covers Postgres, which has no
// sys.jobs, whether the error comes with the query or with its rows.
func TestWaitForJobs_SkippedWithoutSysJobs(t *testing.T) {
	t.Parallel()
	for name, db := range map[string]*fakeJobs{
		"undefined table":     {queryErr: pgError("42P01")},
		"invalid schema name": {queryErr: pgError("3F000")},
		"error on the rows":   {rowsErr: pgError("42P01")},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			require.NoError(t, waitForJobs(t.Context(), db, testMigration))
		})
	}
}

func TestWaitForJobs_AnotherReadFailureFails(t *testing.T) {
	t.Parallel()
	db := &fakeJobs{queryErr: pgError("08006")}

	err := waitForJobs(t.Context(), db, testMigration)

	require.ErrorIs(t, err, db.queryErr)
	assert.ErrorContains(t, err, testMigration)
}
