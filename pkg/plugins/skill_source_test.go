package plugins

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dk-code-factory/agent-workflow/pkg/workflows"
)

func TestSkillPackageUsesPinnedCaptureOnly(t *testing.T) {
	root := filepath.Join(t.TempDir(), "package")
	if err := Init(root, "demo", "0.2.2", ""); err != nil {
		t.Fatal(err)
	}
	captured, err := Capture(root)
	if err != nil {
		t.Fatal(err)
	}
	source, err := ResolveSkillPackage(t.Context(), workflows.SkillPackageReference{Root: root, Digest: captured.Digest(), Skill: "demo"})
	if err != nil {
		t.Fatal(err)
	}
	if source.SourceDigest != captured.Digest() || source.Entry != "skills/demo/SKILL.md" {
		t.Fatal(source)
	}
	if _, err := ResolveSkillPackage(t.Context(), workflows.SkillPackageReference{Root: root, Digest: "changed", Skill: "demo"}); err == nil {
		t.Fatal("changed package digest accepted")
	}
	if _, err := ResolveSkillPackage(t.Context(), workflows.SkillPackageReference{Root: ".", Digest: captured.Digest(), Skill: "demo"}); err == nil {
		t.Fatal("relative package root accepted")
	}
	if err = os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, entry := range source.Entries {
		if entry.Path == source.Entry {
			found = len(entry.Data) > 0
		}
	}
	if !found {
		t.Fatal("captured skill depends on removed package")
	}
	if _, err = ResolveSkillPackage(t.Context(), workflows.SkillPackageReference{Root: root, Digest: captured.Digest(), Skill: "demo"}); err == nil {
		t.Fatal("missing package silently rediscovered")
	}
}

func TestSkillPackageRejectsExternalDeclaredResources(t *testing.T) {
	root, _, _ := capturedFixture(t)
	captured, err := Capture(root)
	if err != nil {
		t.Fatal(err)
	}
	_, err = ResolveSkillPackage(t.Context(), workflows.SkillPackageReference{Root: root, Digest: captured.Digest(), Skill: "demo"})
	if err == nil || !strings.Contains(err.Error(), "resources outside the selected skill directory") {
		t.Fatalf("external package resource silently admitted: %v", err)
	}
}

func TestSkillPackageRejectsNestedAdditionalSkillEntry(t *testing.T) {
	root := filepath.Join(t.TempDir(), "package")
	if err := Init(root, "demo", "0.2.2", ""); err != nil {
		t.Fatal(err)
	}
	descriptor := filepath.Join(root, "plugin.yaml")
	data, err := os.ReadFile(descriptor)
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, []byte("\nresources: [skills/demo]\n")...)
	if err = os.WriteFile(descriptor, data, 0600); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(root, "skills/demo/nested/SKILL.md")
	if err = os.MkdirAll(filepath.Dir(nested), 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(nested, []byte("---\nname: nested\ndescription: Extra.\n---\nNESTED"), 0600); err != nil {
		t.Fatal(err)
	}
	captured, err := Capture(root)
	if err != nil {
		t.Fatal(err)
	}
	source, err := ResolveSkillPackage(t.Context(), workflows.SkillPackageReference{Root: root, Digest: captured.Digest(), Skill: "demo"})
	if err == nil || source != nil || !strings.Contains(err.Error(), "another SKILL.md") {
		t.Fatal("nested selected skill silently filtered", source, err, string(data))
	}
}
