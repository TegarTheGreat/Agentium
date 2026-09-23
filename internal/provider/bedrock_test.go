package provider

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// AWS SigV4 test suite, "get-vanilla".
func TestSignV4Vanilla(t *testing.T) {
	req, _ := http.NewRequest("GET", "https://example.amazonaws.com/", nil)
	signV4(req, nil, "us-east-1", "service", AWSCreds{AccessKey: "AKIDEXAMPLE", SecretKey: "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"},
		time.Date(2015, 8, 30, 12, 36, 0, 0, time.UTC))
	want := "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/service/aws4_request, SignedHeaders=host;x-amz-date, Signature=5fa00fa31553b73ebf1942676e86291e8372ff2a2260956d9b8aae1d763fbf31"
	if got := req.Header.Get("Authorization"); got != want {
		t.Fatalf("\n got %s\nwant %s", got, want)
	}
}

func TestBedrockStream(t *testing.T) {
	events := []string{
		`{"type":"message_start","message":{"usage":{"input_tokens":7}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi from bedrock"}}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}`,
		`{"type":"message_stop"}`,
	}
	var gotPath, gotAuth, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.EscapedPath(), r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		for _, e := range events {
			payload := `{"bytes":"` + base64.StdEncoding.EncodeToString([]byte(e)) + `"}`
			w.Write(encodeEventStream(map[string]string{":event-type": "chunk", ":message-type": "event"}, []byte(payload)))
		}
	}))
	defer srv.Close()
	b := &Bedrock{Region: "us-east-1", Endpoint: srv.URL, Creds: AWSCreds{AccessKey: "AK", SecretKey: "SK"}}
	resp, err := b.Stream(context.Background(), Request{Model: "us.anthropic.claude-sonnet-5", System: "s",
		Messages: []Message{{Role: RoleUser, Text: "hello"}}}, nil)
	if err != nil || resp.Text != "hi from bedrock" || resp.Usage.Input != 7 || resp.Usage.Output != 3 {
		t.Fatalf("resp=%+v err=%v", resp, err)
	}
	if gotPath != "/model/us.anthropic.claude-sonnet-5/invoke-with-response-stream" || !strings.HasPrefix(gotAuth, "AWS4-HMAC-SHA256 Credential=AK/") {
		t.Fatalf("path=%s auth=%s", gotPath, gotAuth)
	}
	if !strings.Contains(gotBody, `"anthropic_version":"bedrock-2023-05-31"`) || strings.Contains(gotBody, `"model"`) || strings.Contains(gotBody, `"stream"`) {
		t.Fatalf("body = %s", gotBody)
	}
	// Bearer (Bedrock API key) auth and throttling exceptions.
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Write(encodeEventStream(map[string]string{":message-type": "exception", ":exception-type": "throttlingException"}, []byte(`{"message":"slow down"}`)))
	}))
	defer srv2.Close()
	_, err = (&Bedrock{Region: "us-east-1", Endpoint: srv2.URL, Bearer: "BK"}).Stream(context.Background(), Request{Model: "m"}, nil)
	if gotAuth != "Bearer BK" || !Retryable(err) {
		t.Fatalf("auth=%s err=%v", gotAuth, err)
	}
}
