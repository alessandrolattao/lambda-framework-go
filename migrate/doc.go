// Package migrate runs the goose SQL migrations of a migration Lambda on the
// container's database: an Aurora DSQL cluster, or Postgres.
//
// A migration Lambda is a main package that embeds its goose SQL files and
// hands Up to Handle, like any other business handler: it gets the
// container's logging, tracing, error reporting and database pool.
//
//	package main
//
//	import (
//		"embed"
//
//		"github.com/aws/aws-lambda-go/lambda"
//
//		"github.com/alessandrolattao/lambda-framework-go/framework"
//		"github.com/alessandrolattao/lambda-framework-go/migrate"
//	)
//
//	//go:embed *.sql
//	var migrations embed.FS
//
//	func main() {
//		c := framework.New()
//		lambda.Start(c.Handle(migrate.Up(migrations)))
//	}
//
// The function connects as the container does: to DSQL with the cluster
// endpoint (WithDSQLEndpoint or DSQL_ENDPOINT), in the region in AWS_REGION,
// as the database user admin with an IAM token, so its role needs
// dsql:DbConnectAdmin on the cluster. Every invocation applies the migrations
// the database has not applied yet, up to the latest, and answers with what
// it did:
//
//	{"version": 3, "applied": [2, 3]}
//
// An invocation with nothing new to apply applies nothing and succeeds. goose
// logs through the container's logger, at Info: every statement it runs and
// every migration it applies, so the logs of a failed deploy show its last
// statement.
//
// # Migrations on DSQL
//
// A DSQL transaction holds at most one DDL statement, and never DDL together
// with DML. Every migration file therefore declares that goose must not wrap
// it in a transaction, so that each of its statements runs as a transaction
// of its own:
//
//	-- +goose NO TRANSACTION
//	-- +goose Up
//	CREATE TABLE orders (id uuid PRIMARY KEY, total bigint NOT NULL);
//	CREATE INDEX ASYNC orders_total ON orders (total);
//
// Up checks every file before it applies anything, and fails naming the files
// without the annotation. Migrations only go up: Up runs no Down section and
// no Go migration.
//
// DSQL builds an index created with CREATE INDEX ASYNC in a job of its own,
// after the statement committed. After each migration Up waits, with
// sys.wait_for_job, for every asynchronous job not yet completed, so the next
// migration finds the schema complete; a job that ends failed fails the
// invocation. The function's timeout must cover these waits.
//
// A statement that loses an optimistic concurrency conflict (SQLSTATE 40001,
// OC000 or OC001) committed nothing, and Up runs it again, up to 5 times:
// after a wait drawn between 0 and 100 ms, then up to 200 ms, doubling each
// time, never more than 3 seconds. When a migration still fails, so does the
// invocation: the migrations applied before it stay applied, and the next
// invocation starts again from the one that failed.
//
// There is no lock: two runs on the same database must never overlap. Invoke
// a context's migration Lambda once per deploy, and wait for it to answer.
//
// On Postgres, where there is no sys.jobs, the job wait is skipped and the
// rest is the same, which is how this package is tested without a cluster.
package migrate
