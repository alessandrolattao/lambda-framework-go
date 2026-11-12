package response

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// ErrSSEEventName is what SSE answers for an event name that is not one
// line: a CR or an LF in it would add fields of its own to the stream.
var ErrSSEEventName = errors.New("the name of a server-sent event must be one line")

// sseLineBreaks turns every line break the server-sent events format knows
// (CRLF, CR, LF) into LF.
var sseLineBreaks = strings.NewReplacer("\r\n", "\n", "\r", "\n")

// SSE writes one server-sent event to w and flushes it to the client: an
// event line when event is not empty, a data line for each line of data, and
// the blank line that ends the event. event must be one line: one with a CR
// or an LF answers ErrSSEEventName, and nothing is written. data is split on
// every line break (CRLF, CR, LF). A streaming handler
// (framework.Container.Stream) sends each event with one call:
//
//	w.Header().Set("Content-Type", "text/event-stream")
//	if err := response.SSE(w, "price", `{"amount":42}`); err != nil {
//		return err
//	}
//
// Any other error is the one writing or flushing failed with: the client went
// away, or w cannot flush.
func SSE(w http.ResponseWriter, event, data string) error {
	if strings.ContainsAny(event, "\r\n") {
		return fmt.Errorf("%w: %q", ErrSSEEventName, event)
	}
	var b strings.Builder
	if event != "" {
		b.WriteString("event: ")
		b.WriteString(event)
		b.WriteByte('\n')
	}
	for line := range strings.SplitSeq(sseLineBreaks.Replace(data), "\n") {
		b.WriteString("data: ")
		b.WriteString(line)
		b.WriteByte('\n')
	}
	b.WriteByte('\n')

	if _, err := io.WriteString(w, b.String()); err != nil {
		return fmt.Errorf("write the event: %w", err)
	}
	if err := http.NewResponseController(w).Flush(); err != nil {
		return fmt.Errorf("flush the event: %w", err)
	}
	return nil
}
