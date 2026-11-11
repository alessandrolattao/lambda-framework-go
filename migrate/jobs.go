package migrate

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// The statements of the job wait, on DSQL's sys.jobs view, whose final states
// are completed and failed, and on its sys.wait_for_job, a procedure that
// returns once the job is in one of them.
const (
	pendingJobsQuery = `SELECT job_id, job_type, coalesce(object_name, '') FROM sys.jobs WHERE status NOT IN ('completed', 'failed') ORDER BY start_time`
	waitForJobCall   = `CALL sys.wait_for_job($1)`
	jobStatusQuery   = `SELECT status, coalesce(details, '') FROM sys.jobs WHERE job_id = $1`
)

// The final states of a job in sys.jobs.
const (
	statusCompleted = "completed"
	statusFailed    = "failed"
)

// SQLSTATEs of a query on sys.jobs where there is no such view: Postgres.
const (
	codeUndefinedTable    = "42P01" // undefined_table
	codeInvalidSchemaName = "3F000" // invalid_schema_name
)

var (
	// errJobFailed marks an asynchronous job that ended failed.
	errJobFailed = errors.New("asynchronous job failed")
	// errJobNotEnded marks an asynchronous job that sys.jobs still lists as
	// submitted or processing after sys.wait_for_job returned.
	errJobNotEnded = errors.New("asynchronous job not ended after the wait")
)

// querier is the part of *pgxpool.Pool the job wait uses.
type querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// job is an asynchronous job in sys.jobs.
type job struct {
	id, kind, object string
}

// waitForJobs waits for every asynchronous job DSQL still runs, such as the
// index build a CREATE INDEX ASYNC leaves behind, after migration: the next
// migration runs on a schema that is complete. A job that ends failed fails
// the run. Where there is no sys.jobs (Postgres) there is nothing to wait for.
func waitForJobs(ctx context.Context, db querier, migration string) error {
	jobs, err := pendingJobs(ctx, db)
	if noJobsView(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read the asynchronous jobs after migration %s: %w", migration, err)
	}
	for _, j := range jobs {
		if err := waitForJob(ctx, db, j, migration); err != nil {
			return err
		}
	}
	return nil
}

// waitForJob calls sys.wait_for_job on j, then reads from sys.jobs how j
// ended, since what the call returns is not documented: a failed job fails
// the run, with the details sys.jobs gives, and so does a job still submitted
// or processing; a completed job, or one no longer listed, does not.
func waitForJob(ctx context.Context, db querier, j job, migration string) error {
	if _, err := db.Exec(ctx, waitForJobCall, j.id); err != nil {
		return fmt.Errorf("wait for %s job %s on %q after migration %s: %w", j.kind, j.id, j.object, migration, err)
	}
	var status, details string
	err := db.QueryRow(ctx, jobStatusQuery, j.id).Scan(&status, &details)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read the status of %s job %s on %q after migration %s: %w", j.kind, j.id, j.object, migration, err)
	}
	switch status {
	case statusCompleted:
		return nil
	case statusFailed:
		return fmt.Errorf("%w: %s job %s on %q after migration %s: %s", errJobFailed, j.kind, j.id, j.object, migration, details)
	default: // submitted or processing
		return fmt.Errorf("%w: %s job %s on %q after migration %s: status %s", errJobNotEnded, j.kind, j.id, j.object, migration, status)
	}
}

// pendingJobs reads the jobs not yet in a final state, the oldest first.
func pendingJobs(ctx context.Context, db querier) ([]job, error) {
	rows, err := db.Query(ctx, pendingJobsQuery)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var jobs []job
	for rows.Next() {
		var j job
		if err := rows.Scan(&j.id, &j.kind, &j.object); err != nil {
			return nil, err //nolint:wrapcheck // wrapped by waitForJobs
		}
		jobs = append(jobs, j)
	}
	return jobs, rows.Err()
}

// noJobsView reports whether err says that the database has no sys.jobs.
func noJobsView(err error) bool {
	pgErr, ok := errors.AsType[*pgconn.PgError](err)
	return ok && (pgErr.Code == codeUndefinedTable || pgErr.Code == codeInvalidSchemaName)
}
