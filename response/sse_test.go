package response

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var errClientGone = errors.New("io: read/write on closed pipe")

func TestSSE_WritesOneEventAndFlushesIt(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name, event, data, want string
	}{
		{"a named event", "price", `{"amount":42}`, "event: price\ndata: {\"amount\":42}\n\n"},
		{"an event without a name", "", "tick", "data: tick\n\n"},
		{"data on several lines", "", "one\ntwo\n\nfour", "data: one\ndata: two\ndata: \ndata: four\n\n"}, //nolint:dupword // an empty line of data is a data line too
		{"every line break of the format", "log", "lf\ncrlf\r\ncr\rend", "event: log\ndata: lf\ndata: crlf\ndata: cr\ndata: end\n\n"},
		{"empty data", "ping", "", "event: ping\ndata: \n\n"},
		{"leading spaces kept", "", "  indented", "data:   indented\n\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := httptest.NewRecorder()
			require.NoError(t, SSE(w, tc.event, tc.data))
			assert.Equal(t, tc.want, w.Body.String())
			assert.True(t, w.Flushed, "the event reaches the client at once")
		})
	}
}

func TestSSE_AnEventNameIsOneLine(t *testing.T) {
	t.Parallel()

	for _, event := range []string{"price\ndata: forged", "price\r", "a\r\nb"} {
		w := httptest.NewRecorder()
		err := SSE(w, event, "42")
		require.ErrorIs(t, err, ErrSSEEventName, "%q", event)
		assert.Empty(t, w.Body.String(), "%q: nothing is written", event)
		assert.False(t, w.Flushed, "%q", event)
	}
}

// unflushableWriter is an http.ResponseWriter that cannot flush, and whose
// writes fail with err when it is set.
type unflushableWriter struct {
	header http.Header
	err    error
}

func (w *unflushableWriter) Header() http.Header { return w.header }
func (*unflushableWriter) WriteHeader(int)       {}

func (w *unflushableWriter) Write(p []byte) (int, error) {
	if w.err != nil {
		return 0, w.err
	}
	return len(p), nil
}

func TestSSE_TellsWhenTheClientIsGone(t *testing.T) {
	t.Parallel()

	err := SSE(&unflushableWriter{header: http.Header{}, err: errClientGone}, "price", "42")
	require.ErrorIs(t, err, errClientGone)
}

func TestSSE_NeedsAWriterThatFlushes(t *testing.T) {
	t.Parallel()

	err := SSE(&unflushableWriter{header: http.Header{}}, "price", "42")
	require.ErrorIs(t, err, http.ErrNotSupported)
}
