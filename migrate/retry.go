package migrate

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"
)

// The retries of a statement that lost an optimistic concurrency conflict:
// up to maxRetries more attempts, each after a wait drawn between 0 and
// min(maxBackoff, firstBackoff × 2^retry).
const (
	maxRetries   = 5
	firstBackoff = 100 * time.Millisecond
	maxBackoff   = 3 * time.Second
)

// SQLSTATEs DSQL fails a transaction with when it lost an optimistic
// concurrency conflict.
const (
	codeSerializationFailure = "40001" // serialization_failure
	codeDataConflict         = "OC000" // a row was changed by a concurrent transaction
	codeSchemaConflict       = "OC001" // the schema was changed by a concurrent transaction
)

// isConflict reports whether err is an optimistic concurrency conflict, the
// failure DSQL means to be retried.
func isConflict(err error) bool {
	pgErr, ok := errors.AsType[*pgconn.PgError](err)
	if !ok {
		return false
	}
	switch pgErr.Code {
	case codeSerializationFailure, codeDataConflict, codeSchemaConflict:
		return true
	default:
		return false
	}
}

// backoff retries a statement that lost a conflict, with full jitter: each
// wait is random between 0 and its ceiling, as AWS recommends ("Exponential
// Backoff And Jitter", AWS Architecture Blog), so that the runs that collided
// do not collide again.
type backoff struct {
	// after is the clock: a channel that receives once the duration passed.
	after func(time.Duration) <-chan time.Time
	// int64N is the randomness: a number in [0, n).
	int64N func(n int64) int64
}

// newBackoff returns the backoff on the real clock and randomness.
func newBackoff() backoff {
	return backoff{after: time.After, int64N: rand.Int64N}
}

// delay is the wait before retry number retry, counted from 0.
func (b backoff) delay(retry int) time.Duration {
	ceiling := min(maxBackoff, firstBackoff<<retry)
	return time.Duration(b.int64N(int64(ceiling) + 1))
}

// errRetriesExhausted marks a conflict that outlived every retry.
var errRetriesExhausted = errors.New("conflict still there after every retry")

// do runs op, and runs it again after a wait for as long as it loses a
// conflict, at most maxRetries more times. Any other failure is returned at
// once.
func (b backoff) do(ctx context.Context, op func() error) error {
	err := op()
	for retry := 0; isConflict(err); retry++ {
		if retry == maxRetries {
			return fmt.Errorf("%w (%d retries): %w", errRetriesExhausted, maxRetries, err)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("stop retrying a conflict: %w: %w", context.Cause(ctx), err)
		case <-b.after(b.delay(retry)):
		}
		err = op()
	}
	return err
}

// retryConnector hands database/sql the connections of the container's pool
// (stdlib.GetPoolConnector), each wrapped in a retryConn: goose takes a
// *sql.DB and retries nothing, so the retry goes under it. The pool keeps
// signing the IAM tokens and tracing the queries.
type retryConnector struct {
	driver.Connector

	backoff backoff
}

// errNotPgx is the failure of a connector that did not hand over a pgx
// connection.
var errNotPgx = errors.New("not a pgx database/sql connection")

// Connect implements driver.Connector.
func (c retryConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.Connector.Connect(ctx)
	if err != nil {
		return nil, err //nolint:wrapcheck // database/sql reads the driver's errors as they are (driver.ErrBadConn)
	}
	pc, ok := conn.(*stdlib.Conn)
	if !ok {
		return nil, errors.Join(fmt.Errorf("%w: %T", errNotPgx, conn), conn.Close())
	}
	return &retryConn{
		pgxConn: pc,
		inTransaction: func() bool {
			return pc.Conn().PgConn().TxStatus() != 'I'
		},
		backoff: c.backoff,
	}, nil
}

// pgxConn is every interface a pgx database/sql connection implements, which
// retryConn forwards.
type pgxConn interface {
	driver.Conn
	driver.ConnPrepareContext
	driver.ConnBeginTx
	driver.ExecerContext
	driver.QueryerContext
	driver.Pinger
	driver.NamedValueChecker
	driver.SessionResetter
}

var _ pgxConn = (*stdlib.Conn)(nil)

// retryConn is a pgx database/sql connection whose ExecContext retries a
// statement that lost a conflict, when it runs outside a transaction: such a
// statement is a transaction of its own and committed nothing, so running it
// again is safe. Inside a transaction the conflict aborted the whole
// transaction, and the statement goes back to its caller as it is.
type retryConn struct {
	pgxConn

	// inTransaction reports whether the session is inside a transaction,
	// from the status the server sent last: it sees a transaction database/sql
	// began and one a migration opened with BEGIN alike.
	inTransaction func() bool
	backoff       backoff
}

// ExecContext implements driver.ExecerContext.
func (c *retryConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	if c.inTransaction() {
		return c.pgxConn.ExecContext(ctx, query, args) //nolint:wrapcheck // database/sql reads the driver's errors as they are
	}
	var result driver.Result
	err := c.backoff.do(ctx, func() error {
		var execErr error
		result, execErr = c.pgxConn.ExecContext(ctx, query, args)
		return execErr //nolint:wrapcheck // read by isConflict, then handed to database/sql as it is
	})
	return result, err
}
