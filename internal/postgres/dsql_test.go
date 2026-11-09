package postgres

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The tests below need neither a cluster nor the network: the pool connects
// lazily, and signing a token is local.

// testEndpoint is the endpoint of a cluster that does not exist.
const testEndpoint = "abcdefghijklmnopqrstuvwxyz.dsql.eu-west-1.on.aws"

// connectTestDSQL opens a DSQL pool to testEndpoint with static credentials.
func connectTestDSQL(t *testing.T, opts ...Option) *pgxpool.Pool {
	t.Helper()
	pool, err := ConnectDSQL(t.Context(), DSQL{
		Endpoint:    testEndpoint,
		Region:      "eu-west-1",
		Credentials: credentials.NewStaticCredentialsProvider("AKIDEXAMPLE", "secret", ""),
	}, opts...)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// token runs the pool's BeforeConnect, as pgxpool does before it opens a
// connection, and returns the connection config it produced.
func token(t *testing.T, pool *pgxpool.Pool) (password, host string, port uint16) {
	t.Helper()
	cfg := pool.Config()
	cc := cfg.ConnConfig.Copy()
	if err := cfg.BeforeConnect(t.Context(), cc); err != nil {
		t.Fatalf("before connect: %v", err)
	}
	return cc.Password, cc.Host, cc.Port
}

func TestConnectDSQL_SizesThePoolForLambda(t *testing.T) {
	t.Parallel()
	cfg := connectTestDSQL(t).Config()

	if cfg.MaxConns != 1 {
		t.Errorf("MaxConns = %d, want 1 (one invocation at a time)", cfg.MaxConns)
	}
	if cfg.MaxConnLifetime != 55*time.Minute || cfg.MaxConnLifetimeJitter != 30*time.Second {
		t.Errorf("lifetime = %s + %s, want 55m + 30s, below DSQL's hour", cfg.MaxConnLifetime, cfg.MaxConnLifetimeJitter)
	}
	if cfg.MaxConnIdleTime != 10*time.Minute {
		t.Errorf("MaxConnIdleTime = %s, want 10m", cfg.MaxConnIdleTime)
	}
	if cfg.PrepareConn != nil {
		t.Error("a PrepareConn is set: DSQL rejects the statement_timeout it would set")
	}
	if cfg.AfterConnect == nil || cfg.ConnConfig.Tracer == nil {
		t.Error("the UTC timestamptz codec or the query tracer is missing")
	}
}

func TestConnectDSQL_ConnectsToTheEndpointOverVerifiedTLS(t *testing.T) {
	// A PGHOST makes ParseConfig("") add a plaintext fallback to it, which
	// the pool must not keep.
	t.Setenv("PGHOST", "db.example.com")
	cc := connectTestDSQL(t).Config().ConnConfig

	if cc.Host != testEndpoint || cc.Port != 5432 {
		t.Errorf("address = %s:%d, want %s:5432", cc.Host, cc.Port, testEndpoint)
	}
	if cc.User != "admin" || cc.Database != "postgres" {
		t.Errorf("user/database = %s/%s, want admin/postgres", cc.User, cc.Database)
	}
	if cc.TLSConfig == nil || cc.TLSConfig.ServerName != testEndpoint || cc.TLSConfig.InsecureSkipVerify {
		t.Errorf("TLS = %+v, want verified against %s", cc.TLSConfig, testEndpoint)
	}
	if len(cc.Fallbacks) != 0 {
		t.Errorf("fallbacks = %d, want none", len(cc.Fallbacks))
	}
}

// TestConnectDSQL_SignsATokenForEveryConnection checks that the token is
// signed when a connection opens, not when the pool is created: a connection
// a warm environment opens after the earlier token expired gets a fresh one.
func TestConnectDSQL_SignsATokenForEveryConnection(t *testing.T) {
	t.Parallel()
	pool := connectTestDSQL(t)
	created := time.Now().UTC()
	// X-Amz-Date counts seconds.
	time.Sleep(1100 * time.Millisecond)

	password, _, _ := token(t, pool)
	if !strings.HasPrefix(password, testEndpoint+"?") {
		t.Fatalf("token %q is not a presigned URL for the endpoint", password)
	}
	signed, err := url.Parse("https://" + password)
	if err != nil {
		t.Fatalf("parse token: %v", err)
	}
	query := signed.Query()
	if query.Get("Action") != "DbConnectAdmin" || query.Get("X-Amz-Signature") == "" {
		t.Errorf("token query %v, want a signed DbConnectAdmin", query)
	}
	if query.Get("X-Amz-Expires") != "900" {
		t.Errorf("X-Amz-Expires = %s, want 900 (15 minutes)", query.Get("X-Amz-Expires"))
	}
	at, err := time.Parse("20060102T150405Z", query.Get("X-Amz-Date"))
	if err != nil {
		t.Fatalf("parse X-Amz-Date: %v", err)
	}
	if !at.After(created.Truncate(time.Second)) {
		t.Errorf("token signed at %s, not after the pool was created at %s", at, created)
	}
}

func TestConnectDSQL_WithHostDialsTheTunnelButSignsForTheEndpoint(t *testing.T) {
	t.Parallel()
	pool := connectTestDSQL(t, WithHost("localhost", "15432"))

	password, host, port := token(t, pool)
	if host != "localhost" || port != 15432 {
		t.Errorf("dials %s:%d, want localhost:15432", host, port)
	}
	if !strings.HasPrefix(password, testEndpoint+"?") {
		t.Errorf("token %q is not signed for the endpoint", password)
	}
	if name := pool.Config().ConnConfig.TLSConfig.ServerName; name != testEndpoint {
		t.Errorf("TLS server name = %s, want %s", name, testEndpoint)
	}
}

func TestConnectDSQL_RejectsABadPort(t *testing.T) {
	t.Parallel()
	_, err := ConnectDSQL(t.Context(), DSQL{Endpoint: testEndpoint, Region: "eu-west-1"}, WithHost("", "not-a-port"))
	if !errors.Is(err, ErrInvalidPort) {
		t.Fatalf("err = %v, want ErrInvalidPort", err)
	}
}

func TestConnectDSQL_FailsWithoutARegion(t *testing.T) {
	t.Setenv("AWS_REGION", "")
	t.Setenv("AWS_DEFAULT_REGION", "")

	_, err := ConnectDSQL(t.Context(), DSQL{
		Endpoint:    "db.example.com",
		Credentials: credentials.NewStaticCredentialsProvider("AKIDEXAMPLE", "secret", ""),
	})
	if err == nil {
		t.Fatal("a pool without a region to sign its tokens for")
	}
}

// TestDSQLPoolStopsAQueryAtTheDeadline runs a pool tuned as ConnectDSQL tunes
// it against the test Postgres: with no statement_timeout set, a query still
// running at the context's deadline fails at that deadline, on the client.
func TestDSQLPoolStopsAQueryAtTheDeadline(t *testing.T) {
	t.Parallel()
	cfg, err := pgxpool.ParseConfig(testDatabaseURL())
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	tuneForDSQL(cfg)
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatalf("create pool: %v", err)
	}
	t.Cleanup(pool.Close)
	if pingErr := pool.Ping(t.Context()); pingErr != nil {
		t.Skipf("no test database available: %v", pingErr)
	}

	before := currentStatementTimeoutMs(t, connectRaw(t))

	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if after := currentStatementTimeoutMs(t, conn.Conn()); after != before {
		t.Errorf("statement_timeout = %dms after an acquire with a deadline, want the server's %dms", after, before)
	}
	conn.Release()

	start := time.Now()
	_, err = pool.Exec(ctx, "SELECT pg_sleep(5)")
	elapsed := time.Since(start)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the context's deadline", err)
	}
	if elapsed > 2*time.Second {
		t.Errorf("the query stopped after %s, well past its deadline", elapsed)
	}
}
