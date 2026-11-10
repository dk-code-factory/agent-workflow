package prfix

import (
	"context"
	"encoding/json"
)

func discussionDigest(ev PREvidence) string {
	return digest(map[string]any{"title": ev.Metadata.Title, "body": ev.Metadata.Body, "comments": ev.Comments, "reviews": ev.Reviews, "inline": ev.Inline, "threads": ev.Threads})
}

// Only exact reconciled application writes can explain drift between preparation
// and dispatch. Incoming comments, edits and reviews still invalidate the intent.
func (r *Runtime) ownDiscussionDelta(ctx context.Context, p Plan, before string, live Snapshot) (bool, error) {
	l, e := r.ledger()
	if e != nil {
		return false, e
	}
	defer l.Close()
	snapshots := []string{}
	rows, e := l.db.QueryContext(ctx, "SELECT snapshot FROM claims WHERE run=? ORDER BY rowid", p.LogicalRun)
	if e != nil {
		return false, e
	}
	for rows.Next() {
		var path string
		if e = rows.Scan(&path); e != nil {
			rows.Close()
			return false, e
		}
		snapshots = append(snapshots, path)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return false, e
	}
	original := ""
	known := []string{}
	if p.Snapshot.Discussion == before {
		original = p.Snapshot.Evidence
		known = p.Snapshot.CompletedEffects
	}
	// The latest matching capture distinguishes writes already observed by the
	// preparing claim from effects completed after that governing snapshot.
	for _, path := range snapshots {
		var snap Snapshot
		if e = readJSON(path, &snap); e != nil {
			return false, e
		}
		if snap.Discussion == before {
			original = snap.Evidence
			known = snap.CompletedEffects
		}
	}
	if original == "" {
		return false, nil
	}
	var old, now struct {
		PR PREvidence `json:"pr"`
	}
	old.PR, e = readPREvidence(original)
	if e != nil {
		return false, e
	}
	now.PR, e = readPREvidence(live.Evidence)
	if e != nil {
		return false, e
	}
	rows, e = l.db.QueryContext(ctx, "SELECT intent,receipt FROM effects WHERE run=? AND state='completed' ORDER BY rowid", p.LogicalRun)
	if e != nil {
		return false, e
	}
	type effect struct {
		intent  Intent
		receipt Receipt
	}
	effects := []effect{}
	for rows.Next() {
		var raw, path string
		if e = rows.Scan(&raw, &path); e != nil {
			rows.Close()
			return false, e
		}
		var item effect
		if e = jsonDecode([]byte(raw), &item.intent); e != nil {
			rows.Close()
			return false, e
		}
		if e = readJSON(path, &item.receipt); e != nil {
			rows.Close()
			return false, e
		}
		effects = append(effects, item)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return false, e
	}
	old.PR = jsonClone(old.PR)
	now.PR = jsonClone(now.PR)
	for n := len(effects) - 1; n >= 0; n-- {
		item := effects[n]
		i, receipt := item.intent, item.receipt
		if contains(known, i.ID) {
			continue
		}
		switch i.Kind {
		case "reply", "reply_inline":
			prior, current := old.PR.Comments, now.PR.Comments
			if i.Kind == "reply_inline" {
				prior, current = old.PR.Inline, now.PR.Inline
			}
			alreadyObserved := false
			for _, c := range prior {
				if githubID(c, "id") != "" && githubID(c, "id") == githubID(receipt.Result, "id") {
					alreadyObserved = true
				}
			}
			if alreadyObserved {
				continue
			}
			filtered := []map[string]any{}
			for _, c := range current {
				if githubID(c, "id") != "" && githubID(c, "id") == githubID(receipt.Result, "id") && stringValue(c, "body") == stringValue(i.Payload, "body") {
					continue
				}
				filtered = append(filtered, c)
			}
			if i.Kind == "reply_inline" {
				now.PR.Inline = filtered
			} else {
				now.PR.Comments = filtered
			}
		case "edit_pr":
			if value, ok := i.Payload["title"]; ok && old.PR.Metadata.Title == stringValue(i.Before, "title") && now.PR.Metadata.Title == value {
				now.PR.Metadata.Title = old.PR.Metadata.Title
			}
			if value, ok := i.Payload["body"]; ok && old.PR.Metadata.Body == stringValue(i.Before, "body") && now.PR.Metadata.Body == value {
				now.PR.Metadata.Body = old.PR.Metadata.Body
			}
		case "resolve_thread":
			for _, prior := range old.PR.Threads {
				if stringValue(prior, "id") != stringValue(i.Payload, "thread_id") || prior["isResolved"] != false {
					continue
				}
				for _, current := range now.PR.Threads {
					if stringValue(current, "id") == stringValue(prior, "id") && current["isResolved"] == true {
						current["isResolved"] = false
					}
				}
			}
		}
	}
	return discussionDigest(old.PR) == discussionDigest(now.PR), nil
}
func jsonClone[T any](value T) T {
	var out T
	if e := jsonDecode(jsonBytes(value), &out); e != nil {
		panic(e)
	}
	return out
}

func readPREvidence(path string) (PREvidence, error) {
	var artifact map[string]json.RawMessage
	if e := readJSON(path, &artifact); e != nil {
		return PREvidence{}, e
	}
	var ev PREvidence
	e := json.Unmarshal(artifact["pr"], &ev)
	return ev, e
}
