package ai

import "testing"

func TestProviderSupportTaxonomy(t *testing.T) {
	tests := []struct {
		provider string
		assert   func(t *testing.T, status ProviderSupportStatus)
	}{
		{
			provider: ProviderOpenAI,
			assert: func(t *testing.T, status ProviderSupportStatus) {
				t.Helper()
				if !status.HasProviderConstant {
					t.Fatal("expected provider constant")
				}
				if !status.HasImplementation || !status.IsRegistered {
					t.Fatalf("expected implemented and registered status, got %+v", status)
				}
				if !status.HasModelMetadata || !status.ReachableViaGetClient {
					t.Fatalf("expected metadata-backed GetClient reachability, got %+v", status)
				}
				if !status.Tested || !status.OfficiallySupported {
					t.Fatalf("expected openai to be tested and officially supported, got %+v", status)
				}
			},
		},
		{
			provider: ProviderGoogleGeminiCLI,
			assert: func(t *testing.T, status ProviderSupportStatus) {
				t.Helper()
				if !status.HasImplementation || !status.IsRegistered || !status.ReachableViaGetClient {
					t.Fatalf("expected gemini-cli to be implemented and reachable, got %+v", status)
				}
				if !status.HasModelMetadata {
					t.Fatalf("expected gemini-cli to have generated model metadata, got %+v", status)
				}
				if status.OfficiallySupported {
					t.Fatalf("expected gemini-cli to remain outside the defended support surface, got %+v", status)
				}
			},
		},
		{
			provider: ProviderOllama,
			assert: func(t *testing.T, status ProviderSupportStatus) {
				t.Helper()
				if !status.HasImplementation || !status.IsRegistered || !status.ReachableViaGetClient {
					t.Fatalf("expected ollama to be implemented and reachable through the shared OpenAI path, got %+v", status)
				}
				if status.HasModelMetadata || status.Tested || status.OfficiallySupported {
					t.Fatalf("expected ollama to remain without generated metadata, untested, and unsupported, got %+v", status)
				}
			},
		},
		{
			provider: ProviderMiniMax,
			assert: func(t *testing.T, status ProviderSupportStatus) {
				t.Helper()
				if !status.HasImplementation || !status.IsRegistered || !status.HasModelMetadata {
					t.Fatalf("expected minimax to be implemented, registered, and discoverable, got %+v", status)
				}
				if status.OfficiallySupported {
					t.Fatalf("expected minimax provider path to remain below provider-wide official support, got %+v", status)
				}
				if !status.HasSupportedModels || status.HighestModelTier != SupportTier2 {
					t.Fatalf("expected minimax to expose tier2 model cohorts, got %+v", status)
				}
			},
		},
		{
			provider: ProviderZAI,
			assert: func(t *testing.T, status ProviderSupportStatus) {
				t.Helper()
				if !status.HasImplementation || !status.IsRegistered || !status.HasModelMetadata {
					t.Fatalf("expected zai to be implemented, registered, and discoverable, got %+v", status)
				}
				if status.OfficiallySupported {
					t.Fatalf("expected zai provider path to remain below provider-wide official support, got %+v", status)
				}
				if !status.HasSupportedModels || status.HighestModelTier != SupportTier2 {
					t.Fatalf("expected zai to expose tier2 model cohorts, got %+v", status)
				}
			},
		},
		{
			provider: ProviderGitHubCopilot,
			assert: func(t *testing.T, status ProviderSupportStatus) {
				t.Helper()
				if !status.HasImplementation {
					t.Fatalf("expected github-copilot to have an implementation path, got %+v", status)
				}
				if !status.IsRegistered {
					t.Fatalf("expected github-copilot to be registered, got %+v", status)
				}
				if !status.HasModelMetadata {
					t.Fatalf("expected github-copilot to remain discoverable through generated metadata, got %+v", status)
				}
				if !status.OfficiallySupported {
					t.Fatalf("expected github-copilot to be officially supported, got %+v", status)
				}
			},
		},
	}

	for _, tt := range tests {
		status, ok := ProviderSupport(tt.provider)
		if !ok {
			t.Fatalf("ProviderSupport(%q) not found", tt.provider)
		}
		tt.assert(t, status)
	}
}

func TestProviderSupportNormalizesAntigravityAlias(t *testing.T) {
	status, ok := ProviderSupport(ProviderAntigravity)
	if !ok {
		t.Fatalf("ProviderSupport(%q) not found", ProviderAntigravity)
	}
	if status.Provider != ProviderGoogleAntigravity {
		t.Fatalf("provider = %q, want %q", status.Provider, ProviderGoogleAntigravity)
	}
}

func TestModelSupportTaxonomy(t *testing.T) {
	// Tier1 provider paths promote every model under the provider.
	for _, provider := range []string{ProviderOpenAI, ProviderAmazonBedrock, ProviderGoogleAntigravity} {
		statuses := ListModelSupport(provider)
		if len(statuses) == 0 {
			t.Fatalf("ListModelSupport(%q) = 0, want > 0", provider)
		}
		status := statuses[0]
		if !status.Available || !status.PubliclyListed || !status.ReachableViaGetClient || !status.OfficiallySupported {
			t.Fatalf("expected fully supported %s model, got %+v", provider, status)
		}
		if status.Tier != SupportTier1 {
			t.Fatalf("tier = %q, want %q", status.Tier, SupportTier1)
		}
	}

	// Promoted cohorts expose tier2 models regardless of exact upstream IDs.
	for _, cohort := range modelSupportCohorts {
		var cohortStatus *ModelSupportStatus
		statuses := ListModelSupport(cohort.provider)
		for i := range statuses {
			if statuses[i].Tier == cohort.tier {
				cohortStatus = &statuses[i]
				break
			}
		}
		if cohortStatus == nil {
			t.Fatalf("no %q tier models found for promoted cohort %s/%s*", cohort.tier, cohort.provider, cohort.idPrefix)
		}
		if !cohortStatus.Available || !cohortStatus.PubliclyListed || !cohortStatus.ReachableViaGetClient || !cohortStatus.OfficiallySupported {
			t.Fatalf("expected supported %s model cohort, got %+v", cohort.provider, *cohortStatus)
		}
	}
}

func TestModelSupportNormalizesAntigravityAlias(t *testing.T) {
	models := ListAvailableModels(ProviderGoogleAntigravity)
	if len(models) == 0 {
		t.Fatalf("ListAvailableModels(%q) = 0, want > 0", ProviderGoogleAntigravity)
	}
	status, ok := ModelSupport(ProviderAntigravity, models[0].ID)
	if !ok {
		t.Fatalf("ModelSupport(%q, %q) not found", ProviderAntigravity, models[0].ID)
	}
	if status.Provider != ProviderGoogleAntigravity {
		t.Fatalf("provider = %q, want %q", status.Provider, ProviderGoogleAntigravity)
	}
}

func TestListModelSupport(t *testing.T) {
	statuses := ListModelSupport(ProviderAmazonBedrock)
	if len(statuses) == 0 {
		t.Fatal("expected model support statuses")
	}

	for _, status := range statuses {
		if !status.Available {
			t.Fatalf("expected available status, got %+v", status)
		}
		if status.Provider != ProviderAmazonBedrock {
			t.Fatalf("provider = %q, want %q", status.Provider, ProviderAmazonBedrock)
		}
	}
}

func TestListProviderSupportIncludesKnownFamilies(t *testing.T) {
	statuses := ListProviderSupport()
	if len(statuses) == 0 {
		t.Fatal("expected provider support statuses")
	}

	foundOpenAI := false
	foundAnthropic := false
	for _, status := range statuses {
		switch status.Provider {
		case ProviderOpenAI:
			foundOpenAI = true
		case ProviderAnthropic:
			foundAnthropic = true
		}
	}

	if !foundOpenAI {
		t.Fatal("expected openai support status")
	}
	if !foundAnthropic {
		t.Fatal("expected anthropic support status")
	}
}
