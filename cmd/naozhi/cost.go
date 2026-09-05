package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/naozhi/naozhi/internal/config"
	"github.com/naozhi/naozhi/internal/costledger"
	"github.com/naozhi/naozhi/internal/cron"
	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/session/runhistory"
	"github.com/naozhi/naozhi/internal/sessionkey"
)

// runCost dispatches `naozhi cost <subcommand>`.
func runCost(args []string) {
	if len(args) == 0 || args[0] != "backfill" {
		fmt.Fprintln(os.Stderr, "usage: naozhi cost backfill [-config config.yaml] [-dry-run]")
		os.Exit(2)
	}
	fs := flag.NewFlagSet("cost backfill", flag.ExitOnError)
	configPath := fs.String("config", "config.yaml", "path to config.yaml")
	dryRun := fs.Bool("dry-run", false, "report what would be imported without writing")
	if err := fs.Parse(args[1:]); err != nil {
		os.Exit(2)
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cost backfill: load config: %v\n", err)
		os.Exit(1)
	}
	rep, err := backfillLedger(backfillPaths{
		SessionStorePath: osutil.ExpandHome(cfg.Session.StorePath),
		CronStorePath:    osutil.ExpandHome(cfg.Cron.StorePath),
		Cost:             cfg.Cost,
	}, *dryRun, os.Stdout)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cost backfill: %v\n", err)
		os.Exit(1)
	}
	if !*dryRun && rep.Imported > 0 {
		fmt.Println("导入的历史记录落在过去的日期文件里；重启 naozhi 后内存汇总才会包含它们。")
	}
}

type backfillPaths struct {
	SessionStorePath string
	CronStorePath    string
	Cost             config.CostConfig
}

// backfillReport counts what the import did.
type backfillReport struct {
	SessionRuns, CronLocal, CronSandbox                            int
	Imported, Duplicate, SkippedCumulative, SkippedOld, Unreadable int
}

// backfillLedger imports historical run costs (session run-history and cron
// run records) as Kind=backfill entries, skipping runs the ledger already
// holds. Persistent-mode cron records predating the ledger carry the CLI's
// cumulative figure, not a per-run increment, and are skipped rather than
// imported wrong. Safe beside a running naozhi: both open the day file with
// O_APPEND and write whole batches per write(2), so lines never interleave.
func backfillLedger(p backfillPaths, dryRun bool, out io.Writer) (backfillReport, error) {
	var rep backfillReport
	if p.SessionStorePath == "" {
		return rep, fmt.Errorf("session.store_path is empty; the ledger has no home")
	}
	if !p.Cost.IsEnabled() {
		return rep, fmt.Errorf("cost.enabled is false")
	}
	ledgerDir := filepath.Join(filepath.Dir(p.SessionStorePath), "cost")
	store := costledger.NewStore(ledgerDir, costledger.Options{RetentionDays: p.Cost.RetentionDays, RollupDays: p.Cost.RollupDays})
	if !store.Enabled() {
		return rep, fmt.Errorf("open ledger at %s", ledgerDir)
	}
	defer store.Close()

	retention := p.Cost.RetentionDays
	if retention <= 0 {
		retention = costledger.DefaultRetentionDays
	}
	now := time.Now()
	cutoff := now.Add(-time.Duration(retention) * 24 * time.Hour)
	known := map[string]bool{}
	err := store.Scan(costledger.Query{From: cutoff, To: now.Add(24 * time.Hour), AllowFullRange: true}, func(e costledger.Entry) bool {
		if e.RunID != "" {
			known[e.RunID] = true
		}
		return true
	})
	if err != nil {
		return rep, fmt.Errorf("scan ledger: %w", err)
	}

	emit := func(e costledger.Entry) {
		if e.TS.Before(cutoff) {
			rep.SkippedOld++
			return
		}
		if known[e.RunID] {
			rep.Duplicate++
			return
		}
		known[e.RunID] = true
		rep.Imported++
		if !dryRun {
			store.Append(e)
		}
	}

	sessionRunsDir := filepath.Join(filepath.Dir(p.SessionStorePath), "session-runs")
	walkJSON(sessionRunsDir, &rep, func(raw []byte) {
		var r runhistory.SessionRun
		if json.Unmarshal(raw, &r) != nil || r.RunID == "" {
			rep.Unreadable++
			return
		}
		rep.SessionRuns++
		// cron sessions' turns are imported from cron runs/ with their run
		// owner's Source; the session-runs copy would mislabel them.
		if r.CostUSD <= 0 || sessionkey.IsCronKey(r.SessionKey) {
			return
		}
		emit(costledger.Entry{TS: r.StartedAt, Source: costledger.SourceSession, Kind: costledger.KindBackfill,
			SessionKey: r.SessionKey, RunID: r.RunID, Backend: "claude", Unit: costledger.UnitUSD, Amount: r.CostUSD})
	})

	if p.CronStorePath != "" {
		cronRunsDir := filepath.Join(filepath.Dir(p.CronStorePath), "runs")
		walkJSON(cronRunsDir, &rep, func(raw []byte) {
			var r cron.CronRun
			if json.Unmarshal(raw, &r) != nil || r.RunID == "" {
				rep.Unreadable++
				return
			}
			base := costledger.Entry{TS: r.StartedAt, Kind: costledger.KindBackfill, JobID: r.JobID, RunID: r.RunID,
				Workspace: filepath.Base(r.WorkDir), Backend: "claude", Unit: costledger.UnitUSD}
			if base.Workspace == "." || base.Workspace == string(filepath.Separator) {
				base.Workspace = ""
			}
			switch {
			case r.SandboxMeta != nil && r.SandboxMeta.CostUSD > 0:
				rep.CronSandbox++
				base.Source, base.Amount = costledger.SourceCronSandbox, r.SandboxMeta.CostUSD
				emit(base)
			case r.CostUSD > 0 && r.Fresh:
				rep.CronLocal++
				base.Source, base.Amount = costledger.SourceCronLocal, r.CostUSD
				emit(base)
			case r.CostUSD > 0:
				rep.SkippedCumulative++
			}
		})
	}

	mode := "已导入"
	if dryRun {
		mode = "将导入（dry-run）"
	}
	fmt.Fprintf(out, "session run 记录 %d 条，cron 本地 run %d 条，云沙箱 run %d 条\n", rep.SessionRuns, rep.CronLocal, rep.CronSandbox)
	fmt.Fprintf(out, "%s %d 条；已存在跳过 %d；persistent cron 累计值跳过 %d；超出保留期跳过 %d；不可读 %d\n",
		mode, rep.Imported, rep.Duplicate, rep.SkippedCumulative, rep.SkippedOld, rep.Unreadable)
	return rep, nil
}

// walkJSON feeds every *.json file two levels below root to fn; a missing
// root is not an error (nothing to import).
func walkJSON(root string, rep *backfillReport, fn func([]byte)) {
	dirs, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		files, err := os.ReadDir(filepath.Join(root, d.Name()))
		if err != nil {
			rep.Unreadable++
			continue
		}
		for _, f := range files {
			if f.IsDir() || filepath.Ext(f.Name()) != ".json" {
				continue
			}
			raw, err := os.ReadFile(filepath.Join(root, d.Name(), f.Name()))
			if err != nil {
				rep.Unreadable++
				continue
			}
			fn(raw)
		}
	}
}
