// Package prfix materializes PR maintenance as ordinary workflows. Actions in this
// package implement individual operations; only YAML chooses execution order.
package prfix

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

const Version = "5.2.0"
const Implementation = "prfix-v3-dk-workflows-5.2.0"

type Identity struct {
	URL        string `json:"url"`
	Repository string `json:"repository"`
	Host       string `json:"host"`
	Owner      string `json:"owner"`
	Name       string `json:"name"`
	Number     int    `json:"number"`
}

func ParseIdentity(raw string) (Identity, error) {
	u, e := url.Parse(raw)
	if e != nil {
		return Identity{}, e
	}
	p := strings.Split(strings.Trim(u.Path, "/"), "/")
	if u.Scheme != "https" || !regexp.MustCompile(`^[A-Za-z0-9.-]+$`).MatchString(u.Hostname()) || u.Host != u.Hostname() || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.RawPath != "" || len(p) != 4 || p[2] != "pull" || p[0] == "." || p[0] == ".." || p[1] == "." || p[1] == ".." || !regexp.MustCompile(`^[A-Za-z0-9_.-]+$`).MatchString(p[0]) || !regexp.MustCompile(`^[A-Za-z0-9_.-]+$`).MatchString(p[1]) {
		return Identity{}, fmt.Errorf("canonical https PR URL required")
	}
	n, e := strconv.Atoi(p[3])
	if e != nil || n < 1 {
		return Identity{}, fmt.Errorf("positive PR number required")
	}
	repo := strings.ToLower(u.Host + "/" + p[0] + "/" + p[1])
	parts := strings.Split(repo, "/")
	return Identity{"https://" + repo + "/pull/" + strconv.Itoa(n), repo, parts[0], parts[1], parts[2], n}, nil
}
func digest(v any) string {
	b, e := json.Marshal(v)
	if e != nil {
		panic(e)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
func validText(s string) bool {
	return utf8.ValidString(s) && strings.TrimSpace(s) != "" && !strings.ContainsRune(s, 0)
}

type Limits struct {
	ReviewCycles int `json:"review_cycles"`
	Retries      int `json:"retries_per_task"`
	CIRepairs    int `json:"ci_repairs_per_cycle"`
	CIReruns     int `json:"ci_reruns_per_head"`
}

func (l Limits) Validate() error {
	if l.ReviewCycles < 1 || l.ReviewCycles > 5 || l.Retries < 0 || l.Retries > 1 || l.CIRepairs < 0 || l.CIRepairs > 5 || l.CIReruns < 0 || l.CIReruns > 3 {
		return fmt.Errorf("limits exceed dk-workflows 5.2.0 ceilings")
	}
	return nil
}

type Invocation struct {
	Skill        string         `json:"skill,omitempty" yaml:"skill,omitempty"`
	Instructions string         `json:"instructions,omitempty" yaml:"instructions,omitempty"`
	Options      map[string]any `json:"options,omitempty" yaml:"options,omitempty"`
	Required     *bool          `json:"required,omitempty" yaml:"required,omitempty"`
	Agent        map[string]any `json:"agent,omitempty" yaml:"agent,omitempty"`
}

func (i Invocation) IsRequired() bool { return i.Required == nil || *i.Required }

type Resolved struct {
	Invocation    Invocation     `json:"invocation"`
	Configuration *Configuration `json:"configuration"`
}
type Configuration struct {
	Options      map[string]any    `json:"options"`
	Sources      []string          `json:"sources"`
	CheckedPaths []string          `json:"checked_paths"`
	Provenance   map[string]string `json:"provenance"`
}
type Account struct {
	Host string `json:"host"`
	ID   int64  `json:"id"`
}
type Selection struct {
	Invocation  Invocation                         `json:"invocation"`
	Config      Configuration                      `json:"config"`
	Tasks       [][]Resolved                       `json:"tasks"`
	Finalizers  [][]Resolved                       `json:"finalizers"`
	Hooks       map[string][][]Resolved            `json:"hooks"`
	Children    map[string]map[string][][]Resolved `json:"children"`
	Accounts    []Account                          `json:"accounts"`
	Preferences map[string]any                     `json:"preferences"`
	Settings    map[string]any                     `json:"settings"`
}

func (s Selection) Limits() Limits {
	b, _ := json.Marshal(s.Config.Options["limits"])
	var l Limits
	_ = json.Unmarshal(b, &l)
	return l
}

type Snapshot struct {
	RepositoryInstructions    map[string]string `json:"repository_instruction_resources"`
	CompletedEffects          []string          `json:"completed_effects,omitempty"`
	ResourceDigests           map[string]string `json:"resource_digests"`
	RoleDigests               map[string]string `json:"role_digests"`
	ReviewPreferences         string            `json:"review_preferences"`
	ImplementationPreferences string            `json:"implementation_preferences"`
	Identity                  Identity          `json:"identity"`
	Author                    string            `json:"author"`
	Viewer                    string            `json:"viewer"`
	HeadRepository            string            `json:"head_repository"`
	HeadBranch                string            `json:"head_branch"`
	TargetBranch              string            `json:"target_branch"`
	Target                    string            `json:"target"`
	Base                      string            `json:"base"`
	Head                      string            `json:"head"`
	LocalHead                 string            `json:"local_head"`
	TargetTree                string            `json:"target_tree"`
	BaseTree                  string            `json:"base_tree"`
	HeadTree                  string            `json:"head_tree"`
	LocalTree                 string            `json:"local_tree"`
	Diff                      string            `json:"diff"`
	History                   string            `json:"history"`
	Discussion                string            `json:"discussion"`
	Decisions                 string            `json:"decisions"`
	Instructions              string            `json:"instructions"`
	Settings                  string            `json:"settings"`
	Resources                 string            `json:"resources"`
	Paths                     []string          `json:"paths"`
	Evidence                  string            `json:"evidence"`
	Open                      bool              `json:"open"`
	Draft                     bool              `json:"draft"`
}

func (s Snapshot) Fingerprint(history, head bool) string {
	m := map[string]any{"pr": s.Identity, "author": s.Author, "viewer": s.Viewer, "head_repository": s.HeadRepository, "head_branch": s.HeadBranch, "target_branch": s.TargetBranch, "target_tree": s.TargetTree, "base_tree": s.BaseTree, "tree": s.LocalTree, "diff": s.Diff, "discussion": s.Discussion, "decisions": s.Decisions, "instructions": s.Instructions, "settings": s.Settings, "resources": s.Resources}
	if history {
		m["history"] = s.History
	}
	if head {
		m["head"] = s.Head
		m["target"] = s.Target
		m["local_head"] = s.LocalHead
	}
	return digest(m)
}

type ChecklistItem struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}
type Check struct {
	ID       string   `json:"id"`
	Status   string   `json:"status"`
	Evidence []string `json:"evidence,omitempty"`
	Reason   string   `json:"reason,omitempty"`
}
type Finding struct {
	ID          string         `json:"id"`
	Text        string         `json:"text"`
	Sources     []string       `json:"sources"`
	Disposition string         `json:"disposition,omitempty"`
	Reason      string         `json:"reason,omitempty"`
	Acceptance  *Authorization `json:"acceptance,omitempty"`
}
type Authorization struct {
	RequestID   string `json:"request_id"`
	Instruction string `json:"instruction"`
	Source      string `json:"source"`
}

func (a Authorization) Validate() error {
	if !validText(a.RequestID) || !validText(a.Instruction) || !validText(a.Source) {
		return fmt.Errorf("stable request ID, exact user instruction and source required")
	}
	return nil
}

type PreferenceDecision struct {
	ID         string   `json:"id"`
	Applicable bool     `json:"applicable"`
	Paths      []string `json:"paths"`
	Reason     string   `json:"reason"`
	Sources    []string `json:"sources"`
}
type FileChange struct {
	Path   string `json:"path"`
	Before string `json:"before"`
	After  string `json:"after"`
}
type ScopeReport struct {
	AddedLines   int             `json:"added_lines"`
	Threshold    int             `json:"threshold"`
	EvacuateMode string          `json:"evacuate_unrelated"`
	RelatedMode  string          `json:"split_related"`
	Enabled      bool            `json:"enabled"`
	Assessment   string          `json:"assessment"`
	Boundaries   []SplitBoundary `json:"boundaries"`
	Evidence     []string        `json:"evidence"`
}
type SplitBoundary struct {
	Branch      string   `json:"branch"`
	Head        string   `json:"head"`
	Base        string   `json:"base"`
	Behavior    string   `json:"behavior"`
	Paths       []string `json:"paths"`
	Reason      string   `json:"reason"`
	Evidence    []string `json:"evidence"`
	RecoveryRef string   `json:"recovery_ref"`
}
type AudienceReport struct {
	Fingerprint string   `json:"fingerprint"`
	Audience    string   `json:"audience"`
	Suitable    bool     `json:"suitable"`
	Evidence    []string `json:"evidence"`
}
type ReplyObligation struct {
	ID        string   `json:"id"`
	Text      string   `json:"text"`
	Required  bool     `json:"required"`
	Reason    string   `json:"reason"`
	Sources   []string `json:"sources"`
	CommentID string   `json:"comment_id,omitempty"`
	ThreadID  string   `json:"thread_id,omitempty"`
}
type ReplyResponse struct {
	ID         string         `json:"id"`
	Outcome    string         `json:"outcome"`
	Reason     string         `json:"reason"`
	Sources    []string       `json:"sources"`
	Operation  string         `json:"operation,omitempty"`
	Acceptance *Authorization `json:"acceptance,omitempty"`
}
type Report struct {
	ReviewRequestDisposition *ReviewRequestDisposition `json:"review_request_disposition,omitempty"`
	PolicyPrompt             string                    `json:"policy_prompt,omitempty"`
	ReplyObligations         []ReplyObligation         `json:"reply_obligations,omitempty"`
	Responses                []ReplyResponse           `json:"responses,omitempty"`
	Audience                 *AudienceReport           `json:"audience,omitempty"`
	Scope                    *ScopeReport              `json:"scope,omitempty"`
	Changes                  []FileChange              `json:"changes,omitempty"`
	Deliverables             map[string]string         `json:"deliverables,omitempty"`
	PreferenceDecisions      []PreferenceDecision      `json:"preference_decisions,omitempty"`
	CommitMessage            string                    `json:"commit_message,omitempty"`
	Complete                 bool                      `json:"complete"`
	Findings                 []Finding                 `json:"findings"`
	Checklist                []Check                   `json:"checklist"`
	Evidence                 []string                  `json:"evidence"`
	Blockers                 []string                  `json:"blockers"`
	Operations               []Intent                  `json:"operations,omitempty"`
	NoReply                  []Finding                 `json:"no_reply,omitempty"`
	Cleanup                  *Cleanup                  `json:"cleanup,omitempty"`
	CI                       *CIReport                 `json:"ci,omitempty"`
}
type Cleanup struct {
	Outcome         string            `json:"outcome"`
	ReviewedHead    string            `json:"reviewed_head"`
	FinalHead       string            `json:"final_head"`
	FinalTree       string            `json:"final_tree"`
	Commits         map[string]string `json:"commits"`
	BoundaryReasons []string          `json:"boundary_reasons"`
	RecoveryRef     string            `json:"recovery_ref"`
	Validation      []string          `json:"validation"`
	Receipt         string            `json:"receipt,omitempty"`
}
type CIReport struct {
	RerunReason     string             `json:"rerun_reason,omitempty"`
	Repair          bool               `json:"repair,omitempty"`
	RepairReason    string             `json:"repair_reason,omitempty"`
	Classifications []CIClassification `json:"classifications,omitempty"`
	Head            string             `json:"head"`
	Status          string             `json:"status"`
	Checks          []string           `json:"checks"`
	Exceptions      []Finding          `json:"exceptions"`
	Evidence        []string           `json:"evidence"`
}

type Occurrence struct {
	InheritedResources    []string        `json:"inherited_resources,omitempty"`
	InheritedInstructions []string        `json:"inherited_invocation_instructions,omitempty"`
	PolicyProviders       []string        `json:"policy_providers,omitempty"`
	ID                    string          `json:"id"`
	Kind                  string          `json:"kind"`
	Call                  Resolved        `json:"call"`
	Adapter               Adapter         `json:"adapter"`
	Pass                  Pass            `json:"pass"`
	Required              bool            `json:"required"`
	Review                bool            `json:"review"`
	History               bool            `json:"history"`
	HeadSensitive         bool            `json:"head_sensitive"`
	Cacheable             bool            `json:"cacheable"`
	Checklist             []ChecklistItem `json:"checklist"`
	MaxChecks             int             `json:"max_checks"`
}
type Pass struct {
	ID         string   `json:"id" yaml:"id"`
	Role       string   `json:"role" yaml:"role"`
	Assignment string   `json:"assignment" yaml:"assignment"`
	Evidence   []string `json:"evidence" yaml:"evidence"`
}
type Adapter struct {
	Writes        []string `json:"writes,omitempty" yaml:"writes,omitempty"`
	Reads         []string `json:"reads,omitempty" yaml:"reads,omitempty"`
	Identity      string   `json:"identity" yaml:"identity"`
	Version       string   `json:"version" yaml:"version"`
	Kind          string   `json:"kind" yaml:"kind"`
	Procedure     [][]Pass `json:"procedure" yaml:"procedure"`
	Controls      []string `json:"controls" yaml:"controls"`
	Options       []string `json:"options" yaml:"options"`
	Resources     []string `json:"resources" yaml:"resources"`
	History       bool     `json:"history" yaml:"history"`
	HeadSensitive bool     `json:"head_sensitive" yaml:"head-sensitive"`
	Cacheable     bool     `json:"cacheable" yaml:"cacheable"`
}

type Plan struct {
	Version        string                   `json:"version"`
	LogicalRun     string                   `json:"logical_run"`
	Identity       Identity                 `json:"identity"`
	Selection      Selection                `json:"selection"`
	Snapshot       Snapshot                 `json:"snapshot"`
	Instructions   string                   `json:"instructions"`
	MaintainOther  *Authorization           `json:"maintain_other,omitempty"`
	Authorizations map[string]Authorization `json:"authorizations"`
	Roles          json.RawMessage          `json:"roles"`
	Occurrences    []Occurrence             `json:"occurrences"`
	Groups         map[string][][]string    `json:"groups"`
	Omitted        []string                 `json:"omitted"`
	Sources        map[string]string        `json:"sources"`
	Options        map[string]any           `json:"options"`
	Upstream       string                   `json:"upstream"`
	Adapters       map[string]Adapter       `json:"adapters"`
	ConfigPath     string                   `json:"config_path"`
	Ref            string                   `json:"ref"`
	Fingerprint    string                   `json:"fingerprint"`
}

func occurrenceFingerprint(s Snapshot, o Occurrence) string {
	prefs := ""
	if o.Review || o.Kind == "verification" {
		prefs = s.ReviewPreferences
	}
	if mutates(o) {
		prefs = s.ImplementationPreferences
	}
	resources := map[string]string{}
	for _, path := range o.resourcePaths() {
		resources[path] = s.ResourceDigests[path]
	}
	return digest([]any{s.Fingerprint(o.History, o.HeadSensitive), prefs, o, resources, s.RoleDigests[o.Pass.Role]})
}

func (o Occurrence) resourcePaths() []string {
	paths := append([]string{}, o.InheritedResources...)
	return append(paths, o.Adapter.Resources...)
}
