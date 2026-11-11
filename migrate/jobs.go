package migrate

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// The queries of the job wait, on DSQL's sys.jobs view, whose final states
// are completed and failed.
const (
	pendingJobsQuery = `SELECT job_id, job_type, coalesce(object_name, '') FROM sys.jobs WHERE status NOT IN ('completed', 'failed') ORDER BY start_time`
	waitForJobQuery  = `SELECT sys.wait_for_job($1)`
	jobDetailsQuery  = `SELECT coalesce(details, '') FROM sys.jobs WHERE job_id = $1`
)

// SQLSTATEs of a query on sys.jobs where there is no such view: Postgres.
const (
	codeUndefinedTable    = "42P01" // undefined_table
	codeInvalidSchemaName = "3F000" // invalid_schema_name
)

// errJobFailed marks an asynchronous job that ended failed.
var errJobFailed = errors.New("asynchronous job failed")

// querier is the part of *pgxpool.Pool the job wait uses.
type querier interface {
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
		var succeeded bool
		if err := db.QueryRow(ctx, waitForJobQuery, j.id).Scan(&succeeded); err != nil {
			return fmt.Errorf("wait for %s job %s on %q after migration %s: %w", j.kind, j.id, j.object, migration, err)
		}
		if !succeeded {
			return fmt.Errorf("%w: %s job %s on %q after migration %s: %s", errJobFailed, j.kind, j.id, j.object, migration, jobDetails(ctx, db, j.id))
		}
	}
	return nil
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

// jobDetails is the reason sys.jobs gives for a failed job, or why it could
// not be read.
func jobDetails(ctx context.Context, db querier, id string) string {
	var details string
	if err := db.QueryRow(ctx, jobDetailsQuery, id).Scan(&details); err != nil {
		return fmt.Sprintf("no details: %v", err)
	}
	return details
}
