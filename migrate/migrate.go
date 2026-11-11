package migrate

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"path"
	"strings"

	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/alessandrolattao/lambda-framework-go/framework"
)

// Result is what one invocation of Up did, which the invocation answers with.
type Result struct {
	// Version is the version of the database at the end of the invocation.
	Version int64 `json:"version"`
	// Applied are the versions this invocation applied, in the order it
	// applied them; empty when there was nothing to apply.
	Applied []int64 `json:"applied"`
}

// errNoDatabase is the failure of a container without a database pool.
var errNoDatabase = errors.New("migrations need a database, and the container has none: set the Aurora DSQL endpoint (WithDSQLEndpoint or DSQL_ENDPOINT) or a Postgres connection string (WithDatabaseURL or DATABASE_URL)")

// errTransactional marks the migration files that do not declare
// "-- +goose NO TRANSACTION".
var errTransactional = errors.New(`every migration must declare "-- +goose NO TRANSACTION", and these do not`)

// Up returns the business handler of a migration Lambda: on every invocation
// it applies the goose SQL migrations in fsys that the container's database
// has not applied yet, up to the latest, and answers with a Result. It
// accepts any payload, and ignores it.
//
//	//go:embed *.sql
//	var migrations embed.FS
//
//	func main() {
//		c := framework.New()
//		lambda.Start(c.Handle(migrate.Up(migrations)))
//	}
//
// The migrations run on the container's pool, one at a time, each statement
// a transaction of its own; after each migration Up waits for every
// asynchronous job DSQL still runs. See the package documentation for the
// rules a migration follows.
func Up(fsys fs.FS) func(context.Context, *framework.Container, jsontext.Value) (Result, error) {
	return func(ctx context.Context, c *framework.Container, _ jsontext.Value) (Result, error) {
		return up(ctx, c, fsys, newBackoff())
	}
}

// up applies the pending migrations, retrying conflicts with b.
func up(ctx context.Context, c *framework.Container, fsys fs.FS, b backoff) (_ Result, err error) {
	if c.DB == nil {
		return Result{}, errNoDatabase
	}

	db := sql.OpenDB(retryConnector{Connector: stdlib.GetPoolConnector(c.DB), backoff: b})
	// The pool owns the connections: database/sql keeps none, so that the one
	// goose used is back in the pool for the job wait, as pgx's
	// GetPoolConnector asks.
	db.SetMaxIdleConns(0)
	defer func() {
		if closeErr := db.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close the migrations' database handle: %w", closeErr))
		}
	}()

	provider, err := goose.NewProvider(goose.DialectAuroraDSQL, db, fsys,
		goose.WithIsolateDDL(true),
		goose.WithDisableGlobalRegistry(true),
		goose.WithSlog(logger(c)),
	)
	if err != nil {
		return Result{}, fmt.Errorf("read the migrations: %w", err)
	}
	if checkErr := requireNoTransaction(fsys, provider.ListSources()); checkErr != nil {
		return Result{}, checkErr
	}

	applied := []int64{}
	for {
		migration, upErr := provider.UpByOne(ctx)
		if errors.Is(upErr, goose.ErrNoNextVersion) {
			break
		}
		if upErr != nil {
			return Result{}, migrationError(upErr, applied)
		}
		applied = append(applied, migration.Source.Version)
		if waitErr := waitForJobs(ctx, c.DB, describe(migration.Source)); waitErr != nil {
			return Result{}, waitErr
		}
	}

	version, err := provider.GetDBVersion(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("read the database version: %w", err)
	}
	return Result{Version: version, Applied: applied}, nil
}

// logger is the container's logger, or slog's default for a container
// assembled without one.
func logger(c *framework.Container) *slog.Logger {
	if c.Logger == nil {
		return slog.Default()
	}
	return c.Logger.Logger
}

// migrationError wraps the failure of a migration. The versions applied
// before it stay recorded, and the next invocation starts from the one that
// failed.
func migrationError(err error, applied []int64) error {
	partial, ok := errors.AsType[*goose.PartialError](err)
	if !ok {
		return fmt.Errorf("apply the migrations (applied before the failure: %v): %w", applied, err)
	}
	return fmt.Errorf("migration %s failed (applied before it: %v): %w", describe(partial.Failed.Source), applied, err)
}

// describe names a migration in errors: its version and its file.
func describe(source *goose.Source) string {
	return fmt.Sprintf("%d (%s)", source.Version, path.Base(source.Path))
}

// requireNoTransaction checks, before anything runs, that every SQL migration
// declares "-- +goose NO TRANSACTION": each statement then runs in a
// transaction of its own, so no transaction holds two DDL statements or DDL
// with DML, whatever goose does with transactions, and a statement that
// failed committed nothing and can be run again alone.
func requireNoTransaction(fsys fs.FS, sources []*goose.Source) error {
	var missing []string
	for _, source := range sources {
		if source.Type != goose.TypeSQL {
			continue
		}
		content, err := fs.ReadFile(fsys, source.Path)
		if err != nil {
			return fmt.Errorf("read migration %s: %w", describe(source), err)
		}
		if !declaresNoTransaction(string(content)) {
			missing = append(missing, path.Base(source.Path))
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("%w: %s", errTransactional, strings.Join(missing, ", "))
	}
	return nil
}

// declaresNoTransaction reports whether a SQL migration has the annotation
// "-- +goose NO TRANSACTION" on a line of its own, read as goose's parser
// reads annotations: a line that starts with "--" and holds "+goose", its
// command compared without regard to case.
func declaresNoTransaction(migration string) bool {
	for line := range strings.Lines(migration) {
		line = strings.TrimRight(line, "\r\n")
		if !strings.HasPrefix(line, "--") || !strings.Contains(line, "+goose") {
			continue
		}
		command := strings.Replace(strings.ReplaceAll(line, "--", ""), "+goose", "", 1)
		if strings.EqualFold(strings.TrimSpace(command), "NO TRANSACTION") {
			return true
		}
	}
	return false
}
