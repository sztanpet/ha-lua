package cards

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMaterializeWritesCard checks the bundled card asset lands on disk
// byte-for-byte and that the served filename is the one DOCS reference
// (/local/ha-lua/enhanced-climate-card.js).
func TestMaterializeWritesCard(t *testing.T) {
	dir := t.TempDir()
	if err := Materialize(dir); err != nil {
		t.Fatal(err)
	}

	want, err := FS.ReadFile("enhanced-climate-card.js")
	if err != nil {
		t.Fatalf("card asset not embedded: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "enhanced-climate-card.js"))
	if err != nil {
		t.Fatalf("card not materialized: %v", err)
	}
	if string(got) != string(want) {
		t.Error("materialized card content differs from the embedded asset")
	}
	// The custom element must be registered under the documented type.
	if !strings.Contains(string(want), `customElements.define("ha-lua-enhanced-climate-card"`) {
		t.Error("card does not define the ha-lua-enhanced-climate-card element")
	}
}

// TestMaterializeOverwrites confirms a locally-modified asset is restored to the
// embedded content on the next boot.
func TestMaterializeOverwrites(t *testing.T) {
	dir := t.TempDir()
	if err := Materialize(dir); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "enhanced-climate-card.js")
	if err := os.WriteFile(target, []byte("// tampered\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Materialize(dir); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(got), "tampered") {
		t.Error("Materialize did not overwrite the tampered asset")
	}
}
