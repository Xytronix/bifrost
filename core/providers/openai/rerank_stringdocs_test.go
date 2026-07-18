package openai

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/maximhq/bifrost/core/schemas"
)

// Voyage, Cohere v2, and most /v1/rerank APIs require `documents` as a plain
// string array. Regression for the object-form 400 (see maximhq/bifrost#5258).
func TestToOpenAIRerankRequestSerializesStringDocuments(t *testing.T) {
	req := ToOpenAIRerankRequest(&schemas.BifrostRerankRequest{
		Model:     "rerank-2.5",
		Query:     "cat",
		Documents: []schemas.RerankDocument{{Text: "a cat"}, {Text: "a dog"}},
	})
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}
	if !strings.Contains(string(body), `"documents":["a cat","a dog"]`) {
		t.Fatalf("expected string-array documents on the wire, got %s", body)
	}
}

// Documents carrying id/meta are JSON-encoded into the string so the
// information survives on the wire rather than being dropped.
func TestToOpenAIRerankRequestEncodesDocumentMetadata(t *testing.T) {
	id := "doc-1"
	req := ToOpenAIRerankRequest(&schemas.BifrostRerankRequest{
		Model:     "rerank-2.5",
		Query:     "cat",
		Documents: []schemas.RerankDocument{{Text: "a cat", ID: &id}},
	})
	if len(req.Documents) != 1 {
		t.Fatalf("expected 1 document, got %d", len(req.Documents))
	}
	if !strings.Contains(req.Documents[0], `"text":"a cat"`) || !strings.Contains(req.Documents[0], `"id":"doc-1"`) {
		t.Fatalf("expected id/text preserved in encoded document, got %q", req.Documents[0])
	}
}

// Voyage returns ranked results under `data` rather than `results`; the parser
// must fall back to it. Regression for null results on Voyage rerank.
func TestOpenAIRerankResponseAcceptsDataArray(t *testing.T) {
	resp := &OpenAIRerankResponse{
		Data: []OpenAIRerankResponseResult{
			{Index: 1, RelevanceScore: 0.9},
			{Index: 0, RelevanceScore: 0.5},
		},
	}
	b := resp.ToBifrostRerankResponse(nil, false)
	if len(b.Results) != 2 {
		t.Fatalf("expected 2 results from data[], got %#v", b.Results)
	}
	if b.Results[0].Index != 1 || b.Results[0].RelevanceScore != 0.9 {
		t.Fatalf("expected top result index=1 score=0.9, got %#v", b.Results[0])
	}
}
