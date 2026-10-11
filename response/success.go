package response

import (
	"net/http"

	"github.com/alessandrolattao/powertools-lambda-go/eventhandler"
)

// OK answers 200 with body as JSON.
func OK[T any](body T, opts ...Option) eventhandler.Response[T] {
	return respond(http.StatusOK, body, opts)
}

// Created answers 201 with body as JSON.
func Created[T any](body T, opts ...Option) eventhandler.Response[T] {
	return respond(http.StatusCreated, body, opts)
}

// Accepted answers 202 with body as JSON.
func Accepted[T any](body T, opts ...Option) eventhandler.Response[T] {
	return respond(http.StatusAccepted, body, opts)
}

// NoContent answers 204 without a body.
func NoContent(opts ...Option) eventhandler.Response[[]byte] {
	return respond[[]byte](http.StatusNoContent, nil, opts)
}

func respond[T any](status int, body T, opts []Option) eventhandler.Response[T] {
	return eventhandler.Response[T]{StatusCode: status, Header: headers(opts), Body: body}
}
