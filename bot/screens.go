package bot

import (
	"fmt"
	"strings"

	"openfabrica/db"
	"openfabrica/db/models"
	"openfabrica/engine"
	"openfabrica/internal/buildstate"
	"openfabrica/providers/llm"
	"openfabrica/tlsproxy"
)

// The screens below are the phone version of the admin dashboard: where a
// project stands, what it needs from you, what it cost, and where it lives.

// stageTrack renders the PLAN, BUILD, LIVE pipeline with the current step marked.
func (v voice) stageTrack(stage string, live bool) string {
	steps := []struct{ code, label string }{{"EXPAND", v.tr("Idea")}, {"PLAN", v.tr("Mapping")}, {"BUILD", v.tr("Building")}, {"LIVE", v.tr("Live")}}
	cur := -1
	for i, st := range steps {
		if strings.EqualFold(stage, st.code) {
			cur = i
		}
	}
	if live {
		cur = len(steps)
	}
	parts := make([]string, 0, len(steps))
	for i, st := range steps {
		mark := "⬜"
		switch {
		case i < cur:
			mark = "✅"
		case i == cur:
			mark = "🔨"
		}
		parts = append(parts, mark+" "+st.label)
	}
	return strings.Join(parts, " → ")
}

// runStage reads where the project's latest pipeline run stands.
func runStage(pdb *db.ProjectDB) (stage string, live bool) {
	var status string
	if err := pdb.QueryRow(`SELECT COALESCE(stage,''), COALESCE(status,'') FROM of_runs
		ORDER BY started_at DESC LIMIT 1`).Scan(&stage, &status); err != nil {
		return "", false
	}
	return stage, stage == engine.StageLive || stage == "done" || status == buildstate.RunCompleted
}

// usageTotals sums tokens, calls, failures and cost, optionally for today only.
func usageTotals(pdb *db.ProjectDB, todayOnly bool) (calls, failed, tokens int, cost float64, costKnown bool) {
	where := ""
	if todayOnly {
		where = " WHERE created_at >= datetime('now','start of day')"
	}
	_ = pdb.QueryRow(`SELECT COUNT(*),
		COALESCE(SUM(CASE WHEN COALESCE(error_message,'') <> '' THEN 1 ELSE 0 END),0),
		COALESCE(SUM(input_tokens + output_tokens),0) FROM of_llm_calls`+where).Scan(&calls, &failed, &tokens)

	cacheWhere := " WHERE cache_hit = 0"
	if todayOnly {
		cacheWhere += " AND created_at >= datetime('now','start of day')"
	}
	rows, err := pdb.Query(`SELECT COALESCE(provider_name,''), COALESCE(model,''),
		SUM(input_tokens), SUM(output_tokens), SUM(cache_read_input_tokens), SUM(cache_creation_input_tokens)
		FROM of_llm_calls` + cacheWhere + ` GROUP BY provider_name, model`)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var prov, model string
		var in, out, cr, cw int
		if rows.Scan(&prov, &model, &in, &out, &cr, &cw) != nil {
			continue
		}
		if usd, ok := llm.CallCostUSD(prov, model, in, out, cr, cw); ok {
			cost += usd
			costKnown = true
		}
	}
	if rows.Err() != nil {
		// A partial sum would understate spend; report it as unknown instead.
		cost, costKnown = 0, false
	}
	return
}

// dashboardScreen is the home screen: where the active project stands and what
// needs attention, with one-tap ways in.
func (s *BotService) dashboardScreen(state *UserState) screen {
	v := s.voice(state)
	if state.ActiveProjectID == 0 {
		return s.mainMenuScreen(v)
	}
	project, err := models.GetProjectByID(s.deps.DB.DB, state.ActiveProjectID)
	if err != nil {
		return s.mainMenuScreen(v)
	}
	var b strings.Builder
	b.WriteString(v.trh("💬 Chatting with <b>%s</b>", project.Name) + "\n")
	pending := 0
	if pdb := s.deps.ProjectDBManager.Get(project.ID); pdb != nil {
		stage, live := runStage(pdb)
		b.WriteString(v.stageTrack(stage, live) + "\n")
		pending = s.pendingQuestionCount(pdb)
	}
	b.WriteString(v.trh("Phase: <b>%s</b>", v.phaseLine(s.projectPhase(project))) + "\n")
	if pending > 0 {
		b.WriteString("\n" + v.trh("❓ <b>%d question(s) waiting for you</b>", pending) + "\n")
	}
	b.WriteString("\n" + v.tr("Send a message to chat with the builder."))

	qLabel := v.tr("❓ Questions")
	if pending > 0 {
		qLabel = v.tr("❓ Questions (%d)", pending)
	}
	return screen{
		text: b.String(),
		labels: []string{
			v.tr("📊 Status"), qLabel,
			v.tr("🌐 Site"), v.tr("📜 LLM log"),
			v.tr("⚙️ Settings"), v.tr("🔀 Projects"),
			v.tr("🔔 Alerts"), v.tr("❔ Help"),
		},
		cbs: []string{
			"nav:status", "nav:questions",
			"nav:site", "nav:log",
			"nav:settings", "nav:projects",
			"nav:alerts", "nav:help",
		},
	}
}

// statusScreen shows stage, progress, mode and usage for the active project.
func (s *BotService) statusScreen(state *UserState) screen {
	v := s.voice(state)
	menuOnly := screen{labels: []string{v.tr("☰ Menu")}, cbs: []string{"nav:menu"}}
	project, err := models.GetProjectByID(s.deps.DB.DB, state.ActiveProjectID)
	pdb := s.deps.ProjectDBManager.Get(state.ActiveProjectID)
	if err != nil || pdb == nil {
		menuOnly.text = v.tr("Pick a project first.")
		return menuOnly
	}

	var b strings.Builder
	fmt.Fprintf(&b, "📊 <b>%s</b>\n", escapeHTML(project.Name))
	stage, live := runStage(pdb)
	b.WriteString(v.stageTrack(stage, live) + "\n\n")

	b.WriteString(v.trh("Phase: <b>%s</b> · Model: %s", v.phaseLine(s.projectPhase(project)), s.modelDisplayName(project.LLMModelID)) + "\n")

	if tasks, err := engine.LoadTasks(pdb.Reader()); err == nil && len(tasks) > 0 {
		done, active := 0, ""
		for _, t := range tasks {
			switch t.Status {
			case engine.TaskDone:
				done++
			case engine.TaskActive:
				if active == "" {
					active = t.Title
				}
			}
		}
		b.WriteString("\n" + v.tr("%s %d/%d tasks", progressBar(done, len(tasks)), done, len(tasks)) + "\n")
		if active != "" {
			fmt.Fprintf(&b, "▶ %s\n", escapeHTML(truncate(active, 90)))
		}
	}

	pending := s.pendingQuestionCount(pdb)
	if pending > 0 {
		b.WriteString("\n" + v.tr("❓ %d question(s) waiting", pending) + "\n")
	}

	usage := func(label string, today bool) {
		calls, failed, tokens, cost, known := usageTotals(pdb, today)
		if calls == 0 {
			return
		}
		line := v.tr("%s: %d calls · %s tokens", label, calls, fmtTokens(tokens))
		if known {
			line += " · " + fmtCost(cost)
		}
		if failed > 0 {
			line += " · " + v.tr("%d failed", failed)
		}
		b.WriteString(escapeHTML(line) + "\n")
	}
	b.WriteString("\n")
	usage(v.tr("Today"), true)
	usage(v.tr("All time"), false)

	labels := []string{v.tr("🔄 Refresh")}
	cbs := []string{"nav:status"}
	if pending > 0 {
		labels = append(labels, v.tr("❓ Questions (%d)", pending))
		cbs = append(cbs, "nav:questions")
	}
	if project.Mode == models.ModePaused {
		labels = append(labels, v.tr("▶ Resume"))
		cbs = append(cbs, "card:resume")
	} else {
		labels = append(labels, v.tr("⏸ Pause"))
		cbs = append(cbs, "card:pause")
	}
	if s.engineRunning(project.ID) {
		labels = append(labels, v.tr("⏹ Stop work"))
		cbs = append(cbs, "card:stop")
	}
	labels = append(labels, v.tr("🌐 Site"), v.tr("📜 LLM log"), v.tr("◀ Back"), v.tr("☰ Menu"))
	cbs = append(cbs, "nav:site", "nav:log", "nav:chat", "nav:menu")
	return screen{text: b.String(), labels: labels, cbs: cbs}
}

// questionsScreen lists the questions waiting for an answer.
func (s *BotService) questionsScreen(state *UserState) screen {
	v := s.voice(state)
	back := screen{labels: []string{v.tr("◀ Back"), v.tr("☰ Menu")}, cbs: []string{"nav:chat", "nav:menu"}}
	pdb := s.deps.ProjectDBManager.Get(state.ActiveProjectID)
	if pdb == nil {
		back.text = v.tr("Pick a project first.")
		return back
	}
	rows, err := pdb.Query(`SELECT id, question, COALESCE(type,'text') FROM of_questions
		WHERE status = 'pending' ORDER BY id LIMIT 8`)
	if err != nil {
		back.text = v.tr("Could not read the questions.")
		return back
	}
	defer rows.Close()

	var b strings.Builder
	var labels, cbs []string
	n := 0
	for rows.Next() {
		var id int
		var q, qType string
		if rows.Scan(&id, &q, &qType) != nil {
			continue
		}
		n++
		icon := map[string]string{"secret": "🔐", "approval": "✅", "form": "📝"}[qType]
		if icon == "" {
			icon = "❓"
		}
		fmt.Fprintf(&b, "%d. %s %s\n", n, icon, escapeHTML(truncate(q, 110)))
		labels = append(labels, fmt.Sprintf("%d. %s", n, truncate(q, 24)))
		cbs = append(cbs, fmt.Sprintf("q:open:%d", id))
	}
	if err := rows.Err(); err != nil {
		s.logger.Warn("bot questions screen: row iteration failed", "error", err)
		back.text = v.tr("Could not read the questions.")
		return back
	}
	if n == 0 {
		back.text = v.tr("❓ <b>Questions</b>\n\nNothing is waiting for you. The builder will ask here when it needs something.")
		return back
	}
	labels = append(labels, v.tr("⏭ Answer all later"), v.tr("◀ Back"), v.tr("☰ Menu"))
	cbs = append(cbs, "q:deferall", "nav:chat", "nav:menu")
	return screen{text: v.tr("❓ <b>Waiting for you</b>") + "\n\n" + b.String() + "\n" + v.tr("Tap one to answer it."), labels: labels, cbs: cbs}
}

// siteScreen shows whether the project is published and how to reach it.
func (s *BotService) siteScreen(state *UserState) screen {
	v := s.voice(state)
	back := screen{labels: []string{v.tr("◀ Back"), v.tr("☰ Menu")}, cbs: []string{"nav:chat", "nav:menu"}}
	project, err := models.GetProjectByID(s.deps.DB.DB, state.ActiveProjectID)
	if err != nil {
		back.text = v.tr("Pick a project first.")
		return back
	}
	published := project.Published
	domain := ""
	if project.Domain != nil {
		domain = *project.Domain
	}

	var b strings.Builder
	fmt.Fprintf(&b, "🌐 <b>%s</b>\n\n", escapeHTML(project.Name))
	if published {
		b.WriteString(v.tr("Site: <b>published</b>") + "\n")
	} else {
		b.WriteString(v.tr("Site: <b>not published</b>") + "\n")
	}

	var labels, cbs []string
	switch kind := tlsproxy.DomainKind(domain); {
	case kind == tlsproxy.DomainKindPublic && !s.deps.TLSDisabled:
		b.WriteString(v.trh("Address: %s", "https://"+domain) + "\n")
		if published {
			labels = append(labels, v.tr("🔗 Open site"))
			cbs = append(cbs, "url:https://"+domain)
		} else {
			b.WriteString("\n" + v.tr("Turn on \"Publish\" below to make it reachable.") + "\n")
		}
	case domain == "":
		b.WriteString(v.tr("Address: none yet\n\nSet a domain in the admin panel (Sharing & status) to publish it.") + "\n")
	default:
		b.WriteString(v.trh("Address: %s", domain) + "\n\n" +
			v.tr("This address only works on the computer running Open Fabrica, so it can't be opened from your phone. Set a real domain in the admin panel to reach it from anywhere.") + "\n")
	}

	if published {
		labels = append(labels, v.tr("🔒 Unpublish"))
		cbs = append(cbs, "nav:unpublish")
	} else {
		labels = append(labels, v.tr("🚀 Publish"))
		cbs = append(cbs, "nav:publish")
	}
	if link := s.adminLink(fmt.Sprintf("/projects/%d", project.ID)); link != "" {
		labels = append(labels, v.tr("🛠 Open in admin"))
		cbs = append(cbs, "url:"+link)
	}
	labels = append(labels, v.tr("◀ Back"), v.tr("☰ Menu"))
	cbs = append(cbs, "nav:chat", "nav:menu")
	return screen{text: b.String(), labels: labels, cbs: cbs}
}

// adminLink builds a deep link into the admin panel, or "" when the panel's
// public address is unknown.
func (s *BotService) adminLink(path string) string {
	base, _ := s.deps.DB.GetSetting("admin_base_url")
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if base == "" {
		return ""
	}
	return base + path
}

// helpScreen lists what the bot can do.
func (s *BotService) helpScreen(v voice) screen {
	return screen{
		text: v.tr("❔ <b>Help</b>\n\n" +
			"Just type to chat with the builder. It replies here and asks when it needs you.\n\n" +
			"/menu - home\n" +
			"/status - phase, progress, usage\n" +
			"/questions - what is waiting for you\n" +
			"/log - every LLM call, with details\n" +
			"/site - publish and open your site\n" +
			"/pause, /resume, /stop - control the builder\n" +
			"/projects - switch project\n" +
			"/alerts - how chatty I am\n" +
			"/cancel - cancel the current step\n" +
			"/selfdestruct - erase everything from the server"),
		labels: []string{v.tr("◀ Back"), v.tr("☰ Menu"), v.tr("⚠️ Self-destruct")},
		cbs:    []string{"nav:menu", "nav:menu", "nav:selfdestruct"},
	}
}
