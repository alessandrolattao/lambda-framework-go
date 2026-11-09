package framework

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/eventbridge"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testRegion is the AWS region the tests configure.
const testRegion = "eu-west-1"

// TestInitEventBridge_CreatesClientWhenNil covers the Fargate-task shape: New
// without any invocation leaves EventBridge nil, and a task that emitted an
// event on it would dereference nil. InitEventBridge must mount a real client
// on that bare container.
func TestInitEventBridge_CreatesClientWhenNil(t *testing.T) {
	t.Setenv("AWS_REGION", testRegion)
	c := New()
	require.Nil(t, c.EventBridge, "a fresh container has no EventBridge client")

	require.NoError(t, c.InitEventBridge(t.Context()))
	assert.NotNil(t, c.EventBridge, "InitEventBridge mounts the client")
}

// TestInitEventBridge_IsIdempotent guards the guarantee that an already-mounted
// client is preserved, never recreated. This is what keeps a test fake alive
// across an InitEventBridge call.
func TestInitEventBridge_IsIdempotent(t *testing.T) {
	t.Setenv("AWS_REGION", testRegion)
	c := New()
	existing := eventbridge.NewFromConfig(aws.Config{Region: testRegion})
	c.EventBridge = existing

	require.NoError(t, c.InitEventBridge(t.Context()))
	assert.Same(t, existing, c.EventBridge, "an existing client is not replaced")
}

func TestInitialize_BuildsTheClientsOnce(t *testing.T) {
	t.Setenv("AWS_REGION", testRegion)
	t.Setenv("DATABASE_URL", "")
	t.Setenv("DSQL_ENDPOINT", "")
	c := New()

	require.NoError(t, c.initialize(t.Context(), quietLogger().Logger))
	require.NotNil(t, c.S3)
	require.NotNil(t, c.EventBridge)
	require.NotNil(t, c.SFN)
	require.NotNil(t, c.Dynamo)
	assert.Nil(t, c.Websocket, "no WS_ENDPOINT, no websocket client")
	assert.Nil(t, c.DB, "no linked database, no pool and no error")

	first := c.S3
	require.NoError(t, c.initialize(t.Context(), quietLogger().Logger))
	assert.Same(t, first, c.S3, "a warm invocation reuses the clients of the cold start")
}

func TestInitialize_LeavesAHandBuiltContainerAlone(t *testing.T) {
	t.Parallel()
	fake := eventbridge.NewFromConfig(aws.Config{Region: testRegion})
	c := &Container{EventBridge: fake}

	require.NoError(t, c.initialize(t.Context(), quietLogger().Logger))
	assert.Same(t, fake, c.EventBridge, "the fake a test put there survives")
	assert.Nil(t, c.S3, "nothing is created behind the test's back")
}

func TestInitialize_KeepsAClientAlreadyInItsField(t *testing.T) {
	t.Setenv("AWS_REGION", testRegion)
	c := New()
	fake := eventbridge.NewFromConfig(aws.Config{Region: testRegion})
	c.EventBridge = fake

	require.NoError(t, c.initialize(t.Context(), quietLogger().Logger))
	assert.Same(t, fake, c.EventBridge, "the fake a test put on a New container survives")
	assert.NotNil(t, c.S3, "the missing clients are created")
}

func TestInitialize_RetriesAfterAFailure(t *testing.T) {
	t.Setenv("AWS_REGION", testRegion)
	t.Setenv("AWS_PROFILE", "profile-that-does-not-exist")
	c := New()

	require.Error(t, c.initialize(t.Context(), quietLogger().Logger), "a broken AWS config fails the invocation")
	assert.False(t, c.lifecycle.done.Load())

	t.Setenv("AWS_PROFILE", "")
	require.NoError(t, c.initialize(t.Context(), quietLogger().Logger), "the next invocation tries again")
	assert.True(t, c.lifecycle.done.Load())
}

func TestInitialize_ConcurrentInvocationsInitializeOnce(t *testing.T) {
	t.Setenv("AWS_REGION", testRegion)
	c := New()
	c.Logger = quietLogger()

	var seen sync.Map // the S3 clients the invocations got
	h := c.Consume(func(_ context.Context, inv *Container, _ events.CloudWatchEvent) error {
		seen.Store(inv.S3, true)
		return nil
	})

	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			assert.NoError(t, h.Call(lambdaContext(t), events.CloudWatchEvent{}))
		})
	}
	wg.Wait()

	clients := 0
	for client := range seen.Range {
		assert.NotNil(t, client)
		clients++
	}
	assert.Equal(t, 1, clients, "every invocation shares the clients of a single initialization")
}

func TestOptions(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://env@db.example.com/app")

	assert.Equal(t, "postgres://env@db.example.com/app", New().databaseURL, "DATABASE_URL by default")
	assert.Equal(t, "postgres://code@db.example.com/app",
		New(WithDatabaseURL("postgres://code@db.example.com/app")).databaseURL, "the option wins")
	assert.Equal(t, 1, New(WithTrustedProxies(1)).trustedProxies)
	assert.Zero(t, New(WithTrustedProxies(-3)).trustedProxies, "a negative count trusts nothing")
}

func TestInitDB_WithoutAConnectionStringThereIsNoPool(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	t.Setenv("DSQL_ENDPOINT", "")
	c := New()

	require.NoError(t, c.InitDB(t.Context()))
	assert.Nil(t, c.DB)
}

func TestInitDBWithHost_OpensThePoolAndCloseReleasesIt(t *testing.T) {
	t.Setenv("DSQL_ENDPOINT", "")
	c := New(WithDatabaseURL("postgres://u:p@db.example.com:5432/app"))

	require.NoError(t, c.InitDBWithHost(t.Context(), "localhost", "15432"))
	require.NotNil(t, c.DB, "pgxpool connects lazily, so the pool exists without a server")
	assert.Equal(t, "localhost", c.DB.Config().ConnConfig.Host)
	require.NoError(t, c.Close(t.Context()))
}

// testDSQLEndpoint is the endpoint of a DSQL cluster that does not exist.
const testDSQLEndpoint = "abcdefghijklmnopqrstuvwxyz.dsql.eu-west-1.on.aws"

// useStaticAWSCredentials points the AWS credential chain at fixed keys, so
// that the DSQL tokens are signed without a real account.
func useStaticAWSCredentials(t *testing.T) {
	t.Helper()
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIDEXAMPLE")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")
	t.Setenv("DATABASE_URL", "")
}

func TestOptions_DSQLEndpoint(t *testing.T) {
	t.Setenv("DSQL_ENDPOINT", "env.dsql.eu-west-1.on.aws")

	assert.Equal(t, "env.dsql.eu-west-1.on.aws", New().dsqlEndpoint, "DSQL_ENDPOINT by default")
	assert.Equal(t, testDSQLEndpoint, New(WithDSQLEndpoint(testDSQLEndpoint)).dsqlEndpoint, "the option wins")
}

func TestInitDB_OpensADSQLPool(t *testing.T) {
	useStaticAWSCredentials(t)
	t.Setenv("AWS_REGION", testRegion)
	c := New(WithDSQLEndpoint(testDSQLEndpoint))

	require.NoError(t, c.InitDB(t.Context()))
	require.NotNil(t, c.DB, "pgxpool connects lazily, so the pool exists without a cluster")
	t.Cleanup(c.DB.Close)
	cfg := c.DB.Config()
	assert.Equal(t, testDSQLEndpoint, cfg.ConnConfig.Host)
	assert.Equal(t, "admin", cfg.ConnConfig.User)
	assert.EqualValues(t, 1, cfg.MaxConns, "one connection per invocation at a time")
	assert.Nil(t, cfg.PrepareConn, "DSQL rejects the statement_timeout Postgres gets")
}

func TestInitDBWithHost_DSQLDialsTheTunnel(t *testing.T) {
	useStaticAWSCredentials(t)
	t.Setenv("AWS_REGION", testRegion)
	c := New(WithDSQLEndpoint(testDSQLEndpoint))

	require.NoError(t, c.InitDBWithHost(t.Context(), "localhost", "15432"))
	require.NotNil(t, c.DB)
	t.Cleanup(c.DB.Close)
	cfg := c.DB.Config()
	cc := cfg.ConnConfig.Copy()
	require.NoError(t, cfg.BeforeConnect(t.Context(), cc))
	assert.Equal(t, "localhost", cc.Host)
	assert.EqualValues(t, 15432, cc.Port)
	assert.True(t, strings.HasPrefix(cc.Password, testDSQLEndpoint+"?"), "the token is signed for the endpoint")
}

func TestInitDB_RefusesTwoDatabases(t *testing.T) { //nolint:paralleltest // New sets the process-wide log and OpenTelemetry state
	c := New(WithDatabaseURL("postgres://u:p@db.example.com:5432/app"), WithDSQLEndpoint(testDSQLEndpoint))

	require.ErrorIs(t, c.InitDB(t.Context()), errTwoDatabases)
	assert.Nil(t, c.DB)
}

// TestInitialize_RetriesAFailedDSQLPool covers a cold start whose DSQL pool
// cannot be created (here, no region to sign the tokens for): that invocation
// fails, and the next one tries again.
func TestInitialize_RetriesAFailedDSQLPool(t *testing.T) {
	useStaticAWSCredentials(t)
	t.Setenv("AWS_REGION", "")
	t.Setenv("AWS_DEFAULT_REGION", "")
	c := New(WithDSQLEndpoint("db.example.com"))

	require.Error(t, c.initialize(t.Context(), quietLogger().Logger))
	assert.Nil(t, c.DB)

	t.Setenv("AWS_REGION", testRegion)
	require.NoError(t, c.initialize(t.Context(), quietLogger().Logger))
	require.NotNil(t, c.DB)
	t.Cleanup(c.DB.Close)
}
