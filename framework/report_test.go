package framework

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/alessandrolattao/powertools-lambda-go/logging"
	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda/messages"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/alessandrolattao/lambda-framework-go/response"
)

// Static test errors: the linter forbids building errors inline, and these
// stand in for what a real handler returns.
var (
	errUpsert     = errors.New("upsert intervention: connection refused")
	errNoRows     = errors.New("no rows in result set")
	errSecondary  = errors.New("and another one")
	errBoom       = errors.New("boom")
	errDisposable = errors.New("disposable email")
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

// recordOf returns the record whose message is msg, as the Powertools logger
// wrote it.
func recordOf(t *testing.T, logged, msg string) string {
	t.Helper()
	for line := range strings.Lines(logged) {
		if strings.Contains(line, `"message":"`+msg+`"`) {
			return line
		}
	}
	t.Fatalf("no record %q in:\n%s", msg, logged)
	return ""
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

func TestReject_OfNothingIsNothing(t *testing.T) {
	t.Parallel()

	require.NoError(t, Reject(nil), "a handler can return Reject(err) without checking err first")
}

func TestReject_TheCallerGetsTheSameError(t *testing.T) {
	t.Parallel()

	invalid := &validationError{field: "email"}
	rejected := Reject(fmt.Errorf("check sign-up: %w", invalid))

	assert.Equal(t, "check sign-up: invalid email", rejected.Error(), "the message is the error's own")
	got, ok := errors.AsType[*validationError](rejected)
	require.True(t, ok, "errors.As reaches the error through the mark")
	assert.Same(t, invalid, got)
	require.ErrorIs(t, Reject(errDisposable), errDisposable, "errors.Is too")
}

func TestReportError_WritesARejectionAtWarn(t *testing.T) {
	t.Parallel()

	logger, logged := bufferLogger()
	got := reportError(t.Context(), logger, Reject(errDisposable))

	require.ErrorIs(t, got, errDisposable, "the caller must receive exactly the error it passed in")
	assert.Contains(t, logged.String(), `"level":"WARN","msg":"handler rejected the event","err":"disposable email"`)
	assert.Equal(t, 1, strings.Count(logged.String(), "\n"), "exactly one record")
}

func TestHandle_ARejectionIsWrittenAtWarnAndAnythingElseAtError(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name      string
		err       error
		wantLevel string
		wantMsg   string
	}{
		{"a rejection", Reject(errDisposable), "WARN", "handler rejected the event"},
		{"a rejection wrapped further", fmt.Errorf("check sign-up: %w", Reject(errDisposable)), "WARN", "handler rejected the event"},
		{"any other error", fmt.Errorf("check sign-up: %w", errDisposable), "ERROR", "handler returned an error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			c, logged := testContainer()
			h := c.Handle(func(context.Context, *Container, jsontext.Value) (any, error) {
				return nil, tc.err
			})

			_, err := h.Call(lambdaContext(t), jsontext.Value(`{}`))
			require.ErrorIs(t, err, errDisposable, "the runtime must still see the failure")

			out := logged.String()
			assert.Equal(t, 1, strings.Count(out, `"level":"WARN"`)+strings.Count(out, `"level":"ERROR"`),
				"one record of the failure, at one level")
			assert.Contains(t, recordOf(t, out, tc.wantMsg), `"level":"`+tc.wantLevel+`"`)
		})
	}
}

func TestHandle_TheWarnRecordNamesTheErrorItMarks(t *testing.T) {
	t.Parallel()

	// record runs a handler returning err and reads back the record of its
	// failure.
	record := func(t *testing.T, err error, msg string) map[string]any {
		t.Helper()
		c, logged := testContainer()
		_, got := c.Handle(func(context.Context, *Container, jsontext.Value) (any, error) {
			return nil, err
		}).Call(lambdaContext(t), jsontext.Value(`{}`))
		require.Error(t, got)
		line := recordOf(t, logged.String(), msg)
		assert.Equal(t, 1, strings.Count(line, `"exception_name"`), "one name, the framework's")
		var fields map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &fields), line)
		return fields
	}

	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"an application's own type", fmt.Errorf("check sign-up: %w", &validationError{field: "email"}), "validationError"},
		{"a library's type", fmt.Errorf("save the user: %w", &pgconn.PgError{Code: "23505"}), "PgError"},
		{"a plain error", fmt.Errorf("check sign-up: %w", errDisposable), "error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			failed := record(t, tc.err, "handler returned an error")
			rejected := record(t, Reject(tc.err), "handler rejected the event")
			assert.Equal(t, tc.want, failed["exception_name"])
			assert.Equal(t, failed["exception_name"], rejected["exception_name"], "the logs name it as Lambda does")
			assert.Equal(t, failed["exception"], rejected["exception"], "the message is unchanged")
			assert.Equal(t, "WARN", rejected["level"])
		})
	}
}

func TestConsume_ARejectionStillFailsTheEvent(t *testing.T) {
	t.Parallel()

	c, logged := testContainer()
	h := c.Consume(func(context.Context, *Container, events.SQSEvent) error {
		return Reject(errDisposable)
	})

	err := h.Call(lambdaContext(t), events.SQSEvent{})
	require.ErrorIs(t, err, errDisposable, "Lambda must retry the event, and it may reach the DLQ")
	assert.Contains(t, recordOf(t, logged.String(), "handler rejected the event"), `"level":"WARN"`)
	assert.NotContains(t, logged.String(), `"level":"ERROR"`)
}

func TestHandle_ACognitoPreSignUpRejection(t *testing.T) {
	t.Parallel()

	c, logged := testContainer()
	h := c.Handle(func(_ context.Context, _ *Container, e events.CognitoEventUserPoolsPreSignup) (events.CognitoEventUserPoolsPreSignup, error) {
		if strings.HasSuffix(e.Request.UserAttributes["email"], "@mailinator.com") {
			return e, Reject(errDisposable)
		}
		return e, nil
	})

	_, err := h.Invoke(lambdaContext(t), []byte(`{"version":"1","triggerSource":"PreSignUp_SignUp",`+
		`"region":"eu-west-1","userPoolId":"eu-west-1_pool","userName":"someone",`+
		`"request":{"userAttributes":{"email":"someone@mailinator.com"}},"response":{}}`))

	failure, ok := errors.AsType[messages.InvokeResponse_Error](err)
	require.True(t, ok, "the invocation fails, so Cognito refuses the sign-up")
	assert.Equal(t, "disposable email", failure.Message, "Cognito shows the rejection's message")
	assert.Equal(t, errorTypeHandler, failure.Type)

	out := logged.String()
	assert.Equal(t, 1, strings.Count(out, `"level":"WARN"`), "one record of the rejection")
	record := recordOf(t, out, "handler rejected the event")
	assert.Contains(t, record, `"level":"WARN"`)
	assert.Contains(t, record, `"exception":"disposable email"`)
	assert.Contains(t, record, `"exception_name":"error"`, "named as the error would be without Reject")
	assert.Contains(t, record, `"function_request_id":"req-1"`)
	assert.NotContains(t, out, `"level":"ERROR"`, "a refusal the handler meant must not reach alerting")
}

func TestAPI_ARejectionAnswersThe500EnvelopeAtError(t *testing.T) {
	t.Parallel()

	rejecting := func(context.Context, *Container, struct{}) (item, error) {
		return item{}, Reject(errDisposable)
	}
	for _, tc := range []struct {
		name     string
		wantCode string
		serve    func(t *testing.T, c *Container) (status int, code string)
	}{
		{"an HTTP API route", "serverError", func(t *testing.T, c *Container) (int, string) {
			t.Helper()
			c.Get("/sign-up", rejecting)
			resp, err := c.HTTP().Call(lambdaContext(t), apiRequest(http.MethodGet, "/sign-up", ""))
			require.NoError(t, err)
			return resp.StatusCode, decodeEnvelope(t, resp).Code
		}},
		{"a REST API route", "serverError", func(t *testing.T, c *Container) (int, string) {
			t.Helper()
			c.Get("/sign-up", rejecting)
			resp, err := c.REST().Call(lambdaContext(t), restRequest(http.MethodGet, "/sign-up", ""))
			require.NoError(t, err)
			return resp.StatusCode, decodeRESTEnvelope(t, resp).Code
		}},
		{"a 5xx of package response", "signUpFailed", func(t *testing.T, c *Container) (int, string) {
			t.Helper()
			c.Get("/sign-up", func(context.Context, *Container, struct{}) (item, error) {
				return item{}, response.InternalServerError(Reject(errDisposable), "signUpFailed", "could not sign up", nil)
			})
			resp, err := c.HTTP().Call(lambdaContext(t), apiRequest(http.MethodGet, "/sign-up", ""))
			require.NoError(t, err)
			return resp.StatusCode, decodeEnvelope(t, resp).Code
		}},
		{"a stream", "serverError", func(t *testing.T, c *Container) (int, string) {
			t.Helper()
			metadata, body := readStream(t, c, func(context.Context, *Container, events.APIGatewayProxyRequest, http.ResponseWriter) error {
				return Reject(errDisposable)
			}, streamRequest())
			var envelope response.ErrorBody
			require.NoError(t, json.Unmarshal([]byte(body), &envelope), body)
			return decodeMetadata(t, metadata).StatusCode, envelope.Code
		}},
		{"a handler of an API event", "serverError", func(t *testing.T, c *Container) (int, string) {
			t.Helper()
			resp, err := apiHandler(c, func() (events.APIGatewayV2HTTPResponse, error) {
				return events.APIGatewayV2HTTPResponse{}, Reject(errDisposable)
			}).Call(lambdaContext(t), apiRequest(http.MethodPost, "/sign-up", ""))
			require.NoError(t, err)
			return resp.StatusCode, decodeEnvelope(t, resp).Code
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			c, logged := testContainer()
			status, code := tc.serve(t, c)
			assert.Equal(t, http.StatusInternalServerError, status, "a route says no with a 4xx of package response")
			assert.Equal(t, tc.wantCode, code)

			out := logged.String()
			assert.Equal(t, 1, strings.Count(out, `"level":"ERROR"`), "a failure like any other, reported once")
			assert.NotContains(t, out, `"level":"WARN"`)
			assert.Contains(t, recordOf(t, out, "handler returned an error"), "disposable email")
		})
	}
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
