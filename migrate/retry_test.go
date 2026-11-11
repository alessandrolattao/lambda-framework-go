package migrate

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The tests below need no database: the clock, the randomness and the
// connection are fakes.

// pgError is a server error with the SQLSTATE code.
func pgError(code string) error {
	return &pgconn.PgError{Severity: "ERROR", Code: code, Message: "test failure " + code}
}

// fakeClock records the waits the backoff asks for, and lets each one pass
// at once.
type fakeClock struct {
	waits []time.Duration
}

func (c *fakeClock) after(d time.Duration) <-chan time.Time {
	c.waits = append(c.waits, d)
	ch := make(chan time.Time, 1)
	ch <- time.Time{}
	return ch
}

// testBackoff is a backoff on clock whose randomness always draws the
// longest wait, so that each wait is its ceiling.
func testBackoff(clock *fakeClock) backoff {
	return backoff{after: clock.after, int64N: func(n int64) int64 { return n - 1 }}
}

// ceilings are the longest waits before the five retries.
var ceilings = []time.Duration{
	100 * time.Millisecond,
	200 * time.Millisecond,
	400 * time.Millisecond,
	800 * time.Millisecond,
	1600 * time.Millisecond,
}

func TestIsConflict(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"serialization failure", pgError("40001"), true},
		{"data conflict", pgError("OC000"), true},
		{"schema conflict", pgError("OC001"), true},
		{"wrapped conflict", fmt.Errorf("exec: %w", pgError("OC000")), true},
		{"feature not supported", pgError("0A000"), false},
		{"unique violation", pgError("23505"), false},
		{"deadlock", pgError("40P01"), false},
		{"not a server error", errors.New("connection reset"), false},
		{"no error", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, isConflict(tc.err))
		})
	}
}

func TestBackoff_DelayIsFullJitterUnderACeiling(t *testing.T) {
	t.Parallel()
	var drawn []int64
	b := backoff{int64N: func(n int64) int64 {
		drawn = append(drawn, n)
		return n - 1
	}}

	for retry, ceiling := range ceilings {
		assert.Equal(t, ceiling, b.delay(retry), "retry %d", retry)
	}
	assert.Equal(t, maxBackoff, b.delay(5), "100 ms × 2^5 is over the 3 s cap")
	// The draw is in [0, ceiling]: Int64N(ceiling+1).
	assert.Equal(t, int64(100*time.Millisecond)+1, drawn[0])

	b.int64N = func(int64) int64 { return 0 }
	assert.Zero(t, b.delay(3), "full jitter goes down to no wait at all")
}

func TestBackoff_RetriesAConflictUntilItPasses(t *testing.T) {
	t.Parallel()
	clock := &fakeClock{}
	failures := []error{pgError("OC000"), pgError("40001")}
	calls := 0

	err := testBackoff(clock).do(t.Context(), func() error {
		calls++
		if calls <= len(failures) {
			return failures[calls-1]
		}
		return nil
	})

	require.NoError(t, err)
	assert.Equal(t, 3, calls)
	assert.Equal(t, ceilings[:2], clock.waits)
}

func TestBackoff_FailsAfterTheFifthRetry(t *testing.T) {
	t.Parallel()
	clock := &fakeClock{}
	calls := 0

	err := testBackoff(clock).do(t.Context(), func() error {
		calls++
		return pgError("OC001")
	})

	require.ErrorIs(t, err, errRetriesExhausted)
	pgErr, ok := errors.AsType[*pgconn.PgError](err)
	require.True(t, ok, "the last conflict is in the chain")
	assert.Equal(t, "OC001", pgErr.Code)
	assert.Equal(t, 1+maxRetries, calls, "the first attempt and five retries")
	assert.Equal(t, ceilings, clock.waits)
}

func TestBackoff_DoesNotRetryOtherFailures(t *testing.T) {
	t.Parallel()
	clock := &fakeClock{}
	calls := 0
	unsupported := pgError("0A000")

	err := testBackoff(clock).do(t.Context(), func() error {
		calls++
		return unsupported
	})

	require.ErrorIs(t, err, unsupported)
	assert.Equal(t, 1, calls)
	assert.Empty(t, clock.waits)
}

func TestBackoff_StopsWhenTheContextEnds(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	b := backoff{
		after:  func(time.Duration) <-chan time.Time { return nil }, // never
		int64N: func(n int64) int64 { return n - 1 },
	}

	err := b.do(ctx, func() error { return pgError("40001") })

	require.ErrorIs(t, err, context.Canceled)
	assert.True(t, isConflict(err), "the conflict that was being retried is in the chain")
}

// fakeConn is a pgx connection whose ExecContext fails with the next of its
// failures, then succeeds. Any other method panics: retryConn must not call
// it.
type fakeConn struct {
	pgxConn

	failures []error
	calls    int
	closed   bool
}

func (c *fakeConn) ExecContext(context.Context, string, []driver.NamedValue) (driver.Result, error) {
	c.calls++
	if c.calls <= len(c.failures) {
		return nil, c.failures[c.calls-1]
	}
	return driver.RowsAffected(1), nil
}

func (c *fakeConn) Close() error {
	c.closed = true
	return nil
}

func TestRetryConn_RetriesAStatementOutsideATransaction(t *testing.T) {
	t.Parallel()
	clock := &fakeClock{}
	conn := &fakeConn{failures: []error{pgError("OC001")}}
	rc := &retryConn{pgxConn: conn, inTransaction: func() bool { return false }, backoff: testBackoff(clock)}

	result, err := rc.ExecContext(t.Context(), "CREATE TABLE orders (id uuid PRIMARY KEY)", nil)

	require.NoError(t, err)
	assert.Equal(t, driver.RowsAffected(1), result)
	assert.Equal(t, 2, conn.calls)
	assert.Equal(t, ceilings[:1], clock.waits)
}

// TestRetryConn_DoesNotRetryInsideATransaction: a conflict aborted the whole
// transaction, so running its last statement again would be wrong.
func TestRetryConn_DoesNotRetryInsideATransaction(t *testing.T) {
	t.Parallel()
	clock := &fakeClock{}
	conn := &fakeConn{failures: []error{pgError("OC000")}}
	rc := &retryConn{pgxConn: conn, inTransaction: func() bool { return true }, backoff: testBackoff(clock)}

	_, err := rc.ExecContext(t.Context(), "INSERT INTO orders VALUES ($1)", nil)

	require.True(t, isConflict(err))
	require.NotErrorIs(t, err, errRetriesExhausted)
	assert.Equal(t, 1, conn.calls)
	assert.Empty(t, clock.waits)
}

// fakeConnector hands over conn.
type fakeConnector struct {
	driver.Connector

	conn driver.Conn
}

func (c fakeConnector) Connect(context.Context) (driver.Conn, error) { return c.conn, nil }

func TestRetryConnector_RejectsAConnectionThatIsNotPgx(t *testing.T) {
	t.Parallel()
	conn := &fakeConn{}

	_, err := retryConnector{Connector: fakeConnector{conn: conn}}.Connect(t.Context())

	require.ErrorIs(t, err, errNotPgx)
	assert.True(t, conn.closed, "the connection goes back")
}
