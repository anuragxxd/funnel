package dynamodb

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/dynamodb"
	"github.com/ohsu-comp-bio/funnel/tes"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestGetMinimalViewUsesConsistentRead(t *testing.T) {
	tests := []struct {
		name          string
		state         tes.State
		staleResponse string
	}{
		{
			name:          "newly created task is visible",
			state:         tes.State_QUEUED,
			staleResponse: `{}`,
		},
		{
			name:          "recent cancellation is visible",
			state:         tes.State_CANCELED,
			staleResponse: fmt.Sprintf(`{"Item":{"id":{"S":"task-1"},"owner":{"S":"owner"},"state":{"N":"%d"}}}`, tes.State_QUEUED),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var consistentRead bool
			client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				var request struct {
					ConsistentRead bool
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					return nil, fmt.Errorf("decode GetItem request: %w", err)
				}
				consistentRead = request.ConsistentRead

				body := tt.staleResponse
				if !request.ConsistentRead {
					return &http.Response{
						StatusCode: http.StatusOK,
						Header:     http.Header{"Content-Type": []string{"application/x-amz-json-1.0"}},
						Body:       io.NopCloser(strings.NewReader(body)),
						Request:    r,
					}, nil
				}
				body = fmt.Sprintf(`{"Item":{"id":{"S":"task-1"},"owner":{"S":"owner"},"state":{"N":"%d"}}}`, tt.state)
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{"application/x-amz-json-1.0"}},
					Body:       io.NopCloser(strings.NewReader(body)),
					Request:    r,
				}, nil
			})}

			sess, err := session.NewSession(aws.NewConfig().
				WithEndpoint("http://dynamodb.test").
				WithRegion("us-east-1").
				WithCredentials(credentials.NewStaticCredentials("test", "secret", "")).
				WithDisableSSL(true).
				WithHTTPClient(client).
				WithMaxRetries(0))
			if err != nil {
				t.Fatalf("create AWS session: %v", err)
			}
			db := &DynamoDB{
				client:         dynamodb.New(sess),
				partitionKey:   "hid",
				partitionValue: "0",
				taskTable:      "tasks",
			}

			task, err := db.GetTask(t.Context(), &tes.GetTaskRequest{
				Id:   "task-1",
				View: tes.View_MINIMAL.String(),
			})
			if err != nil {
				t.Fatalf("GetTask returned an error: %v", err)
			}
			if task.State != tt.state {
				t.Fatalf("GetTask returned state %s, want %s", task.State, tt.state)
			}
			if !consistentRead {
				t.Fatal("GetTask did not request a consistent read")
			}
		})
	}
}
