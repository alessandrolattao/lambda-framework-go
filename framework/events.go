package framework

import (
	"context"
	"encoding/json/jsontext"
	"fmt"
	"log/slog"
	"maps"
	"reflect"
	"strings"

	"github.com/aws/aws-lambda-go/events"
)

// logEvent writes what arrived, with the attributes that matter for its kind.
// An API request is announced at Info; everything else, and the details of a
// request, at Debug.
//
// Debug writes what a request carries, body and query string included, so it
// is for development; on AWS the default level is Info. Even there, the
// credentials in headers are redacted, and of a Cognito event only the kind
// is written, since its payload carries verification codes and challenge
// answers.
func logEvent(ctx context.Context, logger *slog.Logger, event any) {
	switch e := event.(type) {
	case events.APIGatewayV2HTTPRequest:
		logger.InfoContext(ctx, "API request received")
		logger.DebugContext(ctx, "API request details",
			"method", e.RequestContext.HTTP.Method,
			"path", e.RawPath,
			"routeKey", e.RouteKey,
			"pathParameters", e.PathParameters,
			"queryStringParameters", e.QueryStringParameters,
			"headers", redactHeaders(e.Headers),
			"body", e.Body,
		)
	case events.APIGatewayWebsocketProxyRequest:
		logger.DebugContext(ctx, "processing WebSocket event",
			"routeKey", e.RequestContext.RouteKey,
			"eventType", e.RequestContext.EventType,
			"connectionId", e.RequestContext.ConnectionID,
		)
	case events.CloudWatchEvent:
		logger.DebugContext(ctx, "processing EventBridge event",
			"eventID", e.ID,
			"detailType", e.DetailType,
			"source", e.Source,
			"detail", string(e.Detail),
		)
	case events.SQSEvent:
		logger.DebugContext(ctx, "processing SQS event", "recordCount", len(e.Records))
	case events.S3Event:
		logger.DebugContext(ctx, "processing S3 event", "event", e)
	case events.CloudwatchLogsEvent:
		// The payload arrives base64-encoded and gzipped; unpacking it is the
		// business handler's job, through e.AWSLogs.Parse().
		logger.DebugContext(ctx, "processing CloudWatch Logs batch", "payloadBytes", len(e.AWSLogs.Data))
	case jsontext.Value: // json.RawMessage is an alias of it
		logger.DebugContext(ctx, "processing direct invocation", "payloadBytes", len(e))
	default:
		if trigger, ok := cognitoTrigger(event); ok {
			logger.DebugContext(ctx, "processing Cognito event", "type", fmt.Sprintf("%T", event), "trigger", trigger)
			return
		}
		logger.DebugContext(ctx, "processing event", "type", fmt.Sprintf("%T", event))
	}
}

// cognitoTrigger returns the trigger source of a Cognito user pool event, and
// whether event is one: every such event embeds the common user pool header.
func cognitoTrigger(event any) (string, bool) {
	v := reflect.ValueOf(event)
	if v.Kind() != reflect.Struct {
		return "", false
	}
	field := v.FieldByName("CognitoEventUserPoolsHeader")
	if !field.IsValid() {
		return "", false
	}
	header, ok := reflect.TypeAssert[events.CognitoEventUserPoolsHeader](field)
	return header.TriggerSource, ok
}

// sensitiveHeaders carry credentials, so their values never reach the logs.
var sensitiveHeaders = map[string]bool{
	"authorization":       true,
	"proxy-authorization": true,
	"cookie":              true,
	"x-api-key":           true,
}

// redactHeaders returns a copy of headers with the credentials blanked out.
func redactHeaders(headers map[string]string) map[string]string {
	out := maps.Clone(headers)
	for name := range out {
		if sensitiveHeaders[strings.ToLower(name)] {
			out[name] = "[redacted]"
		}
	}
	return out
}
