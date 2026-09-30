package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/naozhi/naozhi/internal/ccmodels"
	"github.com/naozhi/naozhi/internal/ccmodels/ccprobe"
	"github.com/naozhi/naozhi/internal/claudefs"
	"github.com/naozhi/naozhi/internal/config"
	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/session"
)

// seedClaudeModelManifest fills the claude backend's configured model list from
// the settings file cc will actually enforce, for every claude backend that
// declared none in config.
//
// Without this the dashboard popover has nothing to offer until the first
// process is live, which would make deleting cli.backends[].models a
// cold-start regression. An operator-declared list still wins.
func seedClaudeModelManifest(runtimes map[string]session.BackendRuntime, settingsFile, claudeDir string) {
	path := settingsFile
	if path == "" {
		if claudeDir == "" {
			return
		}
		path = filepath.Join(claudeDir, "settings.json")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var doc struct {
		AvailableModels []string `json:"availableModels"`
	}
	if json.Unmarshal(raw, &doc) != nil || len(doc.AvailableModels) == 0 {
		return
	}
	for id, rt := range runtimes {
		if id != "claude" && id != "" {
			continue
		}
		if len(rt.ConfiguredModels) > 0 {
			continue
		}
		rt.ConfiguredModels = doc.AvailableModels
		runtimes[id] = rt
	}
}

// runModels dispatches `naozhi models <subcommand>`.
func runModels(args []string) {
	if len(args) == 0 || args[0] != "sync" {
		fmt.Fprintln(os.Stderr, "usage: naozhi models sync [-config config.yaml] [-write] [-quick] [-no-probe]")
		os.Exit(2)
	}
	fs, configPath := newSubFlagSet("models sync", "config.yaml")
	write := fs.Bool("write", false, "persist the plan (default: print the diff only)")
	quick := fs.Bool("quick", false, "skip the cc turn stage; check profile permissions only")
	noProbe := fs.Bool("no-probe", false, "trust the recommendation as-is; run no calls at all")
	if err := fs.Parse(args[1:]); err != nil {
		os.Exit(2)
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "models sync: load config: %v\n", err)
		os.Exit(1)
	}
	if err := syncModels(cfg, modelsSyncOptions{
		Write:   *write,
		Quick:   *quick,
		NoProbe: *noProbe,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "models sync: %v\n", err)
		os.Exit(1)
	}
}

type modelsSyncOptions struct {
	Write, Quick, NoProbe bool
}

// syncTarget is one settings file the model list is written into.
type syncTarget struct {
	label string
	path  string
	perm  os.FileMode
}

// syncModels reconciles the toolbox recommendation into every settings file
// naozhi and the operator's cc read, printing the diff and only writing when
// asked.
func syncModels(cfg *config.Config, opt modelsSyncOptions) error {
	claudeDir := claudefs.DefaultDir()
	if claudeDir == "" {
		return fmt.Errorf("cannot resolve the Claude home directory")
	}
	snapPath := filepath.Join(claudeDir, ccmodels.SnapshotPath)
	raw, err := os.ReadFile(snapPath)
	if err != nil {
		return fmt.Errorf("read recommendation snapshot %s: %w", snapPath, err)
	}
	snap, err := ccmodels.ParseSnapshot(raw)
	if err != nil {
		return err
	}
	if len(snap.Aliases) == 0 {
		return fmt.Errorf("%s recommends no models; leaving every target alone", snapPath)
	}
	fmt.Printf("推荐清单 %s：%d 个别名，region %s\n", snapPath, len(snap.Aliases), snap.Region)

	targets, err := modelSyncTargets(cfg, claudeDir)
	if err != nil {
		return err
	}
	local, err := os.ReadFile(targets[0].path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read %s: %w", targets[0].path, err)
	}

	verdicts := probeAliases(cfg, snap, local, opt)
	plan := ccmodels.BuildPlan(snap, verdicts)
	reportPlan(plan)
	if plan.Empty() {
		return fmt.Errorf("the plan offers no models; refusing to leave cc with an empty allowlist")
	}
	return applyPlan(targets, plan, opt.Write)
}

// modelSyncTargets lists the settings files to reconcile, local cc first — it
// doubles as the base document the probe stage derives its own settings from.
func modelSyncTargets(cfg *config.Config, claudeDir string) ([]syncTarget, error) {
	targets := []syncTarget{{
		label: "本机交互 cc",
		path:  filepath.Join(claudeDir, "settings.json"),
		perm:  0o644,
	}}
	if !cfg.NaozhiSettings.Enabled {
		fmt.Println("naozhi_settings 未启用：naozhi 的 backend 直接读本机 settings，只同步一处。")
		return targets, nil
	}
	path, err := naozhiSettingsPath(cfg, osutil.ExpandHome(cfg.Session.StorePath))
	if err != nil {
		return nil, fmt.Errorf("resolve naozhi settings path: %w", err)
	}
	return append(targets, syncTarget{label: "naozhi backend", path: path, perm: 0o600}), nil
}

// probeAliases runs the probe unless asked not to. Every failure here is
// reported and then ignored: BuildPlan keeps undecided aliases, so a probe
// outage cannot shrink the list.
func probeAliases(cfg *config.Config, snap ccmodels.Snapshot, base []byte, opt modelsSyncOptions) map[string]ccmodels.Verdict {
	if opt.NoProbe {
		fmt.Println("-no-probe：直接采用推荐清单，不做任何实跑验证。")
		return nil
	}
	candidates := ccmodels.Candidates(snap)
	p := &ccprobe.Prober{
		Region:       snap.Region,
		BaseSettings: base,
		SkipCC:       opt.Quick,
		Observe:      func(v ccmodels.Verdict) { fmt.Println("  " + ccprobe.Describe(v)) },
	}
	if !opt.Quick {
		p.CLIPath = claudeBackendPath(cfg)
	}
	ctx := context.Background()
	if snap.CredExport == "" {
		fmt.Println("推荐清单没有 awsCredentialExport：跳过 Converse 阶段。")
	} else if creds, err := ccprobe.ExportCredentials(ctx, snap.CredExport); err != nil {
		fmt.Printf("取凭证失败，跳过 Converse 阶段：%v\n", err)
	} else {
		p.Creds = creds
	}
	fmt.Printf("验证 %d 个候选别名，预计 %s：\n", len(candidates),
		ccprobe.EstimateDuration(len(candidates), opt.Quick).Round(time.Second))
	return p.Run(ctx, candidates)
}

// claudeBackendPath returns the cc binary to probe with: the claude backend's
// own path, so the probe exercises the binary naozhi actually spawns.
func claudeBackendPath(cfg *config.Config) string {
	for _, b := range cfg.EnabledBackends() {
		if b.ID == "claude" && b.Path != "" {
			return osutil.ExpandHome(b.Path)
		}
	}
	return osutil.ExpandHome(cfg.CLI.Path)
}

// reportPlan prints what the probe concluded, before any file is touched.
func reportPlan(plan ccmodels.Plan) {
	fmt.Printf("\n计划提供 %d 个模型：%s\n", len(plan.Aliases), strings.Join(plan.Available(), ", "))
	for _, r := range plan.Repaired {
		fmt.Printf("  改写拼写 %s -> %s（别名原样发不到 profile）\n", r.From, r.To)
	}
	for _, v := range plan.Excluded {
		fmt.Printf("  排除 %s（%s：%s）\n", v.Alias, v.Status, v.Detail)
	}
	for _, w := range plan.Warnings {
		fmt.Printf("  注意 %s\n", w)
	}
}

// applyPlan prints each target's diff, and writes it when write is set. A write
// backs the file up first, so a bad plan is one `mv` away from undone.
func applyPlan(targets []syncTarget, plan ccmodels.Plan, write bool) error {
	changed := 0
	for _, t := range targets {
		cur, err := os.ReadFile(t.path)
		if err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("read %s: %w", t.path, err)
		}
		diff, err := ccmodels.DiffSettings(cur, plan)
		if err != nil {
			return fmt.Errorf("%s: %w", t.path, err)
		}
		fmt.Printf("\n%s  %s\n", t.label, t.path)
		if diff.Empty() {
			fmt.Println("  已经一致。")
			continue
		}
		changed++
		printDiff(diff)
		if !write {
			continue
		}
		if err := writeTarget(t, cur, plan); err != nil {
			return err
		}
	}
	if changed == 0 {
		return nil
	}
	if !write {
		fmt.Println("\n以上是 dry-run；加 -write 才落盘。")
	}
	return nil
}

func printDiff(diff ccmodels.Diff) {
	for _, a := range diff.Added {
		fmt.Printf("  + %s\n", a)
	}
	for _, a := range diff.Removed {
		fmt.Printf("  - %s\n", a)
	}
	for _, r := range diff.Remapped {
		fmt.Printf("  ~ %s: %s -> %s\n", r.Alias, r.From, r.To)
	}
	if diff.PickerRows {
		fmt.Println("  ~ modelPicker: 重写 /model 选单行（每个别名一行，替换 cc 的 per-family lineup）")
	}
}

// writeTarget backs up an existing file, then atomically writes the patched
// document. A missing file needs no backup.
//
// An existing file keeps its own mode: these files may hold a token, and a sync
// has no business widening permissions the operator chose.
func writeTarget(t syncTarget, cur []byte, plan ccmodels.Plan) error {
	doc, err := ccmodels.PatchSettings(cur, plan)
	if err != nil {
		return fmt.Errorf("%s: %w", t.path, err)
	}
	perm := t.perm
	if fi, err := os.Stat(t.path); err == nil {
		perm = fi.Mode().Perm()
	}
	if len(cur) > 0 {
		backup := fmt.Sprintf("%s.bak-%s", t.path, time.Now().Format("20060102-150405"))
		if err := os.WriteFile(backup, cur, perm); err != nil {
			return fmt.Errorf("back up %s: %w", t.path, err)
		}
		fmt.Printf("  备份 %s\n", backup)
	}
	if err := osutil.WriteFileAtomic(t.path, doc, perm); err != nil {
		return fmt.Errorf("write %s: %w", t.path, err)
	}
	fmt.Println("  已写入。")
	return nil
}
