package handlers

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// The vendor ladder is authoritative: datasheet effort tokens describe only the
// tiers beyond low/medium/high, so a partial set must be replaced outright
// rather than merged, while every non-effort parameter survives.
func TestWithReasoningEffortTiers_ReplacesDatasheetTiersKeepsOtherParams(t *testing.T) {
	got := withReasoningEffortTiers(
		[]string{"tools", "reasoning", "reasoning_effort:xhigh", "reasoning_effort:max", "response_format"},
		[]string{"low", "medium", "high", "xhigh", "max"},
	)

	assert.Equal(t, []string{
		"tools", "reasoning", "response_format",
		"reasoning_effort:low", "reasoning_effort:medium", "reasoning_effort:high",
		"reasoning_effort:xhigh", "reasoning_effort:max",
	}, got)
}

// A model the datasheet knows nothing about still gets a usable ladder, and
// advertising an effort scale implies reasoning.
func TestWithReasoningEffortTiers_ImpliesReasoningOnEmptyParams(t *testing.T) {
	got := withReasoningEffortTiers(nil, []string{"low", "high"})

	assert.Equal(t, []string{"reasoning", "reasoning_effort:low", "reasoning_effort:high"}, got)
}

func TestGeminiFlashFamilyEfforts_36PlusOnly(t *testing.T) {
	assert.Equal(t, []string{"low", "medium", "high"}, geminiFlashFamilyEfforts("omp-gw/google-antigravity/gemini-3.8-flash"))
	assert.Equal(t, []string{"low", "medium", "high"}, geminiFlashFamilyEfforts("gemini-3.6-flash"))
	assert.Equal(t, []string{"low", "medium", "high"}, geminiFlashFamilyEfforts("gemini-3.7-flash"))
	assert.Nil(t, geminiFlashFamilyEfforts("gemini-3.5-flash"))
	assert.Nil(t, geminiFlashFamilyEfforts("gemini-3.8-flash-lite"))
	assert.Nil(t, geminiFlashFamilyEfforts("gemini-3.1-pro"))
	assert.Nil(t, geminiFlashFamilyEfforts("claude-opus-5"))
}
