// Package response answers the typed operations of an Event Handler router
// (github.com/alessandrolattao/powertools-lambda-go/eventhandler): success
// responses with their cache headers, CSV bodies, and the error envelope. It
// also writes the server-sent events of a streaming handler (SSE).
//
// A typed operation returns its result, or a *Error:
//
//	if errors.Is(err, pgx.ErrNoRows) {
//		return order{}, response.NotFound("orderNotFound", "no order with this id", nil)
//	}
//	return o, nil
//
// and an eventhandler.Response when it sets its own status or headers:
//
//	return response.OK(orders, response.WithCacheMaxAge(time.Minute)), nil
//
// An error answers with the envelope {"code","message","details","timestamp"}:
// a stable, camelCase code clients key on (to look up a translated text, for
// instance) and a free-form message meant for logs, never shown to users.
// The framework package renders it, and maps the errors of the Event Handler
// itself (404, validation, unsupported content type) to the same envelope.
package response
