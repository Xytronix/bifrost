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
