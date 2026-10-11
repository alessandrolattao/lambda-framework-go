package framework

import (
	"testing"

	"github.com/alessandrolattao/powertools-lambda-go/eventsource"
	"github.com/aws/aws-lambda-go/events"
	"github.com/stretchr/testify/assert"
)

func TestLogEvent_RedactsCredentials(t *testing.T) {
	t.Parallel()

	logger, logged := bufferLogger()

	logEvent(t.Context(), logger, events.APIGatewayV2HTTPRequest{
		Headers: map[string]string{
			"authorization": "Bearer eyJhbGciOi.secret",
			"cookie":        "session=secret",
			"accept":        "application/json",
		},
	})

	assert.NotContains(t, logged.String(), "secret")
	assert.Contains(t, logged.String(), "[redacted]")
	assert.Contains(t, logged.String(), "application/json", "the other headers stay")
}

func TestLogEvent_CognitoEventsKeepTheirSecrets(t *testing.T) {
	t.Parallel()

	logger, logged := bufferLogger()

	var e events.CognitoEventUserPoolsVerifyAuthChallenge
	e.TriggerSource = "VerifyAuthChallengeResponse_Authentication"
	e.Request.ChallengeAnswer = "424242"
	e.Request.PrivateChallengeParameters = map[string]string{"answer": "424242"}
	logEvent(t.Context(), logger, e)

	assert.NotContains(t, logged.String(), "424242")
	assert.Contains(t, logged.String(), "VerifyAuthChallengeResponse_Authentication")
}

func TestCognitoTrigger(t *testing.T) {
	t.Parallel()

	var migrate events.CognitoEventUserPoolsMigrateUser
	migrate.TriggerSource = "UserMigration_Authentication"
	var sender eventsource.CognitoEventUserPoolsCustomEmailSender
	sender.TriggerSource = "CustomEmailSender_SignUp"

	for _, tc := range []struct {
		event any
		want  string
		ok    bool
	}{
		{migrate, "UserMigration_Authentication", true},
		{sender, "CustomEmailSender_SignUp", true},
		{events.SQSEvent{}, "", false},
		{"not a struct", "", false},
	} {
		got, ok := cognitoTrigger(tc.event)
		assert.Equal(t, tc.ok, ok, "%T", tc.event)
		assert.Equal(t, tc.want, got, "%T", tc.event)
	}
}
