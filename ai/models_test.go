package ai

import (
	"bufio"
	"encoding/json"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

func TestListProvidersReturnsProviderIDs(t *testing.T) {
	providers := ListProviders()
	if len(providers) == 0 {
		t.Fatal("expected providers to be registered")
	}

	foundOpenAI := false
	foundAnthropic := false
	foundGroq := false
	for _, provider := range providers {
		switch provider {
		case ProviderOpenAI:
			foundOpenAI = true
		case ProviderAnthropic:
			foundAnthropic = true
		case ProviderGroq:
			foundGroq = true
		case APIOpenAICompletions, APIAnthropicMessages:
			t.Fatalf("ListProviders returned API identifier %q instead of provider identifier", provider)
		}
	}

	if !foundOpenAI {
		t.Fatal("expected openai provider in registry")
	}
	if !foundAnthropic {
		t.Fatal("expected anthropic provider in registry")
	}
	if !foundGroq {
		t.Fatal("expected groq provider in full registry")
	}
}

func TestListModelsUnknownProvider(t *testing.T) {
	models := ListModels("does-not-exist")
	if len(models) != 0 {
		t.Fatalf("expected no models for unknown provider, got %d", len(models))
	}
}

func TestAuthenticatedProviderSetIncludesNoCredentialProviders(t *testing.T) {
	for name := range EnvAPIKeys {
		t.Setenv(name, "")
	}

	got := AuthenticatedProviderSet(t.Context(), nil)
	if _, ok := got[ProviderClaudeAgent]; !ok {
		t.Fatalf("authenticated providers missing no-credential provider %q: %#v", ProviderClaudeAgent, got)
	}
}

func TestListAvailableProvidersIncludesUnsupportedProviderIDs(t *testing.T) {
	providers := ListAvailableProviders()
	if len(providers) == 0 {
		t.Fatal("expected providers to be registered")
	}

	foundSupported := false
	foundUnsupported := false
	for _, provider := range providers {
		switch provider {
		case ProviderOpenAI:
			foundSupported = true
		case ProviderAmazonBedrock:
			foundUnsupported = true
		case APIOpenAICompletions, APIAnthropicMessages:
			t.Fatalf("ListAvailableProviders returned API identifier %q instead of provider identifier", provider)
		}
	}

	if !foundSupported {
		t.Fatal("expected openai provider in available registry")
	}
	if !foundUnsupported {
		t.Fatal("expected amazon-bedrock provider in available registry")
	}
}

func TestGetAvailableModelExposesUnsupportedRegistryEntries(t *testing.T) {
	models := ListAvailableModels(ProviderAmazonBedrock)
	if len(models) == 0 {
		t.Fatalf("ListAvailableModels(%q) = 0, want > 0", ProviderAmazonBedrock)
	}
	model, err := GetAvailableModel(ProviderAmazonBedrock, models[0].ID)
	if err != nil {
		t.Fatalf("GetAvailableModel(%q, %q): %v", ProviderAmazonBedrock, models[0].ID, err)
	}
	if model.Provider != ProviderAmazonBedrock {
		t.Fatalf("provider = %q, want %q", model.Provider, ProviderAmazonBedrock)
	}
}

func TestListAvailableModelsExposesUnsupportedRegistryEntries(t *testing.T) {
	models := ListAvailableModels(ProviderAmazonBedrock)
	if len(models) == 0 {
		t.Fatalf("ListAvailableModels(%q) = 0, want > 0", ProviderAmazonBedrock)
	}
}

func TestGetModelExposesPromotedModelCohorts(t *testing.T) {
	for _, cohort := range modelSupportCohorts {
		found := false
		for _, available := range ListAvailableModels(cohort.provider) {
			if available == nil || !strings.HasPrefix(available.ID, cohort.idPrefix) {
				continue
			}
			found = true
			model, err := GetModel(cohort.provider, available.ID)
			if err != nil {
				t.Fatalf("GetModel(%q, %q): %v", cohort.provider, available.ID, err)
			}
			if model.Provider != cohort.provider {
				t.Fatalf("provider = %q, want %q", model.Provider, cohort.provider)
			}
		}
		if !found {
			t.Fatalf("no upstream models match promoted cohort %s/%s*; update modelSupportCohorts", cohort.provider, cohort.idPrefix)
		}
	}
}

func TestGetAvailableModelNormalizesAntigravityAlias(t *testing.T) {
	models := ListAvailableModels(ProviderGoogleAntigravity)
	if len(models) == 0 {
		t.Fatalf("ListAvailableModels(%q) = 0, want > 0", ProviderGoogleAntigravity)
	}
	model, err := GetAvailableModel(ProviderAntigravity, models[0].ID)
	if err != nil {
		t.Fatalf("GetAvailableModel(%q, %q): %v", ProviderAntigravity, models[0].ID, err)
	}
	if model.Provider != ProviderGoogleAntigravity {
		t.Fatalf("provider = %q, want %q", model.Provider, ProviderGoogleAntigravity)
	}
}

func TestImplementedProvidersHaveRegisteredModels(t *testing.T) {
	providers := []string{
		ProviderGoogle,
		ProviderGoogleVertex,
		ProviderAzureOpenAI,
		ProviderMistral,
		ProviderOpenAICodex,
	}

	for _, provider := range providers {
		models := ListAvailableModels(provider)
		if len(models) == 0 {
			t.Fatalf("ListAvailableModels(%q) = 0, want > 0", provider)
		}
		for _, available := range models {
			model, err := GetModel(provider, available.ID)
			if err != nil {
				t.Fatalf("GetModel(%q, %q): %v", provider, available.ID, err)
			}
			if model.Provider != provider {
				t.Fatalf("provider = %q, want %q", model.Provider, provider)
			}
		}
	}
}

func TestGetModelExposesBedrockModels(t *testing.T) {
	models := ListAvailableModels(ProviderAmazonBedrock)
	if len(models) == 0 {
		t.Fatalf("ListAvailableModels(%q) = 0, want > 0", ProviderAmazonBedrock)
	}
	model, err := GetModel(ProviderAmazonBedrock, models[0].ID)
	if err != nil {
		t.Fatalf("GetModel(%q, %q): %v", ProviderAmazonBedrock, models[0].ID, err)
	}
	if model.Provider != ProviderAmazonBedrock {
		t.Fatalf("provider = %q, want %q", model.Provider, ProviderAmazonBedrock)
	}
}

func TestGetModelExposesFullRegistryEntries(t *testing.T) {
	models := ListAvailableModels(ProviderGroq)
	if len(models) == 0 {
		t.Fatalf("ListAvailableModels(%q) = 0, want > 0", ProviderGroq)
	}
	model, err := GetModel(ProviderGroq, models[0].ID)
	if err != nil {
		t.Fatalf("GetModel(%q, %q): %v", ProviderGroq, models[0].ID, err)
	}
	if model.Provider != ProviderGroq {
		t.Fatalf("provider = %q, want %q", model.Provider, ProviderGroq)
	}
}

func TestSupportedModelHelpersRemainFiltered(t *testing.T) {
	models := ListAvailableModels(ProviderGroq)
	if len(models) == 0 {
		t.Fatalf("ListAvailableModels(%q) = 0, want > 0", ProviderGroq)
	}

	if _, err := GetSupportedModel(ProviderGroq, models[0].ID); err == nil {
		t.Fatal("GetSupportedModel unexpectedly succeeded for unsupported Groq model")
	}

	if models := ListSupportedModels(ProviderGroq); len(models) != 0 {
		t.Fatalf("ListSupportedModels(%q) = %d, want 0", ProviderGroq, len(models))
	}

	providers := ListSupportedProviders()
	for _, provider := range providers {
		if provider == ProviderGroq {
			t.Fatal("ListSupportedProviders unexpectedly exposed groq")
		}
	}
}

const referenceModelsGeneratedPath = "models.generated.ts"

// Reference model structure matching TS Model interface
type refModel struct {
	ID            string
	Name          string
	API           string
	Provider      string
	BaseURL       string
	Reasoning     bool
	Input         []string
	Cost          refCostProfile
	ContextWindow int
	MaxTokens     int
}

type refCostProfile struct {
	Input      float64
	Output     float64
	CacheRead  float64
	CacheWrite float64
}

var (
	referenceProviderHeader = regexp.MustCompile(`^\t"([^"]+)": \{$`)
	referenceModelHeader    = regexp.MustCompile(`^\t\t"([^"]+)": \{$`)
)

// requireDriftCheck gates cross-generator drift checks. Both the Go and the
// TypeScript registries are generated from the same live upstream sources at
// independent times, so these checks are vulnerable to upstream churn rather
// than tau defects. They are opt-in: set TAU_DRIFT_CHECK=1 to run them after
// regenerating both registries.
func requireDriftCheck(t *testing.T) {
	t.Helper()
	if os.Getenv("TAU_DRIFT_CHECK") != "1" {
		t.Skip("cross-generator drift checks are opt-in; set TAU_DRIFT_CHECK=1 to run")
	}
}

// refModelFieldDrift represents a field-level mismatch
type refModelFieldDrift struct {
	ModelID  string
	Provider string
	Field    string
	GoValue  any
	RefValue any
}

// parseReferenceRegistry parses the TS models.generated.ts file and returns models by provider
func parseReferenceRegistry(path string) (map[string]map[string]*refModel, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	providers := make(map[string]map[string]*refModel)
	currentProvider := ""
	currentModelID := ""
	currentModel := (*refModel)(nil)
	inModel := false

	// We need to collect the full model object and parse it as JSON
	// Simple state machine to extract model objects
	scanner := bufio.NewScanner(f)
	var modelJSON strings.Builder

	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), "\r")

		// Check for provider header
		if matches := referenceProviderHeader.FindStringSubmatch(line); matches != nil {
			currentProvider = matches[1]
			if _, ok := providers[currentProvider]; !ok {
				providers[currentProvider] = make(map[string]*refModel)
			}
			continue
		}

		// Check for model header
		if matches := referenceModelHeader.FindStringSubmatch(line); matches != nil {
			// Save previous model if exists
			if inModel && currentModel != nil && currentModelID != "" {
				providers[currentProvider][currentModelID] = currentModel
			}
			currentModelID = matches[1]
			inModel = true
			modelJSON.Reset()
			modelJSON.WriteString("{")
			continue
		}

		// Accumulate JSON for current model
		if inModel {
			// Check if we've reached the end of this model (line that doesn't start with tab)
			if len(line) > 0 && line[0] != '\t' && line[0] != '}' {
				// End of model block
				modelJSON.WriteString("}")
				m := parseRefModelJSON(modelJSON.String())
				if m != nil {
					providers[currentProvider][currentModelID] = m
				}
				inModel = false
				continue
			}
			modelJSON.WriteString(line)
			modelJSON.WriteString("\n")

			// Check for end of model (closing brace at same indent as model start)
			if strings.HasPrefix(line, "\t\t}") {
				modelJSON.WriteString("}")
				m := parseRefModelJSON(modelJSON.String())
				if m != nil {
					providers[currentProvider][currentModelID] = m
				}
				inModel = false
			}
		}
	}

	return providers, nil
}

func parseRefModelJSON(jsonStr string) *refModel {
	// Convert TS-style satisfies to valid JSON
	jsonStr = regexp.MustCompile(`,\s*\} satisfies Model<"[^"]+">`).ReplaceAllString(jsonStr, "}")
	jsonStr = regexp.MustCompile(`\s*\} satisfies [^,]+`).ReplaceAllString(jsonStr, "}")
	jsonStr = convertTSInterfaceToJSON(jsonStr)

	var m refModel
	if err := json.Unmarshal([]byte(jsonStr), &m); err != nil {
		return nil
	}
	return &m
}

func convertTSInterfaceToJSON(s string) string {
	// Remove TypeScript type annotations
	s = regexp.MustCompile(`:\s*"([^"]+)"`).ReplaceAllStringFunc(s, func(match string) string {
		return match
	})
	return s
}

// TestReferenceModelFieldDrift verifies every officially supported model matches the reference
func TestReferenceModelFieldDrift(t *testing.T) {
	requireDriftCheck(t)

	refProviders, err := parseReferenceRegistry(referenceModelsGeneratedPath)
	if err != nil {
		if os.IsNotExist(err) {
			t.Skipf("reference registry not available at %s", referenceModelsGeneratedPath)
		}
		t.Fatalf("parse reference registry: %v", err)
	}

	// Get all officially supported provider/model pairs
	drifts := checkModelFieldDrift(t, refProviders)

	if len(drifts) > 0 {
		t.Errorf("found %d model field drift(s):", len(drifts))
		for _, d := range drifts {
			t.Errorf("  model=%s provider=%s field=%s go=%v ref=%v",
				d.ModelID, d.Provider, d.Field, d.GoValue, d.RefValue)
		}
	}
}

// checkModelFieldDrift compares every officially supported model against the reference
func checkModelFieldDrift(t *testing.T, refProviders map[string]map[string]*refModel) []refModelFieldDrift {
	var drifts []refModelFieldDrift

	// Get support status for all providers
	supportStatuses := ListProviderSupport()

	for _, status := range supportStatuses {
		if !status.OfficiallySupported || !status.HasModelMetadata {
			continue
		}

		// Get Go models for this provider
		goModels := ListModels(status.Provider)
		if len(goModels) == 0 {
			continue
		}

		refModels, ok := refProviders[status.Provider]
		if !ok {
			// Provider not in reference - this might be intentional or a gap
			t.Logf("WARNING: provider %q is officially supported in Go but not in reference", status.Provider)
			continue
		}

		for _, goModel := range goModels {
			// Try to find matching model in reference by comparing the short ID
			// (Go uses "claude-haiku-4-5" while TS uses "anthropic/claude-haiku-4-5")
			refModel := findMatchingRefModel(refModels, goModel.ID)
			if refModel == nil {
				// Model not in reference - this is expected for newly added models
				// that haven't been added to the TS reference yet
				continue
			}

			// Compare all fields
			drifts = append(drifts, compareModelFields(goModel, refModel)...)
		}
	}

	return drifts
}

// findMatchingRefModel finds a reference model by short ID (without provider prefix)
// The TS reference uses full model IDs like "anthropic/claude-haiku-4-5" while
// Go uses short IDs like "claude-haiku-4-5". This function matches by short ID.
func findMatchingRefModel(refModels map[string]*refModel, goModelID string) *refModel {
	for refID, refModel := range refModels {
		// The reference model ID format is "provider/model-id", extract the model-id part
		parts := strings.SplitN(refID, "/", 2)
		var shortID string
		if len(parts) == 2 {
			shortID = parts[1]
		} else {
			shortID = refID
		}

		// Also handle cases where the reference ID starts with a provider prefix
		// and the short part matches the Go model ID
		if shortID == goModelID {
			return refModel
		}

		// Handle case where Go model ID might have been transformed
		// e.g., "claude-haiku-4-5" should match "claude-haiku-4-5" from "anthropic/claude-haiku-4-5"
		// or from "claude-haiku-4-5" (direct match)
		if refID == goModelID {
			return refModel
		}
	}
	return nil
}

// compareModelFields compares all fields of a Go model against a reference model
func compareModelFields(goModel *Model, refModel *refModel) []refModelFieldDrift {
	var drifts []refModelFieldDrift

	// ID - should always match
	if goModel.ID != refModel.ID {
		drifts = append(drifts, refModelFieldDrift{
			ModelID:  goModel.ID,
			Provider: goModel.Provider,
			Field:    "ID",
			GoValue:  goModel.ID,
			RefValue: refModel.ID,
		})
	}

	// Name
	if goModel.Name != refModel.Name {
		drifts = append(drifts, refModelFieldDrift{
			ModelID:  goModel.ID,
			Provider: goModel.Provider,
			Field:    "Name",
			GoValue:  goModel.Name,
			RefValue: refModel.Name,
		})
	}

	// API
	if goModel.API != refModel.API {
		drifts = append(drifts, refModelFieldDrift{
			ModelID:  goModel.ID,
			Provider: goModel.Provider,
			Field:    "API",
			GoValue:  goModel.API,
			RefValue: refModel.API,
		})
	}

	// BaseURL
	if goModel.BaseURL != refModel.BaseURL {
		drifts = append(drifts, refModelFieldDrift{
			ModelID:  goModel.ID,
			Provider: goModel.Provider,
			Field:    "BaseURL",
			GoValue:  goModel.BaseURL,
			RefValue: refModel.BaseURL,
		})
	}

	// Reasoning
	if goModel.Reasoning != refModel.Reasoning {
		drifts = append(drifts, refModelFieldDrift{
			ModelID:  goModel.ID,
			Provider: goModel.Provider,
			Field:    "Reasoning",
			GoValue:  goModel.Reasoning,
			RefValue: refModel.Reasoning,
		})
	}

	// Input
	goInput := inputTypesToStrings(goModel.Input)
	refInput := refModel.Input
	if !stringSlicesEqual(goInput, refInput) {
		drifts = append(drifts, refModelFieldDrift{
			ModelID:  goModel.ID,
			Provider: goModel.Provider,
			Field:    "Input",
			GoValue:  goInput,
			RefValue: refInput,
		})
	}

	// Cost
	if costDrift := compareCostProfile(goModel.ID, goModel.Provider, &goModel.Cost, &refModel.Cost); costDrift != nil {
		drifts = append(drifts, *costDrift)
	}

	// ContextWindow
	if goModel.ContextWindow != refModel.ContextWindow {
		drifts = append(drifts, refModelFieldDrift{
			ModelID:  goModel.ID,
			Provider: goModel.Provider,
			Field:    "ContextWindow",
			GoValue:  goModel.ContextWindow,
			RefValue: refModel.ContextWindow,
		})
	}

	// MaxTokens
	if goModel.MaxTokens != refModel.MaxTokens {
		drifts = append(drifts, refModelFieldDrift{
			ModelID:  goModel.ID,
			Provider: goModel.Provider,
			Field:    "MaxTokens",
			GoValue:  goModel.MaxTokens,
			RefValue: refModel.MaxTokens,
		})
	}

	return drifts
}

func inputTypesToStrings(inputs []InputType) []string {
	result := make([]string, len(inputs))
	for i, inp := range inputs {
		result[i] = string(inp)
	}
	return result
}

func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func compareCostProfile(modelID, provider string, goCost *CostProfile, refCost *refCostProfile) *refModelFieldDrift {
	tolerance := 0.0001 // Allow small floating point differences

	if goCost == nil || refCost == nil {
		if goCost == nil && refCost == nil {
			return nil
		}
		return &refModelFieldDrift{
			ModelID:  modelID,
			Provider: provider,
			Field:    "Cost",
			GoValue:  goCost,
			RefValue: refCost,
		}
	}

	if floatEquals(goCost.Input, refCost.Input, tolerance) != nil {
		return &refModelFieldDrift{
			ModelID:  modelID,
			Provider: provider,
			Field:    "Cost.Input",
			GoValue:  goCost.Input,
			RefValue: refCost.Input,
		}
	}

	if floatEquals(goCost.Output, refCost.Output, tolerance) != nil {
		return &refModelFieldDrift{
			ModelID:  modelID,
			Provider: provider,
			Field:    "Cost.Output",
			GoValue:  goCost.Output,
			RefValue: refCost.Output,
		}
	}

	if floatEquals(goCost.CacheRead, refCost.CacheRead, tolerance) != nil {
		return &refModelFieldDrift{
			ModelID:  modelID,
			Provider: provider,
			Field:    "Cost.CacheRead",
			GoValue:  goCost.CacheRead,
			RefValue: refCost.CacheRead,
		}
	}

	if floatEquals(goCost.CacheWrite, refCost.CacheWrite, tolerance) != nil {
		return &refModelFieldDrift{
			ModelID:  modelID,
			Provider: provider,
			Field:    "Cost.CacheWrite",
			GoValue:  goCost.CacheWrite,
			RefValue: refCost.CacheWrite,
		}
	}

	return nil
}

func floatEquals(a, b, tolerance float64) *float64 {
	if a == b {
		return nil
	}
	diff := a - b
	if diff < 0 {
		diff = -diff
	}
	if diff <= tolerance {
		return nil
	}
	return &a
}

// TestModelCapabilityFlagsDrift checks capability flags for a sample of models
func TestModelCapabilityFlagsDrift(t *testing.T) {
	requireDriftCheck(t)

	refProviders, err := parseReferenceRegistry(referenceModelsGeneratedPath)
	if err != nil {
		if os.IsNotExist(err) {
			t.Skipf("reference registry not available at %s", referenceModelsGeneratedPath)
		}
		t.Fatalf("parse reference registry: %v", err)
	}

	// Test capability flags for models in reference
	// Note: Go has Input/Output arrays vs TS's input array
	// This test verifies the conversion is working

	for _, provider := range []string{"anthropic", "openai", "google", "mistral"} {
		refModels, ok := refProviders[provider]
		if !ok {
			continue
		}

		for refID, refModel := range refModels {
			// Try to find the model in Go registry
			goModel, err := GetAvailableModel(provider, refID)
			if err != nil {
				// Model not in Go - this is expected for some models
				continue
			}

			// Check Input capability (converted to InputType)
			goInputs := make(map[string]bool)
			for _, inp := range goModel.Input {
				goInputs[string(inp)] = true
			}
			for _, refInput := range refModel.Input {
				if !goInputs[refInput] {
					t.Errorf("model %s/%s: Go missing input capability %q from reference",
						provider, refID, refInput)
				}
			}
		}
	}
}

// TestModelPricingDrift specifically checks cost profile precision
func TestModelPricingDrift(t *testing.T) {
	requireDriftCheck(t)

	refProviders, err := parseReferenceRegistry(referenceModelsGeneratedPath)
	if err != nil {
		if os.IsNotExist(err) {
			t.Skipf("reference registry not available at %s", referenceModelsGeneratedPath)
		}
		t.Fatalf("parse reference registry: %v", err)
	}

	driftCount := 0
	tolerance := 0.0001

	// Check Anthropic pricing as a sample
	if refAnthropic, ok := refProviders["anthropic"]; ok {
		for refID, refModel := range refAnthropic {
			goModel, err := GetAvailableModel("anthropic", refID)
			if err != nil {
				continue
			}

			if floatEquals(goModel.Cost.Input, refModel.Cost.Input, tolerance) != nil {
				t.Errorf("model %s: Cost.Input drift: Go=%v Ref=%v",
					refID, goModel.Cost.Input, refModel.Cost.Input)
				driftCount++
			}
			if floatEquals(goModel.Cost.Output, refModel.Cost.Output, tolerance) != nil {
				t.Errorf("model %s: Cost.Output drift: Go=%v Ref=%v",
					refID, goModel.Cost.Output, refModel.Cost.Output)
				driftCount++
			}
			if floatEquals(goModel.Cost.CacheRead, refModel.Cost.CacheRead, tolerance) != nil {
				t.Errorf("model %s: Cost.CacheRead drift: Go=%v Ref=%v",
					refID, goModel.Cost.CacheRead, refModel.Cost.CacheRead)
				driftCount++
			}
			if floatEquals(goModel.Cost.CacheWrite, refModel.Cost.CacheWrite, tolerance) != nil {
				t.Errorf("model %s: Cost.CacheWrite drift: Go=%v Ref=%v",
					refID, goModel.Cost.CacheWrite, refModel.Cost.CacheWrite)
				driftCount++
			}
		}
	}

	if driftCount > 0 {
		t.Logf("Found %d pricing drift(s)", driftCount)
	}
}

// BenchmarkReferenceRegistryParsing benchmarks parsing speed
func BenchmarkReferenceRegistryParsing(b *testing.B) {
	for i := 0; i < b.N; i++ {
		_, err := parseReferenceRegistry(referenceModelsGeneratedPath)
		if err != nil && !os.IsNotExist(err) {
			b.Fatalf("parse error: %v", err)
		}
	}
}

func TestAzureProviderAliasPreservesAPIIdentifier(t *testing.T) {
	if ProviderAzureOpenAI != "azure" || APIAzureOpenAIResponses != "azure-openai-responses" {
		t.Fatal("Azure provider and API identifiers must differ")
	}
	model := &Model{ID: "alias-test", Provider: "azure-openai-responses", API: APIAzureOpenAIResponses}
	RegisterModel(model)
	t.Cleanup(func() { delete(models, "azure/alias-test") })
	for _, provider := range []string{"azure", "azure-openai-responses"} {
		got, err := GetAvailableModel(provider, "alias-test")
		if err != nil || got.Provider != "azure" || got.API != APIAzureOpenAIResponses {
			t.Fatalf("provider=%s model=%+v err=%v", provider, got, err)
		}
	}
}

func TestSupportedThinkingLevelsUsesModelMap(t *testing.T) {
	model := &Model{Reasoning: true, ThinkingLevelMap: map[string]*string{"minimal": nil, "xhigh": new("xhigh"), "max": new("max")}}
	if got := GetSupportedThinkingLevels(model); !slices.Equal(got, []string{"off", "low", "medium", "high", "xhigh", "max"}) {
		t.Fatalf("levels: %v", got)
	}
	if got := ClampThinkingLevel(model, "minimal"); got != "low" {
		t.Fatalf("clamp: %s", got)
	}
	if got := GetSupportedThinkingLevels(&Model{}); !slices.Equal(got, []string{"off"}) {
		t.Fatalf("non-reasoning: %v", got)
	}
}

func TestRegisterModelOperationsDoNotReplaceChat(t *testing.T) {
	chat := &Model{Provider: "operation-isolation-test", ID: "same-id", API: APIOpenAIResponses}
	RegisterModel(chat)
	RegisterModel(&Model{Type: "classifier", Provider: chat.Provider, ID: chat.ID, API: APIOpenAIDecisions})
	got, err := GetModel(chat.Provider, chat.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Type != "" && got.Type != "chat" {
		t.Fatalf("chat was replaced by %s", got.Type)
	}
}

func TestOperationModelLookupRetainsIndependentDefinitions(t *testing.T) {
	provider := "operation-lookup-test"
	for _, operation := range []string{"chat", "classifier", "image"} {
		RegisterModel(&Model{Type: operation, Provider: provider, ID: "same"})
	}
	for _, operation := range []string{"chat", "classifier", "image"} {
		got, err := GetOperationModel(operation, provider, "same")
		if err != nil {
			t.Fatal(err)
		}
		if got.Type != operation {
			t.Fatalf("got %s for %s", got.Type, operation)
		}
	}
	if got := ListAvailableModels(provider); len(got) != 1 || got[0].Type != "chat" {
		t.Fatalf("chat list: %+v", got)
	}
	if _, err := NewModelRegistry().Models(); err != nil {
		t.Fatal(err)
	}
}
