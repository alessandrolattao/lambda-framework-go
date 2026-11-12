package framework

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/alessandrolattao/powertools-lambda-go/logging"
	"github.com/alessandrolattao/powertools-lambda-go/metrics"
	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"

	"github.com/alessandrolattao/lambda-framework-go/request"
	"github.com/alessandrolattao/lambda-framework-go/response"
)

// delimiter separates the metadata from the body of a streamed response.
const delimiter = "\x00\x00\x00\x00\x00\x00\x00\x00"

// streamTimeout bounds the wait for what a stream must have sent already.
const streamTimeout = 5 * time.Second

// streamOp is the business handler of a streaming endpoint.
type streamOp = func(context.Context, *Container, events.APIGatewayProxyRequest, http.ResponseWriter) error

// streamRequest is the REST API request of the streaming tests: GET /prices.
func streamRequest() events.APIGatewayProxyRequest {
	return restRequest(http.MethodGet, "/prices", "")
}

// splitStream splits a streamed response into its metadata and its body.
func splitStream(t *testing.T, raw string) (metadata, body string) {
	t.Helper()
	metadata, body, found := strings.Cut(raw, delimiter)
	require.True(t, found, "no delimiter in %q", raw)
	return metadata, body
}

// readStream runs op for req and reads its whole response.
func readStream(t *testing.T, c *Container, op streamOp, req events.APIGatewayProxyRequest) (metadata, body string) {
	t.Helper()
	r, err := c.Stream(op)(lambdaContext(t), req)
	require.NoError(t, err)
	raw, err := io.ReadAll(r)
	require.NoError(t, err)
	return splitStream(t, string(raw))
}

// streamedMetadata is the metadata of a streamed response, in API Gateway's
// names.
type streamedMetadata struct {
	StatusCode        int                 `json:"statusCode"`
	Headers           map[string]string   `json:"headers"`
	MultiValueHeaders map[string][]string `json:"multiValueHeaders"`
	Cookies           []string            `json:"cookies"`
}

// decodeMetadata reads the metadata of a streamed response.
func decodeMetadata(t *testing.T, metadata string) streamedMetadata {
	t.Helper()
	var m streamedMetadata
	require.NoError(t, json.Unmarshal([]byte(metadata), &m), metadata)
	return m
}

// firstEvent reads stream up to the blank line that ends the first event,
// metadata included, while op waits for release; it fails the test, and lets
// op go, when that takes longer than streamTimeout.
func firstEvent(t *testing.T, stream *bufio.Reader, release chan struct{}) string {
	t.Helper()
	read := make(chan string, 1)
	go func() {
		var got strings.Builder
		for !strings.HasSuffix(got.String(), "\n\n") {
			b, err := stream.ReadByte()
			if err != nil {
				break
			}
			got.WriteByte(b)
		}
		read <- got.String()
	}()
	select {
	case got := <-read:
		return got
	case <-time.After(streamTimeout):
		close(release)
		t.Fatal("the flushed event did not reach the client while op was running")
		return ""
	}
}

func TestStream_OpensWithTheMetadataThenTheDelimiterThenTheBody(t *testing.T) {
	t.Parallel()

	c, _ := testContainer()
	metadata, body := readStream(t, c, func(_ context.Context, _ *Container, _ events.APIGatewayProxyRequest, w http.ResponseWriter) error {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Add("X-Region", "eu-west-1")
		w.Header().Add("X-Region", "eu-south-1")
		http.SetCookie(w, &http.Cookie{Name: "session", Value: "abc", Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode})
		http.SetCookie(w, &http.Cookie{Name: "theme", Value: "dark", Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode})
		w.WriteHeader(http.StatusAccepted)
		_, err := io.WriteString(w, "hello")
		return err
	}, streamRequest())

	assert.JSONEq(t, `{
		"statusCode": 202,
		"headers": {"Content-Type": "text/event-stream"},
		"multiValueHeaders": {"X-Region": ["eu-west-1", "eu-south-1"]},
		"cookies": [
			"session=abc; HttpOnly; Secure; SameSite=Strict",
			"theme=dark; HttpOnly; Secure; SameSite=Lax"
		]
	}`, metadata)
	assert.Equal(t, "hello", body)
}

func TestStream_TheStatusIs200UnlessOpSetsOne(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		op       streamOp
		metadata string
		body     string
	}{
		{
			"a write without a status",
			func(_ context.Context, _ *Container, _ events.APIGatewayProxyRequest, w http.ResponseWriter) error {
				_, err := io.WriteString(w, "data")
				return err
			},
			`{"statusCode":200}`, "data",
		},
		{
			"nothing written",
			func(context.Context, *Container, events.APIGatewayProxyRequest, http.ResponseWriter) error {
				return nil
			},
			`{"statusCode":200}`, "",
		},
		{
			"a status and nothing written",
			func(_ context.Context, _ *Container, _ events.APIGatewayProxyRequest, w http.ResponseWriter) error {
				w.WriteHeader(http.StatusNoContent)
				return nil
			},
			`{"statusCode":204}`, "",
		},
		{
			"a flush and nothing written",
			func(_ context.Context, _ *Container, _ events.APIGatewayProxyRequest, w http.ResponseWriter) error {
				w.WriteHeader(http.StatusCreated)
				return http.NewResponseController(w).Flush()
			},
			`{"statusCode":201}`, "",
		},
		{
			"the first status wins",
			func(_ context.Context, _ *Container, _ events.APIGatewayProxyRequest, w http.ResponseWriter) error {
				w.WriteHeader(http.StatusCreated)
				w.WriteHeader(http.StatusTeapot)
				_, err := io.WriteString(w, "x")
				return err
			},
			`{"statusCode":201}`, "x",
		},
		{
			"what follows the first byte changes nothing",
			func(_ context.Context, _ *Container, _ events.APIGatewayProxyRequest, w http.ResponseWriter) error {
				if _, err := io.WriteString(w, "a"); err != nil {
					return err
				}
				w.Header().Set("X-Late", "1")
				w.WriteHeader(http.StatusTeapot)
				_, err := io.WriteString(w, "b")
				return err
			},
			`{"statusCode":200}`, "ab",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c, _ := testContainer()
			metadata, body := readStream(t, c, tc.op, streamRequest())
			assert.JSONEq(t, tc.metadata, metadata)
			assert.Equal(t, tc.body, body)
		})
	}
}

func TestStream_AFlushedEventReachesTheClientBeforeOpReturns(t *testing.T) {
	t.Parallel()

	c, logged := testContainer()
	release := make(chan struct{})
	r, err := c.Stream(func(ctx context.Context, c *Container, _ events.APIGatewayProxyRequest, w http.ResponseWriter) error {
		w.Header().Set("Content-Type", "text/event-stream")
		if err := response.SSE(w, "tick", "1"); err != nil {
			return err
		}
		<-release
		c.Logger.InfoContext(ctx, "released")
		return response.SSE(w, "tick", "2")
	})(lambdaContext(t), streamRequest())
	require.NoError(t, err)
	stream := bufio.NewReader(r)

	metadata, body := splitStream(t, firstEvent(t, stream, release))
	assert.JSONEq(t, `{"statusCode":200,"headers":{"Content-Type":"text/event-stream"}}`, metadata)
	assert.Equal(t, "event: tick\ndata: 1\n\n", body)
	// op is waiting: nothing writes the logs now.
	assert.Contains(t, logged.String(), `"message":"API request received"`)
	assert.NotContains(t, logged.String(), `"message":"API request completed"`, "the stream is still open")

	close(release)
	rest, err := io.ReadAll(stream)
	require.NoError(t, err)
	assert.Equal(t, "event: tick\ndata: 2\n\n", string(rest))

	records := logged.String()
	assert.Contains(t, records, `"status":200,"routeKey":"GET /prices","durationMs":`)
	assert.Less(t, strings.Index(records, `"message":"released"`), strings.Index(records, `"message":"API request completed"`),
		"the request is completed when the stream closes")
}

func TestStream_AnErrorBeforeTheFirstByteAnswersWithTheEnvelope(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		err      error
		status   int
		code     string
		reported bool
	}{
		{"a client error", response.NotFound("priceNotFound", "no such price", nil), 404, "priceNotFound", false},
		{"a server error", response.InternalServerError(errUpsert, "feedFailed", "could not read the feed", nil), 500, "feedFailed", true},
		{"any other error", errBoom, 500, "serverError", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c, logged := testContainer()
			metadata, body := readStream(t, c, func(_ context.Context, _ *Container, _ events.APIGatewayProxyRequest, w http.ResponseWriter) error {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				return tc.err
			}, streamRequest())

			got := decodeMetadata(t, metadata)
			assert.Equal(t, tc.status, got.StatusCode)
			assert.Equal(t, map[string]string{"Content-Type": "application/json", "Cache-Control": "no-store"}, got.Headers,
				"the envelope's headers, not the stream's")
			var envelope response.ErrorBody
			require.NoError(t, json.Unmarshal([]byte(body), &envelope), body)
			assert.Equal(t, tc.code, envelope.Code)

			records := logged.String()
			if tc.reported {
				assert.Equal(t, 1, strings.Count(records, `"level":"ERROR"`), "reported once")
				assert.Contains(t, records, "handler returned an error")
			} else {
				assert.NotContains(t, records, `"level":"ERROR"`, "a client error is not a failure")
			}
			assert.Contains(t, records, fmt.Sprintf(`"status":%d,"routeKey":"GET /prices"`, tc.status))
		})
	}
}

func TestStream_AnEnvelopeThatDoesNotEncodeIsAReported500(t *testing.T) {
	t.Parallel()

	c, logged := testContainer()
	metadata, body := readStream(t, c, func(context.Context, *Container, events.APIGatewayProxyRequest, http.ResponseWriter) error {
		return response.NotFound("priceNotFound", "no such price", map[string]any{"nan": math.NaN()})
	}, streamRequest())

	assert.Equal(t, http.StatusNotFound, decodeMetadata(t, metadata).StatusCode, "the details are dropped, the status kept")
	assert.Contains(t, body, `"code":"priceNotFound"`)
	assert.NotContains(t, logged.String(), `"level":"ERROR"`)
}

func TestStream_AServerErrorIsReportedWithItsCause(t *testing.T) {
	t.Parallel()

	c, logged := testContainer()
	_, body := readStream(t, c, func(context.Context, *Container, events.APIGatewayProxyRequest, http.ResponseWriter) error {
		return response.InternalServerError(errUpsert, "feedFailed", "could not read the feed", nil)
	}, streamRequest())

	assert.NotContains(t, body, errUpsert.Error(), "the cause never reaches the client")
	assert.Equal(t, 1, strings.Count(logged.String(), errUpsert.Error()))
}

func TestStream_APanicBeforeTheFirstByteAnswersWithTheEnvelope(t *testing.T) {
	t.Parallel()

	c, logged := testContainer()
	metadata, body := readStream(t, c, func(context.Context, *Container, events.APIGatewayProxyRequest, http.ResponseWriter) error {
		panic(errBoom)
	}, streamRequest())

	assert.Equal(t, http.StatusInternalServerError, decodeMetadata(t, metadata).StatusCode)
	assert.Contains(t, body, `"code":"serverError"`)
	assert.Contains(t, logged.String(), "handler panicked")
	assert.Contains(t, logged.String(), `"status":500,"routeKey":"GET /prices"`)
}

func TestStream_AnErrorAfterTheFirstByteIsReportedAndEndsTheStream(t *testing.T) {
	t.Parallel()

	for _, failure := range []error{errUpsert, response.NotFound("priceNotFound", "the price went away", nil)} {
		t.Run(failure.Error(), func(t *testing.T) {
			t.Parallel()
			c, logged := testContainer()
			metadata, body := readStream(t, c, func(_ context.Context, _ *Container, _ events.APIGatewayProxyRequest, w http.ResponseWriter) error {
				if err := response.SSE(w, "tick", "1"); err != nil {
					return err
				}
				return failure
			}, streamRequest())

			assert.JSONEq(t, `{"statusCode":200}`, metadata)
			assert.Equal(t, "event: tick\ndata: 1\n\n", body, "the stream ends after what op wrote, with no trailer")

			records := logged.String()
			assert.Equal(t, 1, strings.Count(records, `"level":"ERROR"`), "reported once")
			assert.Contains(t, records, "handler returned an error")
			assert.Contains(t, records, failure.Error())
			assert.Contains(t, records, `"status":200,"routeKey":"GET /prices"`)
		})
	}
}

func TestStream_APanicAfterTheFirstByteIsReportedAndEndsTheStream(t *testing.T) {
	t.Parallel()

	c, logged := testContainer()
	metadata, body := readStream(t, c, func(_ context.Context, _ *Container, _ events.APIGatewayProxyRequest, w http.ResponseWriter) error {
		if err := response.SSE(w, "tick", "1"); err != nil {
			return err
		}
		panic(errBoom)
	}, streamRequest())

	assert.JSONEq(t, `{"statusCode":200}`, metadata)
	assert.Equal(t, "event: tick\ndata: 1\n\n", body)
	assert.Contains(t, logged.String(), "handler panicked")
	assert.Contains(t, logged.String(), `"stack":`)
	assert.Contains(t, logged.String(), `"status":200,"routeKey":"GET /prices"`)
}

func TestStream_LogsHowTheRequestArrivedAndEnded(t *testing.T) {
	t.Parallel()

	c, logged := testContainer()
	_, _ = readStream(t, c, func(ctx context.Context, c *Container, _ events.APIGatewayProxyRequest, w http.ResponseWriter) error {
		c.Logger.InfoContext(ctx, "inside")
		return response.SSE(w, "", "1")
	}, streamRequest())

	records := logged.String()
	assert.Contains(t, records, `"message":"API request received"`)
	assert.Contains(t, records, `"method":"GET","path":"/prices","routeKey":"GET /prices"`)
	assert.Contains(t, records, `"message":"inside"`)
	assert.Contains(t, records, `"function_request_id":"req-1"`)
	assert.Contains(t, records, `"client_ip":"203.0.113.7"`)
	assert.Contains(t, records, `"message":"API request completed"`)
	assert.Contains(t, records, `"status":200,"routeKey":"GET /prices","durationMs":`)
}

func TestStream_OpReadsTheClaimsAndTheClientIP(t *testing.T) {
	t.Parallel()

	c, logged := testContainer()
	c.trustedProxies = 1
	id := uuid.New()
	req := streamRequest()
	req.RequestContext.Authorizer = map[string]any{"claims": map[string]any{"sub": "sub-123", "userId": id.String()}}
	req.Headers["X-Forwarded-For"] = "10.0.0.1, 198.51.100.23"
	req.MultiValueHeaders["X-Forwarded-For"] = []string{"10.0.0.1, 198.51.100.23"}

	_, body := readStream(t, c, func(ctx context.Context, c *Container, _ events.APIGatewayProxyRequest, w http.ResponseWriter) error {
		userID, err := request.UserID(ctx)
		if err != nil {
			return err
		}
		sub, err := request.Claim(ctx, "sub")
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(w, "%s %s %s", userID, sub, c.ClientIP(ctx))
		return err
	}, req)

	assert.Equal(t, id.String()+" sub-123 198.51.100.23", body)
	assert.Contains(t, logged.String(), `"client_ip":"198.51.100.23"`)
}

func TestStream_AWSLambdaGoStreamsTheResponse(t *testing.T) {
	t.Parallel()

	c, _ := testContainer()
	h := c.Stream(func(_ context.Context, _ *Container, _ events.APIGatewayProxyRequest, w http.ResponseWriter) error {
		_, err := io.WriteString(w, "hello")
		return err
	})

	payload, err := json.Marshal(streamRequest())
	require.NoError(t, err)
	raw, err := lambda.NewHandler(h).Invoke(lambdaContext(t), payload)
	require.NoError(t, err, "aws-lambda-go reads the stream rather than encoding the reader")
	metadata, body := splitStream(t, string(raw))
	assert.JSONEq(t, `{"statusCode":200}`, metadata)
	assert.Equal(t, "hello", body)

	r, err := h(lambdaContext(t), streamRequest())
	require.NoError(t, err)
	typed, ok := r.(interface{ ContentType() string })
	require.True(t, ok, "Lambda learns the content type from the response")
	assert.Equal(t, "application/vnd.awslambda.http-integration-response", typed.ContentType())
	_, err = io.Copy(io.Discard, r)
	require.NoError(t, err)
}

func TestStream_TheWritesFailOnceTheClientIsGone(t *testing.T) {
	t.Parallel()

	c := &Container{Logger: quietLogger()}
	failed := make(chan error, 1)
	r, err := c.Stream(func(_ context.Context, _ *Container, _ events.APIGatewayProxyRequest, w http.ResponseWriter) error {
		for {
			if err := response.SSE(w, "tick", "1"); err != nil {
				failed <- err
				return err
			}
		}
	})(lambdaContext(t), streamRequest())
	require.NoError(t, err)

	_, err = io.ReadFull(r, make([]byte, 64))
	require.NoError(t, err)
	closer, ok := r.(io.Closer)
	require.True(t, ok, "aws-lambda-go closes the response when Lambda stops reading")
	require.NoError(t, closer.Close())

	select {
	case err := <-failed:
		require.ErrorIs(t, err, io.ErrClosedPipe)
	case <-time.After(streamTimeout):
		t.Fatal("op went on writing to a client that is gone")
	}
}

// The tracing and metrics tests change the global OpenTelemetry state, as
// the tracing tests do, so none of them runs in parallel.

func TestStream_TheInvocationSpansDescribeTheStream(t *testing.T) {
	t.Setenv("AWS_LAMBDA_FUNCTION_NAME", "prices-watch")
	c, recorder := tracedContainer(t)

	h := lambda.NewHandler(c.Stream(func(_ context.Context, _ *Container, _ events.APIGatewayProxyRequest, w http.ResponseWriter) error {
		return response.SSE(w, "tick", "1")
	}))
	payload, err := json.Marshal(streamRequest())
	require.NoError(t, err)
	_, err = h.Invoke(lambdaContext(t), payload)
	require.NoError(t, err)

	server := spanNamed(t, recorder, "prices-watch")
	business := spanNamed(t, recorder, businessSpan)
	assert.Equal(t, server.SpanContext().SpanID(), business.Parent().SpanID(), "op runs in the business span, in the invocation")
	attrs := map[attribute.Key]attribute.Value{}
	for _, kv := range server.Attributes() {
		attrs[kv.Key] = kv.Value
	}
	assert.Equal(t, "/prices", attrs[semconv.HTTPRouteKey].AsString())
	assert.Equal(t, int64(http.StatusOK), attrs[semconv.HTTPResponseStatusCodeKey].AsInt64())
	assert.Equal(t, "203.0.113.7", attrs[semconv.ClientAddressKey].AsString())
	assert.NotEqual(t, codes.Error, server.Status().Code)
}

func TestStream_AFailureAfterTheFirstByteMarksTheSpans(t *testing.T) {
	t.Setenv("AWS_LAMBDA_FUNCTION_NAME", "prices-watch")
	c, recorder := tracedContainer(t)

	h := lambda.NewHandler(c.Stream(func(_ context.Context, _ *Container, _ events.APIGatewayProxyRequest, w http.ResponseWriter) error {
		if err := response.SSE(w, "tick", "1"); err != nil {
			return err
		}
		return errUpsert
	}))
	payload, err := json.Marshal(streamRequest())
	require.NoError(t, err)
	_, err = h.Invoke(lambdaContext(t), payload)
	require.NoError(t, err, "the stream closes normally")

	assert.Equal(t, codes.Error, spanNamed(t, recorder, businessSpan).Status().Code)
	assert.Equal(t, codes.Error, spanNamed(t, recorder, "prices-watch").Status().Code)
}

func TestStream_FlushesTheInvocationMetrics(t *testing.T) {
	t.Setenv("POWERTOOLS_METRICS_NAMESPACE", "")
	recordingTracer(t)
	var emf bytes.Buffer
	c := New(
		WithService("prices"),
		WithLogging(logging.WithWriter(&bytes.Buffer{})),
		WithMetrics(metrics.WithNamespace("Shop"), metrics.WithWriter(&emf)),
	)
	c.lifecycle = nil

	_, _ = readStream(t, c, func(ctx context.Context, c *Container, _ events.APIGatewayProxyRequest, w http.ResponseWriter) error {
		if err := c.Metrics.AddMetric(ctx, "PriceSent", metrics.Count, 1); err != nil {
			return err
		}
		return response.SSE(w, "price", "42")
	}, streamRequest())

	assert.Contains(t, emf.String(), `"Namespace":"Shop"`)
	assert.Contains(t, emf.String(), `"PriceSent":[1.0]`)
}
