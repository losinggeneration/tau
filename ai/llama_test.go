package ai

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func llamaTestServer(t *testing.T, requests *[]map[string]any, mu *sync.Mutex) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if r.Method == http.MethodPost {
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			mu.Lock()
			*requests = append(*requests, map[string]any{"path": r.URL.Path, "body": body, "auth": r.Header.Get("Authorization")})
			mu.Unlock()
		}
		var response any
		switch r.URL.Path {
		case "/models":
			response = map[string]any{"data": []any{
				map[string]any{"id": "julia-1", "status": map[string]any{"value": "loaded"}, "architecture": map[string]any{"output_modalities": []string{"decisions"}}},
				map[string]any{"id": "qwen", "status": map[string]any{"value": "loaded"}, "meta": map[string]any{"n_ctx": 32768}, "architecture": map[string]any{"input_modalities": []string{"text", "image"}}},
				map[string]any{"id": "sleepy", "status": map[string]any{"value": "sleeping", "args": []string{"--ctx-size", "4096"}}},
				map[string]any{"id": "preset", "source": "preset", "status": map[string]any{"value": "unloaded"}, "meta": map[string]any{"n_ctx_train": 8192}},
				map[string]any{"id": "broken", "source": "preset", "status": map[string]any{"value": "unloaded", "failed": true}},
				map[string]any{"id": "cached", "source": "cache", "status": map[string]any{"value": "unloaded"}},
				map[string]any{"id": "fetching", "status": map[string]any{"value": "downloading"}},
			}}
		case "/props":
			switch r.URL.Query().Get("model") {
			case "":
				response = map[string]any{"models_autoload": true}
			case "qwen":
				if r.URL.Query().Get("autoload") != "false" {
					t.Errorf("props query = %s", r.URL.RawQuery)
				}
				response = map[string]any{"chat_template": "{% if enable_thinking %}"}
			default:
				t.Errorf("props requested for %s", r.URL.Query().Get("model"))
				response = map[string]any{}
			}
		case "/v1/systemone":
			response = map[string]any{"answers": map[string]any{"safe": map[string]any{"type": "noul", "noul": 0.9}}, "usage": map[string]any{"input_tokens": 4}}
		case "/tokenize":
			ids := []any{}
			for _, r := range body["content"].(string) {
				ids = append(ids, map[string]any{"id": int(r), "piece": string(r)})
			}
			if body["content"] == "\nYes" || body["content"] == "\nNo" {
				ids = []any{10, map[string]any{"id": map[string]int{"\nYes": 1001, "\nNo": 1002}[body["content"].(string)]}}
			}
			response = map[string]any{"tokens": ids}
		case "/apply-template":
			messages := body["messages"].([]any)
			response = map[string]any{"prompt": "<prompt>" + messages[1].(map[string]any)["content"].(string) + "<think>"}
		case "/completion":
			top := []any{map[string]any{"id": 65, "logprob": math.Log(0.6)}, map[string]any{"id": 1001, "logprob": math.Log(0.2)}, map[string]any{"id": 1002, "logprob": math.Log(0.6)}}
			if body["n_probs"].(float64) > 256 {
				top = append(top, map[string]any{"id": 66, "logprob": math.Log(0.2)})
			}
			response = map[string]any{"completion_probabilities": []any{map[string]any{"top_logprobs": top}}}
		default:
			http.NotFound(w, r)
			return
		}
		if err := json.NewEncoder(w).Encode(response); err != nil {
			t.Error(err)
		}
	}))
}

func TestFetchLlamaCatalogSplitsChatAndClassifierModels(t *testing.T) {
	var (
		requests []map[string]any
		mu       sync.Mutex
	)
	server := llamaTestServer(t, &requests, &mu)
	defer server.Close()

	models, err := FetchLlamaCatalog(t.Context(), server.Client(), server.URL+"/v1/", "")
	if err != nil {
		t.Fatal(err)
	}
	byKey := map[string]*Model{}
	for _, model := range models {
		byKey[model.Type+":"+model.ID] = model
	}
	for _, key := range []string{"chat:julia-1", "chat:broken", "classifier:broken", "chat:cached", "chat:fetching"} {
		if byKey[key] != nil {
			t.Errorf("%s listed", key)
		}
	}
	decision := byKey["classifier:julia-1"]
	if decision == nil || decision.API != APITypesafeSystemOne || decision.BaseURL != server.URL+"/v1" || decision.Provider != ProviderLlamaCpp {
		t.Fatalf("decision classifier = %+v", decision)
	}
	qwen := byKey["chat:qwen"]
	if qwen == nil || qwen.API != APIOpenAICompletions || qwen.BaseURL != server.URL+"/v1" || !qwen.Reasoning || qwen.ContextWindow != 32768 || qwen.MaxTokens != 32768 ||
		!qwen.SupportsInput(InputImage) || qwen.Compat == nil || qwen.Compat.ThinkingFormat != ThinkingFormatQwenChatTmpl || qwen.Compat.MaxTokensField != "max_tokens" ||
		*qwen.Compat.SupportsStrictMode || !*qwen.Compat.SupportsUsageInStreaming || *qwen.ThinkingLevelMap["medium"] != "medium" || qwen.ThinkingLevelMap["high"] != nil {
		t.Fatalf("chat model = %+v", qwen)
	}
	if fallback := byKey["classifier:qwen"]; fallback == nil || fallback.API != APILlamaCppClassify || fallback.BaseURL != server.URL {
		t.Fatalf("fallback classifier = %+v", fallback)
	}
	if sleepy := byKey["chat:sleepy"]; sleepy == nil || sleepy.Reasoning || sleepy.ContextWindow != 4096 {
		t.Fatalf("sleeping model = %+v", sleepy)
	}
	if preset := byKey["chat:preset"]; preset == nil || preset.ContextWindow != 8192 {
		t.Fatalf("autoload preset = %+v", preset)
	}
}

func TestClassifyLlamaCppRoutesDecisionModelsNativelyAndReadsLabelProbabilities(t *testing.T) {
	var (
		requests []map[string]any
		mu       sync.Mutex
	)
	server := llamaTestServer(t, &requests, &mu)
	defer server.Close()
	models, err := FetchLlamaCatalog(t.Context(), server.Client(), server.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	find := func(id string) *Model {
		for _, model := range models {
			if model.Type == "classifier" && model.ID == id {
				return model
			}
		}
		t.Fatalf("no classifier %s", id)
		return nil
	}
	state := map[string]any{"text": "hello"}

	native, err := Classify(t.Context(), find("julia-1"), ClassifierContext{State: state, Questions: map[string]ClassifierQuestion{"safe": {Type: "bool", Instructions: "Safe?"}}})
	if err != nil || string(native.Answers["safe"]) != `{"probability":0.9,"type":"bool"}` {
		t.Fatalf("native = %+v, %v", native, err)
	}
	mu.Lock()
	if requests[0]["path"] != "/v1/systemone" || requests[0]["auth"] != "Bearer local" {
		t.Fatalf("native request = %v", requests[0])
	}
	requests = nil
	mu.Unlock()

	var payloads []any
	result, err := Classify(t.Context(), find("qwen"), ClassifierContext{State: state, Questions: map[string]ClassifierQuestion{
		"pick":  {Type: "choice", Instructions: "Which?", Criteria: map[string]string{"left": "the left one", "right": ""}},
		"agree": {Type: "bool", Instructions: "Agree?", Criteria: map[string]string{"true": "it agrees"}},
	}}, WithPayloadCallback(func(payload any) any { payloads = append(payloads, payload); return nil }))
	if err != nil {
		t.Fatal(err)
	}
	var pick struct {
		Choice        string             `json:"choice"`
		Probabilities map[string]float64 `json:"probabilities"`
		Confidence    float64            `json:"confidence"`
	}
	if err := json.Unmarshal(result.Answers["pick"], &pick); err != nil {
		t.Fatal(err)
	}
	if pick.Choice != "left" || math.Abs(pick.Probabilities["left"]-0.75) > 1e-9 || math.Abs(pick.Confidence-0.5) > 1e-9 {
		t.Fatalf("pick = %+v", pick)
	}
	var agree struct{ Probability float64 }
	if err := json.Unmarshal(result.Answers["agree"], &agree); err != nil || math.Abs(agree.Probability-0.25) > 1e-9 {
		t.Fatalf("agree = %s, %v", result.Answers["agree"], err)
	}
	if len(payloads) != 3 {
		t.Fatalf("payload callback saw %d completion requests, want 3 (one escalation)", len(payloads))
	}
	mu.Lock()
	defer mu.Unlock()
	var depths []float64
	for _, request := range requests {
		body := request["body"].(map[string]any)
		switch request["path"] {
		case "/completion":
			depths = append(depths, body["n_probs"].(float64))
			if !strings.HasSuffix(body["prompt"].(string), "<think></think>") || body["n_predict"].(float64) != 1 || body["model"] != "qwen" {
				t.Errorf("completion body = %v", body)
			}
		case "/apply-template":
			content := body["messages"].([]any)[1].(map[string]any)["content"].(string)
			if !strings.Contains(content, "State:\n{\n \"text\": \"hello\"\n}") || !strings.Contains(content, "Task: answer each of the following questions about the state.") {
				t.Errorf("rendered question = %q", content)
			}
			if body["chat_template_kwargs"].(map[string]any)["enable_thinking"] != false {
				t.Errorf("template body = %v", body)
			}
		}
	}
	if len(depths) != 3 || depths[0] != 256 || depths[1] != 256 || depths[2] != 4096 {
		t.Fatalf("readout depths = %v", depths)
	}

	invalid, err := Classify(t.Context(), find("qwen"), ClassifierContext{State: state, Questions: map[string]ClassifierQuestion{"one": {Type: "choice", Instructions: "?", Criteria: map[string]string{"only": ""}}}})
	if err == nil || invalid.StopReason != StopReasonError || !strings.Contains(invalid.ErrorMessage, "2 to 62 options") {
		t.Fatalf("invalid = %+v, %v", invalid, err)
	}
	if _, err := Classify(t.Context(), find("qwen"), ClassifierContext{State: state, Questions: map[string]ClassifierQuestion{"agree": {Type: "bool"}}}, WithTemperature(0)); err == nil || !strings.Contains(err.Error(), "positive") {
		t.Fatalf("zero temperature err = %v", err)
	}
}
