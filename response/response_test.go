package response

import (
	"encoding/json/v2"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var errDatabase = errors.New("connection refused")

// decode reads an error envelope.
func decode(t *testing.T, body string) ErrorBody {
	t.Helper()
	var e ErrorBody
	require.NoError(t, json.Unmarshal([]byte(body), &e), body)
	return e
}

// assertTimestampRFC3339UTC checks the stamp every envelope carries: never
// empty, RFC 3339, UTC.
func assertTimestampRFC3339UTC(t *testing.T, stamp string) {
	t.Helper()
	require.NotEmpty(t, stamp)
	parsed, err := time.Parse(time.RFC3339, stamp)
	require.NoError(t, err)
	assert.Equal(t, time.UTC, parsed.Location())
}

func TestErrorConstructors(t *testing.T) {
	t.Parallel()
	cases := []struct {
		err    *Error
		status int
	}{
		{BadRequest("c", "m", nil), 400},
		{Unauthorized("c", "m", nil), 401},
		{PaymentRequired("c", "m", nil), 402},
		{Forbidden("c", "m", nil), 403},
		{NotFound("c", "m", nil), 404},
		{Conflict("c", "m", nil), 409},
		{UnprocessableEntity("c", "m", nil), 422},
		{TooManyRequests("c", "m", nil), 429},
		{InternalServerError(errDatabase, "c", "m", nil), 500},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.status, tc.err.Status)
		assert.Equal(t, "c", tc.err.Code)
	}
}

func TestErrorKeepsItsCause(t *testing.T) {
	t.Parallel()
	err := InternalServerError(errDatabase, "serverError", "could not read the order", nil)
	require.ErrorIs(t, err, errDatabase)
	assert.Contains(t, err.Error(), "connection refused")
	assert.NotContains(t, err.APIGatewayV2().Body, "connection refused", "the cause never reaches the client")
}

func TestErrorWrite(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	NotFound("orderNotFound", "no order 42", map[string]int{"id": 42}).Write(rec)

	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
	assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
	body := decode(t, rec.Body.String())
	assert.Equal(t, "orderNotFound", body.Code)
	assert.Equal(t, "no order 42", body.Message)
	assert.Equal(t, map[string]any{"id": float64(42)}, body.Details)
	assertTimestampRFC3339UTC(t, body.Timestamp)
}

func TestErrorAPIGatewayV2(t *testing.T) {
	t.Parallel()
	resp := BadRequest("invalidTotal", "the total must be positive", nil).APIGatewayV2()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	assert.Equal(t, "no-store", resp.Headers["Cache-Control"])
	body := decode(t, resp.Body)
	assert.Equal(t, "invalidTotal", body.Code)
	assert.Nil(t, body.Details, "details are omitted when there are none")
}

func TestErrorDetailsThatCannotBeEncodedAreDropped(t *testing.T) {
	t.Parallel()
	resp := BadRequest("invalid", "bad", math.NaN()).APIGatewayV2()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode, "the status survives")
	body := decode(t, resp.Body)
	assert.Equal(t, "invalid", body.Code)
	assert.Nil(t, body.Details)
}

func TestErrorInvalidUTF8(t *testing.T) {
	t.Parallel()
	body := decode(t, NotFound("bad\xffcode", "bad\xfemessage", nil).APIGatewayV2().Body)
	assert.Equal(t, "bad�code", body.Code)
	assert.Equal(t, "bad�message", body.Message)
}

func TestSuccessResponses(t *testing.T) {
	t.Parallel()
	ok := OK(map[string]string{"id": "1"}, WithCacheMaxAge(time.Minute))
	assert.Equal(t, http.StatusOK, ok.StatusCode)
	assert.Equal(t, "public, max-age=60", ok.Header.Get("Cache-Control"))
	assert.Equal(t, map[string]string{"id": "1"}, ok.Body)

	created := Created("x", WithHeader("Location", "/orders/1"))
	assert.Equal(t, http.StatusCreated, created.StatusCode)
	assert.Equal(t, "/orders/1", created.Header.Get("Location"))

	assert.Equal(t, http.StatusAccepted, Accepted(1).StatusCode)

	none := NoContent(WithNoCache())
	assert.Equal(t, http.StatusNoContent, none.StatusCode)
	assert.Empty(t, none.Body)
	assert.Equal(t, "no-store", none.Header.Get("Cache-Control"))
}

func TestCacheOptions(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "public, max-age=30, s-maxage=600",
		OK(1, WithCacheMaxAgeShared(30*time.Second, 10*time.Minute)).Header.Get("Cache-Control"))
	assert.Equal(t, "public, max-age=0", OK(1, WithCacheMaxAge(-time.Second)).Header.Get("Cache-Control"),
		"a negative age is no caching, not a malformed header")
}

func TestNegotiate(t *testing.T) {
	t.Parallel()
	type row struct {
		ID    int    `json:"id"`
		Label string `json:"label"`
	}
	rows := []row{{ID: 1, Label: "a"}}

	asJSON, err := Negotiate("application/json", rows, WithCacheMaxAge(time.Minute))
	require.NoError(t, err)
	assert.Equal(t, "application/json", asJSON.Header.Get("Content-Type"))
	assert.JSONEq(t, `[{"id":1,"label":"a"}]`, string(asJSON.Body))
	assert.Equal(t, "Accept", asJSON.Header.Get("Vary"))
	assert.Equal(t, "public, max-age=60", asJSON.Header.Get("Cache-Control"))

	asCSV, err := Negotiate("text/csv;q=1.0", rows)
	require.NoError(t, err)
	assert.Equal(t, "text/csv", asCSV.Header.Get("Content-Type"))
	assert.Equal(t, "ID,Label\n1,a\n", string(asCSV.Body))
	assert.Equal(t, "Accept", asCSV.Header.Get("Vary"))

	_, err = Negotiate("", []float64{math.NaN()})
	require.Error(t, err)
}
