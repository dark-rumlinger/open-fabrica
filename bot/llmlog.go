package bot

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"openfabrica/db"
	"openfabrica/providers/llm"
)

// The LLM log screens mirror the admin build log: a paged list of calls, and a
// detail view per call. Data comes straight from of_llm_calls.

const (
	logPerPage = 6
	// Keeps the detail message well under Telegram's 4096 character cap.
	logResponseMax = 1400
)

type llmCallSummary struct {
	id                     int64
	source, stage, model   string
	input, output          int
	cacheRead, cacheCreate int
	durMs                  int64
	cacheHit               bool
	errMsg                 string
	toolNames              []string
	provider               string
}

const llmSummaryCols = `id, COALESCE(source,''), COALESCE(stage,''), COALESCE(model,''),
 input_tokens, output_tokens, cache_read_input_tokens, cache_creation_input_tokens,
 duration_ms, cache_hit, COALESCE(error_message,''), COALESCE(response_tool_calls,''), COALESCE(provider_name,'')`

type rowScan interface{ Scan(dest ...any) error }

func scanSummary(r rowScan) (llmCallSummary, error) {
	var c llmCallSummary
	var hit int
	var calls string
	err := r.Scan(&c.id, &c.source, &c.stage, &c.model, &c.input, &c.output, &c.cacheRead, &c.cacheCreate,
		&c.durMs, &hit, &c.errMsg, &calls, &c.provider)
	c.cacheHit = hit != 0
	c.toolNames = toolNamesFromJSON(calls)
	return c, err
}

func toolNamesFromJSON(raw string) []string {
	if raw == "" {
		return nil
	}
	var calls []llm.ToolCall
	if json.Unmarshal([]byte(raw), &calls) != nil {
		return nil
	}
	names := make([]string, 0, len(calls))
	for _, c := range calls {
		names = append(names, c.Name)
	}
	return names
}

func (c llmCallSummary) mark() string {
	switch {
	case c.errMsg != "":
		return "✗"
	case c.cacheHit:
		return "↻"
	}
	return "✓"
}

func (c llmCallSummary) costText() string {
	if c.cacheHit {
		return ""
	}
	usd, ok := llm.CallCostUSD(c.provider, c.model, c.input, c.output, c.cacheRead, c.cacheCreate)
	if !ok {
		return ""
	}
	return fmtCost(usd)
}

func (c llmCallSummary) kind() string {
	if c.stage != "" {
		return strings.ToLower(c.stage)
	}
	if c.source != "" {
		return c.source
	}
	return "call"
}

// logListScreen shows one page of the project's LLM calls, newest first.
func (s *BotService) logListScreen(state *UserState, page int) screen {
	v := s.voice(state)
	back := screen{
		labels: []string{v.tr("◀ Back"), v.tr("☰ Menu")},
		cbs:    []string{"nav:chat", "nav:menu"},
	}
	pdb := s.deps.ProjectDBManager.Get(state.ActiveProjectID)
	if pdb == nil {
		back.text = v.tr("Pick a project first.")
		return back
	}
	var total int
	_ = pdb.QueryRow(`SELECT COUNT(*) FROM of_llm_calls`).Scan(&total)
	if total == 0 {
		back.text = v.tr("📜 <b>LLM calls</b>\n\nNothing logged yet. Calls appear here as soon as the builder starts working.")
		return back
	}
	if page < 0 || page*logPerPage >= total {
		page = 0
	}
	rows, err := pdb.Query(`SELECT `+llmSummaryCols+` FROM of_llm_calls ORDER BY id DESC LIMIT ? OFFSET ?`,
		logPerPage, page*logPerPage)
	if err != nil {
		back.text = v.tr("Could not read the call log.")
		return back
	}
	defer rows.Close()

	var b strings.Builder
	first := page*logPerPage + 1
	var labels, cbs []string
	n := 0
	for rows.Next() {
		c, err := scanSummary(rows)
		if err != nil {
			continue
		}
		n++
		line := fmt.Sprintf("%s <b>#%d</b> %s · %s · %s/%s · %.1fs", c.mark(), c.id, escapeHTML(c.kind()),
			escapeHTML(truncate(c.model, 22)), fmtTokens(c.input), fmtTokens(c.output), float64(c.durMs)/1000)
		if cost := c.costText(); cost != "" {
			line += " · " + cost
		}
		b.WriteString(line + "\n")
		if len(c.toolNames) > 0 {
			b.WriteString("   " + escapeHTML(truncate(strings.Join(c.toolNames, ", "), 80)) + "\n")
		} else if c.errMsg != "" {
			b.WriteString("   " + escapeHTML(truncate(c.errMsg, 80)) + "\n")
		}
		labels = append(labels, fmt.Sprintf("%s #%d", c.mark(), c.id))
		cbs = append(cbs, fmt.Sprintf("log:d:%d", c.id))
	}
	if err := rows.Err(); err != nil {
		// A cut-off read shows a short page; say so rather than pass it off as complete.
		s.logger.Warn("bot llm log: row iteration failed", "error", err)
		b.WriteString("\n" + v.tr("⚠️ Part of this page could not be read. Tap Refresh to retry.") + "\n")
	}
	if page > 0 {
		labels = append(labels, v.tr("◀ Newer"))
		cbs = append(cbs, fmt.Sprintf("log:p:%d", page-1))
	}
	if (page+1)*logPerPage < total {
		labels = append(labels, v.tr("Older ▶"))
		cbs = append(cbs, fmt.Sprintf("log:p:%d", page+1))
	}
	labels = append(labels, v.tr("🔄 Refresh"), v.tr("◀ Back"), v.tr("☰ Menu"))
	cbs = append(cbs, fmt.Sprintf("log:p:%d", page), "nav:chat", "nav:menu")

	head := v.trh("📜 <b>LLM calls</b> (%d-%d of %d)", first, first+n-1, total) + "\n" + v.tr("✓ ok  ✗ failed  ↻ replayed") + "\n\n"
	return screen{text: head + b.String() + "\n" + v.tr("Tap a call for details."), labels: labels, cbs: cbs}
}

// logDetailScreen shows one call: timing, tokens, cost, tools and the reply.
func (s *BotService) logDetailScreen(state *UserState, id int64) screen {
	v := s.voice(state)
	listBack := screen{labels: []string{v.tr("◀ Calls"), v.tr("☰ Menu")}, cbs: []string{"log:p:0", "nav:menu"}}
	pdb := s.deps.ProjectDBManager.Get(state.ActiveProjectID)
	if pdb == nil {
		listBack.text = v.tr("Pick a project first.")
		return listBack
	}

	var c llmCallSummary
	var iteration, reqBytes, maxTokens int
	var stop, content, callsJSON, toolsJSON, created string
	var hit int
	err := pdb.QueryRow(`SELECT id, COALESCE(source,''), COALESCE(stage,''), COALESCE(model,''),
		input_tokens, output_tokens, cache_read_input_tokens, cache_creation_input_tokens,
		duration_ms, cache_hit, COALESCE(error_message,''), COALESCE(provider_name,''),
		iteration, total_request_bytes, COALESCE(request_max_tokens,0),
		COALESCE(response_stop_reason,''), COALESCE(response_content,''),
		COALESCE(response_tool_calls,''), COALESCE(request_tools,''), created_at
		FROM of_llm_calls WHERE id = ?`, id).Scan(
		&c.id, &c.source, &c.stage, &c.model, &c.input, &c.output, &c.cacheRead, &c.cacheCreate,
		&c.durMs, &hit, &c.errMsg, &c.provider, &iteration, &reqBytes, &maxTokens,
		&stop, &content, &callsJSON, &toolsJSON, &created)
	if err != nil {
		listBack.text = v.tr("That call is no longer in the log.")
		return listBack
	}
	c.cacheHit = hit != 0

	var b strings.Builder
	b.WriteString(v.trh("🔎 <b>Call #%d</b> %s %s", c.id, c.mark(), c.kind()) + "\n")
	fmt.Fprintf(&b, "%s", escapeHTML(c.model))
	if c.provider != "" {
		fmt.Fprintf(&b, " (%s)", escapeHTML(c.provider))
	}
	fmt.Fprintf(&b, "\n%s · %s\n\n", escapeHTML(created), v.tr("step %d", iteration))

	fmt.Fprintf(&b, "⏱ %.1fs", float64(c.durMs)/1000)
	if c.cacheHit {
		b.WriteString(" · " + v.tr("replayed from cache (not billed)"))
	}
	b.WriteString("\n")
	b.WriteString("🔢 " + v.tr("%s in / %s out", fmtTokens(c.input), fmtTokens(c.output)))
	if c.cacheRead > 0 {
		b.WriteString(" · " + v.tr("%s cached", fmtTokens(c.cacheRead)))
	}
	if cost := c.costText(); cost != "" {
		b.WriteString(" · " + cost)
	}
	b.WriteString("\n")
	if reqBytes > 0 {
		b.WriteString(v.tr("📦 request %s KB", fmt.Sprintf("%.0f", float64(reqBytes)/1024)))
		if n := len(toolNamesFromArray(toolsJSON)); n > 0 {
			b.WriteString(" · " + v.tr("%d tools offered", n))
		}
		b.WriteString("\n")
	}
	if stop != "" {
		b.WriteString(v.trh("🏁 stop: %s", stop) + "\n")
	}
	if c.errMsg != "" {
		b.WriteString("\n" + v.tr("⚠️ <b>Error</b>") + "\n" + escapeHTML(truncate(c.errMsg, 500)) + "\n")
	}

	if calls := parseToolCalls(callsJSON); len(calls) > 0 {
		b.WriteString("\n" + v.tr("🔧 <b>Tool calls</b>") + "\n")
		for i, tc := range calls {
			if i >= 6 {
				b.WriteString(v.tr("… and %d more", len(calls)-i) + "\n")
				break
			}
			fmt.Fprintf(&b, "• %s %s\n", escapeHTML(tc.Name), escapeHTML(truncate(compactJSON(tc.Arguments), 110)))
		}
	}
	if strings.TrimSpace(content) != "" {
		b.WriteString("\n" + v.tr("💬 <b>Reply</b>") + "\n<blockquote expandable>" + escapeHTML(truncate(content, logResponseMax)) + "</blockquote>")
	}

	var newer, older int64
	_ = pdb.QueryRow(`SELECT COALESCE(MIN(id),0) FROM of_llm_calls WHERE id > ?`, id).Scan(&newer)
	_ = pdb.QueryRow(`SELECT COALESCE(MAX(id),0) FROM of_llm_calls WHERE id < ?`, id).Scan(&older)
	labels := []string{v.tr("📄 Full log (file)")}
	cbs := []string{fmt.Sprintf("log:f:%d", id)}
	if older > 0 {
		labels = append(labels, v.tr("◀ Earlier"))
		cbs = append(cbs, fmt.Sprintf("log:d:%d", older))
	}
	if newer > 0 {
		labels = append(labels, v.tr("Later ▶"))
		cbs = append(cbs, fmt.Sprintf("log:d:%d", newer))
	}
	labels = append(labels, v.tr("◀ Calls"), v.tr("☰ Menu"))
	cbs = append(cbs, "log:p:0", "nav:menu")
	return screen{text: b.String(), labels: labels, cbs: cbs}
}

func parseToolCalls(raw string) []llm.ToolCall {
	if raw == "" {
		return nil
	}
	var calls []llm.ToolCall
	if json.Unmarshal([]byte(raw), &calls) != nil {
		return nil
	}
	return calls
}

func toolNamesFromArray(raw string) []string {
	var names []string
	if raw == "" || json.Unmarshal([]byte(raw), &names) != nil {
		return nil
	}
	return names
}

func compactJSON(raw string) string {
	var v any
	if json.Unmarshal([]byte(raw), &v) != nil {
		return raw
	}
	b, err := json.Marshal(v)
	if err != nil {
		return raw
	}
	return string(b)
}

// callFile builds the full, untruncated record of one call for download: the
// request as the model saw it (the delta chain replayed), the reply, and the
// accounting.
func (s *BotService) callFile(pdb *db.ProjectDB, id int64) (name string, data []byte, err error) {
	var source, runID, sessionID, stage, model, provider string
	var reqMsgs, reqSys, reqTools, respContent, respCalls, stop, errMsg, created string
	var input, output, cacheRead, cacheCreate, iteration int
	var durMs int64
	if err = pdb.QueryRow(`SELECT COALESCE(source,''), COALESCE(run_id,''), COALESCE(session_id,''),
		COALESCE(stage,''), COALESCE(model,''), COALESCE(provider_name,''),
		COALESCE(request_messages,''), COALESCE(request_system,''), COALESCE(request_tools,''),
		COALESCE(response_content,''), COALESCE(response_tool_calls,''), COALESCE(response_stop_reason,''),
		COALESCE(error_message,''), created_at, input_tokens, output_tokens,
		cache_read_input_tokens, cache_creation_input_tokens, iteration, duration_ms
		FROM of_llm_calls WHERE id = ?`, id).Scan(&source, &runID, &sessionID, &stage, &model, &provider,
		&reqMsgs, &reqSys, &reqTools, &respContent, &respCalls, &stop, &errMsg, &created,
		&input, &output, &cacheRead, &cacheCreate, &iteration, &durMs); err != nil {
		return "", nil, err
	}
	chain := llm.CallChain{Source: source, RunID: runID, SessionID: sessionID}
	reqMsgs, reqSys, reqTools = llm.ReplayRequestMessages(pdb.Reader(), id, chain, reqMsgs, reqSys, reqTools)

	raw := func(v string) any {
		if v != "" && json.Valid([]byte(v)) {
			return json.RawMessage(v)
		}
		return v
	}
	rec := map[string]any{
		"id": id, "created_at": created, "source": source, "stage": stage, "iteration": iteration,
		"model": model, "provider": provider, "duration_ms": durMs,
		"tokens":      map[string]int{"input": input, "output": output, "cache_read": cacheRead, "cache_creation": cacheCreate},
		"stop_reason": stop, "error": errMsg,
		"request":  map[string]any{"system": reqSys, "tools": raw(reqTools), "messages": raw(reqMsgs)},
		"response": map[string]any{"content": respContent, "tool_calls": raw(respCalls)},
	}
	if usd, ok := llm.CallCostUSD(provider, model, input, output, cacheRead, cacheCreate); ok {
		rec["cost_usd"] = usd
	}
	// Keep <, > and & readable: this file is for people, not for embedding in HTML.
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err = enc.Encode(rec); err != nil {
		return "", nil, err
	}
	return fmt.Sprintf("llm-call-%d.json", id), buf.Bytes(), nil
}

// sendCallFile uploads the full call record to the person's chat.
func (s *BotService) sendCallFile(state *UserState, id int64) string {
	v := s.voice(state)
	pdb := s.deps.ProjectDBManager.Get(state.ActiveProjectID)
	if pdb == nil {
		return v.tr("Pick a project first")
	}
	name, data, err := s.callFile(pdb, id)
	if err != nil {
		return v.tr("Call not found")
	}
	p, ok := s.provider(state.Provider)
	if !ok {
		return ""
	}
	if err := p.SendDocument(state.ChatID, name, data, v.tr("Full record of call #%d", id)); err != nil {
		s.logger.Warn("send call file failed", "error", err)
		return v.tr("Could not send the file")
	}
	state.LiveCardStale.Store(true)
	return v.tr("Sent")
}
