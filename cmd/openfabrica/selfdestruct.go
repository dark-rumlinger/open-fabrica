package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"openfabrica/bot"
	"openfabrica/config"
	"openfabrica/db"
	"openfabrica/engine"
	"openfabrica/internal/firewall"
	"openfabrica/internal/selfdestruct"
	"openfabrica/jobs"
	"openfabrica/tlsproxy"
)

// firewallRemoveTimeout bounds the rule removal, which may wait on an elevation
// prompt that nobody is present to answer.
const firewallRemoveTimeout = 60 * time.Second

// selfDestructPlan resolves what an erase would touch for this configuration.
func selfDestructPlan(cfg *config.Config, configPath string) selfdestruct.Plan {
	logFile := cfg.LogFile
	if logFile != "" && !filepath.IsAbs(logFile) {
		logFile = filepath.Join(cfg.DataDir, logFile)
	}
	return selfdestruct.DefaultPlan(cfg.DataDir, cfg.DBPath, logFile, configPath)
}

// selfDestructDeps carries the process-wide handles wireSelfDestruct needs: the bot it
// arms, the config and its path that define what an erase removes, and the shutdown
// handles the erase stops in the same order a normal shutdown does.
type selfDestructDeps struct {
	botService    *bot.BotService
	cfg           *config.Config
	configPath    string
	cancelActions context.CancelFunc
	tlsMgr        *tlsproxy.Manager
	netHelpers    *networkHelpers
	engineMgr     *engine.Engine
	jobsMgr       *jobs.Manager
	database      *db.DB
	projectDBMgr  *db.ProjectDBManager
}

// wireSelfDestruct gives the Telegram bot the ability to erase this installation.
func wireSelfDestruct(d selfDestructDeps) {
	plan := selfDestructPlan(d.cfg, d.configPath)
	run := func() bot.SelfDestructReport {
		rep := selfdestruct.Run(context.Background(), plan, selfdestruct.Actions{
			Quiesce: func() {
				gracefulShutdown(d.cancelActions, d.tlsMgr, d.netHelpers, d.engineMgr, d.jobsMgr)
				d.projectDBMgr.CloseAll()
				_ = d.database.Close()
			},
			RemoveFirewall: removeFirewallRule,
		})
		out := bot.SelfDestructReport{Container: plan.Docker}
		for _, s := range rep.Failed() {
			out.Failed = append(out.Failed, fmt.Sprintf("%s: %v", s.Name, s.Err))
		}
		return out
	}
	d.botService.SetSelfDestruct(run, func() { os.Exit(0) })
}

// removeFirewallRule deletes the OS rule only when it exists, so a machine that
// never had one is not asked for elevation.
func removeFirewallRule(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, firewallRemoveTimeout)
	defer cancel()
	mgr := firewall.New()
	status, err := mgr.Detect(ctx, firewall.Rule{Name: firewall.RuleName})
	if err != nil || !status.RulePresent {
		return nil
	}
	res, err := mgr.Remove(ctx, firewall.RuleName)
	if err != nil {
		return err
	}
	if !res.OK {
		if res.Cancelled {
			return fmt.Errorf("elevation was declined")
		}
		return fmt.Errorf("%s", res.Error)
	}
	return nil
}

// cmdSelfDestruct lists what an erase would remove. It never deletes anything:
// the erase itself is only reachable from the Telegram bot.
func cmdSelfDestruct(args []string) int {
	configPath, rest, err := splitSubArgs(args)
	if err != nil || len(rest) != 1 || rest[0] != "--dry-run" {
		fmt.Fprintln(os.Stderr, "usage: openfabrica selfdestruct --dry-run [--config <path>]")
		return 2
	}
	cfg, err := config.LoadFrom(configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	fmt.Fprintln(os.Stdout, "An erase from Telegram would remove, in this order:")
	for _, line := range selfDestructPlan(cfg, configPath).Describe() {
		fmt.Fprintln(os.Stdout, "  "+line)
	}
	return 0
}
