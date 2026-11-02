package scanner

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"

	"github.com/KVSDURGASURESH/watch-my-harness/go/internal/store"
)

// scanGenericFile ingests the normalised JSONL any agent can emit, one object per API call:
//
//	{"session_id","timestamp","model","input_tokens","output_tokens","cache_read_tokens"?,
//	 "cwd"?,"project"?,"prompt"?,"tools"?:["name",...]}
//
// input_tokens means uncached input. Required: session_id, timestamp.
func scanGenericFile(w *writer, path, base string, start int64, state obj, source string) (int64, obj, error) {
	sum := sha1.Sum([]byte(path))
	tag := hex.EncodeToString(sum[:])[:8]
	end := start
	err := iterLines(path, start, func(lineStart, lineEnd int64, rec obj) {
		end = lineEnd
		if rec == nil || !truthy(rec["session_id"]) || !truthy(rec["timestamp"]) {
			return
		}
		sid, ts := fmt.Sprint(rec["session_id"]), rec["timestamp"]
		cwd := rec["cwd"]
		slug := source + "-unknown"
		if p := rec["project"]; truthy(p) {
			slug = fmt.Sprint(p)
		} else if c, ok := cwd.(string); ok && c != "" {
			slug = store.EncodeSlug(c)
		}
		key := fmt.Sprintf("%s:%s:%d", source, tag, lineStart)
		var parent any
		if truthy(rec["prompt"]) {
			parent = key + ":u"
			pt := fmt.Sprint(rec["prompt"])
			m := blankMsg(source)
			m["uuid"], m["session_id"], m["project_slug"], m["cwd"], m["type"] = parent, sid, slug, cwd, "user"
			m["timestamp"], m["prompt_text"], m["prompt_chars"] = ts, pt, runeLen(pt)
			w.putMsg(m)
			w.msgs++
		}
		var names []string
		for _, n := range asList(rec["tools"]) {
			names = append(names, fmt.Sprint(n))
		}
		m := blankMsg(source)
		m["uuid"], m["parent_uuid"], m["session_id"], m["project_slug"], m["cwd"] = key, parent, sid, slug, cwd
		m["timestamp"], m["model"] = ts, rec["model"]
		m["input_tokens"], m["output_tokens"], m["cache_read_tokens"] = toInt(rec["input_tokens"]), toInt(rec["output_tokens"]), toInt(rec["cache_read_tokens"])
		if len(names) > 0 {
			cs := make([]call, len(names))
			for i, n := range names {
				cs[i] = call{Name: n}
			}
			m["tool_calls_json"] = dumpCalls(cs)
		}
		w.putMsg(m)
		w.msgs++
		for _, n := range names {
			w.putTool(toolRow{MessageUUID: key, SessionID: sid, Slug: slug, Name: n, Timestamp: ts, Source: source})
		}
	})
	if state == nil {
		state = obj{}
	}
	return end, state, err
}
