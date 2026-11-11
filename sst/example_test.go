package sst_test

import (
	"fmt"

	"github.com/alessandrolattao/lambda-framework-go/sst"
)

func ExampleSecret() {
	// In main of a Lambda linked to the secret:
	//
	//	const turnstileKey = new sst.Secret("TurnstileSecretKey");
	//	new sst.aws.Function("Signups", { handler: "./cmd/signups", runtime: "go", link: [turnstileKey] });
	//
	// This process has nothing linked.
	_, err := sst.Secret("TurnstileSecretKey")
	fmt.Println(err)
	// Output: sst: secret "TurnstileSecretKey": not found
}

func ExampleGet() {
	// In main of a Lambda linked to the Aurora DSQL cluster:
	//
	//	const database = new sst.aws.Dsql("Database");
	//	new sst.aws.Function("Orders", { handler: "./cmd/orders", runtime: "go", link: [database] });
	//
	// This process has nothing linked.
	var database struct {
		Endpoint string `json:"endpoint"`
	}
	err := sst.Get("Database", &database)
	fmt.Println(err) // in main: framework.New(framework.WithDSQLEndpoint(database.Endpoint))
	// Output: sst: linked resource "Database": not found
}
