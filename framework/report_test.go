package framework

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/alessandrolattao/powertools-lambda-go/logging"
)

// Static test errors: the linter forbids building errors inline, and these
// stand in for what a real handler returns.
var (
	errUpsert    = errors.New("upsert intervention: connection refused")
	errNoRows    = errors.New("no rows in result set")
	errSecondary = errors.New("and another one")
	errBoom      = errors.New("boom")
)

// bufferLogger returns a logger writing JSON into a buffer, which is what a
// test can read back.
func bufferLogger() (*slog.Logger, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	return slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})), buf
}

// testContainer is a container assembled by hand, the way a test of a
// business handler builds one, whose Powertools logger writes into the
// returned buffer.
func testContainer() (*Container, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	return &Container{Logger: logging.New(logging.WithWriter(buf), logging.WithLevel(slog.LevelDebug))}, buf
}

// quietLogger discards every record, for tests that do not read the logs.
func quietLogger() *logging.Logger {
	return logging.New(logging.WithWriter(io.Discard))
}

func TestReportError_LogsTheErrorTheHandlerReturned(t *testing.T) {
	t.Parallel()

	logger, logged := bufferLogger()
	got := reportError(t.Context(), logger, errUpsert)

	require.ErrorIs(t, got, errUpsert, "the caller must receive exactly the error it passed in")
	assert.Contains(t, logged.String(), "upsert intervention: connection refused",
		"a consumer that returns without logging left no trace at all until now")
	assert.Contains(t, logged.String(), `"level":"ERROR"`,
		"the record has to be Error, or the reporting wrapper never sees it")
}

func TestReportError_SaysNothingOnSuccess(t *testing.T) {
	t.Parallel()

	logger, logged := bufferLogger()

	got := reportError(t.Context(), logger, nil)

	require.NoError(t, got)
	assert.Empty(t, logged.String(), "an invocation that worked is not an event")
}

func TestReportError_KeepsTheWrappedError(t *testing.T) {
	t.Parallel()

	logger, logged := bufferLogger()
	got := reportError(t.Context(), logger, errors.Join(errNoRows, errSecondary))

	require.ErrorIs(t, got, errNoRows, "wrapping must survive, so callers can still match on the cause")
	assert.Contains(t, logged.String(), "no rows in result set")
}

func TestReportError_WritesNothingBeyondTheLog(t *testing.T) {
	t.Parallel()

	logger, logged := bufferLogger()

	// The whole contract of this function: one line, and the error back
	// untouched. Whoever reads that line is somebody else's problem.
	require.ErrorIs(t, reportError(t.Context(), logger, errBoom), errBoom)

	assert.Equal(t, 1, strings.Count(logged.String(), "\n"), "exactly one record")
}

func TestReportPanic_WritesTheStackAndReRaises(t *testing.T) {
	t.Parallel()

	logger, logged := bufferLogger()

	assert.PanicsWithValue(t, errBoom, func() {
		defer reportPanic(t.Context(), logger)
		panic(errBoom)
	}, "the panic has to keep going: Lambda must still record a failure")

	out := logged.String()
	assert.Contains(t, out, "handler panicked", "a panic is a failure like any other and takes the same road")
	assert.Contains(t, out, `"level":"ERROR"`, "at Error, or the reporting wrapper never sees it")
	assert.Contains(t, out, "reportPanic", "the stack is the only thing that says where it happened")
}

func TestReportPanic_DoesNothingWhenNothingPanicked(t *testing.T) {
	t.Parallel()

	logger, logged := bufferLogger()

	assert.NotPanics(t, func() {
		defer reportPanic(t.Context(), logger)
	})
	assert.Empty(t, logged.String())
}
