package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestDecisionsUsesClassifierRoute(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/decisions" || r.Header.Get("Authorization") != "Bearer key" {
			t.Error("wrong decisions route or auth")
		}
		if _, err := io.WriteString(w, `{"answers":[{"name":"safe","type":"predicate","probability":0.9}],"usage":{"input_tokens":10,"output_tokens":2}}`); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	model := &Model{Type: "classifier", ID: "gpt-6-luna", API: APIOpenAIDecisions, BaseURL: server.URL}
	result, err := Classify(context.Background(), model, ClassifierContext{State: map[string]any{"text": "example"}, Questions: map[string]ClassifierQuestion{"safe": {Type: "bool", Criteria: map[string]string{"true": "safe", "false": "unsafe"}}}}, WithAPIKey("key"))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Answers) != 1 || result.Usage.TotalTokens != 12 {
		t.Fatalf("result: %#v", result)
	}
	if _, err := Classify(context.Background(), &Model{}, ClassifierContext{}, WithAPIKey("key")); err == nil {
		t.Fatal("chat model accepted")
	}
}

func TestDecisionsRejectsImagesForTextModel(t *testing.T) {
	_, err := Classify(context.Background(), &Model{Type: "classifier", API: APIOpenAIDecisions}, ClassifierContext{Images: []ContentBlock{{Type: ContentBlockImage}}}, WithAPIKey("key"))
	if err == nil {
		t.Fatal("text-only classifier accepted image")
	}
}

func TestDecisionsRejectsMalformedChoiceProbabilitiesRetainingUsage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.WriteString(w, `{"answers":[{"name":"pick","type":"choice","choice":"yes","confidence":0.9,"probabilities":[{"probability":0.9}]}],"usage":{"input_tokens":10,"output_tokens":2}}`); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	result, err := Classify(context.Background(), &Model{Type: "classifier", API: APIOpenAIDecisions, BaseURL: server.URL}, ClassifierContext{Questions: map[string]ClassifierQuestion{"pick": {Type: "choice", Criteria: map[string]string{"yes": "yes", "no": "no"}}}}, WithAPIKey("key"))
	if err == nil {
		t.Fatal("accepted probability without value")
	}
	if result == nil || result.Usage.TotalTokens != 12 || result.StopReason != StopReasonError || len(result.Answers) != 0 {
		t.Fatalf("result=%+v", result)
	}
}

func TestDecisionsEnforcesDeclaredImageLimitsBeforeRequest(t *testing.T) {
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		if _, err := io.WriteString(w, `{"answers":[{"name":"safe","type":"predicate","probability":0.9}]}`); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	model := &Model{Type: "classifier", API: APIOpenAIDecisions, BaseURL: server.URL, Input: []InputType{InputImage}, InputLimits: &ModelInputLimits{Images: &ModelImageInputLimits{MaxPerRequest: 1}}}
	_, err := Classify(context.Background(), model, ClassifierContext{Images: []ContentBlock{{Type: ContentBlockImage}, {Type: ContentBlockImage}}, Questions: map[string]ClassifierQuestion{"safe": {Type: "bool", Criteria: map[string]string{"true": "safe"}}}}, WithAPIKey("key"))
	if err == nil || called {
		t.Fatalf("image limit bypassed: err=%v request=%t", err, called)
	}
}

func TestDecisionsInvokesPayloadAndResponseHooks(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		if payload["model"] != "rewritten" {
			t.Errorf("payload hook ignored: %v", payload["model"])
		}
		w.Header().Set("X-Request-ID", "decisions-request")
		if _, err := io.WriteString(w, `{"answers":[{"name":"safe","type":"predicate","probability":0.9}]}`); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	model := &Model{Type: "classifier", API: APIOpenAIDecisions, BaseURL: server.URL}
	payloadCalled, responseCalled := false, false
	_, err := Classify(t.Context(), model, ClassifierContext{Questions: map[string]ClassifierQuestion{"safe": {Type: "bool", Criteria: map[string]string{}}}}, WithAPIKey("key"), WithPayloadCallbackForModel(func(payload any, got *Model) any {
		payloadCalled = true
		if got != model {
			t.Error("wrong payload model")
		}
		payload.(map[string]any)["model"] = "rewritten"
		return payload
	}), WithResponseCallbackForModel(func(response ProviderResponse, got *Model) {
		responseCalled = true
		if got != model || response.Status != http.StatusOK {
			t.Errorf("wrong response: %+v", response)
		}
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !payloadCalled || !responseCalled {
		t.Fatalf("hooks: payload=%v response=%v", payloadCalled, responseCalled)
	}
}

func TestDecisionsRetriesTransientStatusButNotGatewayLimit(t *testing.T) {
	for _, status := range []int{http.StatusServiceUnavailable, http.StatusGatewayTimeout} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls++
				if calls == 1 {
					w.Header().Set("Retry-After-Ms", "0")
					w.WriteHeader(status)
					return
				}
				if _, err := io.WriteString(w, `{"answers":[{"name":"safe","type":"predicate","probability":0.9}]}`); err != nil {
					t.Error(err)
				}
			}))
			defer server.Close()
			_, err := Classify(t.Context(), &Model{Type: "classifier", API: APIOpenAIDecisions, BaseURL: server.URL}, ClassifierContext{Questions: map[string]ClassifierQuestion{"safe": {Type: "bool", Criteria: map[string]string{}}}}, WithAPIKey("key"))
			if status == http.StatusServiceUnavailable && (err != nil || calls != 2) {
				t.Fatalf("calls=%d err=%v", calls, err)
			}
			if status == http.StatusGatewayTimeout && (err == nil || calls != 1) {
				t.Fatalf("calls=%d err=%v", calls, err)
			}
		})
	}
}

func TestDecisionsTimeoutAndCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			t.Error(err)
			return
		}
		<-r.Context().Done()
	}))
	defer server.Close()
	model := &Model{Type: "classifier", API: APIOpenAIDecisions, BaseURL: server.URL}
	input := ClassifierContext{Questions: map[string]ClassifierQuestion{"safe": {Type: "bool", Criteria: map[string]string{}}}}
	_, err := Classify(t.Context(), model, input, WithAPIKey("key"), WithTimeout(time.Millisecond), WithMaxRetries(0))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected attempt timeout, got %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = Classify(ctx, model, input, WithAPIKey("key"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation, got %v", err)
	}
}

func TestDecisionsRetryControls(t *testing.T) {
	for _, test := range []struct {
		name, directive, delay string
		options                []Option
	}{
		{name: "disabled", options: []Option{WithMaxRetries(0)}},
		{name: "server veto", directive: "false"},
		{name: "excessive delay", delay: "100000"},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.Header().Set("x-should-retry", test.directive)
				w.Header().Set("retry-after-ms", test.delay)
				w.WriteHeader(http.StatusServiceUnavailable)
			}))
			defer server.Close()
			_, err := Classify(t.Context(), &Model{Type: "classifier", API: APIOpenAIDecisions, BaseURL: server.URL}, ClassifierContext{Questions: map[string]ClassifierQuestion{"safe": {Type: "bool", Criteria: map[string]string{}}}}, append(test.options, WithAPIKey("key"))...)
			if err == nil || calls != 1 {
				t.Fatalf("calls=%d err=%v", calls, err)
			}
		})
	}
}

func TestDecisionsNormalizesPublicAnswers(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Questions []map[string]any `json:"questions"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
			return
		}
		for _, question := range payload.Questions {
			if question["type"] == "choice" {
				for _, choice := range question["choices"].([]any) {
					if _, exists := choice.(map[string]any)["description"]; exists {
						t.Error("empty choice description was not omitted")
					}
				}
			}
		}
		if _, err := io.WriteString(w, `{"answers":[{"name":"safe","type":"predicate","probability":0.9},{"name":"pick","type":"choice","choice":"yes","confidence":0.8,"probabilities":[{"value":"yes","probability":0.8},{"value":"no","probability":0.2}]},{"name":"rating","type":"score","score":2.5,"confidence":0.7}]}`); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	result, err := Classify(t.Context(), &Model{Type: "classifier", API: APIOpenAIDecisions, BaseURL: server.URL}, ClassifierContext{Questions: map[string]ClassifierQuestion{
		"safe":   {Type: "bool", Criteria: map[string]string{}},
		"pick":   {Type: "choice", Criteria: map[string]string{"yes": "", "no": ""}},
		"rating": {Type: "score", Criteria: []string{"low", "high"}},
	}}, WithAPIKey("key"))
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{
		"safe":   `{"type":"bool","probability":0.9}`,
		"pick":   `{"type":"choice","choice":"yes","confidence":0.8,"probabilities":{"yes":0.8,"no":0.2}}`,
		"rating": `{"type":"score","score":2.5,"confidence":0.7}`,
	} {
		var gotValue, wantValue any
		if err := json.Unmarshal(result.Answers[name], &gotValue); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(want), &wantValue); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(gotValue, wantValue) {
			t.Errorf("%s = %s, want %s", name, result.Answers[name], want)
		}
	}
}

func TestDecisionsMalformedUsageDoesNotDiscardAnswers(t *testing.T) {
	for _, usage := range []string{`null`, `"invalid"`, `{"input_tokens":"invalid","output_tokens":2}`, `{"input_tokens":-5,"output_tokens":2}`} {
		t.Run(usage, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if _, err := fmt.Fprintf(w, `{"answers":[{"name":"safe","type":"predicate","probability":0.9}],"usage":%s}`, usage); err != nil {
					t.Error(err)
				}
			}))
			defer server.Close()
			result, err := Classify(t.Context(), &Model{Type: "classifier", API: APIOpenAIDecisions, BaseURL: server.URL}, ClassifierContext{Questions: map[string]ClassifierQuestion{"safe": {Type: "bool", Criteria: map[string]string{}}}}, WithAPIKey("key"))
			if err != nil {
				t.Fatal(err)
			}
			if len(result.Answers) != 1 {
				t.Fatalf("answers=%v", result.Answers)
			}
			if strings.HasPrefix(usage, "{") && (result.Usage.Input != 0 || result.Usage.Output != 2) {
				t.Fatalf("usage=%+v", result.Usage)
			}
		})
	}
}

func TestDecisionsZeroRetryDelayLimitDisablesCap(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After-Ms", "100000")
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	_, err := Classify(ctx, &Model{Type: "classifier", API: APIOpenAIDecisions, BaseURL: server.URL}, ClassifierContext{Questions: map[string]ClassifierQuestion{"safe": {Type: "bool", Criteria: map[string]string{}}}}, WithAPIKey("key"), WithMaxRetryDelayMs(0))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("unlimited delay did not wait for cancellation: %v", err)
	}
}

func TestDecisionsUsesLastNamedAnswer(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.WriteString(w, `{"answers":[null,12,{"name":"safe","type":"refusal"},{"name":"unrequested","type":"refusal"},{"name":"safe","type":"predicate","probability":0.8}]}`); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	result, err := Classify(t.Context(), &Model{Type: "classifier", API: APIOpenAIDecisions, BaseURL: server.URL}, ClassifierContext{Questions: map[string]ClassifierQuestion{"safe": {Type: "bool", Criteria: map[string]string{}}}}, WithAPIKey("key"))
	if err != nil {
		t.Fatal(err)
	}
	var answer struct {
		Probability float64 `json:"probability"`
	}
	if err := json.Unmarshal(result.Answers["safe"], &answer); err != nil {
		t.Fatal(err)
	}
	if answer.Probability != 0.8 {
		t.Fatalf("answer=%+v", answer)
	}
}

func TestSystemOneClassifiersShareProtocolAcrossTransports(t *testing.T) {
	answers := `{"pick":{"type":"choice","choice":"yes","probabilities":{"yes":0.8,"no":0.2},"confidence":0.7},"rank":{"type":"score","score":2,"confidence":0.6},"safe":{"type":"noul","noul":0.9}}`
	usage := `{"input_tokens":10,"output_tokens":2}`
	tests := []struct {
		api, path, body string
		envelope        func(map[string]any) map[string]any
	}{
		{APITypesafeSystemOne, "/v1/systemone", `{"answers":` + answers + `,"usage":` + usage + `}`, func(body map[string]any) map[string]any { return body }},
		{APICloudflareWorkersAISystemOne, "/accounts/acct/ai/run", `{"success":true,"result":{"state":"Completed","result":{"answers":` + answers + `,"usage":` + usage + `}}}`, func(body map[string]any) map[string]any {
			input, _ := body["input"].(map[string]any)
			return input
		}},
		{APICloudflareWorkersAISystemOne, "/accounts/acct/ai/run", `{"success":true,"result":{"model":"m","answers":` + answers + `,"usage":` + usage + `}}`, func(body map[string]any) map[string]any {
			input, _ := body["input"].(map[string]any)
			return input
		}},
	}
	t.Setenv("CLOUDFLARE_ACCOUNT_ID", "acct")
	for _, tt := range tests {
		t.Run(tt.api, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != tt.path || r.Header.Get("Authorization") != "Bearer key" {
					t.Errorf("path=%s auth=%s", r.URL.Path, r.Header.Get("Authorization"))
				}
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				if body["model"] != "m" {
					t.Errorf("model = %v", body["model"])
				}
				request := tt.envelope(body)
				questions, _ := request["questions"].(map[string]any)
				safe, _ := questions["safe"].(map[string]any)
				if safe["type"] != "noul" || request["state"] == nil {
					t.Errorf("request = %v", request)
				}
				if _, err := io.WriteString(w, tt.body); err != nil {
					t.Error(err)
				}
			}))
			defer server.Close()
			baseURL := server.URL + "/v1"
			if tt.api == APICloudflareWorkersAISystemOne {
				baseURL = server.URL + "/accounts/{CLOUDFLARE_ACCOUNT_ID}/ai"
			}
			model := &Model{Type: "classifier", ID: "m", API: tt.api, BaseURL: baseURL, Cost: CostProfile{Input: 1}}
			result, err := Classify(t.Context(), model, ClassifierContext{
				State: map[string]any{"text": "x"},
				Questions: map[string]ClassifierQuestion{
					"pick": {Type: "choice", Criteria: map[string]string{"yes": "y", "no": "n"}},
					"rank": {Type: "score", Criteria: []string{"low", "high"}},
					"safe": {Type: "bool", Criteria: map[string]string{"true": "safe", "false": "unsafe"}},
				},
			}, WithAPIKey("key"))
			if err != nil {
				t.Fatal(err)
			}
			want := map[string]string{
				"pick": `{"choice":"yes","confidence":0.7,"probabilities":{"no":0.2,"yes":0.8},"type":"choice"}`,
				"rank": `{"confidence":0.6,"score":2,"type":"score"}`,
				"safe": `{"probability":0.9,"type":"bool"}`,
			}
			for name, answer := range want {
				if string(result.Answers[name]) != answer {
					t.Errorf("%s = %s, want %s", name, result.Answers[name], answer)
				}
			}
			if result.Usage.TotalTokens != 12 || result.Usage.Cost.Input == 0 || result.StopReason != StopReasonStop {
				t.Fatalf("result = %+v", result)
			}
		})
	}
}

func TestSystemOneClassifierErrorsKeepBilledUsage(t *testing.T) {
	t.Setenv("CLOUDFLARE_ACCOUNT_ID", "acct")
	tests := []struct{ api, body, want string }{
		{APITypesafeSystemOne, `{"answers":{},"usage":{"input_tokens":3,"output_tokens":1}}`, "did not return an answer for safe"},
		{APITypesafeSystemOne, `{"answers":{"safe":{"type":"choice"}},"usage":{"input_tokens":3,"output_tokens":1}}`, "did not return a bool answer for safe"},
		{APICloudflareWorkersAISystemOne, `{"success":false,"errors":[{"message":"bad account"}]}`, "bad account"},
		{APICloudflareWorkersAISystemOne, `{"success":true,"result":{"state":"Queued"}}`, "did not complete"},
	}
	for _, tt := range tests {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			if _, err := io.WriteString(w, tt.body); err != nil {
				t.Error(err)
			}
		}))
		model := &Model{Type: "classifier", ID: "m", API: tt.api, BaseURL: server.URL}
		result, err := Classify(t.Context(), model, ClassifierContext{Questions: map[string]ClassifierQuestion{"safe": {Type: "bool"}}}, WithAPIKey("key"))
		server.Close()
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Fatalf("%s: err = %v, want %q", tt.body, err, tt.want)
		}
		if result == nil || result.StopReason != StopReasonError || result.ErrorMessage == "" {
			t.Fatalf("result = %+v", result)
		}
		if strings.Contains(tt.body, "usage") && result.Usage.TotalTokens != 4 {
			t.Fatalf("usage lost: %+v", result.Usage)
		}
	}

	_, err := Classify(t.Context(), &Model{Type: "classifier", API: APITypesafeSystemOne, BaseURL: "http://unused", Input: []InputType{InputText, InputImage}}, ClassifierContext{Images: []ContentBlock{{Type: ContentBlockImage}}, Questions: map[string]ClassifierQuestion{"safe": {Type: "bool"}}}, WithAPIKey("key"))
	if err == nil || !strings.Contains(err.Error(), "image") {
		t.Fatalf("image err = %v", err)
	}
}

func TestDecisionsParsesAnswersLikeUpstream(t *testing.T) {
	reply := ""
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if reply == "" {
			w.Header().Set("x-should-retry", "maybe")
			w.Header().Set("retry-after-ms", "0")
			w.WriteHeader(http.StatusServiceUnavailable)
			reply = `{"answers":[{"name":"pick","type":"choice","choice":"other","confidence":1.5,"probabilities":[{"value":"other","probability":2}]}]}`
			return
		}
		if _, err := io.WriteString(w, reply); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	model := &Model{Type: "classifier", API: APIOpenAIDecisions, BaseURL: server.URL}
	input := ClassifierContext{Questions: map[string]ClassifierQuestion{"pick": {Type: "choice", Criteria: map[string]string{"yes": "", "no": ""}}}}

	result, err := Classify(t.Context(), model, input, WithAPIKey("key"))
	if err != nil || calls != 2 {
		t.Fatalf("calls=%d err=%v; an unknown x-should-retry value keeps the default policy", calls, err)
	}
	if got := string(result.Answers["pick"]); got != `{"choice":"other","confidence":1.5,"probabilities":{"other":2},"type":"choice"}` {
		t.Fatalf("finite numbers and the returned choice pass through: %s", got)
	}

	reply = `{"answers":[{"name":"pick","type":"choice","choice":"yes","probabilities":[]}],"usage":{"input_tokens":7}}`
	result, err = Classify(t.Context(), model, input, WithAPIKey("key"))
	if err == nil || !strings.Contains(err.Error(), "confidence for pick") || result.Usage.Input != 7 {
		t.Fatalf("missing confidence = %+v, %v", result, err)
	}

	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusGatewayTimeout) }))
	defer gateway.Close()
	model.BaseURL = gateway.URL
	if _, err := Classify(t.Context(), model, input, WithAPIKey("key")); err == nil || !strings.Contains(err.Error(), "above roughly 600K tokens") {
		t.Fatalf("504 err = %v", err)
	}
}

func TestPostOperationRetryDelayOverflow(t *testing.T) {
	for _, tc := range []struct {
		name, header, value string
		opts                []Option
		wantCalls           int
		wantError           string
	}{
		{name: "milliseconds", header: "Retry-After-Ms", value: "1e300", wantCalls: 1, wantError: "exceeds limit"},
		{name: "seconds", header: "Retry-After", value: "1e300", wantCalls: 1, wantError: "exceeds limit"},
		{name: "large cap", header: "Retry-After-Ms", value: "0", opts: []Option{WithMaxRetryDelayMs(int(^uint(0) >> 1))}, wantCalls: 2},
		{name: "disabled cap", header: "Retry-After-Ms", value: "1e300", opts: []Option{WithMaxRetryDelayMs(0)}, wantCalls: 1, wantError: "context deadline exceeded"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls int
			previous := http.DefaultClient
			http.DefaultClient = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
				calls++
				status := http.StatusServiceUnavailable
				if calls > 1 {
					status = http.StatusOK
				}
				headers := make(http.Header)
				headers.Set(tc.header, tc.value)
				return &http.Response{StatusCode: status, Header: headers, Body: io.NopCloser(strings.NewReader("response"))}, nil
			})}
			t.Cleanup(func() { http.DefaultClient = previous })
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
			defer cancel()
			_, err := postOperation(ctx, "test", &Model{}, applyOptions(append(tc.opts, WithMaxRetries(1))), "https://test.invalid", "key", nil)
			if calls != tc.wantCalls {
				t.Errorf("calls=%d, want %d", calls, tc.wantCalls)
			}
			if tc.wantError == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("err=%v, want %q", err, tc.wantError)
			}
			if tc.name == "disabled cap" && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("cancellation cause lost: %v", err)
			}
		})
	}
}

func TestPostOperationTimeoutIsHeaderOnly(t *testing.T) {
	for _, timeout := range []time.Duration{0, 5 * time.Millisecond} {
		for _, status := range []int{http.StatusOK, http.StatusBadRequest} {
			t.Run(fmt.Sprintf("%s/%d", timeout, status), func(t *testing.T) {
				var body *codexTestBody
				previous := http.DefaultClient
				http.DefaultClient = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
					if timeout == 0 {
						if err := sleepWithContext(req.Context(), 10*time.Millisecond); err != nil {
							return nil, err
						}
					}
					body = &codexTestBody{Reader: strings.NewReader("response"), ctx: req.Context(), delay: 30 * time.Millisecond}
					return &http.Response{StatusCode: status, Header: make(http.Header), Body: body}, nil
				})}
				t.Cleanup(func() { http.DefaultClient = previous })
				ctx, cancel := context.WithTimeout(t.Context(), time.Second)
				defer cancel()
				data, err := postOperation(ctx, "test", &Model{}, applyOptions([]Option{WithTimeout(timeout), WithMaxRetries(0)}), "https://test.invalid", "key", nil)
				if status == http.StatusOK {
					if err != nil || string(data) != "response" {
						t.Fatalf("data=%q err=%v", data, err)
					}
				} else if err == nil || !strings.Contains(err.Error(), "status 400: response") {
					t.Fatalf("error body timed out: %v", err)
				}
				if body == nil || !body.closed {
					t.Fatal("body not closed")
				}
				if timeout > 0 && body.ctx.Err() == nil {
					t.Fatal("request context not released")
				}
			})
		}
	}
}

func TestPostOperationBodyHonorsCallerCancellation(t *testing.T) {
	var body *codexTestBody
	previous := http.DefaultClient
	http.DefaultClient = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		body = &codexTestBody{Reader: strings.NewReader("response"), ctx: req.Context(), delay: time.Hour}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: body}, nil
	})}
	t.Cleanup(func() { http.DefaultClient = previous })
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	_, err := postOperation(ctx, "test", &Model{}, applyOptions([]Option{WithTimeout(5 * time.Millisecond), WithMaxRetries(0)}), "https://test.invalid", "key", nil)
	if !errors.Is(err, context.DeadlineExceeded) || ctx.Err() == nil {
		t.Fatalf("body must wait for caller cancellation: %v", err)
	}
	if body == nil || !body.closed {
		t.Fatal("body not closed")
	}
}

func TestPostOperationCloseFailureIsNotRetried(t *testing.T) {
	readErr := errors.New("read failed")
	closeErr := errors.New("close failed")
	for _, status := range []int{http.StatusOK, http.StatusBadRequest, http.StatusServiceUnavailable, http.StatusGatewayTimeout} {
		for _, readFailure := range []error{nil, readErr} {
			t.Run(fmt.Sprintf("%d/read=%v", status, readFailure), func(t *testing.T) {
				var calls int
				body := &codexTestBody{Reader: strings.NewReader("response"), readErr: readFailure, closeErr: closeErr}
				previous := http.DefaultClient
				http.DefaultClient = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
					calls++
					if calls > 1 {
						return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("retried"))}, nil
					}
					body.ctx = req.Context()
					return &http.Response{StatusCode: status, Header: http.Header{"X-Should-Retry": []string{"true"}, "Retry-After-Ms": []string{"0"}}, Body: body}, nil
				})}
				t.Cleanup(func() { http.DefaultClient = previous })
				data, err := postOperation(t.Context(), "test", &Model{}, applyOptions([]Option{WithMaxRetries(1), WithTimeout(time.Second)}), "https://test.invalid", "key", nil)
				if calls != 1 || !errors.Is(err, closeErr) || !body.closed || body.ctx.Err() == nil || data != nil {
					t.Fatalf("cleanup failure retried or lost: calls=%d closed=%v data=%q err=%v", calls, body.closed, data, err)
				}
				if readFailure != nil && !errors.Is(err, readFailure) {
					t.Fatalf("read cause lost: %v", err)
				}
				if readFailure == nil && status == http.StatusGatewayTimeout && !errors.Is(err, errGatewayTimeout) {
					t.Fatalf("gateway cause lost: %v", err)
				}
			})
		}
	}
}

func TestPostOperationCancellationRetainsCloseFailure(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			closeErr := errors.New("close failed")
			var calls int
			body := &codexTestBody{delay: time.Hour, closeErr: closeErr}
			previous := http.DefaultClient
			http.DefaultClient = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
				calls++
				body.ctx = req.Context()
				return &http.Response{StatusCode: status, Header: make(http.Header), Body: body}, nil
			})}
			t.Cleanup(func() { http.DefaultClient = previous })
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
			defer cancel()
			_, err := postOperation(ctx, "test", &Model{}, applyOptions([]Option{WithMaxRetries(1), WithTimeout(time.Second)}), "https://test.invalid", "key", nil)
			if calls != 1 || !body.closed || !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, closeErr) {
				t.Fatalf("cancellation lost cleanup failure: calls=%d closed=%v err=%v", calls, body.closed, err)
			}
		})
	}
}
