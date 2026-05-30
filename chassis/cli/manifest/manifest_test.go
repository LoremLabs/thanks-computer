package manifest

import "testing"

func TestParseRoundTrip(t *testing.T) {
	src := `apiVersion: thanks.computer/v1alpha1
kind: Package
name: support-basic
version: 0.1.0
summary: hi
package:
  kind: department
  install:
    defaultMode: as-stack
    suggestedStack: support
compatibility:
  txco: ">=0.3.0"
operations:
  bundled:
    - name: classify
      path: OPS/support/0100/classify.js
      lang: js
  required:
    - name: AUDIT
      kind: http
      example: https://audit.example.com/op
build:
  requires:
    - "javy >= 1.0"
capabilities:
  - http.fetch
inlets:
  - type: email
    suggestedLocalPart: support
requires:
  packages:
    - ref: moderation/basic@^1
      mode: into-stack
metadata:
  license: Apache-2.0
`
	m, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if m.APIVersion != APIVersion || m.Kind != Kind {
		t.Errorf("header = %q/%q", m.APIVersion, m.Kind)
	}
	if m.Name != "support-basic" || m.Version != "0.1.0" {
		t.Errorf("identity = %q/%q", m.Name, m.Version)
	}
	if m.Package.Kind != "department" || m.Package.Install.DefaultMode != "as-stack" {
		t.Errorf("package = %+v", m.Package)
	}
	if len(m.Operations.Bundled) != 1 || m.Operations.Bundled[0].Name != "classify" {
		t.Errorf("bundled = %+v", m.Operations.Bundled)
	}
	if len(m.Operations.Required) != 1 || m.Operations.Required[0].Name != "AUDIT" {
		t.Errorf("required = %+v", m.Operations.Required)
	}
	if len(m.Build.Requires) != 1 {
		t.Errorf("build = %+v", m.Build)
	}
	if len(m.Capabilities) != 1 || m.Capabilities[0] != "http.fetch" {
		t.Errorf("capabilities = %+v", m.Capabilities)
	}
	if len(m.Requires.Packages) != 1 || m.Requires.Packages[0].Ref != "moderation/basic@^1" {
		t.Errorf("requires = %+v", m.Requires)
	}
	if m.Metadata.License != "Apache-2.0" {
		t.Errorf("metadata = %+v", m.Metadata)
	}
}

func TestParseBadYAML(t *testing.T) {
	if _, err := Parse([]byte("name: [unterminated\n")); err == nil {
		t.Error("expected parse error for malformed yaml")
	}
}
