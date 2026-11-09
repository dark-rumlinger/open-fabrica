package probe

import "testing"

func TestPageErrorCollectorKeepsABoundedConsoleTail(t *testing.T) {
	c := &pageErrorCollector{}
	c.addLog("log", "first")
	c.addLog("log", "first") // repeated back to back: kept once
	c.addLog("error", "an error is not a log line")
	c.addLog("table", "ignored type")
	c.addLog("warning", "second")
	got := c.recentLogs(10)
	if len(got) != 2 || got[0] != "[log] first" || got[1] != "[warning] second" {
		t.Fatalf("want the two distinct log lines, got %v", got)
	}
	for i := 0; i < maxConsoleLogs+20; i++ {
		c.addLog("log", "line "+string(rune('a'+i%26))+string(rune('A'+i/26)))
	}
	if n := len(c.recentLogs(1000)); n != maxConsoleLogs {
		t.Fatalf("the tail must be capped at %d, got %d", maxConsoleLogs, n)
	}
	if n := len(c.recentLogs(5)); n != 5 {
		t.Fatalf("recentLogs(5) must return 5 lines, got %d", n)
	}
}
