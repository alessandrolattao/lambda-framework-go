package postgres

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/awslabs/aurora-dsql-connectors/go/pgx/dsql"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DSQL is the Aurora DSQL cluster ConnectDSQL connects to.
type DSQL struct {
	// Endpoint is the cluster endpoint, <cluster id>.dsql.<region>.on.aws.
	Endpoint string
	// Region is the region of the cluster. Empty, the connector reads it from
	// the endpoint, then from AWS_REGION and AWS_DEFAULT_REGION.
	Region string
	// Credentials sign the IAM tokens. Nil, the connector loads the default
	// AWS credential chain.
	Credentials aws.CredentialsProvider
}

// ConnectDSQL opens a pgxpool to an Aurora DSQL cluster through the AWS
// connector (github.com/awslabs/aurora-dsql-connectors/go/pgx), as the
// database user admin. DSQL has no password: the connector signs a fresh IAM
// token for every new connection, so a connection opened after the previous
// token expired still gets in, and it verifies the server certificate against
// the endpoint.
//
// The pool is sized for Lambda as Connect sizes it, one connection per
// invocation the execution environment can run at once, and recycled with the
// connector's own lifetimes, below the hour after which DSQL closes a
// connection. Every connection scans timestamptz as UTC and every query is
// traced, as with Connect.
//
// The statements are not capped server side: DSQL rejects statement_timeout
// and limits every transaction to 5 minutes on its own. A query still running
// when the context's deadline passes fails on the client at that deadline,
// as pgx does by default.
func ConnectDSQL(ctx context.Context, cluster DSQL, opts ...Option) (*pgxpool.Pool, error) {
	var o options
	for _, opt := range opts {
		opt(&o)
	}

	// The connector wants a pool config made by ParseConfig; it fills in the
	// endpoint, the database, the user, TLS and the IAM token itself.
	cfg, err := pgxpool.ParseConfig("")
	if err != nil {
		return nil, fmt.Errorf("parse dsql pool config: %w", err)
	}
	tuneForDSQL(cfg)
	if o.host != "" || o.port != "" {
		dial, dialErr := dialThrough(o.host, o.port)
		if dialErr != nil {
			return nil, dialErr
		}
		cfg.BeforeConnect = dial
	}

	// The pool outlives ctx, as in Connect.
	pool, err := dsql.NewPool(context.WithoutCancel(ctx), dsql.Config{
		Host:                      cluster.Endpoint,
		Region:                    cluster.Region,
		CustomCredentialsProvider: cluster.Credentials,
	}, cfg)
	if err != nil {
		return nil, fmt.Errorf("create dsql pool: %w", err)
	}
	return pool, nil
}

// tuneForDSQL sizes and recycles a DSQL pool for Lambda. It sets no
// PrepareConn: the statement_timeout Connect sets before every acquire would
// fail every acquire on DSQL.
//
// ParseConfig("") with libpq's default sslmode, prefer, can leave a plaintext
// fallback to the default host, which the connector does not remove: the
// fallbacks are dropped so that every connection goes to the endpoint, over
// verified TLS.
func tuneForDSQL(cfg *pgxpool.Config) {
	cfg.MaxConns = lambdaMaxConns()
	cfg.MaxConnLifetime = dsql.DefaultMaxConnLifetime
	cfg.MaxConnLifetimeJitter = maxConnLifetimeJitter
	cfg.MaxConnIdleTime = dsql.DefaultMaxConnIdleTime
	tuneForLambda(cfg)
	cfg.ConnConfig.Fallbacks = nil
}

// dialThrough returns the BeforeConnect hook that points every connection at
// host:port. The connector runs it before it signs the token: the token and
// the TLS server name stay those of the endpoint, so a tunnel to the cluster
// works with the same verification.
func dialThrough(host, port string) (func(context.Context, *pgx.ConnConfig) error, error) {
	var p uint16
	if port != "" {
		var err error
		if p, err = parsePort(port); err != nil {
			return nil, err
		}
	}
	return func(_ context.Context, cc *pgx.ConnConfig) error {
		if host != "" {
			cc.Host = host
		}
		if p != 0 {
			cc.Port = p
		}
		cc.Fallbacks = nil
		return nil
	}, nil
}
