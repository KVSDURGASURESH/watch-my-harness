package scanner

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"

	"github.com/KVSDURGASURESH/watch-my-harness/go/internal/store"
)

var (
	uuidTail  = regexp.MustCompile(`([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})\.jsonl$`)
	jsTool    = regexp.MustCompile(`tools\.([A-Za-z0-9_]+)\(`)
	jsCmd     = regexp.MustCompile(`"cmd"\s*:\s*"((?:[^"\\]|\\.)*)"`)
	patchFile = regexp.MustCompile(`\*\*\* (?:Update|Add|Delete) File: ([^\n\\"]+)`)
	skillMD   = regexp.MustCompile(`skills/([^/\s"']+)/SKILL\.md`)
	exitCode  = regexp.MustCompile(`(?i)(?:Process exited with code|exit code|exited with code)[: ]+(-?\d+)`)
)

// Codex tool -> the Claude Code name for the same job.
var codexToolClass = map[string]string{
	"exec_command": "Bash", "write_stdin": "Bash", "shell": "Bash", "local_shell": "Bash",
	"apply_patch": "Edit", "view_image": "Read",
	"web__run": "WebSearch", "web_search": "WebSearch",
	"spawn_agent": "Task",
}

func classOf(name string) string {
	if c, ok := codexToolClass[name]; ok {
		return c
	}
	return name
}

func codexTarget(args any) any {
	s, ok := args.(string)
	if !ok {
		return nil
	}
	var a any
	if json.Unmarshal([]byte(s), &a) != nil {
		return nil
	}
	ao, _ := a.(map[string]any)
	if ao == nil {
		return nil
	}
	for _, k := range []string{"cmd", "command", "path", "file_path", "pattern", "query", "url", "task_name"} {
		v := ao[k]
		if l, ok := v.([]any); ok {
			parts := make([]string, len(l))
			for i, e := range l {
				if es, ok := e.(string); ok {
					parts[i] = es
				} else {
					parts[i] = pyDumps(e)
				}
			}
			return trunc(strings.Join(parts, " "), 500)
		}
		if sv, ok := v.(string); ok {
			return trunc(sv, 500)
		}
	}
	return nil
}

func unjs(s string) string {
	var out string
	if json.Unmarshal([]byte(`"`+s+`"`), &out) == nil {
		return out
	}
	return s
}

type ct struct {
	cls    string
	target any
}

func codexCalls(p obj) []ct {
	name, _ := p["name"].(string)
	if name == "" {
		name = "unknown"
	}
	pt, _ := p["type"].(string)
	var out []ct
	if pt == "function_call" {
		out = append(out, ct{classOf(name), codexTarget(p["arguments"])})
	} else {
		src, _ := p["input"].(string)
		var cmds []string
		for _, m := range jsCmd.FindAllStringSubmatch(src, -1) {
			cmds = append(cmds, unjs(m[1]))
		}
		names := jsTool.FindAllStringSubmatch(src, -1)
		for _, nm := range names {
			switch n := nm[1]; n {
			case "apply_patch":
				files := patchFile.FindAllStringSubmatch(src, -1)
				if len(files) == 0 {
					out = append(out, ct{"Edit", nil})
				}
				for _, f := range files {
					out = append(out, ct{"Edit", f[1]})
				}
			case "exec_command":
				var t any
				if len(cmds) > 0 {
					t, cmds = cmds[0], cmds[1:]
				}
				out = append(out, ct{"Bash", t})
			default:
				out = append(out, ct{classOf(n), nil})
			}
		}
		if len(names) == 0 {
			out = append(out, ct{classOf(name), nil})
		}
	}
	// Codex has no Skill tool; a skill is "used" when its SKILL.md is read.
	n := len(out)
	for i := 0; i < n; i++ {
		if out[i].cls == "Bash" {
			if tgt, ok := out[i].target.(string); ok && tgt != "" {
				for _, m := range skillMD.FindAllStringSubmatch(tgt, -1) {
					out = append(out, ct{"Skill", m[1]})
				}
			}
		}
	}
	for i := range out {
		if s, ok := out[i].target.(string); ok {
			out[i].target = trunc(s, 500)
		}
	}
	return out
}

func codexOutput(p obj) (int64, int64) {
	var body string
	switch b := p["output"].(type) {
	case []any:
		for _, e := range b {
			if eo := asObj(e); eo != nil {
				s, _ := eo["text"].(string)
				body += s
			}
		}
	case string:
		body = b
	default:
		if truthy(b) {
			body = pyDumps(b)
		} else {
			body = `""`
		}
	}
	var isErr int64
	if m := exitCode.FindStringSubmatch(body); m != nil {
		if n, err := strconv.Atoi(m[1]); err == nil && n != 0 {
			isErr = 1
		}
	}
	return runeLen(body) / 4, isErr
}

func strOr(v any, def string) string {
	if s, ok := v.(string); ok && s != "" {
		return s
	}
	return def
}

func firstTruthy(vs ...any) any {
	for _, v := range vs {
		if truthy(v) {
			return v
		}
	}
	return nil
}

func scanCodexFile(w *writer, path, base string, start int64, state obj, source string) (int64, obj, error) {
	st := obj{}
	for k, v := range state {
		st[k] = v
	}
	name := filepath.Base(path)
	tid := strings.TrimSuffix(name, filepath.Ext(name))
	if m := uuidTail.FindStringSubmatch(name); m != nil {
		tid = m[1]
	}
	sid := strOr(st["sid"], tid)
	end := start
	var pending []call
	slug := func() string {
		if c := strOr(st["cwd"], ""); c != "" {
			return store.EncodeSlug(c)
		}
		return source + "-unknown"
	}
	err := iterLines(path, start, func(lineStart, lineEnd int64, rec obj) {
		end = lineEnd
		if rec == nil {
			return
		}
		t, _ := rec["type"].(string)
		p := asObj(rec["payload"])
		ts := rec["timestamp"]
		key := fmt.Sprintf("%s:%s:b%d", source, tid, lineStart)
		if o, ok := rec["ordinal"]; ok && o != nil {
			switch ov := o.(type) {
			case float64:
				key = fmt.Sprintf("%s:%s:%s", source, tid, strconv.FormatFloat(ov, 'f', -1, 64))
			default:
				key = fmt.Sprintf("%s:%s:%v", source, tid, ov)
			}
		}
		switch {
		case t == "session_meta" && !truthy(st["meta_seen"]):
			sid = strOr(firstTruthy(p["session_id"], p["id"]), tid)
			st["meta_seen"] = true
			st["sid"] = sid
			if c := p["cwd"]; truthy(c) {
				st["cwd"] = c
			}
			st["version"] = p["cli_version"]
			st["entrypoint"] = p["originator"]
			sc := int64(0)
			if p["thread_source"] == "subagent" {
				sc = 1
			}
			st["sidechain"] = sc
			st["agent"] = p["agent_nickname"]
		case t == "turn_context":
			if m := p["model"]; truthy(m) {
				st["model"] = m
			}
			if c := p["cwd"]; truthy(c) {
				st["cwd"] = c
			}
		case t == "response_item" && truthy(ts):
			pt, _ := p["type"].(string)
			switch pt {
			case "message":
				if p["role"] != "user" {
					return
				}
				text := ""
				for _, b := range asList(p["content"]) {
					if bo := asObj(b); bo != nil && bo["type"] == "input_text" {
						s, _ := bo["text"].(string)
						text += s
					}
				}
				if text != "" {
					l := lstripSpace(text)
					if !strings.HasPrefix(l, "<") && !strings.HasPrefix(l, "# AGENTS.md") {
						m := blankMsg(source)
						m["uuid"], m["session_id"], m["project_slug"], m["cwd"] = key, sid, slug(), st["cwd"]
						m["cc_version"], m["entrypoint"], m["type"] = st["version"], st["entrypoint"], "user"
						m["is_sidechain"], m["agent_id"], m["timestamp"] = toInt(st["sidechain"]), st["agent"], ts
						m["prompt_text"], m["prompt_chars"] = text, runeLen(text)
						w.putMsg(m)
						st["last_user"] = key
						w.msgs++
					}
				}
			case "function_call", "custom_tool_call":
				for _, c := range codexCalls(p) {
					w.putTool(toolRow{MessageUUID: key, SessionID: sid, Slug: slug(), Name: c.cls, Target: c.target, IsError: 0, Timestamp: ts, Source: source})
					var tp *string
					if s, ok := c.target.(string); ok {
						tp = &s
					}
					pending = append(pending, call{c.cls, tp})
				}
			case "function_call_output", "custom_tool_call_output":
				toks, e := codexOutput(p)
				w.putTool(toolRow{MessageUUID: key, SessionID: sid, Slug: slug(), Name: "_tool_result", Target: p["call_id"],
					ResultTokens: toks, IsError: e, Timestamp: ts, Source: source})
			}
		case t == "event_msg" && truthy(ts) && p["type"] == "token_count":
			info := asObj(p["info"])
			total, last := info["total_token_usage"], asObj(info["last_token_usage"])
			if !truthy(total) || reflect.DeepEqual(total, st["last_total"]) {
				return
			}
			st["last_total"] = total
			cached := toInt(last["cached_input_tokens"])
			in := toInt(last["input_tokens"]) - cached
			if in < 0 {
				in = 0
			}
			m := blankMsg(source)
			m["uuid"], m["parent_uuid"], m["session_id"], m["project_slug"] = key, st["last_user"], sid, slug()
			m["cwd"], m["cc_version"], m["entrypoint"] = st["cwd"], st["version"], st["entrypoint"]
			m["is_sidechain"], m["agent_id"], m["timestamp"], m["model"] = toInt(st["sidechain"]), st["agent"], ts, st["model"]
			m["input_tokens"], m["output_tokens"], m["cache_read_tokens"] = in, toInt(last["output_tokens"]), cached
			if len(pending) > 0 {
				m["tool_calls_json"] = dumpCalls(pending)
			}
			w.putMsg(m)
			pending = nil
			w.msgs++
		}
	})
	st["sid"] = sid
	return end, st, err
}
