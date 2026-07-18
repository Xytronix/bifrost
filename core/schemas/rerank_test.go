package schemas

import (
	"encoding/json"
	"testing"
)

func TestRerankDocumentUnmarshalBareString(t *testing.T) {
	var d RerankDocument
	if err := json.Unmarshal([]byte(`"hello world"`), &d); err != nil {
		t.Fatalf("bare string unmarshal failed: %v", err)
	}
	if d.Text != "hello world" || d.ID != nil || d.Meta != nil {
		t.Fatalf("expected {Text:\"hello world\"}, got %#v", d)
	}
}

func TestRerankDocumentUnmarshalObject(t *testing.T) {
	var d RerankDocument
	if err := json.Unmarshal([]byte(`{"text":"doc","id":"x"}`), &d); err != nil {
		t.Fatalf("object unmarshal failed: %v", err)
	}
	if d.Text != "doc" || d.ID == nil || *d.ID != "x" {
		t.Fatalf("expected object parse with id, got %#v", d)
	}
}

// A documents array given as plain strings (Cohere v2 / Voyage / most
// /v1/rerank clients) must decode into RerankDocument values.
func TestRerankDocumentUnmarshalStringArray(t *testing.T) {
	var docs []RerankDocument
	if err := json.Unmarshal([]byte(`["a cat","a dog"]`), &docs); err != nil {
		t.Fatalf("string-array unmarshal failed: %v", err)
	}
	if len(docs) != 2 || docs[0].Text != "a cat" || docs[1].Text != "a dog" {
		t.Fatalf("expected [a cat, a dog], got %#v", docs)
	}
}
