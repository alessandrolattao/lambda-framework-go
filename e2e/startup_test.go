package e2e

import (
	"cmp"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// memorySizesMB are the Lambda sizes the startup is measured at. CPU follows
// memory on Lambda, so they are three different machines.
var memorySizesMB = []int{128, 512, 1024}

const (
	// coldStarts is how many fresh processes each size is measured on. With
	// a fraction of a CPU the kernel hands out CPU time in 100 ms windows, so
	// one start can take twice another: the limits apply to the median.
	coldStarts = 5

	// warmInvocations is how many invocations follow each cold one.
	warmInvocations = 10
)

// maxColdStart is the longest the median cold invocation may take, process
// start and first initialization included, per memory size. Measured on the reference
// machine and given room; E2E_MAX_COLD_START_SCALE scales them for a slower
// one (2 doubles every limit).
var maxColdStart = map[int]time.Duration{
	128:  800 * time.Millisecond, // measured: about 300-400 ms
	512:  250 * time.Millisecond, // measured: about 100 ms
	1024: 100 * time.Millisecond, // measured: about 30 ms
}

// startup is what one function did from coldStarts cold starts: medians,
// except for the warm percentiles (over every warm invocation) and the peak
// memory (the highest).
type startup struct {
	function  string
	memoryMB  int
	initMs    float64 // process start to runtime ready (Lambda's Init Duration)
	firstInit float64 // the framework's own first initialization, inside the first invocation
	cold      time.Duration
	warmP50   time.Duration
	warmP95   time.Duration
	peakRSSMB float64
	binaryMB  float64
}

func TestStartup(t *testing.T) {
	scale := 1.0
	if s := os.Getenv("E2E_MAX_COLD_START_SCALE"); s != "" {
		var err error
		scale, err = strconv.ParseFloat(s, 64)
		require.NoError(t, err, "E2E_MAX_COLD_START_SCALE")
	}

	var results []startup
	for _, memoryMB := range memorySizesMB {
		for _, name := range []string{ordersAPI, ordersEvents} {
			t.Run(fmt.Sprintf("%s/%dMB", name, memoryMB), func(t *testing.T) {
				result := measureStartup(t, name, memoryMB)
				results = append(results, result)

				limit := time.Duration(float64(maxColdStart[memoryMB]) * scale)
				assert.LessOrEqual(t, result.cold, limit,
					"the median cold start at %d MB took %s, over the %s limit", memoryMB, result.cold, limit)
			})
		}
	}

	report := startupReport(results)
	t.Log("\n" + report)
	if path := os.Getenv("E2E_STARTUP_REPORT"); path != "" {
		//nolint:gosec // the path comes from whoever runs the tests, on purpose
		require.NoError(t, os.WriteFile(path, []byte(report), 0o600))
	}
}

// measureStartup starts the function fresh coldStarts times; each time it
// invokes it once cold and then warmInvocations times warm, every invocation
// a real one on Postgres.
func measureStartup(t *testing.T, name string, memoryMB int) startup {
	t.Helper()

	id := newOrder(t)
	payload := map[string]any{
		ordersAPI:    apiRequest("GET /orders/{id}", "/orders/"+id, withID(id)),
		ordersEvents: eventBridgeEvent("OrderPaid", `{"orderId":"`+id+`"}`),
	}[name]
	succeeded := func(inv invocation) {
		t.Helper()
		failure, failed := inv.failure()
		require.False(t, failed, "%+v", failure)
		if name == ordersAPI {
			require.Equal(t, 200, httpResponse(t, inv).StatusCode)
		}
	}

	inits := make([]float64, 0, coldStarts)
	firstInits := make([]float64, 0, coldStarts)
	colds := make([]time.Duration, 0, coldStarts)
	warm := make([]time.Duration, 0, warmInvocations*coldStarts)
	var peak float64
	for range coldStarts {
		fn := startFunction(t, name, memoryMB)

		cold := fn.invoke(t, payload)
		succeeded(cold)
		require.NotZero(t, cold.initMs, "the first invocation is the cold one")
		firstInit, ok := cold.recordWith(t, "container initialized")["durationMs"].(float64)
		require.True(t, ok, "container initialized carries durationMs")
		inits, firstInits, colds = append(inits, cold.initMs), append(firstInits, firstInit), append(colds, cold.latency)

		for range warmInvocations {
			inv := fn.invoke(t, payload)
			succeeded(inv)
			assert.Zero(t, inv.initMs, "a warm invocation does not start the process again")
			warm = append(warm, inv.latency)
		}
		peak = max(peak, fn.peakRSSMB(t))
	}
	slices.Sort(warm)

	return startup{
		function:  name,
		memoryMB:  memoryMB,
		initMs:    median(inits),
		firstInit: median(firstInits),
		cold:      median(colds),
		warmP50:   warm[len(warm)/2],
		warmP95:   warm[len(warm)*95/100],
		peakRSSMB: peak,
		binaryMB:  float64(suite.binaries[name].size) / (1 << 20),
	}
}

func median[T cmp.Ordered](values []T) T {
	sorted := slices.Sorted(slices.Values(values))
	return sorted[len(sorted)/2]
}

func startupReport(results []startup) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Medians of %d cold starts per row; warm percentiles over %d invocations.\n\n", coldStarts, coldStarts*warmInvocations)
	b.WriteString("| function | memory | init (process) | first init (framework) | cold invocation | warm p50 | warm p95 | peak RSS | binary |\n")
	b.WriteString("|---|---:|---:|---:|---:|---:|---:|---:|---:|\n")
	for _, r := range results {
		fmt.Fprintf(&b, "| %s | %d MB | %.1f ms | %.1f ms | %s | %s | %s | %.1f MB | %.1f MB |\n",
			r.function, r.memoryMB, r.initMs, r.firstInit,
			r.cold.Round(100*time.Microsecond), r.warmP50.Round(10*time.Microsecond), r.warmP95.Round(10*time.Microsecond),
			r.peakRSSMB, r.binaryMB)
	}
	return b.String()
}
