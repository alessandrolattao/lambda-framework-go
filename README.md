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
| `framework` | `Container` with the dependencies; `Get`/`Post`/`Put`/`Patch`/`Delete`, then `HTTP` for API Gateway HTTP APIs or `REST` for REST APIs; `Handle` and `Consume` for every other event; error and panic reporting |
| `request`  | What typed binding does not cover: the claims API Gateway's authorizer verified, the client IP behind trusted proxies, comma-separated id lists |
| `response` | The error envelope, success responses with cache headers, CSV and content negotiation |
| `migrate`  | The business handler of a migration Lambda: goose SQL migrations on Aurora DSQL (or Postgres) |
| `sst`      | The resources SST links to a Lambda: the value of an `sst.Secret`, the properties of any other component |

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

## REST API

The routes serve an API Gateway REST API with a Lambda proxy integration
(payload format 1.0) as they serve an HTTP API: moving a Lambda from one to
the other changes only the line that starts it.

```go
func main() {
	c := framework.New()
	c.Get("/orders/{id}", getOrder)
	lambda.Start(c.REST())
}
```

An HTTP API is cheaper and faster; choose a REST API when the API needs what
only REST APIs offer, response streaming (server-sent events) for one. `REST`
answers each request with one buffered response, as `HTTP` does.

A REST API request gets what an HTTP API request gets: the same binding,
validation, responses and error envelope, the 500 envelope for a failure, the
`API request received` and `API request completed` records, the span, and
`client_ip`. In the records `routeKey` is the method and the resource
(`GET /orders/{id}`); on the span the route is the resource and the path is
the request path. `c.ClientIP(ctx)` reads `requestContext.identity.sourceIp`
and `X-Forwarded-For`, with the trusted proxies of `WithTrustedProxies`, as
behind an HTTP API.

### Cognito user pool authorizer

The REST API counterpart of an HTTP API's JWT authorizer is a Cognito user
pool authorizer. It verifies the token in the `Authorization` header (the ID
token; the access token when the route sets scopes) and hands its claims to
the function in `requestContext.authorizer.claims`. `request.Claims`,
`request.Claim` and `request.UserID` read them as they read the JWT claims of
an HTTP API:

```go
func getProfile(ctx context.Context, c *framework.Container, _ struct{}) (profile, error) {
	userID, err := request.UserID(ctx) // the userId claim
	if err != nil {
		return profile{}, response.Unauthorized("unauthorized", "no user in the token", nil)
	}
	...
}
```

Every claim is a string, as behind an HTTP API: a claim the authorizer hands
over as a number, a boolean, an array or an object is its JSON text
(`1700000000` is `"1700000000"`), and a `null` claim is missing. Without
claims (no authorizer, or a Lambda authorizer, whose context is not read) the
functions answer `request.ErrClaimsNotFound`.

### With SST

One Lambda serves every route of the router, behind a proxy resource;
`api.deploy()` comes after the routes.

```ts
const userPool = new sst.aws.CognitoUserPool("UserPool");
const database = new sst.aws.Dsql("Database");

const api = new sst.aws.ApiGatewayV1("Api");
const cognito = api.addAuthorizer({
  name: "Cognito",
  userPools: [userPool.arn],
});
api.route(
  "ANY /{proxy+}",
  { handler: "./cmd/orders", runtime: "go", link: [database] },
  { auth: { cognito: { authorizer: cognito.id } } },
);
api.deploy();
```

```go
func main() {
	var db struct {
		Endpoint string `json:"endpoint"`
	}
	if err := sst.Get("Database", &db); err != nil {
		panic(err)
	}
	c := framework.New(framework.WithDSQLEndpoint(db.Endpoint))
	c.Get("/orders/{id}", getOrder)
	lambda.Start(c.REST())
}
```

Behind the proxy resource every request has the resource `/{proxy+}`, which
is then the `routeKey` of the records and the route of the span; a route of
its own (`api.route("GET /orders/{id}", ...)`) keeps them apart.

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
read the linked resource with the `sst` package in `main`, once per cold
start, and pass it on. `sst.Get` decodes a linked resource into a struct
whose JSON tags name the properties its component links; `sst.Secret` returns
the value of an `sst.Secret`. Both take the name the component has in
`sst.config.ts`, and a resource that is not linked to the function is an error
that names it: `sst: linked resource "Database": not found`.

#### Postgres

```go
import (
	"net"
	"net/url"
	"strconv"

	"github.com/alessandrolattao/lambda-framework-go/sst"
)

// database holds the properties an sst.aws.Postgres links.
type database struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Username string `json:"username"`
	Password string `json:"password"`
	Database string `json:"database"`
}

func databaseURL() (string, error) {
	var db database
	if err := sst.Get("Database", &db); err != nil {
		return "", err
	}
	u := url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(db.Username, db.Password),
		Host:   net.JoinHostPort(db.Host, strconv.Itoa(db.Port)),
		Path:   "/" + db.Database,
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
import "github.com/alessandrolattao/lambda-framework-go/sst"

func main() {
	var db struct {
		Endpoint string `json:"endpoint"`
	}
	if err := sst.Get("Database", &db); err != nil {
		panic(err)
	}
	c := framework.New(framework.WithDSQLEndpoint(db.Endpoint))
	c.Get("/orders/{id}", getOrder)
	lambda.Start(c.HTTP())
}
```

#### Secrets

Link the `sst.Secret` to the function and read its value in `main`:

```ts
const turnstileKey = new sst.Secret("TurnstileSecretKey");

new sst.aws.Function("Signups", {
  handler: "./cmd/signups",
  runtime: "go",
  link: [turnstileKey],
});
```

```go
import "github.com/alessandrolattao/lambda-framework-go/sst"

func main() {
	turnstileKey, err := sst.Secret("TurnstileSecretKey")
	if err != nil {
		panic(err)
	}
	s := signups{turnstileKey: turnstileKey}
	c := framework.New()
	c.Post("/signups", s.create)
	lambda.Start(c.HTTP())
}

type signups struct {
	turnstileKey string
}

func (s signups) create(ctx context.Context, c *framework.Container, in signup) (account, error) {
	// Verify in.Body.Token with s.turnstileKey, then write to c.DB.
	...
}
```

## Migrations

Each database gets a migration Lambda: a `main` package that embeds its goose
SQL files and hands `migrate.Up` to `Handle`. The code that runs them is in
the framework; the Lambda brings only its SQL.

```go
package main

import (
	"embed"

	"github.com/aws/aws-lambda-go/lambda"

	"github.com/alessandrolattao/lambda-framework-go/framework"
	"github.com/alessandrolattao/lambda-framework-go/migrate"
)

//go:embed *.sql
var migrations embed.FS

func main() {
	c := framework.New()
	lambda.Start(c.Handle(migrate.Up(migrations)))
}
```

It is a business handler like any other: it gets the container's logging,
tracing, error reporting and pool, and connects as the container does, to
DSQL with `WithDSQLEndpoint` or `DSQL_ENDPOINT`, in the region in
`AWS_REGION`, as `admin` with an IAM token. Every invocation, whatever its
payload, applies the migrations the database has not applied yet, up to the
latest, and answers with what it did:

```json
{"version": 3, "applied": [2, 3]}
```

An invocation with nothing new applies nothing and succeeds. goose logs
through the container's logger, at Info, every statement it runs and every
migration it applies (`logger=goose`): the logs of a failed deploy show its
last statement. Migrations go up only: no `Down` section runs, and there are no Go migrations. `migrate` is a
package of its own, which `framework` does not import: goose is linked into
the migration Lambdas only, and the other Lambdas keep their size and their
cold start.

What DSQL asks of a migration:

- **`-- +goose NO TRANSACTION` in every file.** A DSQL transaction holds at
  most one DDL statement, and never DDL together with DML. Without a
  transaction around the file, each statement is a transaction of its own.
  `Up` checks every file before it applies anything, and fails naming the
  files without the annotation.

  ```sql
  -- +goose NO TRANSACTION
  -- +goose Up
  CREATE TABLE orders (id uuid PRIMARY KEY, total bigint NOT NULL);
  CREATE INDEX ASYNC orders_total ON orders (total);
  ```

- **`CREATE INDEX ASYNC`** is how DSQL creates an index: it returns at once
  and builds the index in a job. After each migration `Up` waits, with
  `sys.wait_for_job`, for every asynchronous job not yet completed, so the next
  migration finds the schema complete; a job that ends failed fails the
  invocation, naming the job and the migration.
- **Conflicts are retried.** A statement that loses an optimistic concurrency
  conflict (SQLSTATE `40001`, `OC000`, `OC001`) committed nothing, and runs
  again, up to 5 times, after a random wait of at most 100 ms, then 200 ms,
  doubling, never more than 3 seconds. A migration that still fails fails the
  invocation: the migrations before it stay applied, and the next invocation
  starts from the one that failed. So does the deploy that invoked it.
- **The role needs `dsql:DbConnectAdmin`** on the cluster: migrations change
  the schema as `admin`.
- **The timeout covers the job waits.** An index build on a large table takes
  minutes: give the function a timeout above the longest one (Lambda allows up
  to 15 minutes).
- **Runs never overlap.** There is no lock (DSQL has no advisory locks):
  invoke the migration Lambda once per deploy and wait for its answer before
  the next deploy.

On Postgres the same Lambda works with `DATABASE_URL`; there is no `sys.jobs`
there, and the job wait is skipped.

### With SST

Link the cluster to the migration function, as to any other, and give it a
timeout for the index builds. Invoking it on each deploy is up to your
infrastructure.

```ts
const database = new sst.aws.Dsql("Database");

new sst.aws.Function("OrdersMigrations", {
  handler: "./cmd/orders-migrations",
  runtime: "go",
  link: [database],
  timeout: "15 minutes",
});
```

```go
func main() {
	var db struct {
		Endpoint string `json:"endpoint"`
	}
	if err := sst.Get("Database", &db); err != nil {
		panic(err)
	}
	c := framework.New(framework.WithDSQLEndpoint(db.Endpoint))
	lambda.Start(c.Handle(migrate.Up(migrations)))
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
Lambda handler with an event: `c.HTTP().Call(ctx, request)`,
`c.REST().Call(ctx, request)`, or `c.Handle(h).Call(ctx, event)`.

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

The migration tests run goose on that Postgres, each in a schema of its own,
and skip without `TEST_DATABASE_URL`. One of them needs a real Aurora DSQL
cluster, because only DSQL has `CREATE INDEX ASYNC` and its jobs: it runs
with the cluster endpoint in `TEST_DSQL_ENDPOINT` and AWS credentials allowed
`dsql:DbConnectAdmin` on it, and skips without them.

The parsers that read what a client sends have fuzz tests; `go test` runs
their seeds, and fuzzing one goes like this:

```sh
go test -run '^$' -fuzz '^FuzzClientIP$' -fuzztime 1m ./request
```

### End-to-end tests

`e2e/` is a module of its own (so its test dependencies stay out of this
one) that runs real Lambdas, the orders API behind an API Gateway HTTP API
and behind a REST API (the same routes, in `e2e/lambdas/orders`) and an
EventBridge consumer, the way Lambda does: built for `provided.al2023`, run in the
Lambda base image under the Runtime Interface Emulator, with the memory and
the CPU share of a Lambda size. Postgres runs next to them (Aurora DSQL has no
local version, so the suite runs on Postgres), and a UDP listener
stands in for the X-Ray endpoint, so the tests check responses, database
writes, log records and exported spans, on the happy paths and the failing
ones. It needs Docker.

```sh
cd e2e
go test -run TestOrders ./...   # behaviour, on HTTP API and REST API events
go test ./...                   # plus startup times at 128, 512 and 1024 MB, about 100 s
```

`TestStartup` measures five cold starts per size and fails when the median
cold invocation goes over its limit. `E2E_MAX_COLD_START_SCALE=2` doubles the
limits on a slower machine; `E2E_STARTUP_REPORT=startup.md` writes the table
of measurements to a file.

## License

MIT, see [LICENSE](LICENSE).
