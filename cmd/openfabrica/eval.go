package main

// `openfabrica eval` measures build quality on a fixed corpus of briefs, so a change to
// the prompts, the model settings or the checks can be compared before and after on
// the same model. It drives a RUNNING server through its admin API (a build needs the
// live public server: the probes load and click real pages), one project per brief,
// and reads each result back from that project's database, read-only.
//
//	openfabrica eval [--briefs testdata/eval/briefs/*.md] [--out eval-results]
//	                 [--model-id N] [--ambition medium] [--user-flows]
//	                 [--parallel 1] [--timeout 90m] [--server http://127.0.0.1:5734]
//	openfabrica eval collect <project-id>...   re-read finished projects
//	openfabrica eval compare <dirA> <dirB>     per-brief and total deltas
//
// The projects are kept, so every build can be opened and looked at afterwards.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"openfabrica/config"
	"openfabrica/db/models"
	"openfabrica/security"
)

// evalPollInterval is how often a running build's status is read.
const evalPollInterval = 10 * time.Second

// evalDefaultAnswer answers a planning question so an unattended build keeps going.
const evalDefaultAnswer = "Decide for me: pick the option that best fits the brief and keep going."

type evalOptions struct {
	server    string
	briefs    string
	out       string
	modelID   int
	ambition  string
	userFlows bool
	parallel  int
	timeout   time.Duration
}

func cmdEval(args []string) int {
	configPath, rest, err := splitSubArgs(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 2
	}
	if len(rest) > 0 {
		switch rest[0] {
		case "compare":
			if len(rest) != 3 {
				fmt.Fprintln(os.Stderr, "usage: openfabrica eval compare <dirA> <dirB>")
				return 2
			}
			return cmdEvalCompare(rest[1], rest[2], os.Stdout)
		case "collect":
			return cmdEvalCollect(configPath, rest[1:])
		}
	}

	fs := flag.NewFlagSet("eval", flag.ContinueOnError)
	var o evalOptions
	fs.StringVar(&o.server, "server", "", "admin URL of the running server (default http://127.0.0.1:<admin port>)")
	fs.StringVar(&o.briefs, "briefs", filepath.Join("testdata", "eval", "briefs", "*.md"), "glob of brief files")
	fs.StringVar(&o.out, "out", "eval-results", "directory for the results")
	fs.IntVar(&o.modelID, "model-id", 0, "llm_model_id for the projects (default: the system default model)")
	fs.StringVar(&o.ambition, "ambition", "medium", "ambition level: off, low, medium, high")
	fs.BoolVar(&o.userFlows, "user-flows", false, "turn user-flow tests on for the projects")
	fs.IntVar(&o.parallel, "parallel", 1, "builds to run at once")
	fs.DurationVar(&o.timeout, "timeout", 90*time.Minute, "give up on one build after this long")
	if err := fs.Parse(rest); err != nil {
		return 2
	}

	cfg, err := config.LoadFrom(configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	if o.server == "" {
		o.server = fmt.Sprintf("http://127.0.0.1:%d", cfg.AdminPort)
	}
	files, err := filepath.Glob(o.briefs)
	if err != nil || len(files) == 0 {
		fmt.Fprintf(os.Stderr, "error: no briefs match %q\n", o.briefs)
		return 1
	}
	sort.Strings(files)
	token, err := evalAdminToken(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	dir := filepath.Join(o.out, time.Now().Format("20060102-150405"))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}

	c := &evalClient{base: strings.TrimRight(o.server, "/"), token: token, http: &http.Client{Timeout: 60 * time.Second}}
	results := make([]evalResult, len(files))
	sem := make(chan struct{}, max(1, o.parallel))
	var wg sync.WaitGroup
	for i, f := range files {
		wg.Add(1)
		go func(i int, f string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results[i] = runEvalBrief(c, cfg.DataDir, f, o)
			writeJSONFile(filepath.Join(dir, strings.TrimSuffix(filepath.Base(f), filepath.Ext(f))+".json"), results[i])
			fmt.Fprintf(os.Stdout, "%s: %s\n", results[i].Brief, evalOneLine(results[i]))
		}(i, f)
	}
	wg.Wait()

	sum := summarize(results)
	writeJSONFile(filepath.Join(dir, "summary.json"), sum)
	fmt.Fprintf(os.Stdout, "\n%s\nResults in %s\n", renderSummary(sum), dir)
	return 0
}

// runEvalBrief builds one brief end to end and collects its result. Failures are
// recorded on the result rather than aborting the corpus.
func runEvalBrief(c *evalClient, dataDir, file string, o evalOptions) evalResult {
	r := evalResult{Brief: strings.TrimSuffix(filepath.Base(file), filepath.Ext(file))}
	raw, err := os.ReadFile(file)
	if err != nil {
		r.Error = err.Error()
		return r
	}
	brief, ambition := parseBriefFile(string(raw), o.ambition)
	id, err := c.createProject(brief, ambition, o.modelID, o.userFlows)
	if err != nil {
		r.Error = "create project: " + err.Error()
		return r
	}
	r.ProjectID = id
	pdb, err := openProjectReadOnly(dataDir, id)
	if err != nil {
		r.Error = "open project database: " + err.Error()
		return r
	}
	defer pdb.Close()

	ctx, cancel := context.WithTimeout(context.Background(), o.timeout)
	defer cancel()
	if err := waitForBuild(ctx, c, pdb); err != nil {
		r.Error = err.Error()
	}
	if err := collectResult(pdb, &r); err != nil && r.Error == "" {
		r.Error = "collect: " + err.Error()
	}
	return r
}

// waitForBuild polls the project's latest run until it ends, answering any planning
// questions on the way. A run that pauses for anything else (an owner stop, the daily
// budget) ends the wait: an unattended eval cannot resolve it.
func waitForBuild(ctx context.Context, c *evalClient, pdb *sql.DB) error {
	for {
		var status, reason string
		err := pdb.QueryRow(`SELECT status, COALESCE(pause_reason,'') FROM of_runs ORDER BY started_at DESC, id DESC LIMIT 1`).Scan(&status, &reason)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			// The build has not created its run yet.
		case err != nil:
			return err
		case status == "completed" || status == "failed":
			return nil
		case status == "paused" && reason == "awaiting_owner_answers":
			if err := answerPendingQuestions(c, pdb); err != nil {
				return fmt.Errorf("answering planning questions: %w", err)
			}
		case status == "paused":
			return fmt.Errorf("build paused: %s", reason)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("build did not finish in time")
		case <-time.After(evalPollInterval):
		}
	}
}

// answerPendingQuestions answers every pending question with the default answer;
// a secret is deferred, since an eval has none to give.
func answerPendingQuestions(c *evalClient, pdb *sql.DB) error {
	rows, err := pdb.Query(`SELECT id, COALESCE(type,'text') FROM of_questions WHERE status = 'pending'`)
	if err != nil {
		return err
	}
	type q struct {
		id   int
		kind string
	}
	var qs []q
	for rows.Next() {
		var x q
		if rows.Scan(&x.id, &x.kind) == nil {
			qs = append(qs, x)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, x := range qs {
		body := map[string]any{"answer": evalDefaultAnswer}
		if x.kind == "secret" {
			body = map[string]any{"deferred": true}
		}
		if err := c.post(fmt.Sprintf("/admin/api/questions/%d/answer", x.id), body, nil); err != nil {
			return err
		}
	}
	return nil
}

// parseBriefFile reads a brief, with an optional front-matter block setting the
// ambition for that brief ("---\nambition: high\n---").
func parseBriefFile(raw, defaultAmbition string) (brief, ambition string) {
	ambition = defaultAmbition
	raw = strings.TrimSpace(strings.ReplaceAll(raw, "\r\n", "\n"))
	if strings.HasPrefix(raw, "---\n") {
		if end := strings.Index(raw[4:], "\n---"); end >= 0 {
			for _, line := range strings.Split(raw[4:4+end], "\n") {
				if k, v, ok := strings.Cut(line, ":"); ok && strings.TrimSpace(k) == "ambition" {
					ambition = strings.TrimSpace(v)
				}
			}
			raw = strings.TrimSpace(raw[4+end+4:])
		}
	}
	return raw, ambition
}

// evalAdminToken mints an admin session token from the instance's own signing key,
// the way `reset-admin` works from the data directory: the eval runs on the machine
// that hosts the server, so it needs no password.
func evalAdminToken(cfg *config.Config) (string, error) {
	core, err := sql.Open("sqlite", "file:"+filepath.ToSlash(cfg.DBPath)+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return "", err
	}
	defer core.Close()
	users, err := models.ListUsers(core)
	if err != nil {
		return "", fmt.Errorf("reading users: %w", err)
	}
	for _, u := range users {
		if u.Role != "admin" {
			continue
		}
		version, err := models.GetUserTokenVersion(core, u.ID)
		if err != nil {
			return "", err
		}
		return security.NewJWTManager(cfg.JWTSecret, 12*time.Hour).Generate(security.TokenRequest{
			UserID: u.ID, Username: u.Username, Role: u.Role, TokenVersion: version,
		})
	}
	return "", errors.New("there is no admin user yet; open the admin page to run first-time setup")
}

// openProjectReadOnly opens a project database for reading only.
func openProjectReadOnly(dataDir string, id int) (*sql.DB, error) {
	path := filepath.Join(dataDir, "projects", fmt.Sprint(id), "project.db")
	for i := 0; i < 30; i++ { // the server creates it right after the project row
		if _, err := os.Stat(path); err == nil {
			break
		}
		time.Sleep(time.Second)
	}
	d, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	return d, d.Ping()
}

// cmdEvalCollect re-reads finished projects into results, without building anything.
func cmdEvalCollect(configPath string, ids []string) int {
	cfg, err := config.LoadFrom(configPath)
	if err != nil || len(ids) == 0 {
		fmt.Fprintln(os.Stderr, "usage: openfabrica eval collect <project-id>...")
		return 2
	}
	var results []evalResult
	for _, s := range ids {
		var id int
		if _, err := fmt.Sscan(s, &id); err != nil {
			fmt.Fprintf(os.Stderr, "error: %q is not a project id\n", s)
			return 2
		}
		r := evalResult{Brief: fmt.Sprintf("project-%d", id), ProjectID: id}
		pdb, err := openProjectReadOnly(cfg.DataDir, id)
		if err == nil {
			err = collectResult(pdb, &r)
			pdb.Close()
		}
		if err != nil {
			r.Error = err.Error()
		}
		results = append(results, r)
		fmt.Fprintf(os.Stdout, "%s: %s\n", r.Brief, evalOneLine(r))
	}
	fmt.Fprintf(os.Stdout, "\n%s\n", renderSummary(summarize(results)))
	return 0
}

// parseSQLTime reads a SQLite DATETIME as stored ("2006-01-02 15:04:05" or RFC 3339).
func parseSQLTime(s string) (time.Time, bool) {
	for _, layout := range []string{"2006-01-02 15:04:05", time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, strings.TrimSpace(s)); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

func writeJSONFile(path string, v any) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err == nil {
		err = os.WriteFile(path, append(b, '\n'), 0o644)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: writing %s: %v\n", path, err)
	}
}

// evalClient is the admin API client.
type evalClient struct {
	base  string
	token string
	http  *http.Client
}

func (c *evalClient) post(path string, body, out any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, c.base+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s: HTTP %d: %s", path, resp.StatusCode, strings.TrimSpace(string(data)))
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

// createProject creates a project from a brief; the server starts its build.
func (c *evalClient) createProject(brief, ambition string, modelID int, userFlows bool) (int, error) {
	body := map[string]any{"description": brief, "ambition_level": ambition, "user_flows": userFlows}
	if modelID > 0 {
		body["llm_model_id"] = modelID
	}
	var out struct {
		ID int `json:"id"`
	}
	if err := c.post("/admin/api/projects", body, &out); err != nil {
		return 0, err
	}
	if out.ID == 0 {
		return 0, errors.New("server returned no project id")
	}
	return out.ID, nil
}
