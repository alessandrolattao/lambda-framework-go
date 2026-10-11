package framework

import (
	"bytes"
	"encoding/json/v2"
	"errors"
	"log/slog"
	"reflect"

	"github.com/aws/aws-lambda-go/lambda/messages"
)

// The errorType Lambda records for a failure the framework knows the place of.
// Step Functions matches its Retry and Catch rules on it, so they are stable.
const (
	errorTypeHandler       = "HandlerError"
	errorTypeInvalidEvent  = "InvalidEvent"
	errorTypeInvalidResult = "InvalidResult"
	errorTypeInit          = "InitError"
)

// toRuntime hands err to aws-lambda-go as the failure Lambda records, named
// typ. Left as it is, Lambda would name it after its Go type, and a wrapped
// error is a *fmt.wrapError: errorType "wrapError".
func toRuntime(err error, typ string) error {
	return messages.InvokeResponse_Error{Message: err.Error(), Type: typ}
}

// errorType names a handler's failure: the type of the first error in the
// chain that an application or a library defined (a PgError, a
// ValidationError), or else fallback.
func errorType(err error, fallback string) string {
	for e := err; e != nil; e = errors.Unwrap(e) {
		t := reflect.TypeOf(e)
		if t.Kind() == reflect.Pointer {
			t = t.Elem()
		}
		// The standard library's own wrappers and sentinels (wrapError,
		// errorString, joinError) say nothing about the failure.
		if t.Name() != "" && t.PkgPath() != "errors" && t.PkgPath() != "fmt" {
			return t.Name()
		}
	}
	return fallback
}

// runtimeLog takes what aws-lambda-go writes through the standard log package
// and writes it as structured records instead of plain text lines.
//
// aws-lambda-go writes every failure it reports to Lambda, unconditionally.
// Every failure that leaves a Handler or a Consumer has already been written
// down at Error by the framework, with its request id, so that copy goes to
// Debug: invisible by default on AWS, there when debugging. Anything else it
// writes is kept at Info.
type runtimeLog struct {
	logger *slog.Logger
}

func (w runtimeLog) Write(p []byte) (int, error) {
	line := bytes.TrimSpace(p)

	var failure messages.InvokeResponse_Error
	if json.Unmarshal(line, &failure) == nil && failure.Message != "" {
		w.logger.Debug("runtime reported the failure", "err", failure.Message, "errorType", failure.Type)
		return len(p), nil
	}
	w.logger.Info("aws-lambda-go", "detail", string(line))
	return len(p), nil
}
