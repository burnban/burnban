package sourceadapter

import "testing"

func TestManifestValidate(t *testing.T) {
	valid := Manifest{
		APIVersion: APIVersion, ID: "fixture-source", DisplayName: "Fixture Source",
		Store:   "append-only fixture records",
		Privacy: Privacy{ReadOnly: true},
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid manifest: %v", err)
	}

	tests := []struct {
		name string
		edit func(*Manifest)
	}{
		{"version", func(m *Manifest) { m.APIVersion = "burnban.source/v2" }},
		{"id", func(m *Manifest) { m.ID = "Fixture Source" }},
		{"write", func(m *Manifest) { m.Privacy.ReadOnly = false }},
		{"network", func(m *Manifest) { m.Privacy.NetworkAccess = true }},
		{"content", func(m *Manifest) { m.Privacy.EmitsPromptOrResponse = true }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manifest := valid
			tt.edit(&manifest)
			if err := manifest.Validate(); err == nil {
				t.Fatalf("invalid manifest accepted: %+v", manifest)
			}
		})
	}
}
