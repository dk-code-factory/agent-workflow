package prfix

import "fmt"

// The feedback being answered can differ from the inline root used for routing.
type FeedbackSource struct {
	Kind     string `json:"kind"`
	ID       string `json:"id"`
	URL      string `json:"url"`
	ThreadID string `json:"thread_id,omitempty"`
}

func verifyFeedbackSource(i Intent, ev PREvidence) error {
	source := i.Source
	if source == nil || !numeric(source.ID) {
		return fmt.Errorf("structured original feedback provenance required for reply recovery")
	}
	var objects []map[string]any
	fragment := ""
	switch source.Kind {
	case "inline_comment":
		objects, fragment = ev.Inline, "discussion_r"
		if i.Kind != "reply_inline" || source.ThreadID == "" {
			return fmt.Errorf("original inline source and thread required")
		}
	case "review":
		objects, fragment = ev.Reviews, "pullrequestreview-"
		if i.Kind != "reply" {
			return fmt.Errorf("review body requires top-level reply destination")
		}
	case "issue_comment":
		objects, fragment = ev.Comments, "issuecomment-"
		if i.Kind != "reply" {
			return fmt.Errorf("issue comment requires top-level reply destination")
		}
	default:
		return fmt.Errorf("unsupported original feedback kind")
	}
	if source.URL != i.PR.URL+"#"+fragment+source.ID {
		return fmt.Errorf("exact original feedback URL required")
	}
	matches := 0
	for _, object := range objects {
		if githubID(object, "id") != source.ID {
			continue
		}
		if stringValue(object, "html_url") != source.URL {
			return fmt.Errorf("original feedback URL differs from current object")
		}
		if i.Kind == "reply_inline" && source.ID != stringValue(i.Payload, "comment_id") && githubID(object, "in_reply_to_id") != stringValue(i.Payload, "comment_id") {
			return fmt.Errorf("original feedback moved outside routing parent")
		}
		matches++
	}
	if matches != 1 {
		return fmt.Errorf("exact original feedback is unavailable or ambiguous")
	}
	if i.Kind == "reply_inline" {
		matches = 0
		for _, thread := range ev.Threads {
			if stringValue(thread, "id") != source.ThreadID {
				continue
			}
			comments, _ := thread["comments"].(map[string]any)
			nodes, _ := comments["nodes"].([]any)
			// The complete REST inventory above binds the feedback to its root.
			// Capture's first GraphQL comment binds that root to this thread.
			parent := false
			for _, raw := range nodes {
				node, _ := raw.(map[string]any)
				id := githubID(node, "databaseId")
				parent = parent || id == stringValue(i.Payload, "comment_id")
			}
			if parent {
				matches++
			}
		}
		if matches != 1 {
			return fmt.Errorf("original feedback and routing parent must remain in the original thread")
		}
	}
	return nil
}
