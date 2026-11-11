package migrate_test

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"
	"testing/fstest"

	"github.com/alessandrolattao/powertools-lambda-go/logging"

	"github.com/alessandrolattao/lambda-framework-go/framework"
	"github.com/alessandrolattao/lambda-framework-go/migrate"
)

// migrations stands in for the migration Lambda's embedded SQL files:
//
//	//go:embed *.sql
//	var migrations embed.FS
var migrations = fstest.MapFS{
	"00001_orders.sql": {Data: []byte(`-- +goose NO TRANSACTION
-- +goose Up
CREATE TABLE orders (id uuid PRIMARY KEY, total bigint NOT NULL);
CREATE INDEX ASYNC orders_total ON orders (total);
`)},
}

func ExampleUp() {
	// In main: c := framework.New(), with the cluster endpoint in
	// DSQL_ENDPOINT. This container, assembled by hand, has no database.
	c := &framework.Container{Logger: logging.New(logging.WithWriter(io.Discard))}
	h := c.Handle(migrate.Up(migrations)) // in main: lambda.Start(h)

	_, err := h.Call(context.Background(), jsontext.Value(`{}`))
	fmt.Println(err)
	// Output: migrations need a database, and the container has none: set the Aurora DSQL endpoint (WithDSQLEndpoint or DSQL_ENDPOINT) or a Postgres connection string (WithDatabaseURL or DATABASE_URL)
}

func ExampleResult() {
	// What a migration Lambda answers when it applied versions 2 and 3.
	out, err := json.Marshal(migrate.Result{Version: 3, Applied: []int64{2, 3}})
	fmt.Println(string(out), err)
	// Output: {"version":3,"applied":[2,3]} <nil>
}
