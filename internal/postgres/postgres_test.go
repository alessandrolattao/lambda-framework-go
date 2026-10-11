package postgres

import (
	"context"
	"encoding/json/v2"
	"errors"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// The tests below need a real Postgres. They skip when none is reachable at
// TEST_DATABASE_URL (or the local default), so `go test ./...` stays green on
// a machine without one.

// defaultTestDatabaseURL is the local default; TEST_DATABASE_URL overrides it.
const defaultTestDatabaseURL = "postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable"

// rfc3339UTCZ matches "YYYY-MM-DDTHH:MM:SS(.frac)?Z", the only timestamp
// shape we accept on the wire. Anything with a numeric offset (+02:00) or a
// space separator is a regression of the UTC-scan configuration.
var rfc3339UTCZ = regexp.MustCompile(`^"\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?Z"$`)

// connectRaw opens a single pgx connection to the test DB with the same
// AfterConnect hook Connect installs, skipping the test when no database is
// reachable.
func connectRaw(t *testing.T) *pgx.Conn {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		dsn = defaultTestDatabaseURL
	}
	conn, err := pgx.Connect(t.Context(), dsn)
	if err != nil {
		t.Skipf("no test database available: %v", err)
	}
	t.Cleanup(func() {
		// t.Context() is already cancelled when cleanups run.
		if closeErr := conn.Close(context.Background()); closeErr != nil {
			t.Logf("close conn: %v", closeErr)
		}
	})
	if err := registerTimestamptzUTC(t.Context(), conn); err != nil {
		t.Fatalf("register timestamptz codec: %v", err)
	}
	return conn
}

// TestTimestamptzScansAsUTC asserts that the AfterConnect codec override forces
// every timestamptz value to UTC location, so pgtype.Timestamptz.MarshalJSON
// emits the trailing "Z" instead of a numeric offset. This is the load-bearing
// invariant for the entire timestamps-on-the-wire contract.
func TestTimestamptzScansAsUTC(t *testing.T) {
	t.Parallel()
	conn := connectRaw(t)

	// Force a non-UTC server timezone for the session so we know the codec
	// (not the server) is what brings the value back to UTC.
	if _, err := conn.Exec(t.Context(), "SET TIME ZONE 'America/Los_Angeles'"); err != nil {
		t.Fatalf("set timezone: %v", err)
	}

	var got pgtype.Timestamptz
	if err := conn.QueryRow(t.Context(), "SELECT now()").Scan(&got); err != nil {
		t.Fatalf("query: %v", err)
	}

	if loc := got.Time.Location(); loc != time.UTC {
		t.Fatalf("expected UTC location, got %s", loc)
	}

	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !rfc3339UTCZ.Match(encoded) {
		t.Fatalf("JSON output %s does not match RFC 3339 UTC Z shape", encoded)
	}
}

// TestTimestamptzRoundTrip writes a timestamptz into a temporary table, reads
// it back, and verifies that (a) the instant is preserved across the
// round-trip and (b) the JSON output is RFC 3339 UTC with the Z suffix. This
// proves that the codec only affects scan presentation, never the underlying
// instant stored by Postgres.
func TestTimestamptzRoundTrip(t *testing.T) {
	t.Parallel()
	conn := connectRaw(t)

	// A temporary table lives and dies with this session, so the test needs no
	// schema of its own and leaves nothing behind.
	if _, err := conn.Exec(t.Context(), "CREATE TEMP TABLE roundtrip (at timestamptz NOT NULL)"); err != nil {
		t.Fatalf("create temp table: %v", err)
	}

	// Write from a non-UTC zone to make sure the instant survives the round-trip
	// regardless of the originating location.
	tokyo, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		t.Fatalf("load location: %v", err)
	}
	written := time.Date(2026, 4, 27, 14, 30, 45, 123456000, tokyo)

	var read pgtype.Timestamptz
	err = conn.QueryRow(t.Context(),
		"INSERT INTO roundtrip (at) VALUES ($1) RETURNING at",
		pgtype.Timestamptz{Time: written, Valid: true},
	).Scan(&read)
	if err != nil {
		t.Fatalf("insert+select: %v", err)
	}

	if !read.Valid {
		t.Fatalf("read value is invalid")
	}
	if loc := read.Time.Location(); loc != time.UTC {
		t.Fatalf("expected UTC location, got %s", loc)
	}
	if !read.Time.Equal(written) {
		t.Fatalf("instant mismatch: wrote %s, read %s", written.Format(time.RFC3339Nano), read.Time.Format(time.RFC3339Nano))
	}

	encoded, err := json.Marshal(read)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !rfc3339UTCZ.Match(encoded) {
		t.Fatalf("JSON output %s does not match RFC 3339 UTC Z shape", encoded)
	}
}

// currentStatementTimeoutMs reads statement_timeout as an integer number of
// milliseconds. pg_settings.setting stores it as a plain numeric string in ms
// (e.g. "30000"), which avoids parsing the human-readable SHOW form ("30s").
func currentStatementTimeoutMs(t *testing.T, conn *pgx.Conn) int {
	t.Helper()
	var ms int
	err := conn.QueryRow(t.Context(),
		"SELECT setting::int FROM pg_settings WHERE name = 'statement_timeout'").Scan(&ms)
	if err != nil {
		t.Fatalf("read statement_timeout: %v", err)
	}
	return ms
}

// TestSetStatementTimeoutFromDeadline asserts that a context carrying an
// invocation deadline makes setStatementTimeout install a server-side
// statement_timeout close to the remaining time. This is the happy path that
// every Lambda DB connection takes.
func TestSetStatementTimeoutFromDeadline(t *testing.T) {
	t.Parallel()
	conn := connectRaw(t)

	const window = 30 * time.Second
	ctx, cancel := context.WithDeadline(t.Context(), time.Now().Add(window))
	defer cancel()

	ok, err := setStatementTimeout(ctx, conn)
	if err != nil {
		t.Fatalf("setStatementTimeout: %v", err)
	}
	if !ok {
		t.Fatalf("expected ok=true")
	}

	ms := currentStatementTimeoutMs(t, conn)
	// Some milliseconds elapse between deadline capture and the SET, so allow a
	// small lower margin; it must never exceed the full window.
	if ms <= 0 || ms > int(window.Milliseconds()) {
		t.Fatalf("statement_timeout %dms is outside (0, %d]", ms, window.Milliseconds())
	}
	if ms < int(window.Milliseconds())-2000 {
		t.Fatalf("statement_timeout %dms is suspiciously far below the %d window", ms, window.Milliseconds())
	}
}

// TestSetStatementTimeoutNoDeadline asserts that a context WITHOUT a deadline
// (Fargate tasks, tests, CLI) is a no-op so long-running jobs are not capped. It
// asserts the setting is UNCHANGED rather than literally 0, since the server may
// carry a cluster-level default statement_timeout.
func TestSetStatementTimeoutNoDeadline(t *testing.T) {
	t.Parallel()
	conn := connectRaw(t)

	before := currentStatementTimeoutMs(t, conn)

	ok, err := setStatementTimeout(t.Context(), conn)
	if err != nil {
		t.Fatalf("setStatementTimeout: %v", err)
	}
	if !ok {
		t.Fatalf("expected ok=true")
	}

	if after := currentStatementTimeoutMs(t, conn); after != before {
		t.Fatalf("expected statement_timeout unchanged (no-op), was %dms now %dms", before, after)
	}
}

// TestSetStatementTimeoutExpiredDeadline asserts that an already-expired
// deadline is a no-op: the function returns before issuing any SET (which would
// otherwise fail on the dead context), leaving the timeout untouched. Like the
// no-deadline case it asserts the setting is unchanged, not literally 0.
func TestSetStatementTimeoutExpiredDeadline(t *testing.T) {
	t.Parallel()
	conn := connectRaw(t)

	before := currentStatementTimeoutMs(t, conn)

	ctx, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer cancel()

	ok, err := setStatementTimeout(ctx, conn)
	if err != nil {
		t.Fatalf("setStatementTimeout: %v", err)
	}
	if !ok {
		t.Fatalf("expected ok=true")
	}

	if after := currentStatementTimeoutMs(t, conn); after != before {
		t.Fatalf("expected statement_timeout unchanged (no-op), was %dms now %dms", before, after)
	}
}

// TestStatementTimeoutCancelsLongQuery is the load-bearing test: it proves the
// installed timeout actually makes Postgres kill an over-running query on its
// own, independent of the Go context. This is exactly the zombie-query
// scenario: a query whose client context is gone but whose server-side
// execution must still be terminated. pg_sleep runs with a fresh background context so the
// cancellation can only come from the server-side statement_timeout.
func TestStatementTimeoutCancelsLongQuery(t *testing.T) {
	t.Parallel()
	conn := connectRaw(t)

	ctx, cancel := context.WithDeadline(t.Context(), time.Now().Add(300*time.Millisecond))
	defer cancel()

	if _, err := setStatementTimeout(ctx, conn); err != nil {
		t.Fatalf("setStatementTimeout: %v", err)
	}

	_, err := conn.Exec(t.Context(), "SELECT pg_sleep(5)")
	if err == nil {
		t.Fatalf("expected the long query to be cancelled by statement_timeout")
	}

	if pgErr, ok := errors.AsType[*pgconn.PgError](err); !ok || pgErr.Code != pgerrcodeQueryCanceled {
		t.Fatalf("expected Postgres query_canceled (57014), got %v", err)
	}
}

// pgerrcodeQueryCanceled is the SQLSTATE Postgres returns when statement_timeout
// (or a cancel request) aborts a running statement.
const pgerrcodeQueryCanceled = "57014"

// TestSetStatementTimeoutResetsAfterADeadline covers a pooled connection that
// an invocation with a deadline capped, then an acquire without one: the old
// cap must not outlive the invocation that set it.
func TestSetStatementTimeoutResetsAfterADeadline(t *testing.T) {
	t.Parallel()
	conn := connectRaw(t)

	before := currentStatementTimeoutMs(t, conn)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if _, err := setStatementTimeout(ctx, conn); err != nil {
		t.Fatalf("setStatementTimeout with deadline: %v", err)
	}
	if capped := currentStatementTimeoutMs(t, conn); capped == before {
		t.Fatalf("expected a capped statement_timeout, still %dms", capped)
	}

	if _, err := setStatementTimeout(t.Context(), conn); err != nil {
		t.Fatalf("setStatementTimeout without deadline: %v", err)
	}
	if after := currentStatementTimeoutMs(t, conn); after != before {
		t.Fatalf("expected statement_timeout back to %dms, got %dms", before, after)
	}
}

// The tests below need no database: pgxpool connects lazily.

func TestConnect_SizesThePoolForLambda(t *testing.T) {
	t.Parallel()
	pool, err := Connect(t.Context(), "postgres://u:p@db.example.com:5432/app")
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	cfg := pool.Config()
	if cfg.MaxConns != 1 {
		t.Errorf("MaxConns = %d, want 1 (one invocation at a time)", cfg.MaxConns)
	}
	if cfg.MaxConnIdleTime != 5*time.Minute {
		t.Errorf("MaxConnIdleTime = %s, want 5m", cfg.MaxConnIdleTime)
	}
}

func TestConnect_PoolParametersInTheStringWin(t *testing.T) {
	t.Parallel()
	pool, err := Connect(t.Context(), "postgres://u:p@db.example.com:5432/app?pool_max_conns=7&pool_max_conn_idle_time=1m")
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	cfg := pool.Config()
	if cfg.MaxConns != 7 {
		t.Errorf("MaxConns = %d, want 7 from the connection string", cfg.MaxConns)
	}
	if cfg.MaxConnIdleTime != time.Minute {
		t.Errorf("MaxConnIdleTime = %s, want 1m from the connection string", cfg.MaxConnIdleTime)
	}
}

func TestConnect_WithHostKeepsTheCredentials(t *testing.T) {
	t.Parallel()
	pool, err := Connect(t.Context(), "postgres://u:p%40ss@db.example.com:5432/app", WithHost("localhost", "15432"))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	cc := pool.Config().ConnConfig
	if cc.Host != "localhost" || cc.Port != 15432 {
		t.Errorf("address = %s:%d, want localhost:15432", cc.Host, cc.Port)
	}
	if cc.User != "u" || cc.Password != "p@ss" || cc.Database != "app" {
		t.Errorf("credentials changed: %s / %s / %s", cc.User, cc.Password, cc.Database)
	}
}

func TestConnect_RejectsABadPort(t *testing.T) {
	t.Parallel()
	_, err := Connect(t.Context(), "postgres://u:p@db.example.com:5432/app", WithHost("", "not-a-port"))
	if !errors.Is(err, ErrInvalidPort) {
		t.Fatalf("err = %v, want ErrInvalidPort", err)
	}
}

// TestConnect_TracesQueriesButNotTheTimeout checks what a trace gets from the
// pool: one span per query, named after its operation, and nothing for the
// statement_timeout set before it.
func TestConnect_TracesQueriesButNotTheTimeout(t *testing.T) { //nolint:paralleltest // it swaps the global tracer provider
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		dsn = defaultTestDatabaseURL
	}
	previous := otel.GetTracerProvider()
	recorder := tracetest.NewSpanRecorder()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder)))
	t.Cleanup(func() { otel.SetTracerProvider(previous) })

	pool, err := Connect(t.Context(), dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	// otelpgx traces only under a span, as a handler always has one
	// (businessLogic): a query outside any trace produces no orphan span.
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	ctx, parent := otel.Tracer("test").Start(ctx, "handler")
	var one int
	err = pool.QueryRow(ctx, "SELECT 1").Scan(&one)
	parent.End()
	if err != nil {
		t.Skipf("no test database available: %v", err)
	}

	ended := recorder.Ended()
	names := make([]string, 0, len(ended))
	for _, span := range ended {
		names = append(names, span.Name())
		for _, kv := range span.Attributes() {
			if kv.Value.AsString() != "" && strings.HasPrefix(kv.Value.AsString(), "SET statement_timeout") {
				t.Errorf("the timeout was traced: %s", span.Name())
			}
		}
	}
	if !slices.Contains(names, "SELECT") {
		t.Errorf("no SELECT span among %v", names)
	}
}
