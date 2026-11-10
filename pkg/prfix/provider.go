package prfix

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// Provider methods perform one observation or one external operation. They must
// never decide which workflow stage runs next.
type Provider interface {
	Capture(context.Context, Identity) (PREvidence, error)
	ObserveCI(context.Context, Identity, string) (CIObservation, error)
	Execute(context.Context, Intent) (Receipt, error)
	Reconcile(context.Context, Intent) (string, Receipt, error)
}
type PREvidence struct {
	Metadata PRMetadata       `json:"metadata"`
	Viewer   string           `json:"viewer"`
	ViewerID int64            `json:"viewer_id"`
	Comments []map[string]any `json:"comments"`
	Reviews  []map[string]any `json:"reviews"`
	Inline   []map[string]any `json:"inline"`
	Threads  []map[string]any `json:"threads"`
}
type PRRef struct {
	SHA  string `json:"sha"`
	Ref  string `json:"ref"`
	Repo struct {
		FullName string `json:"full_name"`
		CloneURL string `json:"clone_url"`
		HTMLURL  string `json:"html_url"`
	} `json:"repo"`
}
type PRMetadata struct {
	URL         string `json:"html_url"`
	NodeID      string `json:"node_id"`
	Number      int    `json:"number"`
	State       string `json:"state"`
	Draft       bool   `json:"draft"`
	Merged      bool   `json:"merged"`
	MergeCommit string `json:"merge_commit_sha"`
	MergedBy    struct {
		ID int64 `json:"id"`
	} `json:"merged_by"`
	Title   string `json:"title"`
	Body    string `json:"body"`
	Updated string `json:"updated_at"`
	User    struct {
		ID    int64  `json:"id"`
		Login string `json:"login"`
	} `json:"user"`
	Head           PRRef  `json:"head"`
	Base           PRRef  `json:"base"`
	MergeableState string `json:"mergeable_state"`
}
type CIObservation struct {
	Verified []CIProof         `json:"verified,omitempty"`
	Logs     map[string]string `json:"logs,omitempty"`
	Head     string            `json:"head"`
	Checks   []CICheck         `json:"checks"`
	Evidence string            `json:"evidence"`
}
type CICheck struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	URL        string `json:"url"`
	Attempt    int    `json:"attempt"`
}

func (c CIObservation) Success() bool {
	if len(c.Checks) == 0 {
		return false
	}
	for _, v := range c.Checks {
		if v.Status != "completed" {
			return false
		}
		if contains([]string{"success", "neutral", "skipped"}, v.Conclusion) {
			continue
		}
		exempt := false
		for _, proof := range c.Verified {
			if proof.Head == c.Head && proof.CheckID == v.ID && proof.Exempt && len(proof.Evidence) > 0 {
				exempt = true
			}
		}
		if !exempt {
			return false
		}
	}

	return true
}

type Intent struct {
	Replaces       *ReplyReplacement `json:"replaces,omitempty"`
	CreatedHead    string            `json:"created_head,omitempty"`
	ActorID        int64             `json:"actor_id,omitempty"`
	Before         map[string]any    `json:"before,omitempty"`
	Discussion     string            `json:"discussion,omitempty"`
	ID             string            `json:"id"`
	Run            string            `json:"run"`
	Kind           string            `json:"kind"`
	PR             Identity          `json:"pr"`
	Head           string            `json:"head"`
	Lease          string            `json:"lease"`
	Target         string            `json:"target"`
	Branch         string            `json:"branch"`
	HeadRepository string            `json:"head_repository"`
	Payload        map[string]any    `json:"payload"`
	Authorization  Authorization     `json:"authorization"`
	Sources        []string          `json:"sources"`
	AudienceClaim  string            `json:"audience_claim,omitempty"`
}

func (i Intent) Fingerprint() string { i.AudienceClaim = ""; return digest(i) }

type Receipt struct {
	ID       string         `json:"id"`
	Kind     string         `json:"kind"`
	PR       string         `json:"pr"`
	Head     string         `json:"head"`
	Result   map[string]any `json:"result"`
	Evidence []string       `json:"evidence"`
}

type GitHub struct{ Root string }

func (g *GitHub) api(ctx context.Context, id Identity, method, path string, payload any, out any) error {
	args := []string{"api", "--hostname", id.Host, "--method", method, path}
	var b []byte
	var e error
	if payload != nil {
		b, e = json.Marshal(payload)
		if e != nil {
			return e
		}
		args = append(args, "--input", "-")
	}
	raw, e := runCommand(ctx, g.Root, b, "gh", args...)
	if e != nil {
		return e
	}
	if out == nil {
		return nil
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		raw = []byte(`{}`)
	}
	return decodeGitHubJSON(raw, out)
}
func (g *GitHub) pages(ctx context.Context, id Identity, path string) ([]map[string]any, error) {
	raw, e := runCommand(ctx, g.Root, nil, "gh", "api", "--hostname", id.Host, "--paginate", "--slurp", path)
	if e != nil {
		return nil, e
	}
	var pages [][]map[string]any
	if e = decodeGitHubJSON(raw, &pages); e != nil {
		return nil, e
	}
	all := []map[string]any{}
	for _, p := range pages {
		all = append(all, p...)
	}
	return all, nil
}
func prPath(id Identity) string {
	return "repos/" + id.Owner + "/" + id.Name + "/pulls/" + strconv.Itoa(id.Number)
}
func issuePath(id Identity) string {
	return "repos/" + id.Owner + "/" + id.Name + "/issues/" + strconv.Itoa(id.Number)
}
func (g *GitHub) Capture(ctx context.Context, id Identity) (PREvidence, error) {
	var ev PREvidence
	if e := g.api(ctx, id, "GET", prPath(id), nil, &ev.Metadata); e != nil {
		return ev, e
	}
	canon, e := ParseIdentity(ev.Metadata.URL)
	if e != nil || canon != id || ev.Metadata.Number != id.Number {
		return ev, fmt.Errorf("GitHub returned a different PR identity")
	}
	var viewer struct {
		ID    int64  `json:"id"`
		Login string `json:"login"`
	}
	if e = g.api(ctx, id, "GET", "user", nil, &viewer); e != nil {
		return ev, e
	}
	ev.Viewer = viewer.Login
	ev.ViewerID = viewer.ID
	if ev.Comments, e = g.pages(ctx, id, issuePath(id)+"/comments?per_page=100"); e != nil {
		return ev, e
	}
	if ev.Reviews, e = g.pages(ctx, id, prPath(id)+"/reviews?per_page=100"); e != nil {
		return ev, e
	}
	if ev.Inline, e = g.pages(ctx, id, prPath(id)+"/comments?per_page=100"); e != nil {
		return ev, e
	}
	cursor := ""
	for {
		query := `query($owner:String!,$name:String!,$number:Int!,$cursor:String){repository(owner:$owner,name:$name){pullRequest(number:$number){reviewThreads(first:100,after:$cursor){totalCount nodes{id isResolved isOutdated comments(first:1){nodes{id databaseId}}} pageInfo{hasNextPage endCursor}}}}}`
		var cursorValue any
		if cursor != "" {
			cursorValue = cursor
		}
		request := map[string]any{"query": query, "variables": map[string]any{"owner": id.Owner, "name": id.Name, "number": id.Number, "cursor": cursorValue}}
		var response struct {
			Errors []any `json:"errors"`
			Data   struct {
				Repository struct {
					PullRequest struct {
						Threads struct {
							Total    *int             `json:"totalCount"`
							Nodes    []map[string]any `json:"nodes"`
							PageInfo struct {
								HasNext bool   `json:"hasNextPage"`
								End     string `json:"endCursor"`
							} `json:"pageInfo"`
						} `json:"reviewThreads"`
					} `json:"pullRequest"`
				} `json:"repository"`
			} `json:"data"`
		}
		if e = g.api(ctx, id, "POST", "graphql", request, &response); e != nil {
			return ev, e
		}
		if len(response.Errors) > 0 {
			return ev, fmt.Errorf("complete discussion unavailable: GraphQL errors")
		}
		t := response.Data.Repository.PullRequest.Threads
		if t.Total == nil {
			return ev, fmt.Errorf("complete thread enumeration unavailable")
		}
		ev.Threads = append(ev.Threads, t.Nodes...)
		if !t.PageInfo.HasNext && len(ev.Threads) != *t.Total {
			return ev, fmt.Errorf("incomplete thread enumeration")
		}
		if !t.PageInfo.HasNext {
			break
		}
		if t.PageInfo.End == "" || t.PageInfo.End == cursor {
			return ev, fmt.Errorf("discussion pagination did not advance")
		}
		cursor = t.PageInfo.End
	}
	// Detect concurrent PR changes across pagination; discussion contents themselves
	// are hashed and captured again at subsequent boundaries, including completion.
	var after PRMetadata
	if e = g.api(ctx, id, "GET", prPath(id), nil, &after); e != nil {
		return ev, e
	}
	if after.Head.SHA != ev.Metadata.Head.SHA || after.Base.SHA != ev.Metadata.Base.SHA || after.Updated != ev.Metadata.Updated {
		return ev, fmt.Errorf("PR changed while capturing complete discussion; retry read")
	}
	return ev, nil
}
func (g *GitHub) ObserveCI(ctx context.Context, id Identity, head string) (CIObservation, error) {
	obs := CIObservation{Head: head, Checks: []CICheck{}, Logs: map[string]string{}}
	prefix := "repos/" + id.Owner + "/" + id.Name + "/commits/" + head
	raw, e := runCommand(ctx, g.Root, nil, "gh", "api", "--hostname", id.Host, "--paginate", "--slurp", prefix+"/check-runs?filter=latest&per_page=100")
	if e != nil {
		return obs, e
	}
	var pages []struct {
		Checks []struct {
			ID         int64  `json:"id"`
			Name       string `json:"name"`
			Status     string `json:"status"`
			Conclusion string `json:"conclusion"`
			Head       string `json:"head_sha"`
			URL        string `json:"html_url"`
			Details    string `json:"details_url"`
		} `json:"check_runs"`
	}
	if e = decodeGitHubJSON(raw, &pages); e != nil {
		return obs, e
	}
	for _, p := range pages {
		for _, c := range p.Checks {
			if c.Head != head {
				return obs, fmt.Errorf("CI returned stale head")
			}
			check := CICheck{strconv.FormatInt(c.ID, 10), c.Name, c.Status, c.Conclusion, c.URL, 0}
			if c.Status == "completed" && !contains([]string{"success", "neutral", "skipped"}, c.Conclusion) {
				details := c.Details
				if details == "" {
					details = c.URL
				}
				u, err := url.Parse(details)
				parts := []string{}
				if err == nil {
					parts = strings.Split(strings.Trim(u.Path, "/"), "/")
				}
				if err == nil && u.Host == id.Host && len(parts) == 7 && parts[0] == id.Owner && parts[1] == id.Name && parts[2] == "actions" && parts[3] == "runs" && numeric(parts[4]) && parts[5] == "job" && numeric(parts[6]) {
					var job map[string]any
					if err = g.api(ctx, id, "GET", "repos/"+id.Owner+"/"+id.Name+"/actions/jobs/"+parts[6], nil, &job); err != nil {
						return obs, err
					}
					attempt, err := strconv.Atoi(githubID(job, "run_attempt"))
					if err != nil {
						return obs, fmt.Errorf("invalid exact Actions attempt: %w", err)
					}
					check.Attempt = attempt
					if stringValue(job, "head_sha") != head || !strings.HasSuffix(stringValue(job, "check_run_url"), "/check-runs/"+check.ID) {
						return obs, fmt.Errorf("Actions failure logs do not match exact head/check")
					}
					logs, err := runCommand(ctx, g.Root, nil, "gh", "api", "--hostname", id.Host, "repos/"+id.Owner+"/"+id.Name+"/actions/jobs/"+parts[6]+"/logs")
					if err != nil {
						return obs, fmt.Errorf("exact failure logs unavailable: %w", err)
					}
					path, err := artifact(g.Root, map[string]any{"head": head, "check": check, "job": job, "logs": string(logs)})
					if err != nil {
						return obs, err
					}
					obs.Logs[check.ID] = path
				}
			}
			obs.Checks = append(obs.Checks, check)
		}
	}
	statuses, e := g.pages(ctx, id, prefix+"/statuses?per_page=100")
	if e != nil {
		return obs, e
	}
	seen := map[string]bool{}
	for _, s := range statuses {
		n := stringValue(s, "context")
		if seen[n] {
			continue
		}
		seen[n] = true
		state := stringValue(s, "state")
		status := "completed"
		conclusion := state
		if state == "pending" {
			status = "pending"
		}
		obs.Checks = append(obs.Checks, CICheck{githubID(s, "id"), n, status, conclusion, stringValue(s, "target_url"), 0})
	}
	obs.Evidence, e = artifact(g.Root, map[string]any{"head": head, "checks": pages, "statuses": statuses})
	return obs, e
}
func stringValue(m map[string]any, key string) string { v, _ := m[key].(string); return v }
func (g *GitHub) Execute(ctx context.Context, i Intent) (Receipt, error) {
	if i.Kind == "request_review" {
		state, receipt, err := g.Reconcile(ctx, i)
		if err != nil {
			return Receipt{}, err
		}
		if state == "completed" {
			receipt.Evidence = append(receipt.Evidence, "requested set already satisfied; no write")
			return receipt, nil
		}
	}
	var raw map[string]any
	id := i.PR
	method, path := "POST", ""
	payload := i.Payload
	switch i.Kind {
	case "edit_pr":
		method = "PATCH"
		path = prPath(id)
	case "reply":
		path = issuePath(id) + "/comments"
	case "reply_inline":
		path = prPath(id) + "/comments/" + stringValue(payload, "comment_id") + "/replies"
		payload = map[string]any{"body": payload["body"]}
	case "resolve_thread":
		path = "graphql"
		payload = map[string]any{"query": `mutation($id:ID!){resolveReviewThread(input:{threadId:$id}){thread{id isResolved}}}`, "variables": map[string]any{"id": payload["thread_id"]}}
	case "split_pr":
		path = "repos/" + id.Owner + "/" + id.Name + "/pulls"
		payload = map[string]any{}
		for k, v := range i.Payload {
			if k != "push_operation" {
				payload[k] = v
			}
		}
	case "request_review":
		path = prPath(id) + "/requested_reviewers"
	case "ci_rerun":
		if _, e := rerunAttempt(i.Payload); e != nil {
			return Receipt{}, e
		}
		path = "repos/" + id.Owner + "/" + id.Name + "/actions/runs/" + stringValue(payload, "run_id") + "/rerun-failed-jobs"
		payload = nil
	case "merge":
		method = "PUT"
		path = prPath(id) + "/merge"
	default:
		return Receipt{}, fmt.Errorf("unsupported GitHub operation %q", i.Kind)
	}
	if e := g.api(ctx, id, method, path, payload, &raw); e != nil {
		// Endpoints returning 204 have an empty body; api handles those below.
		return Receipt{}, e
	}
	if errs, ok := raw["errors"].([]any); ok && len(errs) > 0 {
		return Receipt{}, fmt.Errorf("GitHub mutation returned GraphQL errors")
	}
	if i.Kind == "merge" {
		merged, _ := raw["merged"].(bool)
		if !merged {
			return Receipt{}, fmt.Errorf("GitHub did not merge the PR: %v", raw["message"])
		}
	}
	outcome, receipt, e := g.Reconcile(ctx, i)
	if e != nil {
		return Receipt{}, e
	}
	if outcome != "completed" {
		return Receipt{}, fmt.Errorf("GitHub accepted the request but its effect is not yet verified")
	}
	return receipt, nil
}
func (g *GitHub) Reconcile(ctx context.Context, i Intent) (string, Receipt, error) {
	ev, e := g.Capture(ctx, i.PR)
	if e != nil {
		return "unknown", Receipt{}, e
	}
	found := false
	absent := false
	var result any = ev.Metadata
	switch i.Kind {
	case "edit_pr":
		found = true
		for _, key := range []string{"title", "body"} {
			if v, ok := i.Payload[key]; ok {
				actual := ev.Metadata.Body
				if key == "title" {
					actual = ev.Metadata.Title
				}
				found = found && actual == v
			}
		}
		absent = !found && ev.Metadata.Head.SHA == i.Lease && len(i.Before) > 0
		for key, value := range i.Before {
			actual := ev.Metadata.Body
			if key == "title" {
				actual = ev.Metadata.Title
			}
			absent = absent && actual == value
		}
	case "reply", "reply_inline":
		before, ok := i.Before["comment_ids"]
		if !ok || i.ActorID <= 0 || ev.ViewerID != i.ActorID {
			return "unknown", Receipt{}, fmt.Errorf("exact creation actor and comment baseline required")
		}
		baseline := stringList(before)
		comments := ev.Comments
		if i.Kind == "reply_inline" {
			comments = ev.Inline
		}
		matches := []map[string]any{}
		for _, c := range comments {
			user, _ := c["user"].(map[string]any)
			if githubID(c, "id") != "" && !contains(baseline, githubID(c, "id")) && stringValue(c, "body") == stringValue(i.Payload, "body") && githubID(user, "id") == strconv.FormatInt(i.ActorID, 10) && stringValue(user, "login") == ev.Viewer && (i.Kind != "reply_inline" || githubID(c, "in_reply_to_id") == stringValue(i.Payload, "comment_id")) {
				matches = append(matches, c)
			}
		}
		if len(matches) == 1 {
			found = true
			result = matches[0]
		}
		// Missing or ambiguous post-baseline creations remain uncertain; never resend.

	case "resolve_thread":
		l, err := openLedgerReadOnly(ctx, g.Root)
		if err != nil {
			return "unknown", Receipt{}, err
		}
		reply, receipt, err := completedReply(ctx, l, i.Run, stringValue(i.Payload, "reply_operation"))
		l.Close()
		if err == nil {
			err = verifyThreadAnswer(ev, i, reply, receipt)
		}
		if err != nil {
			return "unknown", Receipt{}, err
		}
		for _, t := range ev.Threads {
			if stringValue(t, "id") == stringValue(i.Payload, "thread_id") {
				found, _ = t["isResolved"].(bool)
				absent = !found
				result = t
			}
		}
	case "merge":
		if ev.Metadata.Merged && ev.Metadata.State == "closed" && ev.Metadata.Head.SHA == i.Head && i.ActorID > 0 && ev.Metadata.MergedBy.ID == i.ActorID && validSHA(ev.Metadata.MergeCommit) {
			var commit struct {
				SHA     string `json:"sha"`
				Parents []struct {
					SHA string `json:"sha"`
				} `json:"parents"`
			}
			if e = g.api(ctx, i.PR, "GET", "repos/"+i.PR.Owner+"/"+i.PR.Name+"/git/commits/"+ev.Metadata.MergeCommit, nil, &commit); e != nil {
				return "unknown", Receipt{}, e
			}
			parents := []string{}
			for _, parent := range commit.Parents {
				parents = append(parents, parent.SHA)
			}
			wanted := []string{i.Target}
			if stringValue(i.Payload, "merge_method") == "merge" {
				wanted = append(wanted, i.Head)
			}
			found = contains([]string{"merge", "squash"}, stringValue(i.Payload, "merge_method")) && commit.SHA == ev.Metadata.MergeCommit && digest(parents) == digest(wanted)
			result = map[string]any{"pr": ev.Metadata, "commit": commit, "actor_id": i.ActorID, "target": i.Target}
		}
		absent = !found && ev.Metadata.State == "open" && !ev.Metadata.Merged && ev.Metadata.Head.SHA == i.Head && ev.Metadata.Base.SHA == i.Target
	case "split_pr":
		prs, err := g.pages(ctx, i.PR, "repos/"+i.PR.Owner+"/"+i.PR.Name+"/pulls?state=all&per_page=100")
		if err != nil {
			return "unknown", Receipt{}, err
		}
		before, ok := i.Before["pr_numbers"]
		if !ok {
			return "unknown", Receipt{}, fmt.Errorf("complete split creation baseline required")
		}
		baseline := stringList(before)
		matches := []map[string]any{}
		for _, pr := range prs {
			if !contains(baseline, githubID(pr, "number")) && stringValue(pr, "body") == stringValue(i.Payload, "body") {
				matches = append(matches, pr)
			}
		}
		if len(matches) == 1 {
			pr := matches[0]
			head, _ := pr["head"].(map[string]any)
			base, _ := pr["base"].(map[string]any)
			user, _ := pr["user"].(map[string]any)
			headRepo, _ := head["repo"].(map[string]any)
			baseRepo, _ := base["repo"].(map[string]any)
			wantedHead := stringValue(i.Payload, "head")
			parts := strings.SplitN(wantedHead, ":", 2)
			sourceBranch := wantedHead
			if len(parts) == 2 {
				sourceBranch = parts[1]
			}
			found = i.ActorID > 0 && githubID(user, "id") == strconv.FormatInt(i.ActorID, 10) && stringValue(user, "login") == ev.Viewer && stringValue(pr, "state") == "open" && pr["draft"] == true && stringValue(pr, "title") == stringValue(i.Payload, "title") && stringValue(pr, "body") == stringValue(i.Payload, "body") && stringValue(head, "sha") == i.CreatedHead && stringValue(head, "ref") == sourceBranch && strings.EqualFold(stringValue(headRepo, "full_name"), strings.TrimPrefix(i.HeadRepository, i.PR.Host+"/")) && stringValue(base, "sha") == i.Target && stringValue(base, "ref") == stringValue(i.Payload, "base") && strings.EqualFold(stringValue(baseRepo, "full_name"), i.PR.Owner+"/"+i.PR.Name)
			if len(parts) == 2 {
				found = found && strings.EqualFold(parts[0], strings.SplitN(stringValue(headRepo, "full_name"), "/", 2)[0])
			}
			result = pr
		}
		// A missing PR cannot prove that a creation was never dispatched.
	case "request_review":
		var requested struct {
			Users []struct {
				Login string `json:"login"`
			} `json:"users"`
			Teams []struct {
				Slug string `json:"slug"`
			} `json:"teams"`
		}
		if e = g.api(ctx, i.PR, "GET", prPath(i.PR)+"/requested_reviewers", nil, &requested); e != nil {
			return "unknown", Receipt{}, e
		}
		wanted := stringList(i.Payload["reviewers"])
		teams := stringList(i.Payload["team_reviewers"])
		found = len(wanted)+len(teams) > 0
		for _, w := range wanted {
			has := false
			for _, u := range requested.Users {
				has = has || u.Login == w
			}
			found = found && has
		}
		for _, w := range teams {
			has := false
			for _, t := range requested.Teams {
				has = has || t.Slug == w
			}
			found = found && has
		}
		result = requested
	case "ci_rerun":
		var run map[string]any
		if e = g.api(ctx, i.PR, "GET", "repos/"+i.PR.Owner+"/"+i.PR.Name+"/actions/runs/"+stringValue(i.Payload, "run_id"), nil, &run); e != nil {
			return "unknown", Receipt{}, e
		}
		attempt, err := strconv.ParseInt(githubID(run, "run_attempt"), 10, 64)
		if err != nil {
			return "unknown", Receipt{}, fmt.Errorf("invalid observed Actions attempt: %w", err)
		}
		previous, err := strconv.ParseInt(githubID(i.Payload, "attempt"), 10, 64)
		if err != nil {
			return "unknown", Receipt{}, fmt.Errorf("invalid retained Actions attempt: %w", err)
		}
		found = stringValue(run, "head_sha") == i.Head && attempt == previous+1
		result = run
	default:
		return "unknown", Receipt{}, fmt.Errorf("unsupported reconciliation %s", i.Kind)
	}
	receipt := Receipt{i.ID, i.Kind, i.PR.URL, i.Head, githubObject(result), []string{"read-only GitHub reconciliation"}}
	if found {
		return "completed", receipt, nil
	}
	if absent {
		return "no_effect", receipt, nil
	}
	return "unknown", receipt, nil
}

// PullRequestInventory supplies a complete private baseline before split writes.
type PullRequestInventory interface {
	PullRequestNumbers(context.Context, Identity) ([]string, error)
}

func (g *GitHub) PullRequestNumbers(ctx context.Context, id Identity) ([]string, error) {
	prs, e := g.pages(ctx, id, "repos/"+id.Owner+"/"+id.Name+"/pulls?state=all&per_page=100")
	if e != nil {
		return nil, e
	}
	numbers := []string{}
	for _, pr := range prs {
		number := githubID(pr, "number")
		if !numeric(number) {
			return nil, fmt.Errorf("complete numeric PR baseline required")
		}
		numbers = append(numbers, number)
	}
	if !contains(numbers, strconv.Itoa(id.Number)) {
		return nil, fmt.Errorf("split creation baseline omitted the canonical PR")
	}
	return numbers, nil
}

func stringList(value any) []string {
	b := jsonBytes(value)
	var out []string
	_ = json.Unmarshal(b, &out)
	return out
}

type MergeProof struct {
	Head      string
	Satisfied bool
	Evidence  []string
}
type MergeReadiness interface {
	MergeReady(context.Context, Identity, string) (MergeProof, error)
}

func (g *GitHub) MergeReady(ctx context.Context, id Identity, head string) (MergeProof, error) {
	query := `query($owner:String!,$name:String!,$number:Int!){repository(owner:$owner,name:$name){pullRequest(number:$number){headRefOid isDraft mergeable mergeStateStatus reviewDecision}}}`
	request := map[string]any{"query": query, "variables": map[string]any{"owner": id.Owner, "name": id.Name, "number": id.Number}}
	var response struct {
		Errors []any `json:"errors"`
		Data   struct {
			Repository struct {
				PR struct {
					Head      string `json:"headRefOid"`
					Draft     bool   `json:"isDraft"`
					Mergeable string `json:"mergeable"`
					State     string `json:"mergeStateStatus"`
					Review    string `json:"reviewDecision"`
				} `json:"pullRequest"`
			} `json:"repository"`
		} `json:"data"`
	}
	if e := g.api(ctx, id, "POST", "graphql", request, &response); e != nil {
		return MergeProof{}, e
	}
	if len(response.Errors) > 0 {
		return MergeProof{}, fmt.Errorf("repository merge requirements unavailable")
	}
	pr := response.Data.Repository.PR
	var rest PRMetadata
	if e := g.api(ctx, id, "GET", prPath(id), nil, &rest); e != nil {
		return MergeProof{}, e
	}
	path, e := artifact(g.Root, map[string]any{"graphql": response, "rest": rest})
	return MergeProof{pr.Head, rest.Head.SHA == head && rest.State == "open" && !rest.Draft && rest.MergeableState == "clean" && pr.Head == head && !pr.Draft && pr.Mergeable == "MERGEABLE" && pr.State == "CLEAN" && (pr.Review == "" || pr.Review == "APPROVED"), []string{path}}, e
}
