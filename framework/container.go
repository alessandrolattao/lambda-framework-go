package framework

import (
	"cmp"
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/alessandrolattao/powertools-lambda-go/eventhandler"
	"github.com/alessandrolattao/powertools-lambda-go/logging"
	"github.com/alessandrolattao/powertools-lambda-go/metrics"
	"github.com/alessandrolattao/powertools-lambda-go/tracing"
	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/apigatewaymanagementapi"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/eventbridge"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sfn"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"

	"github.com/alessandrolattao/lambda-framework-go/internal/clientip"
	"github.com/alessandrolattao/lambda-framework-go/request"
)

// Container holds the dependencies a business handler works with: the
// Powertools logger, tracer and metrics, the AWS SDK clients and the
// database pool, to Postgres or to Aurora DSQL.
//
// A Container built by New creates the clients it is missing and the
// database pool on the first invocation, once per cold start; a client already
// in a field (a test fake) is kept. A Container assembled by hand is used
// exactly as it is; a nil Logger, Tracer or Metrics is simply not used.
type Container struct {
	Logger      *logging.Logger
	Tracer      *tracing.Tracer
	Metrics     *metrics.Metrics
	S3          *s3.Client
	EventBridge EventBridgeClient
	SFN         SFNClient
	Dynamo      *dynamodb.Client
	Websocket   *apigatewaymanagementapi.Client
	DB          *pgxpool.Pool

	awsConfig      aws.Config
	databaseURL    string
	dsqlEndpoint   string
	trustedProxies int
	service        string
	loggingOpts    []logging.Option
	tracingOpts    []tracing.Option
	metricsOpts    []metrics.Option
	routerOpts     []eventhandler.Option
	router         *eventhandler.Router

	// lifecycle is a pointer so that copies of the container share it with
	// the original.
	lifecycle *lifecycle
}

// EventBridgeClient is the part of *eventbridge.Client handlers use.
// Using an interface allows tests to inject a fake without hitting AWS.
type EventBridgeClient interface {
	PutEvents(ctx context.Context, params *eventbridge.PutEventsInput, optFns ...func(*eventbridge.Options)) (*eventbridge.PutEventsOutput, error)
}

// SFNClient is the part of *sfn.Client handlers use.
// Using an interface allows tests to inject a fake without hitting AWS.
type SFNClient interface {
	StartExecution(ctx context.Context, params *sfn.StartExecutionInput, optFns ...func(*sfn.Options)) (*sfn.StartExecutionOutput, error)
}

var (
	_ EventBridgeClient = (*eventbridge.Client)(nil)
	_ SFNClient         = (*sfn.Client)(nil)
)

// Option configures a Container built by New.
type Option func(*Container)

// WithDatabaseURL sets the Postgres connection string, in URL
// (postgres://user:pass@host:5432/db?sslmode=require) or keyword/value form.
// Without it New reads DATABASE_URL; with neither, and no DSQL endpoint, the
// container has no pool.
//
// An application deployed with SST reads the linked database resource in its
// main and passes the string here: the password then stays in the encrypted
// bundle SST ships, instead of a plaintext environment variable.
func WithDatabaseURL(url string) Option {
	return func(c *Container) { c.databaseURL = url }
}

// WithDSQLEndpoint sets the endpoint of the Aurora DSQL cluster the pool
// connects to, <cluster id>.dsql.<region>.on.aws, in the region in
// AWS_REGION. Without it New reads DSQL_ENDPOINT.
//
// DSQL has no password: the pool connects as the database user admin with an
// IAM token, which the AWS connector signs for every new connection with the
// function's credentials. The function's role needs dsql:DbConnectAdmin on
// the cluster; an SST link to the cluster grants it.
//
// A container gets one database: a DSQL endpoint together with a Postgres
// connection string (WithDatabaseURL or DATABASE_URL) fails the
// initialization.
func WithDSQLEndpoint(endpoint string) Option {
	return func(c *Container) { c.dsqlEndpoint = endpoint }
}

// WithTrustedProxies tells the container how many proxies of yours sit in
// front of API Gateway (CloudFront is one), so that ClientIP and the logs find
// the caller's address behind them. The default, 0, trusts nothing but the
// address API Gateway itself saw.
func WithTrustedProxies(n int) Option {
	return func(c *Container) { c.trustedProxies = max(n, 0) }
}

// WithService names the service the function belongs to, for the logger (the
// service key), the tracer (the X-Ray service and the Service annotation) and
// the metrics (the service dimension). The default is POWERTOOLS_SERVICE_NAME,
// else the function's name, which reads better in X-Ray and CloudWatch than
// Powertools' own "service_undefined".
func WithService(name string) Option {
	return func(c *Container) { c.service = name }
}

// WithTracerProvider replaces the tracer provider the Powertools tracer builds
// for Lambda (X-Ray UDP exporter), e.g. with one that exports over OTLP to a
// collector outside Lambda, or with one that records spans in a test.
func WithTracerProvider(tp trace.TracerProvider) Option {
	return WithTracing(tracing.WithTracerProvider(tp))
}

// WithLogging passes options to the Powertools logger (level, sampling,
// buffering, output format).
func WithLogging(opts ...logging.Option) Option {
	return func(c *Container) { c.loggingOpts = append(c.loggingOpts, opts...) }
}

// WithTracing passes options to the Powertools tracer.
func WithTracing(opts ...tracing.Option) Option {
	return func(c *Container) { c.tracingOpts = append(c.tracingOpts, opts...) }
}

// WithMetrics turns the Powertools metrics on and passes them options
// (namespace, default dimensions, a Datadog provider). Metrics are also on
// when POWERTOOLS_METRICS_NAMESPACE is set; otherwise the container has no
// Metrics, and no invocation flushes an empty metric set.
func WithMetrics(opts ...metrics.Option) Option {
	return func(c *Container) { c.metricsOpts = append(c.metricsOpts, opts...) }
}

// New returns a Container with the Powertools logger and tracer ready, and
// the metrics when they are configured (see WithMetrics). The AWS clients and the database pool follow on the first invocation
// (see Handle), or on an explicit InitDB / InitEventBridge outside the Lambda
// lifecycle.
//
// The utilities read their usual POWERTOOLS_* environment variables; the
// options above come first. New also routes the standard log package, which
// aws-lambda-go writes through, into the logger as structured records
// (runtimeLog), and OpenTelemetry's own errors to the logger at Warn: they are
// tracing trouble, not a failed invocation.
//
// The tracer registers its provider and the X-Ray propagator as the global
// OpenTelemetry ones, as the AWS migration guide does in main: that state is
// per process, so build one Container per process.
func New(opts ...Option) *Container {
	c := &Container{
		databaseURL:  os.Getenv("DATABASE_URL"),
		dsqlEndpoint: os.Getenv("DSQL_ENDPOINT"),
		service:      defaultService(),
		lifecycle:    &lifecycle{},
	}
	for _, opt := range opts {
		opt(c)
	}

	c.Logger = logging.New(append([]logging.Option{logging.WithService(c.service)}, c.loggingOpts...)...)
	c.Tracer = tracing.New(append([]tracing.Option{tracing.WithService(c.service)}, c.tracingOpts...)...)
	if len(c.metricsOpts) > 0 || os.Getenv("POWERTOOLS_METRICS_NAMESPACE") != "" {
		c.Metrics = metrics.New(append([]metrics.Option{metrics.WithService(c.service)}, c.metricsOpts...)...)
	}

	// aws-lambda-go writes through the standard log package; see runtimeLog.
	log.SetFlags(0)
	log.SetOutput(runtimeLog{logger: c.Logger.Logger})
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		c.Logger.Warn("opentelemetry", "err", err)
	}))
	return c
}

// defaultService is POWERTOOLS_SERVICE_NAME, else the function's name.
func defaultService() string {
	return cmp.Or(
		os.Getenv("POWERTOOLS_SERVICE_NAME"),
		os.Getenv("AWS_LAMBDA_FUNCTION_NAME"),
		filepath.Base(os.Args[0]),
	)
}

// clientIP returns the address of the client that sent r, looking past the
// trusted proxies set with WithTrustedProxies.
func (c *Container) clientIP(r *events.APIGatewayV2HTTPRequest) string {
	return request.ClientIP(r, c.trustedProxies)
}

// restClientIP is clientIP for a REST API request: the same rule, on
// requestContext.identity.sourceIp.
func (c *Container) restClientIP(r *events.APIGatewayProxyRequest) string {
	return clientip.REST(r, c.trustedProxies)
}

// Close releases what the container holds open: the database pool, and the
// tracer provider the tracer built, after exporting the spans it still holds
// (one passed with WithTracerProvider belongs to the caller). A Lambda never
// needs it, since every invocation flushes its spans and the environment is
// frozen and discarded as a whole; a task or CLI calls it before exiting.
func (c *Container) Close(ctx context.Context) error {
	if c.DB != nil {
		c.DB.Close()
	}
	if c.Tracer == nil {
		return nil
	}
	return shutdownError(c.Tracer.Shutdown(ctx))
}

// shutdownError wraps the error of the tracer's shutdown, if any.
func shutdownError(err error) error {
	if err != nil {
		return fmt.Errorf("shut the tracer down: %w", err)
	}
	return nil
}
