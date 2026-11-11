package migrate

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/alessandrolattao/powertools-lambda-go/logging"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/alessandrolattao/lambda-framework-go/framework"
	"github.com/alessandrolattao/lambda-framework-go/internal/postgres"
)

// migrations is a set of goose SQL files, by name.
func migrations(files map[string]string) fstest.MapFS {
	fsys := fstest.MapFS{}
	for name, content := range files {
		fsys[name] = &fstest.MapFile{Data: []byte(content)}
	}
	return fsys
}

// noTx starts a migration that runs each statement on its own.
const noTx = "-- +goose NO TRANSACTION\n-- +goose Up\n"

// quietLogger is a container logger that writes nowhere.
func quietLogger() *logging.Logger {
	return logging.New(logging.WithWriter(io.Discard))
}

// The tests below need no database.

func TestResult_EncodesAsTheInvocationAnswer(t *testing.T) {
	t.Parallel()
	for want, result := range map[string]Result{
		`{"version":3,"applied":[2,3]}`: {Version: 3, Applied: []int64{2, 3}},
		`{"version":3,"applied":[]}`:    {Version: 3, Applied: []int64{}},
	} {
		got, err := json.Marshal(result)
		require.NoError(t, err)
		assert.JSONEq(t, want, string(got))
	}
}

func TestUp_FailsWithoutADatabase(t *testing.T) {
	t.Parallel()
	fsys := migrations(map[string]string{"00001_orders.sql": noTx + "CREATE TABLE orders (id bigint PRIMARY KEY);\n"})

	_, err := Up(fsys)(t.Context(), &framework.Container{Logger: quietLogger()}, nil)

	require.ErrorIs(t, err, errNoDatabase)
	assert.ErrorContains(t, err, "DSQL_ENDPOINT")
}

func TestDeclaresNoTransaction(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, migration string
		want            bool
	}{
		{"annotation", noTx + "CREATE TABLE a (id int);\n", true},
		{"after the Up annotation", "-- +goose Up\n-- +goose NO TRANSACTION\nCREATE TABLE a (id int);\n", true},
		{"any case, no space", "--+goose no transaction\n-- +goose Up\nSELECT 1;\n", true},
		{"windows line ends", "-- +goose NO TRANSACTION\r\n-- +goose Up\r\nSELECT 1;\r\n", true},
		{"missing", "-- +goose Up\nCREATE TABLE a (id int);\n", false},
		{"indented, which goose rejects", "  -- +goose NO TRANSACTION\n-- +goose Up\nSELECT 1;\n", false},
		{"in a plain comment", "-- +goose Up\n-- remember: NO TRANSACTION\nSELECT 1;\n", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, declaresNoTransaction(tc.migration))
		})
	}
}

// TestUp_RefusesMigrationsInATransactionBeforeRunningAnything: the check
// names every file without the annotation, and the pool, which points
// nowhere, is never used.
func TestUp_RefusesMigrationsInATransactionBeforeRunningAnything(t *testing.T) {
	t.Parallel()
	pool, err := pgxpool.New(t.Context(), "postgres://u:p@127.0.0.1:1/app?connect_timeout=1")
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	fsys := migrations(map[string]string{
		"00001_orders.sql":    "-- +goose Up\nCREATE TABLE orders (id bigint PRIMARY KEY);\n",
		"00002_customers.sql": noTx + "CREATE TABLE customers (id bigint PRIMARY KEY);\n",
		"00003_seed.sql":      "-- +goose Up\nINSERT INTO customers VALUES (1);\n",
	})

	_, err = Up(fsys)(t.Context(), &framework.Container{Logger: quietLogger(), DB: pool}, nil)

	require.ErrorIs(t, err, errTransactional)
	require.ErrorContains(t, err, "00001_orders.sql, 00003_seed.sql")
	assert.NotContains(t, err.Error(), "00002_customers.sql")
	assert.Zero(t, pool.Stat().AcquireCount(), "nothing ran")
}

// The tests below need a real Postgres, at TEST_DATABASE_URL; they skip
// without it. DSQL has no local version, and the whole goose path is the same
// on Postgres, minus the job wait.

// postgresContainer returns a container whose pool, of one connection as on
// Lambda, works in a schema of its own, dropped at the end of the test.
func postgresContainer(t *testing.T) *framework.Container {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	admin, err := pgx.Connect(t.Context(), url)
	if err != nil {
		t.Skipf("no test database available: %v", err)
	}
	schema := pgx.Identifier{"migrate_test_" + strings.ToLower(rand.Text())}.Sanitize()
	if _, createErr := admin.Exec(t.Context(), "CREATE SCHEMA "+schema); createErr != nil {
		t.Fatalf("create schema: %v", createErr)
	}
	t.Cleanup(func() {
		// t.Context() is already cancelled when cleanups run.
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, dropErr := admin.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE"); dropErr != nil {
			t.Logf("drop schema: %v", dropErr)
		}
		if closeErr := admin.Close(ctx); closeErr != nil {
			t.Logf("close: %v", closeErr)
		}
	})

	cfg, err := pgxpool.ParseConfig(url)
	require.NoError(t, err)
	cfg.MaxConns = 1
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return &framework.Container{Logger: quietLogger(), DB: pool}
}

// testContext bounds a test that could deadlock on its one connection.
func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// dbVersion is the version goose recorded.
func dbVersion(t *testing.T, c *framework.Container) int64 {
	t.Helper()
	var version int64
	require.NoError(t, c.DB.QueryRow(t.Context(), "SELECT max(version_id) FROM goose_db_version").Scan(&version))
	return version
}

// tableExists reports whether the test's schema has the table.
func tableExists(t *testing.T, c *framework.Container, table string) bool {
	t.Helper()
	var exists bool
	require.NoError(t, c.DB.QueryRow(t.Context(), "SELECT to_regclass($1) IS NOT NULL", table).Scan(&exists))
	return exists
}

// TestUp_OnPostgres_AppliesTheMigrationsThenNothing runs the migration
// Lambda's handler as Lambda does, with a payload it ignores: the first
// invocation applies every migration, the second applies nothing.
func TestUp_OnPostgres_AppliesTheMigrationsThenNothing(t *testing.T) {
	t.Parallel()
	c := postgresContainer(t)
	ctx := testContext(t)
	h := c.Handle(Up(migrations(map[string]string{
		"00001_orders.sql":    noTx + "CREATE TABLE orders (id bigint PRIMARY KEY, total bigint NOT NULL);\nCREATE INDEX orders_total ON orders (total);\n",
		"00002_customers.sql": noTx + "CREATE TABLE customers (id bigint PRIMARY KEY, name text NOT NULL);\n",
		"00003_seed.sql":      noTx + "INSERT INTO customers VALUES (1, 'Ada');\nINSERT INTO orders VALUES (1, 42);\n",
	})))

	out, err := h.Invoke(ctx, []byte(`{"source":"deploy"}`))
	require.NoError(t, err)
	assert.JSONEq(t, `{"version":3,"applied":[1,2,3]}`, string(out))

	var total int64
	require.NoError(t, c.DB.QueryRow(ctx, "SELECT total FROM orders WHERE id = 1").Scan(&total))
	assert.Equal(t, int64(42), total)

	out, err = h.Invoke(ctx, []byte(`{}`))
	require.NoError(t, err)
	assert.JSONEq(t, `{"version":3,"applied":[]}`, string(out))
}

// TestUp_OnPostgres_RetriesAConflictUntilItPasses: a statement that fails
// with OC000 twice, as a DSQL statement that lost two conflicts, then passes.
// The sequence counts the attempts, outside any transaction.
func TestUp_OnPostgres_RetriesAConflictUntilItPasses(t *testing.T) {
	t.Parallel()
	c := postgresContainer(t)
	clock := &fakeClock{}
	fsys := migrations(map[string]string{
		"00001_attempts.sql": noTx + "CREATE SEQUENCE attempts;\n",
		"00002_conflict.sql": noTx + "-- +goose StatementBegin\n" +
			"DO $$ BEGIN IF nextval('attempts') < 3 THEN RAISE EXCEPTION 'lost a conflict' USING ERRCODE = 'OC000'; END IF; END $$;\n" +
			"-- +goose StatementEnd\n" +
			"CREATE TABLE after_conflict (id bigint PRIMARY KEY);\n",
	})

	result, err := up(testContext(t), c, fsys, testBackoff(clock))

	require.NoError(t, err)
	assert.Equal(t, Result{Version: 2, Applied: []int64{1, 2}}, result)
	assert.Equal(t, ceilings[:2], clock.waits)
	assert.True(t, tableExists(t, c, "after_conflict"))
}

// TestUp_OnPostgres_AFailingMigrationKeepsTheEarlierVersions: migration 3
// keeps losing its conflict, the invocation fails after the fifth retry with
// goose's PartialError, and migrations 1 and 2 stay applied.
func TestUp_OnPostgres_AFailingMigrationKeepsTheEarlierVersions(t *testing.T) {
	t.Parallel()
	c := postgresContainer(t)
	clock := &fakeClock{}
	fsys := migrations(map[string]string{
		"00001_orders.sql":    noTx + "CREATE TABLE orders (id bigint PRIMARY KEY);\n",
		"00002_customers.sql": noTx + "CREATE TABLE customers (id bigint PRIMARY KEY);\n",
		"00003_conflict.sql": noTx + "-- +goose StatementBegin\n" +
			"DO $$ BEGIN RAISE EXCEPTION 'lost a conflict' USING ERRCODE = '40001'; END $$;\n" +
			"-- +goose StatementEnd\n",
		"00004_invoices.sql": noTx + "CREATE TABLE invoices (id bigint PRIMARY KEY);\n",
	})

	_, err := up(testContext(t), c, fsys, testBackoff(clock))

	require.ErrorIs(t, err, errRetriesExhausted)
	partial, ok := errors.AsType[*goose.PartialError](err)
	require.True(t, ok, "goose's PartialError is in the chain: %v", err)
	assert.Equal(t, int64(3), partial.Failed.Source.Version)
	require.ErrorContains(t, err, "00003_conflict.sql")
	assert.Equal(t, ceilings, clock.waits)
	assert.Equal(t, int64(2), dbVersion(t, c))
	assert.False(t, tableExists(t, c, "invoices"), "nothing runs after the failed migration")
}

// TestRetryConnector_OnPostgres_RetriesOnlyOutsideATransaction checks the
// transaction status the retry reads from the server: a conflict inside a
// transaction, opened by database/sql or by a BEGIN of the migration's own,
// is not retried.
func TestRetryConnector_OnPostgres_RetriesOnlyOutsideATransaction(t *testing.T) {
	t.Parallel()
	c := postgresContainer(t)
	ctx := testContext(t)
	clock := &fakeClock{}
	db := sql.OpenDB(retryConnector{Connector: stdlib.GetPoolConnector(c.DB), backoff: testBackoff(clock)})
	db.SetMaxIdleConns(0)
	t.Cleanup(func() { assert.NoError(t, db.Close()) })
	const conflict = "DO $$ BEGIN RAISE EXCEPTION 'lost a conflict' USING ERRCODE = 'OC001'; END $$"

	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, conflict)
	require.True(t, isConflict(err))
	require.NoError(t, tx.Rollback())
	assert.Empty(t, clock.waits, "database/sql's transaction")

	conn, err := db.Conn(ctx)
	require.NoError(t, err)
	_, err = conn.ExecContext(ctx, "BEGIN")
	require.NoError(t, err)
	_, err = conn.ExecContext(ctx, conflict)
	require.True(t, isConflict(err))
	_, err = conn.ExecContext(ctx, "ROLLBACK")
	require.NoError(t, err)
	require.NoError(t, conn.Close())
	assert.Empty(t, clock.waits, "the migration's own BEGIN")

	_, err = db.ExecContext(ctx, conflict)
	require.ErrorIs(t, err, errRetriesExhausted)
	assert.Equal(t, ceilings, clock.waits, "outside a transaction")
}

// The test below needs a real Aurora DSQL cluster, at TEST_DSQL_ENDPOINT, in
// the region of the AWS config, and AWS credentials allowed
// dsql:DbConnectAdmin on it; it skips without them.

// TestUp_OnDSQL_WaitsForTheIndexJob applies a migration with two DDL
// statements and a CREATE INDEX ASYNC: the invocation answers only once the
// index's job has completed, and a second invocation applies nothing.
func TestUp_OnDSQL_WaitsForTheIndexJob(t *testing.T) {
	t.Parallel()
	endpoint := os.Getenv("TEST_DSQL_ENDPOINT")
	if endpoint == "" {
		t.Skip("TEST_DSQL_ENDPOINT is not set")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		t.Skipf("no AWS config: %v", err)
	}
	if _, credErr := cfg.Credentials.Retrieve(ctx); credErr != nil {
		t.Skipf("no AWS credentials: %v", credErr)
	}
	pool, err := postgres.ConnectDSQL(ctx, postgres.DSQL{Endpoint: endpoint, Region: cfg.Region, Credentials: cfg.Credentials})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	require.EqualValues(t, 1, pool.Config().MaxConns, "the search_path below holds on the pool's one connection")

	name := "migrate_test_" + strings.ToLower(rand.Text())
	schema := pgx.Identifier{name}.Sanitize()
	_, err = pool.Exec(ctx, "CREATE SCHEMA "+schema)
	require.NoError(t, err)
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), time.Minute)
		defer cleanupCancel()
		// One DDL statement per transaction, and DSQL has no DROP ... CASCADE.
		for _, table := range []string{"orders", "customers", "goose_db_version"} {
			if _, dropErr := pool.Exec(cleanupCtx, "DROP TABLE IF EXISTS "+schema+"."+table); dropErr != nil {
				t.Logf("drop table %s: %v", table, dropErr)
			}
		}
		if _, dropErr := pool.Exec(cleanupCtx, "DROP SCHEMA "+schema); dropErr != nil {
			t.Logf("drop schema: %v", dropErr)
		}
	})
	_, err = pool.Exec(ctx, "SET search_path = "+schema)
	require.NoError(t, err)

	c := &framework.Container{Logger: quietLogger(), DB: pool}
	h := c.Handle(Up(migrations(map[string]string{
		"00001_orders.sql": noTx +
			"CREATE TABLE orders (id uuid PRIMARY KEY, total bigint NOT NULL);\n" +
			"CREATE TABLE customers (id uuid PRIMARY KEY, name text NOT NULL);\n" +
			"CREATE INDEX ASYNC orders_total ON orders (total);\n",
	})))

	out, err := h.Invoke(ctx, []byte(`{}`))
	require.NoError(t, err)
	assert.JSONEq(t, `{"version":1,"applied":[1]}`, string(out))

	var status string
	require.NoError(t, pool.QueryRow(ctx,
		"SELECT status FROM sys.jobs WHERE job_type = 'INDEX_BUILD' AND object_name = $1 ORDER BY start_time DESC LIMIT 1",
		name+".orders_total",
	).Scan(&status))
	assert.Equal(t, "completed", status, "the invocation answered before the index was built")

	out, err = h.Invoke(ctx, []byte(`{}`))
	require.NoError(t, err)
	assert.JSONEq(t, `{"version":1,"applied":[]}`, string(out))
}
