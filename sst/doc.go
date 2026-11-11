// Package sst reads the resources SST links to a Lambda with link: the value
// of an sst.Secret, and the properties of any other linked component (a
// Postgres database, an Aurora DSQL cluster, a bucket).
//
// Read them in main, once per cold start, and pass them on: to framework.New,
// or to the handlers that need them.
//
//	package main
//
//	import (
//		"context"
//
//		"github.com/aws/aws-lambda-go/lambda"
//
//		"github.com/alessandrolattao/lambda-framework-go/framework"
//		"github.com/alessandrolattao/lambda-framework-go/sst"
//	)
//
//	// database holds the properties the Aurora DSQL cluster links.
//	type database struct {
//		Endpoint string `json:"endpoint"`
//	}
//
//	func main() {
//		var db database
//		if err := sst.Get("Database", &db); err != nil {
//			panic(err)
//		}
//		turnstileKey, err := sst.Secret("TurnstileSecretKey")
//		if err != nil {
//			panic(err)
//		}
//
//		c := framework.New(framework.WithDSQLEndpoint(db.Endpoint))
//		s := signups{turnstileKey: turnstileKey}
//		c.Post("/signups", s.create)
//		lambda.Start(c.HTTP())
//	}
//
//	type signups struct {
//		turnstileKey string
//	}
//
//	func (s signups) create(ctx context.Context, c *framework.Container, in signup) (account, error) {
//		// Verify in.Body.Token with s.turnstileKey, then write to c.DB.
//		...
//	}
//
// with, in sst.config.ts:
//
//	const database = new sst.aws.Dsql("Database");
//	const turnstileKey = new sst.Secret("TurnstileSecretKey");
//
//	new sst.aws.Function("Signups", {
//	  handler: "./cmd/signups",
//	  runtime: "go",
//	  link: [database, turnstileKey],
//	});
//
// A resource has the name its component has in sst.config.ts, and the
// properties the component links: an sst.Secret links its value, an
// sst.aws.Dsql cluster its endpoint and region, an sst.aws.Postgres database
// its host, port, username, password and database. Get decodes them as
// encoding/json/v2 does, into a struct whose JSON tags name them; the
// properties the struct does not name are ignored.
//
// The package wraps SST's Go SDK, which reads every linked resource once,
// when the process starts. A Lambda deployed by SST, or run by sst dev, finds
// them encrypted in its bundle: the file in SST_KEY_FILE, decrypted with the
// key in SST_KEY. The SDK also reads the SST_RESOURCE_* variables, as sst
// shell sets them. A resource that is not linked to the function is an error
// that names it.
//
// Only the Lambdas that import this package link SST's SDK: framework,
// request, response and migrate do not import it.
package sst
