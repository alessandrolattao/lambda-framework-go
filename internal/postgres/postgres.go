// Package postgres opens a pool tuned for Lambda, to Postgres (Connect) or to
// Aurora DSQL (ConnectDSQL).
package postgres

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-lambda-go/lambdacontext"
	"github.com/exaring/otelpgx"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrInvalidPort is returned when WithHost carries a port that is not a TCP port.
var ErrInvalidPort = errors.New("invalid postgres port")

// Option tunes Connect and ConnectDSQL.
type Option func(*options)

type options struct {
	host, port string
}

// WithHost connects to an overridden host/port while keeping everything else,
// e.g. to reach the database through a tunnel on localhost. With Connect the
// credentials and database name of the connection string stay; with
// ConnectDSQL the IAM token and the TLS verification stay those of the
// cluster endpoint. Empty values keep the original ones.
func WithHost(host, port string) Option {
	return func(o *options) {
		o.host, o.port = host, port
	}
}

// Connect opens a pgxpool from a connection string, in URL
// (postgres://user:pass@host:5432/db?sslmode=require) or keyword/value form.
//
// Unless the connection string sets them (pool_max_conns, pool_max_conn_*),
// the pool is sized and recycled for Lambda: one connection per invocation the
// execution environment can run at once, connections recycled before Aurora
// drops idle ones. Every connection scans timestamptz as UTC and, while the
// context carries a deadline, caps its statements to the time left. Every
// query is traced with OpenTelemetry, through the global tracer provider.
func Connect(ctx context.Context, connString string, opts ...Option) (*pgxpool.Pool, error) {
	var o options
	for _, opt := range opts {
		opt(&o)
	}

	cfg, err := pgxpool.ParseConfig(connString)
	if err != nil {
		return nil, fmt.Errorf("parse postgres config: %w", err)
	}

	if o.host != "" || o.port != "" {
		if hostErr := overrideHost(cfg, o.host, o.port); hostErr != nil {
			return nil, hostErr
		}
	}

	if !sets(connString, "pool_max_conns") {
		cfg.MaxConns = lambdaMaxConns()
	}
	// Recycle connections before Aurora silently closes idle ones, and close
	// idle connections proactively on warm-but-inactive instances. Jitter
	// prevents all Lambda instances from reconnecting at the same moment.
	if !sets(connString, "pool_max_conn_lifetime") {
		cfg.MaxConnLifetime = 30 * time.Minute
	}
	if !sets(connString, "pool_max_conn_lifetime_jitter") {
		cfg.MaxConnLifetimeJitter = maxConnLifetimeJitter
	}
	if !sets(connString, "pool_max_conn_idle_time") {
		cfg.MaxConnIdleTime = 5 * time.Minute
	}
	tuneForLambda(cfg)
	cfg.PrepareConn = setStatementTimeout

	// The pool outlives ctx, typically the first invocation's, which Lambda
	// cancels when that invocation returns: the connections the pool opens in
	// the background must not die with it.
	pool, err := pgxpool.NewWithConfig(context.WithoutCancel(ctx), cfg)
	if err != nil {
		return nil, fmt.Errorf("create postgres pool: %w", err)
	}
	return pool, nil
}

// maxConnLifetimeJitter spreads the reconnections of the Lambda instances
// that started together, so that they do not all reconnect at the same moment.
const maxConnLifetimeJitter = 30 * time.Second

// lambdaMaxConns is the pool size for the execution environment. A standard
// Lambda runs one invocation at a time per execution environment, so one
// connection is enough; pgxpool's default of max(4, NumCPU) would only waste
// database connections. An environment that runs several at once (Lambda
// Managed Instances) gets one each.
func lambdaMaxConns() int32 {
	return int32(min(max(lambdacontext.MaxConcurrency(), 1), 1<<16))
}

// tuneForLambda sets what every pool gets, whatever the server: timestamptz
// scanned as UTC, and every query traced.
//
// Every query becomes a span under the one in its context, named after its
// operation (SELECT, INSERT) and carrying its SQL text, never its parameters.
// The pool's acquire spans are left out: with one connection per invocation
// there is nothing to wait for, only noise in every trace.
func tuneForLambda(cfg *pgxpool.Config) {
	cfg.AfterConnect = registerTimestamptzUTC
	cfg.ConnConfig.Tracer = otelpgx.NewTracer(otelpgx.WithDisableAcquireTracer())
}

// sets reports whether the connection string sets the pool parameter key,
// in either of the forms pgxpool.ParseConfig accepts.
func sets(connString, key string) bool {
	return strings.Contains(connString, key+"=")
}

// overrideHost points every connection at host:port, dropping the fallback
// hosts of the original string: a tunnel is one address.
func overrideHost(cfg *pgxpool.Config, host, port string) error {
	if host != "" {
		cfg.ConnConfig.Host = host
	}
	if port != "" {
		p, err := parsePort(port)
		if err != nil {
			return err
		}
		cfg.ConnConfig.Port = p
	}
	cfg.ConnConfig.Fallbacks = nil
	return nil
}

// parsePort reads the port of WithHost.
func parsePort(port string) (uint16, error) {
	p, err := strconv.ParseUint(port, 10, 16)
	if err != nil {
		return 0, fmt.Errorf("%w %q: %w", ErrInvalidPort, port, err)
	}
	return uint16(p), nil
}

// registerTimestamptzUTC overrides the default timestamptz codec on every new
// pgx connection so that scanned values carry time.UTC location. Combined with
// pgtype.Timestamptz.MarshalJSON (which formats via time.RFC3339Nano), this
// guarantees JSON output ends in "Z" instead of "+00:00", which strict RFC 3339
// parsers like Safari require. ScanLocation is read only on the decode path,
// so writes are unaffected.
func registerTimestamptzUTC(_ context.Context, conn *pgx.Conn) error {
	conn.TypeMap().RegisterType(&pgtype.Type{
		Name:  "timestamptz",
		OID:   pgtype.TimestamptzOID,
		Codec: &pgtype.TimestamptzCodec{ScanLocation: time.UTC},
	})
	return nil
}

// timeoutSetKey marks, in the connection's custom data, that
// setStatementTimeout changed the session's statement_timeout.
const timeoutSetKey = "lambda-framework-go/statement-timeout"

// setStatementTimeout runs before every acquire, through pgconn rather than
// pgx so that it is not traced: a span for it would precede every query.
//
// It sets statement_timeout on the connection to match the
// remaining time in the current invocation context. This ensures orphaned DB
// queries (connection dropped by Lambda timeout or SIGKILL before the query
// finishes) are killed by Postgres itself rather than running indefinitely.
//
// statement_timeout is a session setting and the pool reuses connections, so
// a context without a deadline (a Fargate task, a test, a detached context)
// puts back the server default when an earlier acquire had capped this
// connection; otherwise it changes nothing. An already expired deadline is
// left to the context, which fails the query on its own.
func setStatementTimeout(ctx context.Context, conn *pgx.Conn) (bool, error) {
	data := conn.PgConn().CustomData()

	deadline, ok := ctx.Deadline()
	if !ok {
		if data[timeoutSetKey] == nil {
			return true, nil
		}
		if err := conn.PgConn().Exec(ctx, "SET statement_timeout = DEFAULT").Close(); err != nil {
			return false, fmt.Errorf("reset statement_timeout: %w", err)
		}
		delete(data, timeoutSetKey)
		return true, nil
	}

	remaining := time.Until(deadline)
	if remaining <= 0 {
		return true, nil
	}
	// Milliseconds() floors: a sub-millisecond (but still positive) budget would
	// round to 0, which Postgres reads as "no timeout". Clamp to 1ms so a tight
	// deadline still caps the query instead of disabling the guard.
	ms := max(remaining.Milliseconds(), 1)
	if err := conn.PgConn().Exec(ctx, fmt.Sprintf("SET statement_timeout = %d", ms)).Close(); err != nil {
		return false, fmt.Errorf("set statement_timeout: %w", err)
	}
	data[timeoutSetKey] = true
	return true, nil
}
