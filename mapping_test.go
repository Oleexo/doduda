package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNormalizeUnityRIDTypesCopiesLanguages(t *testing.T) {
	srcDir := t.TempDir()
	dstDir := t.TempDir()

	if err := os.WriteFile(filepath.Join(srcDir, "items.json"), []byte(`{"1":{"rid":123}}`), 0644); err != nil {
		t.Fatal(err)
	}

	srcLangDir := filepath.Join(srcDir, "languages")
	if err := os.MkdirAll(srcLangDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcLangDir, "fr.json"), []byte(`{"entries":{"1":"Bonjour"}}`), 0644); err != nil {
		t.Fatal(err)
	}

	if err := normalizeUnityRIDTypes(srcDir, dstDir); err != nil {
		t.Fatal(err)
	}

	itemData, err := os.ReadFile(filepath.Join(dstDir, "items.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(itemData) != `{"1":{"rid":"123"}}` {
		t.Fatalf("normalized items.json = %s", itemData)
	}

	langData, err := os.ReadFile(filepath.Join(dstDir, "languages", "fr.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(langData) != `{"entries":{"1":"Bonjour"}}` {
		t.Fatalf("copied fr.json = %s", langData)
	}
}
