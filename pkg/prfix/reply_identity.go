package prfix

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

func replyIdentity(i Intent) string {
	return digest([]any{i.PR.URL, i.Kind, i.ActorID, stringValue(i.Payload, "comment_id"), stringValue(i.Payload, "body"), i.Source})
}

// Aliases retain caller IDs while making a reply's source, actor, destination and
// exact text identify one effect. Transport pins and observation baselines belong to
// the original intent, including after a lost response or a later logical run.
func prepareReplyAlias(ctx context.Context, tx sqlRW, i Intent) (bool, error) {
	var proposal, state string
	err := tx.QueryRowContext(ctx, "SELECT a.proposal,e.state FROM effect_aliases a JOIN effects e ON e.run=a.canonical_run AND e.id=a.canonical_id WHERE a.run=? AND a.id=?", i.Run, i.ID).Scan(&proposal, &state)
	if err == nil {
		var old Intent
		if err = json.Unmarshal([]byte(proposal), &old); err != nil {
			return true, err
		}
		old.Sources = i.Sources
		if state == "completed" {
			old.Before, old.Discussion = i.Before, i.Discussion
		}
		if old.Fingerprint() != i.Fingerprint() {
			return true, fmt.Errorf("conflicting aliased operation identity")
		}
		return true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	if i.Kind != "reply" && i.Kind != "reply_inline" {
		return false, nil
	}
	rows, err := tx.QueryContext(ctx, "SELECT intent,state FROM effects WHERE json_extract(intent,'$.pr.url')=? AND json_extract(intent,'$.kind')=? ORDER BY rowid", i.PR.URL, i.Kind)
	if err != nil {
		return false, err
	}
	var match *Intent
	for rows.Next() {
		var raw string
		if err = rows.Scan(&raw, &state); err != nil {
			break
		}
		var old Intent
		if err = json.Unmarshal([]byte(raw), &old); err != nil {
			break
		}
		if state != "superseded" && replyIdentity(old) == replyIdentity(i) {
			if old.Run != i.Run && (state != "completed" || i.ActorID <= 0) {
				err = fmt.Errorf("identical reply belongs to retained run %s operation %s; reconcile it first", old.Run, old.ID)
				break
			}
			match = &old
			break
		}
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil || match == nil {
		return false, err
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO effect_aliases(run,id,canonical_run,canonical_id,proposal) VALUES(?,?,?,?,?)", i.Run, i.ID, match.Run, match.ID, string(jsonBytes(i)))
	return true, err
}
