package framework

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/alessandrolattao/powertools-lambda-go/middleware"
	"github.com/aws/aws-lambda-go/events"

	"github.com/alessandrolattao/lambda-framework-go/internal/apievent"
	"github.com/alessandrolattao/lambda-framework-go/response"
)

// contentTypeStream is the content type of a streamed HTTP integration
// response, which tells Lambda the stream holds the JSON metadata, the
// delimiter and then the body.
const contentTypeStream = "application/vnd.awslambda.http-integration-response"

// delimiterSize is the number of null bytes between the metadata and the
// body of a streamed response.
const delimiterSize = 8

// errStreamNotJSON is what a streamed response answers whoever tries to
// encode it as JSON.
var errStreamNotJSON = errors.New("a streamed response is not JSON")

// Stream returns the Lambda handler of one API Gateway REST API endpoint that
// streams its response, server-sent events for one:
//
//	lambda.Start(c.Stream(watch))
//
//	func watch(ctx context.Context, c *framework.Container, req events.APIGatewayProxyRequest, w http.ResponseWriter) error {
//		w.Header().Set("Content-Type", "text/event-stream")
//		w.Header().Set("Cache-Control", "no-store")
//		for price := range prices(ctx) {
//			if err := response.SSE(w, "price", price); err != nil {
//				return err
//			}
//		}
//		return nil
//	}
//
// op writes the response through w, a plain http.ResponseWriter that is also
// an http.Flusher. Header and WriteHeader set the headers and the status
// (200 unless op sets one); the first Write or Flush sends them, and from
// then on whatever op writes reaches the client as it is written, Flush (or
// http.NewResponseController(w).Flush()) included. A Set-Cookie header goes
// out as the cookies of the response, a header with several values as its
// multiValueHeaders.
//
// API Gateway streams a response only from an integration whose
// responseTransferMode is STREAM, which invokes the function through the
// response-streaming-invocations URI: a streaming endpoint is a Lambda of its
// own, and the router does not serve it. The metadata (status and headers)
// must stay within 16 KB.
//
// The invocation gets what Handle gives an API request: the Powertools
// decorators, initialization, the "API request received" record, op in the
// "## businessLogic" span, and the "API request completed" record when the
// stream closes, with the status sent and the time the stream stayed open.
// In op, ClientIP and the claim functions of package request read the
// request as in a typed operation.
//
// An error op returns before anything has gone out answers as a typed
// operation's does: a *response.Error with its status and envelope (reported
// when it is a 5xx), any other error with the 500 envelope, reported. After
// the first byte the status is gone: an error or a panic is reported, and the
// stream ends there.
func (c *Container) Stream(op func(ctx context.Context, c *Container, req events.APIGatewayProxyRequest, w http.ResponseWriter) error) func(context.Context, events.APIGatewayProxyRequest) (io.Reader, error) {
	s := &streamer{c: c, op: op}
	s.run = decorate(c, s.serve, correlationPath[events.APIGatewayProxyRequest]())
	return s.start
}

// streamer is the handler Stream returns.
type streamer struct {
	c   *Container
	op  func(context.Context, *Container, events.APIGatewayProxyRequest, http.ResponseWriter) error
	run middleware.Handler[events.APIGatewayProxyRequest, noResult] // the decorated serve
}

// streamWriterKey is the context key of the writer of the invocation serve
// runs.
type streamWriterKey struct{}

// start answers aws-lambda-go at once with the reader of the stream, and
// serves the request into it meanwhile. The stream closes after the whole
// invocation, decorators included, so that the logs, the spans and the
// metrics are written before Lambda considers the invocation done.
func (s *streamer) start(ctx context.Context, req events.APIGatewayProxyRequest) (io.Reader, error) {
	reader, pipe := io.Pipe()
	w := &streamWriter{pipe: pipe, header: http.Header{}, origin: s.c.corsOrigin(req)}
	go func() {
		defer pipe.Close() //nolint:errcheck // closing a PipeWriter always succeeds
		if _, err := s.run(context.WithValue(ctx, streamWriterKey{}, w), req); err != nil {
			// serve answers its own failures: this one is a decorator's.
			_ = s.c.report(ctx, err) //nolint:errcheck // report returns its argument
			w.fail()
		}
	}()
	return streamReader{reader}, nil
}

// serve runs one invocation inside the Powertools decorators, as
// Handler.callInner does for an API request answered in one response.
func (s *streamer) serve(ctx context.Context, req events.APIGatewayProxyRequest) (_ noResult, err error) {
	w, _ := ctx.Value(streamWriterKey{}).(*streamWriter) //nolint:errcheck // start always puts it there
	ctx = apievent.WithREST(ctx, &req)
	begin := time.Now()
	defer func() {
		// A panic has already been written down by reportPanic.
		if recover() != nil || err != nil {
			w.fail()
		}
		w.Flush() // a stream op left without a byte still gets its status
		s.c.apiCompleted(ctx, req, w.status, time.Since(begin))
		err = nil
	}()

	err = s.c.invoke(ctx, req, func(ctx context.Context, c *Container) error {
		return answerBeforeFirstByte(w, s.op(ctx, c, req, w))
	})
	return noResult{}, err
}

// answerBeforeFirstByte answers err, the error op returned, with its envelope
// when nothing has gone out yet, and returns what the invocation reports: a
// *response.Error below 500 is the client's, answered and not reported, as a
// typed operation's. After the first byte err is reported as it is.
func answerBeforeFirstByte(w *streamWriter, err error) error {
	if err == nil || w.sent {
		return err
	}
	answer := errUnhandled
	if respErr, ok := errors.AsType[*response.Error](err); ok {
		answer = respErr
	}
	w.answer(answer)
	if w.status >= http.StatusInternalServerError { // the status sent: an envelope that does not encode is a 500
		return err
	}
	return nil
}

// streamWriter is the http.ResponseWriter of a streaming handler. It writes
// into the pipe aws-lambda-go reads the response from: the metadata (status
// and headers) on the first Write or Flush, then the body. The pipe holds
// nothing back, so a Write returns once aws-lambda-go has taken the bytes.
type streamWriter struct {
	pipe   *io.PipeWriter
	header http.Header
	origin string // the origin the metadata allows (CORS), "" for none
	status int    // the status WriteHeader set, then the status sent
	sent   bool   // whether the metadata has gone out
	err    error  // what sending the metadata failed with
}

var (
	_ http.ResponseWriter = (*streamWriter)(nil)
	_ http.Flusher        = (*streamWriter)(nil)
)

// Header returns the headers the metadata will carry. Changing them after
// the first Write or Flush changes nothing.
func (w *streamWriter) Header() http.Header {
	return w.header
}

// WriteHeader sets the status the metadata will carry. The first status set
// wins, and none changes the status once it has gone out.
func (w *streamWriter) WriteHeader(status int) {
	if !w.sent && w.status == 0 {
		w.status = status
	}
}

// Write sends the metadata if it has not gone out yet, then p.
func (w *streamWriter) Write(p []byte) (int, error) {
	if err := w.FlushError(); err != nil {
		return 0, err
	}
	if len(p) == 0 {
		return 0, nil
	}
	return w.pipe.Write(p) //nolint:wrapcheck // a writer answers the error of what it writes to, io.ErrClosedPipe when the client is gone
}

// Flush implements http.Flusher: it sends the metadata if it has not gone
// out yet. What op wrote has already gone out.
func (w *streamWriter) Flush() {
	_ = w.FlushError() //nolint:errcheck // http.Flusher has no error to return; the next Write answers it
}

// FlushError is Flush with its error, which http.ResponseController's Flush
// returns.
func (w *streamWriter) FlushError() error {
	if w.sent {
		return w.err
	}
	w.sent = true
	if w.status == 0 {
		w.status = http.StatusOK
	}
	metadata := newStreamMetadata(w.status, w.header)
	metadata.Headers = allowOrigin(w.origin, metadata.Headers, metadata.MultiValueHeaders)
	prelude, err := json.Marshal(metadata, json.Deterministic(true), jsontext.AllowInvalidUTF8(true))
	if err != nil {
		w.err = fmt.Errorf("encode the stream metadata: %w", err)
		return w.err
	}
	if _, err := w.pipe.Write(append(prelude, make([]byte, delimiterSize)...)); err != nil {
		w.err = err
		return err //nolint:wrapcheck // as Write does
	}
	return nil
}

// answer sends the envelope of e as the whole response, in place of the
// status and the headers op set. Nothing has gone out yet.
func (w *streamWriter) answer(e *response.Error) {
	resp := e.APIGatewayProxy()
	w.status = resp.StatusCode
	w.header = http.Header{}
	for name, value := range resp.Headers {
		w.header.Set(name, value)
	}
	_, _ = w.Write([]byte(resp.Body)) //nolint:errcheck // the client went away; there is nobody left to tell
}

// fail answers the 500 envelope of a failure nothing answered, if nothing
// has gone out yet.
func (w *streamWriter) fail() {
	if !w.sent {
		w.answer(errUnhandled)
	}
}

// streamMetadata is the JSON that opens a streamed response, in API
// Gateway's names.
type streamMetadata struct {
	StatusCode        int                 `json:"statusCode"`
	Headers           map[string]string   `json:"headers,omitempty"`
	MultiValueHeaders map[string][]string `json:"multiValueHeaders,omitempty"`
	Cookies           []string            `json:"cookies,omitempty"`
}

// newStreamMetadata describes status and header: Set-Cookie as the cookies,
// a header with one value in headers, one with several in
// multiValueHeaders.
func newStreamMetadata(status int, header http.Header) streamMetadata {
	m := streamMetadata{StatusCode: status}
	for _, name := range slices.Sorted(maps.Keys(header)) {
		values := header[name]
		switch {
		case strings.EqualFold(name, "Set-Cookie"):
			m.Cookies = append(m.Cookies, values...)
		case len(values) == 1:
			if m.Headers == nil {
				m.Headers = map[string]string{}
			}
			m.Headers[name] = values[0]
		case len(values) > 1:
			if m.MultiValueHeaders == nil {
				m.MultiValueHeaders = map[string][]string{}
			}
			m.MultiValueHeaders[name] = values
		}
	}
	return m
}

// streamReader is the response a streaming handler answers aws-lambda-go
// with: the read end of the pipe the request is served into. aws-lambda-go
// streams it to Lambda, and closes it when Lambda stops reading; the writes
// that follow fail.
type streamReader struct {
	*io.PipeReader
}

// ContentType tells Lambda what the stream holds.
func (streamReader) ContentType() string {
	return contentTypeStream
}

// MarshalJSON refuses to encode the reader: aws-lambda-go encodes a response
// that can be encoded, and streams one that cannot.
func (streamReader) MarshalJSON() ([]byte, error) {
	return nil, errStreamNotJSON
}
