package apierr_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/jahrulnr/fakeagents/internal/apierr"
)

func TestProviderPresets(t *testing.T) {
	openAI, ok := apierr.Preset(apierr.ProviderOpenAI, "billing")
	if !ok || openAI.Status != http.StatusTooManyRequests || openAI.Code != "insufficient_quota" {
		t.Fatalf("openai billing = %+v", openAI)
	}
	anthropic, ok := apierr.Preset(apierr.ProviderAnthropic, "billing")
	if !ok || anthropic.Status != http.StatusPaymentRequired || anthropic.Type != "billing_error" {
		t.Fatalf("anthropic billing = %+v", anthropic)
	}
}

func TestPresetAliases(t *testing.T) {
	for _, name := range []string{"quota", "insufficient_quota", "payment_required", "billing_error"} {
		if _, ok := apierr.Preset(apierr.ProviderOpenAI, name); !ok {
			t.Fatalf("alias %q not recognised", name)
		}
	}
}

func TestPresetCloneIsIndependent(t *testing.T) {
	first := apierr.MustPreset(apierr.ProviderOpenAI, "rate_limit")
	first.Message = "changed"
	first.RetryAfter = time.Second
	second := apierr.MustPreset(apierr.ProviderOpenAI, "rate_limit")
	if second.Message == "changed" || second.RetryAfter != 0 {
		t.Fatalf("preset was mutated: %+v", second)
	}
}

func TestUnknownPreset(t *testing.T) {
	if _, ok := apierr.Preset(apierr.ProviderOpenAI, "does-not-exist"); ok {
		t.Fatal("unknown preset accepted")
	}
	if !apierr.KnownPreset("overloaded") {
		t.Fatal("known preset not found")
	}
	generic := apierr.PresetOrGeneric(apierr.ProviderOpenAI, "does-not-exist")
	if generic.Status != http.StatusBadRequest {
		t.Fatalf("generic = %+v", generic)
	}
}
