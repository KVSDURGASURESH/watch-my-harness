package scanner

import "path/filepath"

var targetFields = map[string]string{
	"Read": "file_path", "Edit": "file_path", "Write": "file_path", "Glob": "pattern", "Grep": "pattern",
	"Bash": "command", "WebFetch": "url", "WebSearch": "query", "Task": "subagent_type", "Skill": "skill",
}

func claudeUsage(rec obj) (in, out, cr, c5, c1 int64) {
	u := asObj(asObj(rec["message"])["usage"])
	cc := asObj(u["cache_creation"])
	return toInt(u["input_tokens"]), toInt(u["output_tokens"]), toInt(u["cache_read_input_tokens"]),
		toInt(cc["ephemeral_5m_input_tokens"]), toInt(cc["ephemeral_1h_input_tokens"])
}

func promptText(rec obj) (any, any) {
	if t, _ := rec["type"].(string); t != "user" {
		return nil, nil
	}
	switch c := asObj(rec["message"])["content"].(type) {
	case string:
		return c, runeLen(c)
	case []any:
		var parts []string
		for _, b := range c {
			if bo := asObj(b); bo != nil && bo["type"] == "text" {
				s, _ := bo["text"].(string)
				parts = append(parts, s)
			}
		}
		if len(parts) == 0 {
			return nil, nil
		}
		text := ""
		for _, p := range parts {
			text += p
		}
		if text == "" {
			return text, nil
		}
		return text, runeLen(text)
	}
	return nil, nil
}

type parsedTool struct {
	name   string
	target any // string or nil
	result any // int64 or nil
	isErr  int64
}

func extractTools(rec obj) []parsedTool {
	var out []parsedTool
	content := asList(asObj(rec["message"])["content"])
	for _, b := range content {
		blk := asObj(b)
		if blk == nil || blk["type"] != "tool_use" {
			continue
		}
		name := "unknown"
		if n, ok := blk["name"].(string); ok && n != "" {
			name = n
		}
		var target any
		if f, ok := targetFields[name]; ok {
			if v, ok := asObj(blk["input"])[f].(string); ok {
				target = trunc(v, 500)
			}
		}
		out = append(out, parsedTool{name: name, target: target})
	}
	return out
}

func extractResults(rec obj) []parsedTool {
	var out []parsedTool
	for _, b := range asList(asObj(rec["message"])["content"]) {
		blk := asObj(b)
		if blk == nil || blk["type"] != "tool_result" {
			continue
		}
		var chars int64
		switch body := blk["content"].(type) {
		case string:
			chars = runeLen(body)
		case []any:
			for _, p := range body {
				if po := asObj(p); po != nil {
					if s, ok := po["text"].(string); ok {
						chars += runeLen(s)
					}
				}
			}
		}
		var e int64
		if truthy(blk["is_error"]) {
			e = 1
		}
		out = append(out, parsedTool{name: "_tool_result", target: blk["tool_use_id"], result: chars / 4, isErr: e})
	}
	return out
}

func parseClaudeRecord(rec obj, slug, source string) (msgRow, []parsedTool) {
	mo := asObj(rec["message"])
	text, chars := promptText(rec)
	m := blankMsg(source)
	in, out, cr, c5, c1 := claudeUsage(rec)
	side := int64(0)
	if truthy(rec["isSidechain"]) {
		side = 1
	}
	for k, v := range map[string]any{
		"uuid": rec["uuid"], "parent_uuid": rec["parentUuid"], "session_id": rec["sessionId"], "project_slug": slug,
		"cwd": rec["cwd"], "git_branch": rec["gitBranch"], "cc_version": rec["version"], "entrypoint": rec["entrypoint"],
		"type": rec["type"], "is_sidechain": side, "agent_id": rec["agentId"], "timestamp": rec["timestamp"],
		"model": mo["model"], "stop_reason": mo["stop_reason"], "prompt_id": rec["promptId"], "message_id": mo["id"],
		"prompt_text": text, "prompt_chars": chars,
		"input_tokens": in, "output_tokens": out, "cache_read_tokens": cr,
		"cache_create_5m_tokens": c5, "cache_create_1h_tokens": c1,
	} {
		m[k] = v
	}
	tools := append(extractTools(rec), extractResults(rec)...)
	if len(tools) > 0 {
		var cs []call
		for _, t := range tools {
			if t.name != "_tool_result" {
				var tp *string
				if s, ok := t.target.(string); ok {
					tp = &s
				}
				cs = append(cs, call{t.name, tp})
			}
		}
		m["tool_calls_json"] = dumpCalls(cs)
	}
	return m, tools
}

func scanClaudeFile(w *writer, path, base string, start int64, state obj, source string) (int64, obj, error) {
	rel, err := filepath.Rel(base, path)
	if err != nil {
		rel = path
	}
	slug := rel
	if i := indexSep(rel); i >= 0 {
		slug = rel[:i]
	}
	end := start
	err = iterLines(path, start, func(ls, le int64, rec obj) {
		end = le
		if rec == nil {
			return
		}
		if _, ok := rec["uuid"]; !ok {
			return
		}
		if _, ok := rec["type"]; !ok {
			return
		}
		msg, tl := parseClaudeRecord(rec, slug, source)
		if !truthy(msg["session_id"]) || !truthy(msg["timestamp"]) {
			return
		}
		if mid := msg["message_id"]; truthy(mid) {
			evictPriorSnapshots(w, msg["session_id"], mid, msg["uuid"])
		}
		w.putMsg(msg)
		w.msgs++
		// tool_calls has no natural unique key; clear prior rows for this uuid so rescans stay idempotent.
		_, _ = w.delTools.Exec(sqlArg(msg["uuid"]))
		for _, t := range tl {
			w.putTool(toolRow{MessageUUID: msg["uuid"], SessionID: msg["session_id"], Slug: slug, Name: t.name,
				Target: t.target, ResultTokens: t.result, IsError: t.isErr, Timestamp: msg["timestamp"], Source: source})
		}
	})
	if state == nil {
		state = obj{}
	}
	return end, state, err
}

func indexSep(s string) int {
	for i := 0; i < len(s); i++ {
		if s[i] == '/' || s[i] == '\\' {
			return i
		}
	}
	return -1
}

// evictPriorSnapshots drops older streaming snapshots of one API response: Claude Code
// writes 2-3 lines per response with the same message.id and only the last matches billing.
func evictPriorSnapshots(w *writer, sid, mid, keep any) {
	rows, err := w.tx.Query("SELECT uuid FROM messages WHERE session_id=? AND message_id=? AND uuid!=?",
		sqlArg(sid), sqlArg(mid), sqlArg(keep))
	if err != nil {
		panic(err)
	}
	var old []string
	for rows.Next() {
		var u string
		if rows.Scan(&u) == nil {
			old = append(old, u)
		}
	}
	rows.Close()
	for _, u := range old {
		w.tx.Exec("DELETE FROM tool_calls WHERE message_uuid=?", u)
		w.tx.Exec("DELETE FROM messages WHERE uuid=?", u)
	}
}
