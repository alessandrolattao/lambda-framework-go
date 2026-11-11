package migrate

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The tests below need no database: sys.jobs and sys.wait_for_job are a fake.

// dsqlWaitForJob is the only statement that runs DSQL's sys.wait_for_job, a
// procedure: CALL, with the job id bound as a parameter. A SELECT of it fails
// with SQLSTATE 42809, "is a procedure".
const dsqlWaitForJob = "CALL sys.wait_for_job($1)"

// fakeJobs is a database with DSQL's sys.jobs view and its sys.wait_for_job
// procedure: the jobs not yet in a final state, and the status and details
// each one has in sys.jobs. A job without a status is no longer listed.
type fakeJobs struct {
	pending   []job
	queryErr  error // returned by the query of the pending jobs
	rowsErr   error // returned by its rows, after the last one
	status    map[string]string
	details   map[string]string
	statusErr error // returned by the read of a job's status

	waited []string
}

func (f *fakeJobs) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if sql != dsqlWaitForJob {
		return pgconn.CommandTag{}, errors.New("unexpected statement: " + sql)
	}
	id, err := jobID(args)
	if err != nil {
		return pgconn.CommandTag{}, err
	}
	f.waited = append(f.waited, id)
	return pgconn.NewCommandTag("CALL"), nil
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
	if sql != jobStatusQuery {
		return fakeRow{err: errors.New("unexpected query: " + sql)}
	}
	id, err := jobID(args)
	if err != nil {
		return fakeRow{err: err}
	}
	if f.statusErr != nil {
		return fakeRow{err: f.statusErr}
	}
	status, listed := f.status[id]
	if !listed {
		return fakeRow{err: pgx.ErrNoRows}
	}
	return fakeRow{values: []string{status, f.details[id]}}
}

// jobID is the job id a statement binds as its only parameter.
func jobID(args []any) (string, error) {
	if len(args) != 1 {
		return "", errors.New("the job id is not the only parameter")
	}
	id, ok := args[0].(string)
	if !ok {
		return "", errors.New("the job id is not a string")
	}
	return id, nil
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
	return fakeRow{values: []string{j.id, j.kind, j.object}}.Scan(dest...)
}

// fakeRow is a row of text columns.
type fakeRow struct {
	values []string
	err    error
}

func (r fakeRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	if len(dest) != len(r.values) {
		return errors.New("unexpected number of destinations")
	}
	for i, value := range r.values {
		d, ok := dest[i].(*string)
		if !ok {
			return errors.New("unexpected destination")
		}
		*d = value
	}
	return nil
}

const testMigration = "2 (00002_orders_total.sql)"

func TestWaitForJobs_WaitsForEveryJobInProgressWithCall(t *testing.T) {
	t.Parallel()
	db := &fakeJobs{
		pending: []job{
			{id: "wqhu6ewifze5xitg3umt24h5ua", kind: "INDEX_BUILD", object: "public.orders_total"},
			{id: "kkngzf33dndl3daacxehpx5eba", kind: "ANALYZE", object: "public.orders"},
		},
		status: map[string]string{"wqhu6ewifze5xitg3umt24h5ua": "completed", "kkngzf33dndl3daacxehpx5eba": "completed"},
	}

	require.NoError(t, waitForJobs(t.Context(), db, testMigration))
	assert.Equal(t, []string{"wqhu6ewifze5xitg3umt24h5ua", "kkngzf33dndl3daacxehpx5eba"}, db.waited)
}

// TestWaitForJobs_SucceedsWhenTheJobCompletedOrIsNoLongerListed covers how a
// job that did not fail is read after the wait: completed, or gone from
// sys.jobs.
func TestWaitForJobs_SucceedsWhenTheJobCompletedOrIsNoLongerListed(t *testing.T) {
	t.Parallel()
	for name, status := range map[string]map[string]string{
		"completed":        {"vhmo5s7k5zdfdpvl2lrmebaek4": "completed"},
		"no longer listed": {},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			db := &fakeJobs{
				pending: []job{{id: "vhmo5s7k5zdfdpvl2lrmebaek4", kind: "ANALYZE", object: "public.goose_db_version"}},
				status:  status,
			}

			require.NoError(t, waitForJobs(t.Context(), db, testMigration))
			assert.Equal(t, []string{"vhmo5s7k5zdfdpvl2lrmebaek4"}, db.waited)
		})
	}
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
		status:  map[string]string{"wqhu6ewifze5xitg3umt24h5ua": "failed", "kkngzf33dndl3daacxehpx5eba": "completed"},
		details: map[string]string{"wqhu6ewifze5xitg3umt24h5ua": "duplicate key value violates unique constraint"},
	}

	err := waitForJobs(t.Context(), db, testMigration)

	require.ErrorIs(t, err, errJobFailed)
	for _, part := range []string{"wqhu6ewifze5xitg3umt24h5ua", "INDEX_BUILD", "public.orders_total", testMigration, "duplicate key value"} {
		require.ErrorContains(t, err, part)
	}
	assert.Equal(t, []string{"wqhu6ewifze5xitg3umt24h5ua"}, db.waited, "nothing runs after a failed job")
}

// TestWaitForJobs_AJobStillInProgressAfterTheCallFails covers a call that
// returned while sys.jobs still lists the job in a state that is not final.
func TestWaitForJobs_AJobStillInProgressAfterTheCallFails(t *testing.T) {
	t.Parallel()
	for _, status := range []string{"submitted", "processing"} {
		t.Run(status, func(t *testing.T) {
			t.Parallel()
			db := &fakeJobs{
				pending: []job{
					{id: "wqhu6ewifze5xitg3umt24h5ua", kind: "INDEX_BUILD", object: "public.orders_total"},
					{id: "kkngzf33dndl3daacxehpx5eba", kind: "ANALYZE", object: "public.orders"},
				},
				status: map[string]string{"wqhu6ewifze5xitg3umt24h5ua": status, "kkngzf33dndl3daacxehpx5eba": "completed"},
			}

			err := waitForJobs(t.Context(), db, testMigration)

			require.ErrorIs(t, err, errJobNotEnded)
			require.NotErrorIs(t, err, errJobFailed)
			for _, part := range []string{"wqhu6ewifze5xitg3umt24h5ua", "INDEX_BUILD", "public.orders_total", testMigration, status} {
				require.ErrorContains(t, err, part)
			}
			assert.Equal(t, []string{"wqhu6ewifze5xitg3umt24h5ua"}, db.waited, "nothing runs after a job still in progress")
		})
	}
}

func TestWaitForJobs_AFailedStatusReadFails(t *testing.T) {
	t.Parallel()
	db := &fakeJobs{
		pending:   []job{{id: "vhmo5s7k5zdfdpvl2lrmebaek4", kind: "ANALYZE", object: "public.goose_db_version"}},
		statusErr: pgError("08006"),
	}

	err := waitForJobs(t.Context(), db, testMigration)

	require.ErrorIs(t, err, db.statusErr)
	require.NotErrorIs(t, err, errJobFailed)
	assert.EqualError(t, err, `read the status of ANALYZE job vhmo5s7k5zdfdpvl2lrmebaek4 on "public.goose_db_version" after migration 2 (00002_orders_total.sql): ERROR: test failure 08006 (SQLSTATE 08006)`)
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
