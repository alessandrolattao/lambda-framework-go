package framework

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/apigatewaymanagementapi"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/eventbridge"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sfn"
	"github.com/aws/smithy-go/tracing"

	"github.com/alessandrolattao/lambda-framework-go/internal/postgres"
)

// lifecycle records whether a Container built by New is initialized.
type lifecycle struct {
	mu   sync.Mutex
	done atomic.Bool
}

// initialize creates the AWS clients and the database pool the first time a
// handler runs, which AWS recommends over doing it on every invocation: the
// clients and the pool outlive the invocation and are reused by every warm one.
//
// It logs how long it took at Info: that is the part of the cold start that
// happens inside the first invocation. It is marked done only on success, so a failure goes back to the invocation
// that hit it and the next one tries again. Once done it costs one atomic
// load; until then the lock makes concurrent invocations wait for a single
// initialization. Clients already in their fields are kept. A Container
// assembled by hand has no lifecycle and is never initialized.
func (c *Container) initialize(ctx context.Context, logger *slog.Logger) error {
	if c.lifecycle == nil || c.lifecycle.done.Load() {
		return nil
	}
	c.lifecycle.mu.Lock()
	defer c.lifecycle.mu.Unlock()
	if c.lifecycle.done.Load() {
		return nil
	}
	start := time.Now()

	if err := c.loadAWSConfig(ctx); err != nil {
		return err
	}
	// Every client traces its calls through the SDK's own OpenTelemetry
	// support, as the AWS SDK for Go v2 guide prescribes.
	tracer := c.sdkTracerProvider()

	if c.S3 == nil {
		c.S3 = s3.NewFromConfig(c.awsConfig, func(o *s3.Options) {
			// Path-style addressing is for S3-compatible local endpoints
			// (MinIO, LocalStack), which cannot resolve bucket subdomains.
			// Real S3 gets the virtual-hosted style AWS recommends.
			o.UsePathStyle = customS3Endpoint()
			o.TracerProvider = tracer
		})
	}
	if c.EventBridge == nil {
		c.EventBridge = c.newEventBridge()
	}
	if c.SFN == nil {
		c.SFN = sfn.NewFromConfig(c.awsConfig, func(o *sfn.Options) {
			o.TracerProvider = tracer
		})
	}
	if c.Dynamo == nil {
		c.Dynamo = dynamodb.NewFromConfig(c.awsConfig, func(o *dynamodb.Options) {
			o.TracerProvider = tracer
		})
	}
	if c.Websocket == nil {
		c.Websocket = newWebsocketClient(c.awsConfig, tracer)
	}

	if err := c.InitDB(ctx); err != nil {
		return err
	}

	c.lifecycle.done.Store(true)
	// The cold start the Lambda REPORT line cannot show: Init Duration stops
	// before the first invocation, and this work happens inside it.
	logger.InfoContext(ctx, "container initialized", "durationMs", milliseconds(time.Since(start)))
	return nil
}

// customS3Endpoint reports whether the AWS SDK is pointed at an S3 endpoint
// other than AWS's own, through the variables it reads for that.
func customS3Endpoint() bool {
	return os.Getenv("AWS_ENDPOINT_URL_S3") != "" || os.Getenv("AWS_ENDPOINT_URL") != ""
}

// loadAWSConfig loads the default AWS configuration for the container.
func (c *Container) loadAWSConfig(ctx context.Context) error {
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return fmt.Errorf("load the AWS config: %w", err)
	}
	c.awsConfig = cfg
	return nil
}

// newWebsocketClient creates an API Gateway Management API client scoped to
// the WebSocket API endpoint in WS_ENDPOINT. Despite the AWS service name
// ("management"), this client only sends messages to and closes existing
// client connections: it doesn't configure the API itself. Lambdas that don't
// push to WebSocket connections get nil.
func newWebsocketClient(cfg aws.Config, tracer tracing.TracerProvider) *apigatewaymanagementapi.Client {
	endpoint := os.Getenv("WS_ENDPOINT")
	if endpoint == "" {
		return nil
	}
	return apigatewaymanagementapi.NewFromConfig(cfg, func(o *apigatewaymanagementapi.Options) {
		o.BaseEndpoint = new(endpoint)
		o.TracerProvider = tracer
	})
}

// sdkTracerProvider is the tracer's provider adapted to the AWS SDK, or nil,
// which the SDK reads as no tracing, for a container without a tracer.
func (c *Container) sdkTracerProvider() tracing.TracerProvider {
	if c.Tracer == nil {
		return nil
	}
	return c.Tracer.AWSTracerProvider()
}

// InitDB opens the Postgres pool from the connection string (WithDatabaseURL,
// or DATABASE_URL). A Lambda gets it on its first invocation; code that runs
// outside the handler lifecycle (e.g. a Fargate task CLI) calls it directly.
// Without a connection string there is no pool and no error. Idempotent: an
// existing pool is kept.
func (c *Container) InitDB(ctx context.Context) error {
	return c.InitDBWithHost(ctx, "", "")
}

// InitDBWithHost is like InitDB but connects to an overridden host/port while
// keeping the credentials and database name of the connection string, e.g. to
// reach the database through a tunnel on localhost. Empty overrides keep the
// original values.
func (c *Container) InitDBWithHost(ctx context.Context, host, port string) error {
	if c.DB != nil || c.databaseURL == "" {
		return nil
	}
	pool, err := postgres.Connect(ctx, c.databaseURL, postgres.WithHost(host, port))
	if err != nil {
		return fmt.Errorf("connect to Postgres: %w", err)
	}
	c.DB = pool
	return nil
}

// InitEventBridge mounts the EventBridge client for code that runs outside the
// Lambda lifecycle (e.g. a Fargate task CLI), which never invokes a handler.
// It loads the AWS config on demand since a bare New has none. Idempotent: an
// already-mounted client (including a test fake) is left untouched, mirroring
// InitDB.
func (c *Container) InitEventBridge(ctx context.Context) error {
	if c.EventBridge != nil {
		return nil
	}
	if err := c.loadAWSConfig(ctx); err != nil {
		return err
	}
	c.EventBridge = c.newEventBridge()
	return nil
}

func (c *Container) newEventBridge() *eventbridge.Client {
	return eventbridge.NewFromConfig(c.awsConfig, func(o *eventbridge.Options) {
		o.TracerProvider = c.sdkTracerProvider()
	})
}
