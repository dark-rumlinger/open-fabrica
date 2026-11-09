package api

import (
	"strings"
	"testing"

	"openfabrica/tools"
	"openfabrica/tools/tooltest"
)

// An empty string arg means "leave unchanged" on update, so a binding or allowlist
// that could only ever be replaced, never removed, needs an explicit sentinel.

func TestLLMProxyUpdate_UnbindsKnowledgeBase(t *testing.T) {
	db := tooltest.ProjectDB(t)
	ctx := &tools.ToolContext{DB: db}

	if res, err := (&LLMProxyCreate{EndpointPath: "docs", KnowledgeBase: "manual"}).Run(ctx); err != nil || !res.Success {
		t.Fatalf("create: err=%v res=%+v", err, res)
	}
	bound := func() string {
		var kb string
		if err := db.QueryRow("SELECT knowledge_base FROM of_llm_endpoints WHERE path = 'docs'").Scan(&kb); err != nil {
			t.Fatalf("read knowledge_base: %v", err)
		}
		return kb
	}
	if got := bound(); got != "manual" {
		t.Fatalf("knowledge_base after create = %q, want manual", got)
	}

	if res, err := (&LLMProxyUpdate{EndpointPath: "docs", KnowledgeBase: "none"}).Run(ctx); err != nil || !res.Success {
		t.Fatalf("unbind: err=%v res=%+v", err, res)
	}
	if got := bound(); got != "" {
		t.Errorf("knowledge_base after unbind = %q, want empty", got)
	}
}

func TestLLMProxyList_ReportsKnowledgeBase(t *testing.T) {
	db := tooltest.ProjectDB(t)
	ctx := &tools.ToolContext{DB: db}
	if res, err := (&LLMProxyCreate{EndpointPath: "docs", KnowledgeBase: "manual"}).Run(ctx); err != nil || !res.Success {
		t.Fatalf("create: err=%v res=%+v", err, res)
	}
	res, err := epListLLM(ctx)
	if err != nil || !res.Success {
		t.Fatalf("list: err=%v res=%+v", err, res)
	}
	data, _ := res.Data.(map[string]any)
	eps, _ := data["endpoints"].([]map[string]any)
	if len(eps) != 1 || eps[0]["knowledge_base"] != "manual" {
		t.Fatalf("list did not report the binding: %+v", data)
	}
}

func TestAgentUpdate_NoneClearsFunctionsAndCORS(t *testing.T) {
	db := tooltest.ProjectDB(t)
	ctx := &tools.ToolContext{DB: db}

	if res, err := (&AgentCreate{EndpointPath: "helper", Functions: "lookup_order", CORSOrigins: "https://a.example"}).Run(ctx); err != nil || !res.Success {
		t.Fatalf("create: err=%v res=%+v", err, res)
	}
	read := func() (fns, cors string) {
		if err := db.QueryRow("SELECT functions, cors_origins FROM of_agent_endpoints WHERE path = 'helper'").Scan(&fns, &cors); err != nil {
			t.Fatalf("read agent: %v", err)
		}
		return
	}
	if fns, cors := read(); fns == "" || cors == "" {
		t.Fatalf("setup: functions=%q cors=%q", fns, cors)
	}

	res, err := (&AgentUpdate{EndpointPath: "helper", Functions: "none", CORSOrigins: "none"}).Run(ctx)
	if err != nil || !res.Success {
		t.Fatalf("update: err=%v res=%+v", err, res)
	}
	if fns, cors := read(); fns != "" || cors != "" {
		t.Errorf("after none: functions=%q cors=%q, want both empty", fns, cors)
	}
}

func TestAgentCreate_WarnsOnMissingFunction(t *testing.T) {
	db := tooltest.ProjectDB(t)
	ctx := &tools.ToolContext{DB: db}

	res, err := (&AgentCreate{EndpointPath: "helper", Functions: "no_such_function"}).Run(ctx)
	if err != nil || !res.Success {
		t.Fatalf("create: err=%v res=%+v", err, res)
	}
	data, _ := res.Data.(map[string]any)
	warn, _ := data["warning"].(string)
	if !strings.Contains(warn, "no_such_function") {
		t.Errorf("create did not warn about the missing function: %+v", data)
	}
}
