package workflows

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

const SkillAdmissionVersion = 1

// SkillSource owns captured bytes. Only fresh admission consumes source paths;
// provider sessions and recovery receive retained, verified assets instead.
type SkillSource struct {
	Name          string
	Entry         string
	SourceDigest  string
	ContentDigest string
	Entries       []AssetInput
}

type SkillPackageReference struct {
	Root   string `json:"root"`
	Digest string `json:"digest"`
	Skill  string `json:"skill"`
}

type SkillReference struct {
	Directory string                 `json:"directory,omitempty"`
	Package   *SkillPackageReference `json:"package,omitempty"`
}

type SkillBinding struct {
	Version      int          `json:"version"`
	Name         string       `json:"name"`
	Entry        string       `json:"entry"`
	SourceDigest string       `json:"source_digest"`
	Assets       *AssetBundle `json:"assets"`
}

// SkillInvocation is pinned before run admission. It never grants tools,
// credentials, publication authority or a different provider runtime.
type SkillInvocation struct {
	Children       []SkillChild `json:"children,omitempty"`
	EffectiveTools []string     `json:"effective_parent_tools,omitempty"`
	ResolvedEffort string       `json:"resolved_effort,omitempty"`
	// ContinuationRequiresEvidence pins profiles whose resumed process cannot
	// publish a fresh native-consumption event. Recovery must retain the original.
	ContinuationRequiresEvidence bool         `json:"continuation_requires_evidence,omitempty"`
	Version                      int          `json:"version"`
	ID                           string       `json:"id"`
	Binding                      SkillBinding `json:"binding"`
	Arguments                    string       `json:"arguments"`
	Resources                    []string     `json:"resources"`
	Model                        string       `json:"model"`
	Effort                       string       `json:"effort,omitempty"`
	Profile                      string       `json:"profile"`
}

type SkillSelection struct {
	Input     string   `json:"input"`
	Arguments string   `json:"arguments,omitempty"`
	Resources []string `json:"resources,omitempty"`
	Evidence  struct {
		Invocation string   `json:"invocation"`
		Children   []string `json:"children,omitempty"`
	} `json:"evidence"`
	Children []SkillChildSelection `json:"children,omitempty"`
}

type SkillTaskRunner interface {
	AdmitSkillTask(*SkillInvocation) (*SkillInvocation, error)
}

var skillNamePattern = regexp.MustCompile(`^[a-z][a-z0-9]*(?:-[a-z0-9]+)*$`)

func decodeSkillValue(value any, target any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(target); err != nil {
		return fmt.Errorf("invalid skill contract: %w", err)
	}
	return nil
}

func parseSkillSelection(value any) (*SkillSelection, error) {
	if value == nil {
		return nil, nil
	}
	var selection SkillSelection
	if err := decodeSkillValue(value, &selection); err != nil {
		return nil, err
	}
	if selection.Input == "" || strings.Contains(selection.Input, "${{") || len(selection.Input) > 128 {
		return nil, fmt.Errorf("skill requires an explicit declared input name")
	}
	if len(selection.Children) > 1 || len(selection.Evidence.Children) > 1 || len(selection.Children) != len(selection.Evidence.Children) {
		return nil, fmt.Errorf("skill requires exactly one declared and required child")
	}
	if selection.Evidence.Invocation != "required" {
		return nil, fmt.Errorf("skill evidence.invocation must be required")
	}
	if len(selection.Arguments) > 64<<10 || len(selection.Resources) > 128 {
		return nil, fmt.Errorf("skill arguments/resources exceed admission limits")
	}
	return &selection, nil
}

// ParseSkillMetadata deliberately accepts only portable metadata. Provider
// frontmatter that changes agents, hooks, tools or models cannot expand policy.
func ParseSkillMetadata(data []byte) (string, error) {
	if len(data) > 1<<20 || !bytes.HasPrefix(data, []byte("---\n")) {
		return "", fmt.Errorf("skill requires bounded YAML name/description frontmatter")
	}
	end := bytes.Index(data[4:], []byte("\n---\n"))
	if end < 0 {
		return "", fmt.Errorf("unterminated skill frontmatter")
	}
	var metadata struct {
		Name        string `yaml:"name"`
		Description string `yaml:"description"`
	}
	var node yaml.Node
	decoder := yaml.NewDecoder(bytes.NewReader(data[4 : 4+end]))
	if err := decoder.Decode(&node); err != nil {
		return "", err
	}
	if len(node.Content) != 1 || node.Content[0].Kind != yaml.MappingNode {
		return "", fmt.Errorf("skill metadata must be a mapping")
	}
	seen := map[string]bool{}
	for i := 0; i < len(node.Content[0].Content); i += 2 {
		k, v := node.Content[0].Content[i], node.Content[0].Content[i+1]
		if k.Kind != yaml.ScalarNode || seen[k.Value] || (k.Value != "name" && k.Value != "description") || v.Kind != yaml.ScalarNode || v.Tag != "!!str" {
			return "", fmt.Errorf("unsupported or duplicate skill metadata %q", k.Value)
		}
		seen[k.Value] = true
	}
	if err := node.Decode(&metadata); err != nil {
		return "", err
	}
	if !skillNamePattern.MatchString(metadata.Name) || len(metadata.Name) > 64 || strings.TrimSpace(metadata.Description) == "" {
		return "", fmt.Errorf("skill requires a portable name and description")
	}
	return metadata.Name, nil
}

// CaptureSkillDirectory captures a single explicitly supplied skill, without
// discovery, executing metadata or reading any installation settings.
func CaptureSkillDirectory(ctx context.Context, directory string) (*SkillSource, error) {
	if !filepath.IsAbs(directory) {
		return nil, fmt.Errorf("skill directory must be absolute")
	}
	for ancestor := filepath.Clean(directory); ; ancestor = filepath.Dir(ancestor) {
		info, err := os.Lstat(ancestor)
		if err != nil {
			return nil, err
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return nil, fmt.Errorf("skill source symlinks are unsupported")
		}
		if filepath.Dir(ancestor) == ancestor {
			break
		}
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	source := &SkillSource{}
	var total int64
	err = fs.WalkDir(root.FS(), ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if name == "." {
			return nil
		}
		if len(source.Entries) >= MaxAssetPaths {
			return fmt.Errorf("skill exceeds retained path limit")
		}
		if err := noSymlinkComponents(root, name); err != nil {
			return err
		}
		info, err := root.Lstat(name)
		if err != nil {
			return err
		}
		input := AssetInput{Path: name, Mode: info.Mode()}
		if info.IsDir() {
			source.Entries = append(source.Entries, input)
			return nil
		}
		if !info.Mode().IsRegular() || info.Size() > MaxHelperBytes-total {
			return fmt.Errorf("unsupported skill file or byte limit: %s", name)
		}
		file, err := openRegularAsset(root, name)
		if err != nil {
			return err
		}
		opened, err := file.Stat()
		if err != nil {
			file.Close()
			return err
		}
		data, readErr := io.ReadAll(io.LimitReader(file, MaxHelperBytes-total+1))
		after, statErr := file.Stat()
		closeErr := file.Close()
		if readErr != nil {
			return readErr
		}
		if statErr != nil {
			return statErr
		}
		if closeErr != nil {
			return closeErr
		}
		if !os.SameFile(info, opened) || opened.Size() != info.Size() || !opened.ModTime().Equal(info.ModTime()) || !after.ModTime().Equal(info.ModTime()) || int64(len(data)) != info.Size() {
			return fmt.Errorf("skill source changed during capture: %s", name)
		}
		if err := noSymlinkComponents(root, name); err != nil {
			return err
		}
		total += int64(len(data))
		input.Data = data
		source.Entries = append(source.Entries, input)
		return nil
	})
	if err != nil {
		return nil, err
	}
	for _, input := range source.Entries {
		if input.Path == "SKILL.md" {
			source.Name, err = ParseSkillMetadata(input.Data)
		}
	}
	if err != nil {
		return nil, err
	}
	if source.Name == "" {
		return nil, fmt.Errorf("skill directory requires SKILL.md")
	}
	prefix := "skills/" + source.Name + "/"
	for i := range source.Entries {
		source.Entries[i].Path = prefix + source.Entries[i].Path
	}
	source.Entry = prefix + "SKILL.md"
	source.SourceDigest, err = skillSourceDigest(source.Entries)
	source.ContentDigest = source.SourceDigest
	return source, err
}

// NewSkillSource binds an application's explicit package capture to its exact
// selected entry bytes. ProvenanceDigest remains the complete package identity.
// The returned source owns its slices; resolution never borrows mutable files.
func NewSkillSource(name, entry, provenanceDigest string, inputs []AssetInput) (*SkillSource, error) {
	source := &SkillSource{Name: name, Entry: entry, SourceDigest: provenanceDigest}
	for _, input := range inputs {
		input.Data = append([]byte(nil), input.Data...)
		source.Entries = append(source.Entries, input)
	}
	var err error
	source.ContentDigest, err = skillSourceDigest(source.Entries)
	if err != nil {
		return nil, err
	}
	if err := validateSkillSource(source); err != nil {
		return nil, err
	}
	return source, nil
}

func skillSourceDigest(inputs []AssetInput) (string, error) {
	entries := make([]AssetEntry, 0, len(inputs))
	for _, input := range inputs {
		entry := AssetEntry{Path: input.Path, Kind: "file", Mode: uint32(input.Mode.Perm()), Size: int64(len(input.Data)), SHA256: digestBytes(input.Data)}
		if input.Mode.IsDir() {
			entry.Kind, entry.Size, entry.SHA256 = "directory", 0, ""
		}
		entries = append(entries, entry)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	if err := ValidateAssetEntries(entries); err != nil {
		return "", err
	}
	return assetBundleID(entries), nil
}

// CaptureSkillInputs is a fresh-admission operation. The controller uses the
// same capture for idempotency comparison and Prepare; callers cannot substitute
// a mutable directory after comparison. Package resolution is application-owned.
func (e *Executor) CaptureSkillInputs(ctx context.Context, ref string, inputs map[string]any, pack *PackageSource) (map[string]*SkillSource, error) {
	var data []byte
	if pack != nil {
		for _, input := range pack.Entries {
			if input.Path == ref {
				data = input.Data
				break
			}
		}
	} else {
		resolved, err := (Resolver{WorkspaceDir: e.sourceRoot(), DefinitionsDir: e.DefinitionsDir}).ResolveLocal(ref)
		if err != nil {
			return nil, err
		}
		file, err := os.Open(resolved.Path)
		if err != nil {
			return nil, err
		}
		data, err = readDefinition(file)
		file.Close()
		if err != nil {
			return nil, err
		}
	}
	w, err := Parse(data)
	if err != nil {
		return nil, err
	}
	if err = Validate(w); err != nil {
		return nil, err
	}
	call := w.On.WorkflowCall
	if call != nil {
		copy := *call
		copy.Secrets = nil
		call = &copy
	}
	resolved, err := resolveInputs(call, inputs, nil)
	if err != nil {
		return nil, err
	}
	out := map[string]*SkillSource{}
	if w.On.WorkflowCall == nil {
		return out, nil
	}
	for name, spec := range w.On.WorkflowCall.Inputs {
		if spec.Type != "skill" {
			continue
		}
		value, ok := resolved[name]
		if !ok {
			continue
		}
		var reference SkillReference
		if err := decodeSkillValue(value, &reference); err != nil {
			return nil, err
		}
		if (reference.Directory == "") == (reference.Package == nil) {
			return nil, fmt.Errorf("skill input %s requires exactly one directory or pinned package", name)
		}
		var source *SkillSource
		if reference.Package != nil {
			if e.ResolveSkillPackage == nil {
				return nil, fmt.Errorf("skill package resolution is unavailable")
			}
			if !hexID(reference.Package.Digest) || len(reference.Package.Digest) != 64 {
				return nil, fmt.Errorf("skill package requires exact digest")
			}
			source, err = e.ResolveSkillPackage(ctx, *reference.Package)
		} else {
			source, err = CaptureSkillDirectory(ctx, reference.Directory)
		}
		if err != nil {
			return nil, fmt.Errorf("skill input %s: %w", name, err)
		}
		if err := validateSkillSource(source); err != nil {
			return nil, fmt.Errorf("skill input %s: %w", name, err)
		}
		out[name] = source
	}
	return out, nil
}

func SkillInputIdentities(sources map[string]*SkillSource) map[string]string {
	if len(sources) == 0 {
		return nil
	}
	out := map[string]string{}
	for name, source := range sources {
		if source == nil {
			out[name] = "invalid"
			continue
		}
		content, err := skillSourceDigest(source.Entries)
		if err != nil {
			out[name] = "invalid"
			continue
		}
		out[name] = actionHash([]any{source.Name, source.Entry, source.SourceDigest, content})
	}
	return out
}

func skillPlanID(plan *SkillInvocation) string {
	copy := *plan
	copy.ID = ""
	if copy.Binding.Assets != nil {
		bundle := *copy.Binding.Assets
		bundle.OriginID = ""
		copy.Binding.Assets = &bundle
	}
	return actionHash(copy)
}

func (e *Executor) admitSkills(ctx context.Context, all map[string]*Workflow, req *RunRequest) error {
	canonical, _ := CanonicalLocalRef(req.Ref)
	sources := req.SkillSources
	var err error
	if sources == nil {
		sources, err = e.CaptureSkillInputs(ctx, req.Ref, req.Inputs, req.Package)
		if err != nil {
			return err
		}
	}
	req.skillInputs = SkillInputIdentities(sources)
	// For now all skill controls must be resolvable from the entry workflow's
	// admitted inputs. Reusable runtime-dependent selections remain closed.
	for reference, w := range all {
		for id, job := range w.Jobs {
			job.Steps, err = mapSteps(job.Steps, "", func(step Step) (Step, error) {
				if step.With["skill"] == nil {
					return step, nil
				}
				if reference != canonical {
					return step, fmt.Errorf("skill selections in reusable workflows are not certified")
				}
				inputs, err := resolveInputs(w.On.WorkflowCall, req.Inputs, req.Secrets)
				if err != nil {
					return step, err
				}
				selection, err := parseSkillSelection(step.With["skill"])
				if err != nil {
					return step, err
				}
				if w.On.WorkflowCall == nil || w.On.WorkflowCall.Inputs[selection.Input].Type != "skill" {
					return step, fmt.Errorf("skill selection requires declared type: skill input %q", selection.Input)
				}
				source := sources[selection.Input]
				if source == nil {
					return step, fmt.Errorf("selected skill input %s is missing", selection.Input)
				}
				if err := validateSkillSource(source); err != nil {
					return step, err
				}
				if step.Definition == nil || len(step.Definitions) != 0 {
					return step, fmt.Errorf("skill task requires one admitted literal agent definition")
				}
				context := expressionContext{Inputs: inputs}
				arguments, err := admitSkillText(selection.Arguments, context)
				if err != nil {
					return step, err
				}
				// Only literal and inputs expressions are admitted; other expression roots
				// are unavailable in this context and must not become late control changes.
				if strings.Contains(arguments, "${{") {
					return step, fmt.Errorf("unresolved skill arguments")
				}
				selected := map[string]bool{source.Entry: true}
				resources := []string{}
				for _, resource := range selection.Resources {
					if strings.Contains(resource, "${{") {
						return step, fmt.Errorf("skill resources must be literal paths")
					}
					name, err := assetPath(resource)
					if err != nil || name == "." {
						return step, fmt.Errorf("invalid skill resource %q", resource)
					}
					name = path.Join(path.Dir(source.Entry), name)
					found := false
					for _, input := range source.Entries {
						if input.Path == name || strings.HasPrefix(input.Path, name+"/") {
							selected[input.Path] = true
							found = true
						}
					}
					if !found {
						return step, fmt.Errorf("undeclared skill resource %q", resource)
					}
					resources = append(resources, resource)
				}
				for _, input := range source.Entries {
					if secret, err := containsCredential(string(input.Data), e.redactions(req.Secrets)); err != nil || secret {
						return step, fmt.Errorf("skill source contains credential material")
					}
				}
				entries := []AssetInput{}
				for _, input := range source.Entries {
					if selected[input.Path] {
						entries = append(entries, input)
					}
				}
				store := invocationStore(e.Store)
				if store == nil {
					return step, fmt.Errorf("skill tasks require durable owned asset storage")
				}
				manifest, _ := json.Marshal(map[string]string{"name": "aw-" + source.Name, "version": "1.0.0"})
				entries = append(entries, AssetInput{Path: ".claude-plugin/plugin.json", Mode: 0644, Data: manifest})
				// Gate provider capabilities against the already captured bytes
				// before materializing their persistent occurrence projection.
				bundle, _, _, err := describeAssetInputs(ctx, e.bundleOrigin, entries)
				if err != nil {
					return step, err
				}
				plan := &SkillInvocation{Version: SkillAdmissionVersion, Binding: SkillBinding{Version: SkillAdmissionVersion, Name: source.Name, Entry: source.Entry, SourceDigest: source.SourceDigest, Assets: bundle}, Arguments: arguments, Resources: resources, Model: step.Definition.Model, Effort: step.Definition.ReasoningEffort}
				if value, ok := step.With["model"]; ok && value != nil {
					text, ok := value.(string)
					if !ok {
						return step, fmt.Errorf("skill model must be a string or null")
					}
					model, err := admitSkillText(text, context)
					if err != nil {
						return step, err
					}
					plan.Model = model
				}
				if strings.Contains(plan.Model, "${{") {
					return step, fmt.Errorf("unresolved skill model")
				}
				output, err := resolveNamedOutput(w, step.With["output"])
				if err != nil {
					return step, err
				}
				contract, err := ParseAgentOutputContract(output)
				if err != nil {
					return step, err
				}
				if contract != nil && contract.RepairAttempts > 0 {
					return step, fmt.Errorf("selected-skill output repairs are unverified; set output.repair-attempts: 0")
				}
				if err := admitSkillChildren(plan, selection, context); err != nil {
					return step, err
				}
				runner, ok := e.Agents.(SkillTaskRunner)
				if !ok {
					return step, fmt.Errorf("runtime does not support admitted skills")
				}
				plan, err = runner.AdmitSkillTask(plan)
				if err != nil {
					return step, err
				}
				if plan == nil {
					return step, fmt.Errorf("runtime returned no admitted skill contract")
				}
				retained, err := store.CaptureAssetBundle(ctx, e.bundleOrigin, entries)
				if err != nil {
					return step, err
				}
				if actionHash(retained) != actionHash(plan.Binding.Assets) {
					return step, fmt.Errorf("runtime changed admitted skill assets")
				}
				plan.Binding.Assets = retained
				plan.ID = skillPlanID(plan)
				if secret, err := containsCredential(plan, e.redactions(req.Secrets)); err != nil || secret {
					return step, fmt.Errorf("skill admission contains credential material")
				}
				step.With["model"] = plan.Model
				step.Skill = plan
				return step, nil
			})
			if err != nil {
				return fmt.Errorf("%s job %s: %w", reference, id, err)
			}
			w.Jobs[id] = job
		}
	}
	return nil
}

// Revalidate retained controls without consulting any mutable source. A changed
// adapter profile cannot silently rewrite a previously admitted invocation.
func (e *Executor) validateRetainedSkills(definitions map[string]*Workflow) error {
	for _, w := range definitions {
		for _, job := range w.Jobs {
			_, err := mapSteps(job.Steps, "", func(step Step) (Step, error) {
				if step.Skill == nil {
					return step, nil
				}
				plan := step.Skill
				if plan.ID == "" || plan.ID != skillPlanID(plan) || plan.Binding.Assets == nil {
					return step, fmt.Errorf("retained skill invocation identity changed")
				}
				runner, ok := e.Agents.(SkillTaskRunner)
				if !ok {
					return step, fmt.Errorf("runtime does not support retained skills")
				}
				admitted, err := runner.AdmitSkillTask(plan)
				if err != nil {
					return step, err
				}
				if admitted == nil || skillPlanID(admitted) != plan.ID || admitted.ID != plan.ID {
					return step, fmt.Errorf("retained skill capability profile changed")
				}
				return step, nil
			})
			if err != nil {
				return err
			}
		}
	}
	return nil
}

var skillInputExpression = regexp.MustCompile(`\$\{\{\s*inputs\.[A-Za-z_][A-Za-z0-9_-]*\s*\}\}`)

func admitSkillText(value string, ec expressionContext) (string, error) {
	for _, expression := range skillInputExpression.FindAllString(value, -1) {
		name := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(expression, "${{"), "}}"))
		name = strings.TrimPrefix(name, "inputs.")
		if _, ok := ec.Inputs[name]; !ok {
			return "", fmt.Errorf("skill control references missing admitted input %s", name)
		}
	}
	remainder := skillInputExpression.ReplaceAllString(value, "")
	if strings.Contains(remainder, "${{") {
		return "", fmt.Errorf("skill controls allow only admitted input expressions")
	}
	rendered, err := renderString(value, ec)
	if err != nil {
		return "", err
	}
	result, ok := rendered.(string)
	if !ok {
		return "", fmt.Errorf("skill control must resolve to a string")
	}
	if strings.Contains(result, "${{") {
		return "", fmt.Errorf("unresolved skill control expression")
	}
	if len(result) > 64<<10 {
		return "", fmt.Errorf("skill control exceeds 64 KiB")
	}
	return result, nil
}
func validateSkillSource(source *SkillSource) error {
	if source == nil || !skillNamePattern.MatchString(source.Name) || source.Entry != "skills/"+source.Name+"/SKILL.md" || len(source.SourceDigest) != 64 || !hexID(source.SourceDigest) {
		return fmt.Errorf("invalid captured skill source identity")
	}
	if content, err := skillSourceDigest(source.Entries); err != nil {
		return err
	} else if content != source.ContentDigest {
		return fmt.Errorf("captured skill content identity changed")
	}
	found := false
	for _, entry := range source.Entries {
		if path.Base(entry.Path) == "SKILL.md" && entry.Path != source.Entry {
			return fmt.Errorf("skill source contains another SKILL.md")
		}
		if entry.Path == source.Entry {
			name, err := ParseSkillMetadata(entry.Data)
			if err != nil {
				return err
			}
			if name != source.Name {
				return fmt.Errorf("skill name differs from captured metadata")
			}
			found = true
		}
	}
	if !found {
		return fmt.Errorf("captured skill entry is missing")
	}
	return nil
}
