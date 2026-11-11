package sst_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sst/sst/v3/sdk/golang/resource"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/alessandrolattao/lambda-framework-go/sst"
)

// SST's SDK reads the linked resources once, when its package initializes,
// as the Lambda's process starts: the bundle file named by SST_KEY_FILE,
// decrypted with the key in SST_KEY, and the SST_RESOURCE_* variables. A test
// cannot hand them over afterwards, so each test runs itself again in a child
// process started with the environment of the Lambda it stands for, and makes
// its assertions there.

// lambdaVar marks the child process.
const lambdaVar = "LAMBDA_FRAMEWORK_GO_SST_TEST"

// bundleKey is the base64 AES-256 key of testdata/resource.enc, the bundle
// SST would build for a function linked to an Aurora DSQL cluster Database
// and an sst.Secret TurnstileSecretKey. It was sealed as SST's bundler seals
// one, AES-GCM with a zero nonce and the tag after the ciphertext:
//
//	block, _ := aes.NewCipher([]byte("lambda-framework-go sst fixture!"))
//	gcm, _ := cipher.NewGCM(block)
//	gcm.Seal(nil, make([]byte, 12), plaintext, nil)
//
// with this plaintext:
//
//	{"Database":{"endpoint":"bundle.dsql.eu-west-1.on.aws","region":"eu-west-1","type":"sst:aws:Dsql"},"TurnstileSecretKey":{"type":"sst:sst:Secret","value":"turnstile-from-bundle"}}
const bundleKey = "bGFtYmRhLWZyYW1ld29yay1nbyBzc3QgZml4dHVyZSE="

// inLambda reports whether this process is the Lambda of t. When it is not,
// it runs t again in a child process with the given environment variables,
// on top of this process's environment without its SST variables, and fails
// t when the child fails.
func inLambda(t *testing.T, env ...string) bool {
	t.Helper()
	if os.Getenv(lambdaVar) == "1" {
		return true
	}

	//nolint:gosec // G204: the test binary runs one of its own tests again.
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^"+t.Name()+"$", "-test.v")
	cmd.Env = slices.DeleteFunc(os.Environ(), func(kv string) bool { return strings.HasPrefix(kv, "SST_") })
	cmd.Env = append(cmd.Env, lambdaVar+"=1")
	cmd.Env = append(cmd.Env, env...)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "the Lambda failed:\n%s", out)
	// A pattern that matches no test passes too: make sure this one ran.
	require.Contains(t, string(out), "--- PASS: "+t.Name()+" ", "the Lambda did not run the test:\n%s", out)
	return false
}

type dsqlCluster struct {
	Endpoint string `json:"endpoint"`
	Region   string `json:"region"`
}

type postgresDatabase struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Username string `json:"username"`
	Password string `json:"password"`
	Database string `json:"database"`
}

// Under sst dev and sst shell the links arrive as SST_RESOURCE_* variables.
func TestSecretFromEnvironment(t *testing.T) {
	t.Parallel()
	if !inLambda(t, `SST_RESOURCE_TurnstileSecretKey={"type":"sst:sst:Secret","value":"turnstile-from-env"}`) {
		return
	}

	got, err := sst.Secret("TurnstileSecretKey")
	require.NoError(t, err)
	assert.Equal(t, "turnstile-from-env", got)
}

func TestGetFromEnvironment(t *testing.T) {
	t.Parallel()
	if !inLambda(t,
		`SST_RESOURCE_Database={"endpoint":"env.dsql.eu-west-1.on.aws","region":"eu-west-1","type":"sst:aws:Dsql"}`,
		`SST_RESOURCE_Postgres={"database":"orders","host":"db.internal","password":"p@ss","port":5432,"type":"sst:aws:Postgres","username":"app"}`,
	) {
		return
	}

	var cluster dsqlCluster
	require.NoError(t, sst.Get("Database", &cluster))
	assert.Equal(t, dsqlCluster{Endpoint: "env.dsql.eu-west-1.on.aws", Region: "eu-west-1"}, cluster)

	var db postgresDatabase
	require.NoError(t, sst.Get("Postgres", &db))
	assert.Equal(t, postgresDatabase{Host: "db.internal", Port: 5432, Username: "app", Password: "p@ss", Database: "orders"}, db)
}

// A deployed Lambda finds its links encrypted in its bundle.
func TestReadFromBundle(t *testing.T) {
	t.Parallel()
	bundle, err := filepath.Abs(filepath.Join("testdata", "resource.enc"))
	require.NoError(t, err)
	if !inLambda(t, "SST_KEY="+bundleKey, "SST_KEY_FILE="+bundle) {
		return
	}

	secret, err := sst.Secret("TurnstileSecretKey")
	require.NoError(t, err)
	assert.Equal(t, "turnstile-from-bundle", secret)

	var cluster dsqlCluster
	require.NoError(t, sst.Get("Database", &cluster))
	assert.Equal(t, dsqlCluster{Endpoint: "bundle.dsql.eu-west-1.on.aws", Region: "eu-west-1"}, cluster)
}

func TestNotLinked(t *testing.T) {
	t.Parallel()
	if !inLambda(t, `SST_RESOURCE_Other={"type":"sst:sst:Secret","value":"other"}`) {
		return
	}

	secret, err := sst.Secret("TurnstileSecretKey")
	require.ErrorIs(t, err, resource.ErrNotFound)
	require.EqualError(t, err, `sst: secret "TurnstileSecretKey": not found`)
	assert.Empty(t, secret)

	var cluster dsqlCluster
	err = sst.Get("Database", &cluster)
	require.ErrorIs(t, err, resource.ErrNotFound)
	require.EqualError(t, err, `sst: linked resource "Database": not found`)
	assert.Zero(t, cluster)
}

func TestSecretWithoutStringValue(t *testing.T) {
	t.Parallel()
	if !inLambda(t,
		`SST_RESOURCE_Number={"type":"sst:sst:Secret","value":42}`,
		`SST_RESOURCE_NoValue={"type":"sst:sst:Secret"}`,
	) {
		return
	}

	secret, err := sst.Secret("Number")
	require.EqualError(t, err, `sst: secret "Number": value is not a string: float64`)
	assert.Empty(t, secret)

	secret, err = sst.Secret("NoValue")
	require.ErrorIs(t, err, resource.ErrNotFound)
	require.EqualError(t, err, `sst: secret "NoValue": not found`)
	assert.Empty(t, secret)
}

func TestGetDecodeError(t *testing.T) {
	t.Parallel()
	if !inLambda(t, `SST_RESOURCE_Postgres={"host":"db.internal","port":"5432","type":"sst:aws:Postgres"}`) {
		return
	}

	var db postgresDatabase
	err := sst.Get("Postgres", &db)
	require.ErrorContains(t, err, `sst: decode linked resource "Postgres": `)

	// As json.Unmarshal, Get needs a pointer to decode into.
	err = sst.Get("Postgres", db)
	require.ErrorContains(t, err, `sst: decode linked resource "Postgres": `)
}
