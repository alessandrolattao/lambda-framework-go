package e2e

import (
	"bytes"
	"encoding/base64"
	"errors"
	"net"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

// The X-Ray UDP exporter writes one datagram per export: the X-Ray daemon
// protocol header, then the OTLP traces signal prefix, then the base64 of an
// OTLP ExportTraceServiceRequest.
var (
	xrayProtocolHeader = []byte("{\"format\":\"json\",\"version\":1}\n")
	xrayTracesPrefix   = []byte("T1S")

	errNoProtocolHeader = errors.New("no X-Ray protocol header")
	errNotTraces        = errors.New("not an OTLP traces datagram")
)

// xrayCollector stands in for the X-Ray endpoint Lambda runs next to a
// function: it keeps every span the functions export.
type xrayCollector struct {
	conn  *net.UDPConn
	mu    sync.Mutex
	spans []*tracepb.Span
	done  chan struct{}
}

func newXRayCollector() (*xrayCollector, error) {
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4zero})
	if err != nil {
		return nil, err
	}
	x := &xrayCollector{conn: conn, done: make(chan struct{})}
	go x.receive()
	return x, nil
}

func (x *xrayCollector) port() int {
	if addr, ok := x.conn.LocalAddr().(*net.UDPAddr); ok {
		return addr.Port
	}
	return 0
}

func (x *xrayCollector) close() {
	warn(x.conn.Close())
	<-x.done
}

func (x *xrayCollector) receive() {
	defer close(x.done)
	buf := make([]byte, 64*1024)
	for {
		n, _, err := x.conn.ReadFromUDP(buf)
		if errors.Is(err, net.ErrClosed) {
			return
		}
		if err != nil {
			continue
		}
		spans, err := decodeXRayDatagram(buf[:n])
		if err != nil {
			continue
		}
		x.mu.Lock()
		x.spans = append(x.spans, spans...)
		x.mu.Unlock()
	}
}

func decodeXRayDatagram(datagram []byte) ([]*tracepb.Span, error) {
	payload, ok := bytes.CutPrefix(datagram, xrayProtocolHeader)
	if !ok {
		return nil, errNoProtocolHeader
	}
	payload, ok = bytes.CutPrefix(payload, xrayTracesPrefix)
	if !ok {
		return nil, errNotTraces
	}
	raw, err := base64.StdEncoding.DecodeString(string(payload))
	if err != nil {
		return nil, err
	}
	var req coltracepb.ExportTraceServiceRequest
	if err := proto.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	var spans []*tracepb.Span
	for _, rs := range req.GetResourceSpans() {
		for _, ss := range rs.GetScopeSpans() {
			spans = append(spans, ss.GetSpans()...)
		}
	}
	return spans, nil
}

// trace waits for the spans of the invocation requestID: the invocation's
// server span, which otellambda tags with the request id, and every span of
// its trace.
func (x *xrayCollector) trace(t *testing.T, requestID string) (server *tracepb.Span, all []*tracepb.Span) {
	t.Helper()
	require.Eventually(t, func() bool {
		x.mu.Lock()
		defer x.mu.Unlock()
		i := slices.IndexFunc(x.spans, func(s *tracepb.Span) bool {
			return s.GetKind() == tracepb.Span_SPAN_KIND_SERVER && attribute(s, "faas.invocation_id") == requestID
		})
		if i < 0 {
			return false
		}
		server = x.spans[i]
		all = all[:0]
		for _, s := range x.spans {
			if bytes.Equal(s.GetTraceId(), server.GetTraceId()) {
				all = append(all, s)
			}
		}
		return true
	}, 5*time.Second, 20*time.Millisecond, "no span for invocation %s", requestID)
	return server, all
}

// spanNamed returns the span called name among spans.
func spanNamed(t *testing.T, spans []*tracepb.Span, name string) *tracepb.Span {
	t.Helper()
	i := slices.IndexFunc(spans, func(s *tracepb.Span) bool { return s.GetName() == name })
	require.GreaterOrEqual(t, i, 0, "no span named %q", name)
	return spans[i]
}

func attribute(s *tracepb.Span, key string) string {
	for _, kv := range s.GetAttributes() {
		if kv.GetKey() == key {
			return kv.GetValue().GetStringValue()
		}
	}
	return ""
}

func intAttribute(s *tracepb.Span, key string) int64 {
	for _, kv := range s.GetAttributes() {
		if kv.GetKey() == key {
			return kv.GetValue().GetIntValue()
		}
	}
	return 0
}
