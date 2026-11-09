# lambda-framework-go

A small framework for AWS Lambda functions written in Go, on
[Powertools for AWS Lambda (Go)](https://github.com/alessandrolattao/powertools-lambda-go): what AWS recommends,
done the way AWS recommends it, in a comfortable shape. `main.go` configures;
each handler receives its clients and does its job.

It wires the Powertools logger, tracer and metrics, the Powertools Event
Handler router with typed, validated operations, the AWS SDK clients and a
database pool, to Postgres or to Aurora DSQL, around your business handlers,
and gives every failure one place to be reported.

Requires Go 1.27.

```sh
go get github.com/alessandrolattao/lambda-framework-go
```

## Packages

| Package    | What it does |
|------------|--------------|
| `framework` | `Container` with the dependencies; `Get`/`Post`/`Put`/`Patch`/`Delete` and `HTTP` for API Gateway HTTP APIs; `Handle` and `Consume` for every other event; error and panic reporting |
| `request`  | What typed binding does not cover: the JWT claims API Gateway verified, the client IP behind trusted proxies, comma-separated id lists |
| `response` | The error envelope, success responses with cache headers, CSV and content negotiation |

## Quick start

### An HTTP API

```go
package main

import (
	"context"
	"errors"
	"uuid"

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/jackc/pgx/v5"

	"github.com/alessandrolattao/lambda-framework-go/framework"
	"github.com/alessandrolattao/lambda-framework-go/response"
)

func main() {
	c := framework.New()
	c.Get("/orders/{id}", getOrder)
	lambda.Start(c.HTTP())
}

// orderID is what the route reads; the router binds and validates it.
type orderID struct {
	ID uuid.UUID `path:"id" format:"uuid"`
}

type order struct {
	ID    uuid.UUID `json:"id"`
	Total int64     `json:"total"`
}

func getOrder(ctx context.Context, c *framework.Container, in orderID) (order, error) {
	o := order{ID: in.ID}
	err := c.DB.QueryRow(ctx, "SELECT total FROM orders WHERE id = $1", in.ID).Scan(&o.Total)
	if errors.Is(err, pgx.ErrNoRows) {
		return order{}, response.NotFound("orderNotFound", "no order with this id", nil)
	}
	if err != nil {
		return order{}, response.InternalServerError(err, "serverError", "could not read the order", nil)
	}
	return o, nil
}
```

Input structs follow the Powertools Event Handler: fields tagged `path`,
`query`, `header`, `cookie`, a `Body` field decoded from JSON, constraints as
tags (`minimum`, `maxLength`, `pattern`, `enum`, `format`, `required`...). A
request that does not conform never reaches the handler: it gets a 422. The
same types describe the API: `c.Router().OpenAPIJSON()` is its OpenAPI
document.

### Any other event

Handlers that only succeed or fail (EventBridge, SQS, S3, CloudWatch Logs) use
`Consume`; handlers that answer with a result use `Handle`:

```go
lambda.Start(c.Consume(func(ctx context.Context, c *framework.Container, e events.SQSEvent) error {
	// ...
	return nil
}))
```

The other Powertools utilities (batch, idempotency, parameters, feature flags,
validation, parser...) work inside these handlers as they do anywhere.

## What happens around a handler

- **The Powertools decorators, stacked as Powertools recommends.** The logger's
  `InjectContext` (Lambda context keys, correlation id for API Gateway, ALB,
  Function URL and EventBridge events, sampling, buffering), the tracer's
  `CaptureLambdaHandler` (the invocation span, `ColdStart` and `Service`
  annotations) and, when metrics are configured, `LogMetrics` (one metric set
  per invocation, flushed at its end).
- **Initialization once per cold start.** The AWS config, the clients
  (`S3`, `EventBridge`, `SFN`, `Dynamo`, `Websocket`) and the database pool
  (Postgres or Aurora DSQL) are created on the first invocation and reused
  afterwards. A failed
  initialization is retried by the next invocation. A client you put in a field
  before that (a fake, in a test) is kept.
- **Per-invocation state in the context.** The handler shares the container,
  which is read-only once initialized; the request id, the client's address
  (`client_ip`) and the metric set live in the context. Log with the Context
  methods (`c.Logger.InfoContext(ctx, ...)`) and the records carry them, also
  when one execution environment runs several invocations at once (Lambda
  Managed Instances).
- **One door for failures.** Return the error and log nothing: the framework
  writes it once, at Error, with the request id. Wrap the context only the
  handler knows into the error (`fmt.Errorf("charge order %v: %w", id, err)`).
  A panic is written down with its stack.
- **Failures Lambda can tell apart.** A failure reaches Lambda (and Step
  Functions, which matches Retry and Catch rules on it) with an `errorType`
  that means something: the type of the application's or library's error in
  the chain (`PgError`, ...), or `HandlerError`, `InvalidEvent`,
  `InvalidResult`, `InitError`. aws-lambda-go's own plain text copy of the
  failure goes to Debug.
- **Every API request leaves a line.** `API request completed` carries the
  status, the route and the duration, so a 404 or a 422 is visible too.
- **API Gateway always gets an answer, in one envelope.** A `*response.Error`
  answers with `{"code","message","details","timestamp"}`; so do the router's
  own errors: an unknown route (`routeNotFound`), a request that fails
  validation (`invalidParam`, `invalidBody`, with the Powertools details), an
  unsupported content type, a Powertools `ServiceError`. A failure no handler
  answered becomes a 500 `serverError`. A 5xx is reported once, with its
  cause, which never reaches the client. Lambda counts such invocations as
  successes, so alarm on the error logs rather than on the `Errors` metric.
- **JSON v2 both ways.** Events are decoded and results encoded with
  `encoding/json/v2`; response bodies with sorted map keys.
- **Tracing with OpenTelemetry**, through the Powertools tracer: the X-Ray UDP
  exporter the AWS X-Ray migration guide prescribes for Lambda. Each
  invocation is a server span under the trace Lambda started, the handler runs
  in a `## businessLogic` span, and the AWS SDK calls and the database queries
  (through `otelpgx`: operation and SQL text, never the parameters) made with
  its context are spans below it. An API request is described on the
  invocation's span (X-Ray's http block, `http_route` and `http_status`
  annotations); a 5xx marks both spans as failed. Open your own spans with
  `c.Tracer.Capture(ctx, name, fn)`. Turn on active tracing on the function:
  without an X-Ray endpoint, tracing is off.

## Configuration

Options of `framework.New`:

| Option | Default | Meaning |
|--------|---------|---------|
| `WithService(name)` | `POWERTOOLS_SERVICE_NAME`, else the function name | The service of the logs, the traces and the metrics |
| `WithLogging(opts...)` | | Options of the Powertools logger (level, sampling, buffering, format) |
| `WithTracing(opts...)` | | Options of the Powertools tracer |
| `WithTracerProvider(tp)` | X-Ray UDP exporter | Another OpenTelemetry tracer provider |
| `WithMetrics(opts...)` | off | Turns the Powertools metrics on (namespace, dimensions, Datadog) |
| `WithRouter(opts...)` | | Options of the Event Handler router (CORS, debug, strip prefixes, OpenAPI) |
| `WithDatabaseURL(url)` | `DATABASE_URL` | Postgres connection string |
| `WithDSQLEndpoint(endpoint)` | `DSQL_ENDPOINT` | Aurora DSQL cluster endpoint, in the region in `AWS_REGION` |
| `WithTrustedProxies(n)` | `0` | Proxies in front of API Gateway (CloudFront is 1), used to find the client IP |

Environment variables: every `POWERTOOLS_*` variable of the utilities
(`POWERTOOLS_SERVICE_NAME`, `POWERTOOLS_LOG_LEVEL`, `POWERTOOLS_LOGGER_SAMPLE_RATE`,
`POWERTOOLS_METRICS_NAMESPACE`, `POWERTOOLS_TRACE_DISABLED`, `POWERTOOLS_DEV`
for indented logs while developing...), Lambda's `AWS_LAMBDA_LOG_LEVEL`, and:

| Variable | Meaning |
|----------|---------|
| `DATABASE_URL` | Postgres connection string, when `WithDatabaseURL` is not used |
| `DSQL_ENDPOINT` | Aurora DSQL cluster endpoint, when `WithDSQLEndpoint` is not used |
| `AWS_REGION` | The region of the Aurora DSQL cluster; Lambda sets it to the function's own |
| `WS_ENDPOINT` | WebSocket API endpoint; without it `Websocket` is nil |
| `AWS_ENDPOINT_URL`, `AWS_ENDPOINT_URL_S3` | A custom S3 endpoint (MinIO, LocalStack, Floci); S3 then uses path-style addressing |

Metrics are on when `WithMetrics` is used or `POWERTOOLS_METRICS_NAMESPACE` is
set; without them no invocation flushes an empty metric set.

A container has at most one database: Postgres with a connection string,
Aurora DSQL with a cluster endpoint. With neither there is no pool and `DB` is
nil; with both, the first invocation fails. Either way `DB` is a
`*pgxpool.Pool`, and handlers use it the same way.

### Postgres

The Postgres pool holds one connection per invocation the execution
environment can run at once, unless the connection string sets
`pool_max_conns`. Every statement is capped to the time the invocation has left.

### Aurora DSQL

DSQL has no password: a connection authenticates with an IAM token. The pool
goes through AWS's own connector,
[Aurora DSQL Connector for Go](https://docs.aws.amazon.com/aurora-dsql/latest/userguide/SECTION_program-with-go-pgx-connector.html)
on pgx, which signs a fresh token, valid 15 minutes, for every new connection
with the function's credentials. A warm execution environment that opens a
connection long after its cold start still gets in.

- The pool connects to the `postgres` database as the database user `admin`,
  over TLS verified against the endpoint. The function's role needs
  `dsql:DbConnectAdmin` on the cluster.
- It holds one connection per invocation the execution environment can run at
  once, and recycles them with the connector's own lifetimes (55 minutes, 10
  minutes idle), below the hour after which DSQL closes a connection.
- Statements are not capped server side: DSQL rejects `statement_timeout` and
  ends every transaction after 5 minutes on its own. A query still running
  when the invocation's deadline passes fails at that deadline, on the client.
- Queries run in pgx's `QueryExecModeDescribeExec`, as the connector sets it:
  safe across live schema changes, at the cost of one more round trip per
  query.
- DSQL resolves conflicting transactions with optimistic concurrency control:
  the transaction that commits second fails with SQLSTATE `OC000`, `OC001` or
  `40001`, and is meant to be retried. The connector's
  [`occretry`](https://pkg.go.dev/github.com/awslabs/aurora-dsql-connectors/go/pgx/occretry)
  package retries a transaction or a statement on `c.DB`.

### With SST

SST encrypts linked resources into the function bundle, so the database
password never sits in a plaintext environment variable. Keep it that way:
read the linked resource in `main` and pass the connection string.

#### Postgres

```go
import (
	"fmt"
	"net"
	"net/url"

	"github.com/sst/sst/v3/sdk/golang/resource"
)

func databaseURL() (string, error) {
	get := func(key string) (string, error) {
		v, err := resource.Get("Database", key)
		if err != nil {
			return "", err
		}
		return fmt.Sprint(v), nil
	}
	host, err := get("host")
	if err != nil {
		return "", err
	}
	port, _ := get("port")
	user, _ := get("username")
	password, _ := get("password")
	name, _ := get("database")

	u := url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(user, password),
		Host:   net.JoinHostPort(host, port),
		Path:   "/" + name,
	}
	return u.String(), nil
}

func main() {
	dsn, err := databaseURL()
	if err != nil {
		panic(err)
	}
	c := framework.New(framework.WithDatabaseURL(dsn))
	c.Get("/orders/{id}", getOrder)
	lambda.Start(c.HTTP())
}
```

#### Aurora DSQL

Link the cluster to the function: the link carries the endpoint and grants the
function's role `dsql:DbConnect` and `dsql:DbConnectAdmin` on the cluster.

```ts
const database = new sst.aws.Dsql("Database");

new sst.aws.Function("Orders", {
  handler: "./cmd/orders",
  runtime: "go",
  link: [database],
});
```

Read the endpoint in `main` and pass it on; the region is the function's own,
in `AWS_REGION`.

```go
import (
	"fmt"

	"github.com/sst/sst/v3/sdk/golang/resource"
)

func main() {
	endpoint, err := resource.Get("Database", "endpoint")
	if err != nil {
		panic(err)
	}
	c := framework.New(framework.WithDSQLEndpoint(fmt.Sprint(endpoint)))
	c.Get("/orders/{id}", getOrder)
	lambda.Start(c.HTTP())
}
```

## Testing a handler

Build a `Container` by hand with fakes in its fields and call the business
handler directly: a hand-built container is never initialized behind your back,
and it does not trace.

```go
c := &framework.Container{Logger: logging.New(logging.WithWriter(io.Discard)), EventBridge: fakeBus}
got, err := getOrder(t.Context(), c, orderID{ID: id})
```

To go through the framework as well, routing and validation included, call the
Lambda handler with an event: `c.HTTP().Call(ctx, request)`, or
`c.Handle(h).Call(ctx, event)`.

## Development

```sh
go test ./...
golangci-lint run ./...
```

The lint configuration is strict, the same as Powertools'. The Aurora DSQL
tests need neither a cluster nor the network: the pool connects lazily, and
they sign tokens with fixed credentials. The Postgres tests skip themselves
without a database. To run them:

```sh
docker run -d --rm -e POSTGRES_PASSWORD=postgres -p 55432:5432 postgres:18-alpine
TEST_DATABASE_URL='postgres://postgres:postgres@localhost:55432/postgres?sslmode=disable' go test ./...
```

The parsers that read what a client sends have fuzz tests; `go test` runs
their seeds, and fuzzing one goes like this:

```sh
go test -run '^$' -fuzz '^FuzzClientIP$' -fuzztime 1m ./request
```

### End-to-end tests

`e2e/` is a module of its own (so its test dependencies stay out of this
one) that runs two real Lambdas, an API Gateway HTTP API one and an
EventBridge one, the way Lambda does: built for `provided.al2023`, run in the
Lambda base image under the Runtime Interface Emulator, with the memory and
the CPU share of a Lambda size. Postgres runs next to them (Aurora DSQL has no
local version, so the suite runs on Postgres), and a UDP listener
stands in for the X-Ray endpoint, so the tests check responses, database
writes, log records and exported spans, on the happy paths and the failing
ones. It needs Docker.

```sh
cd e2e
go test -run TestOrders ./...   # behaviour, about 15 s
go test ./...                   # plus startup times at 128, 512 and 1024 MB, about 100 s
```

`TestStartup` measures five cold starts per size and fails when the median
cold invocation goes over its limit. `E2E_MAX_COLD_START_SCALE=2` doubles the
limits on a slower machine; `E2E_STARTUP_REPORT=startup.md` writes the table
of measurements to a file.

## License

MIT, see [LICENSE](LICENSE).
