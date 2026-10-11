// Package e2e runs the framework the way Lambda does. The two functions in
// lambdas/ are built for linux/amd64 and run in the Lambda base image, under
// the Runtime Interface Emulator, with the CPU and memory Lambda would give
// them. Postgres runs next to them on a Docker network, and a UDP listener on
// the host stands in for the X-Ray endpoint, so every span the functions
// export can be checked.
package e2e

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/moby/moby/api/types/container"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/network"
	"github.com/testcontainers/testcontainers-go/wait"
)

const (
	baseImage     = "public.ecr.aws/lambda/provided:al2023"
	postgresImage = "postgres:18-alpine"
	riePort       = "8080/tcp"
	invokePath    = "/2015-03-31/functions/function/invocations"

	// functionMemoryMB is the size the functional tests run at.
	functionMemoryMB = 512

	// lambdaMBPerVCPU is where Lambda gives a function one full vCPU; below
	// and above it, CPU is proportional to memory.
	lambdaMBPerVCPU = 1769

	ordersAPI    = "orders-api"
	ordersEvents = "orders-events"
)

// suite is what every test shares, set up once by TestMain.
var suite struct {
	dbURL    string        // Postgres as the functions reach it, inside the network
	db       *pgxpool.Pool // Postgres as the tests reach it, from the host
	network  *testcontainers.DockerNetwork
	binaries map[string]binary
	xray     *xrayCollector
}

type binary struct {
	path string
	size int64
}

func TestMain(m *testing.M) {
	os.Exit(run(m))
}

func run(m *testing.M) int {
	ctx := context.Background()
	teardown, err := setUp(ctx)
	defer teardown()
	if err != nil {
		fmt.Fprintln(os.Stderr, "e2e setup:", err)
		return 1
	}
	return m.Run()
}

// setUp builds the functions and starts the shared infrastructure. The
// returned teardown releases whatever was started, also after a failure.
func setUp(ctx context.Context) (teardown func(), err error) {
	var cleanups []func()
	teardown = func() {
		for _, cleanup := range cleanups {
			cleanup()
		}
	}

	buildDir, err := os.MkdirTemp("", "lambda-framework-e2e")
	if err != nil {
		return teardown, err
	}
	cleanups = append(cleanups, func() { warn(os.RemoveAll(buildDir)) })

	suite.binaries = map[string]binary{}
	for _, name := range []string{ordersAPI, ordersEvents} {
		bin, buildErr := build(ctx, name, buildDir)
		if buildErr != nil {
			return teardown, buildErr
		}
		suite.binaries[name] = bin
	}

	suite.network, err = network.New(ctx)
	if err != nil {
		return teardown, fmt.Errorf("create network: %w", err)
	}
	cleanups = append(cleanups, func() { warn(suite.network.Remove(context.Background())) })

	pg, err := postgres.Run(ctx, postgresImage,
		postgres.WithDatabase("orders"),
		postgres.WithUsername("orders"),
		postgres.WithPassword("orders"),
		postgres.BasicWaitStrategies(),
		network.WithNetwork([]string{"postgres"}, suite.network),
	)
	if pg != nil {
		cleanups = append(cleanups, func() { warn(testcontainers.TerminateContainer(pg)) })
	}
	if err != nil {
		return teardown, fmt.Errorf("start postgres: %w", err)
	}
	suite.dbURL = "postgres://orders:orders@postgres:5432/orders?sslmode=disable"

	hostURL, err := pg.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		return teardown, err
	}
	suite.db, err = pgxpool.New(ctx, hostURL)
	if err != nil {
		return teardown, err
	}
	cleanups = append(cleanups, suite.db.Close)
	if _, err = suite.db.Exec(ctx, `CREATE TABLE orders (
		id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
		total_cents bigint NOT NULL,
		status      text NOT NULL DEFAULT 'new',
		created_at  timestamptz NOT NULL DEFAULT now()
	)`); err != nil {
		return teardown, fmt.Errorf("create schema: %w", err)
	}

	suite.xray, err = newXRayCollector()
	if err != nil {
		return teardown, err
	}
	cleanups = append(cleanups, suite.xray.close)

	return teardown, nil
}

// warn reports a teardown step that failed: the tests have already run, so
// there is nothing left to fail.
func warn(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "e2e teardown:", err)
	}
}

// build compiles lambdas/<name> the way it is deployed to the provided.al2023
// runtime: static, stripped, without the RPC mode of the retired go1.x runtime.
func build(ctx context.Context, name, dir string) (binary, error) {
	out := filepath.Join(dir, name)
	//nolint:gosec // name is one of the functions in lambdas/, chosen by this file
	cmd := exec.CommandContext(ctx, "go", "build", "-tags", "lambda.norpc", "-trimpath",
		"-ldflags", "-s -w", "-o", out, "./lambdas/"+name)
	cmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH=amd64", "CGO_ENABLED=0")
	if output, err := cmd.CombinedOutput(); err != nil {
		return binary{}, fmt.Errorf("build %s: %w\n%s", name, err, output)
	}
	info, err := os.Stat(out)
	if err != nil {
		return binary{}, err
	}
	return binary{path: out, size: info.Size()}, nil
}

// function is one Lambda running under the emulator.
type function struct {
	name    string
	ctr     *testcontainers.DockerContainer
	url     string
	invoked int // invocations so far, to find the log block of the next one
}

// startFunction runs the named function with memoryMB of memory and the CPU
// share Lambda gives that size. The process only starts on the first
// invocation, as on Lambda, so that one is the cold start.
func startFunction(t *testing.T, name string, memoryMB int) *function {
	t.Helper()

	bin, ok := suite.binaries[name]
	require.True(t, ok, "no binary for %s", name)

	ctr, err := testcontainers.Run(t.Context(), baseImage,
		testcontainers.WithFiles(testcontainers.ContainerFile{
			HostFilePath:      bin.path,
			ContainerFilePath: "/var/task/bootstrap",
			FileMode:          0o755,
		}),
		testcontainers.WithEntrypoint("/usr/local/bin/aws-lambda-rie"),
		testcontainers.WithCmd("/var/task/bootstrap"),
		testcontainers.WithEnv(map[string]string{
			"AWS_LAMBDA_FUNCTION_NAME":        name,
			"AWS_LAMBDA_FUNCTION_MEMORY_SIZE": strconv.Itoa(memoryMB),
			"AWS_LAMBDA_FUNCTION_TIMEOUT":     "30",
			"AWS_REGION":                      "eu-west-1",
			"AWS_ACCESS_KEY_ID":               "e2e",
			"AWS_SECRET_ACCESS_KEY":           "e2e",
			"DATABASE_URL":                    suite.dbURL,
			"AWS_XRAY_DAEMON_ADDRESS":         "host.docker.internal:" + strconv.Itoa(suite.xray.port()),
		}),
		testcontainers.WithExposedPorts(riePort),
		testcontainers.WithWaitStrategy(wait.ForListeningPort(riePort)),
		network.WithNetwork(nil, suite.network),
		testcontainers.WithHostConfigModifier(func(hc *container.HostConfig) {
			hc.Memory = int64(memoryMB) << 20
			hc.NanoCPUs = int64(memoryMB) * 1_000_000_000 / lambdaMBPerVCPU
			hc.ExtraHosts = append(hc.ExtraHosts, "host.docker.internal:host-gateway")
		}),
	)
	testcontainers.CleanupContainer(t, ctr)
	require.NoError(t, err)

	endpoint, err := ctr.PortEndpoint(t.Context(), riePort, "http")
	require.NoError(t, err)
	return &function{name: name, ctr: ctr, url: endpoint + invokePath}
}

// invocation is what one call to a function produced.
type invocation struct {
	body      []byte
	latency   time.Duration // as the caller saw it, cold start included
	requestID string
	lines     []string // everything logged between START and REPORT
	records   []record // the function's own JSON log records
	initMs    float64  // INIT REPORT of the emulator: process start to ready, on a cold start only
}

// record is one JSON log line the function wrote.
type record map[string]any

func (r record) str(key string) string {
	if s, ok := r[key].(string); ok {
		return s
	}
	return ""
}

// invoke calls the function with payload (a value to encode as JSON, or raw
// bytes) and collects what that invocation did.
func (f *function) invoke(t *testing.T, payload any) invocation {
	t.Helper()

	data, ok := payload.([]byte)
	if !ok {
		var err error
		data, err = json.Marshal(payload)
		require.NoError(t, err)
	}

	start := time.Now()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, f.url, bytes.NewReader(data))
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, resp.Body.Close())
	require.NoError(t, err)
	latency := time.Since(start)

	f.invoked++
	inv := f.logBlock(t, f.invoked)
	inv.body, inv.latency = body, latency
	return inv
}

var (
	startLine  = regexp.MustCompile(`^START RequestId: (\S+)`)
	reportLine = regexp.MustCompile(`^REPORT RequestId: `)
	initReport = regexp.MustCompile(`INIT REPORT\(durationMs: ([0-9.]+)\)`)
)

// logBlock waits for the emulator to close the n-th invocation in the logs,
// and parses what was written between its START and REPORT lines.
func (f *function) logBlock(t *testing.T, n int) invocation {
	t.Helper()

	var lines []string
	require.Eventually(t, func() bool {
		lines = f.logLines(t)
		reports := 0
		for _, line := range lines {
			if reportLine.MatchString(line) {
				reports++
			}
		}
		return reports >= n
	}, 10*time.Second, 20*time.Millisecond, "the emulator never reported invocation %d", n)

	var inv invocation
	block, requestID := invocationLines(lines, n)
	inv.requestID, inv.lines = requestID, block
	for _, line := range block {
		if m := initReport.FindStringSubmatch(line); m != nil {
			initMs, err := strconv.ParseFloat(m[1], 64)
			require.NoError(t, err, line)
			inv.initMs = initMs
		}
		var rec record
		if strings.HasPrefix(line, "{") && json.Unmarshal([]byte(line), &rec) == nil {
			inv.records = append(inv.records, rec)
		}
	}
	return inv
}

// invocationLines returns the lines between the n-th START line and the
// REPORT line that closes it, with the request id the START line names.
func invocationLines(lines []string, n int) (block []string, requestID string) {
	starts := 0
	for _, line := range lines {
		if m := startLine.FindStringSubmatch(line); m != nil {
			starts++
			if starts == n {
				requestID = m[1]
			}
			continue
		}
		if starts != n {
			continue
		}
		if reportLine.MatchString(line) {
			break
		}
		block = append(block, line)
	}
	return block, requestID
}

func (f *function) logLines(t *testing.T) []string {
	t.Helper()
	logs, err := f.ctr.Logs(context.Background())
	require.NoError(t, err)
	defer func() { require.NoError(t, logs.Close()) }()

	var lines []string
	scanner := bufio.NewScanner(logs)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	require.NoError(t, scanner.Err())
	return lines
}

// peakRSSMB is the most memory the function process has held, from the kernel
// (VmHWM): the number Lambda reports as Max Memory Used, without the emulator.
func (f *function) peakRSSMB(t *testing.T) float64 {
	t.Helper()
	code, out, err := f.ctr.Exec(t.Context(), []string{
		"sh", "-c",
		`for p in /proc/[0-9]*; do tr '\0' ' ' < $p/cmdline 2>/dev/null | grep -q '^/var/task/bootstrap' && grep VmHWM $p/status; done; true`,
	})
	require.NoError(t, err)
	require.Zero(t, code)
	raw, err := io.ReadAll(out)
	require.NoError(t, err)

	_, after, found := bytes.Cut(raw, []byte("VmHWM:"))
	require.True(t, found, "no VmHWM in %q", raw)
	fields := strings.Fields(string(after))
	require.NotEmpty(t, fields, "no VmHWM value in %q", raw)
	kb, err := strconv.ParseFloat(fields[0], 64)
	require.NoError(t, err)
	return kb / 1024
}

// lambdaError is the error the runtime answers with when a function fails.
type lambdaError struct {
	Message string `json:"errorMessage"`
	Type    string `json:"errorType"`
}

// failure decodes the invocation as a function error, reporting whether it
// is one.
func (inv invocation) failure() (lambdaError, bool) {
	var e lambdaError
	if json.Unmarshal(inv.body, &e) != nil || e.Message == "" {
		return lambdaError{}, false
	}
	return e, true
}

// assertNoPlainTextFailure checks that the failure was written once, by the
// framework, and not again as the plain text line aws-lambda-go prints.
func (inv invocation) assertNoPlainTextFailure(t *testing.T) {
	t.Helper()
	for _, line := range inv.lines {
		if !strings.HasPrefix(line, "{") {
			assert.NotContains(t, line, "errorMessage", "a plain text copy of the failure")
		}
	}
}

// recordWith returns the first log record whose message is msg.
func (inv invocation) recordWith(t *testing.T, msg string) record {
	t.Helper()
	for _, rec := range inv.records {
		if rec.str("message") == msg {
			return rec
		}
	}
	t.Fatalf("no %q record among %v", msg, inv.records)
	return nil
}
