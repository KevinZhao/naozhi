package main

import (
	"context"
	"fmt"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/cli/backend"
	"github.com/naozhi/naozhi/internal/config"
	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/transcribe"
)

// doctorConfig memoises the run's single config.Load: each Load re-emits its
// fallback diags, so loading once per check would repeat them on stderr.
type doctorConfig struct {
	cfg *config.Config
	err error
}

// loadConfig returns the config at d.configPath, loading it once per run.
func (d *doctor) loadConfig() (*config.Config, error) {
	if d.config == nil {
		cfg, err := config.Load(d.configPath)
		if err == nil && cfg == nil {
			err = fmt.Errorf("config %s loaded as nil", d.configPath)
		}
		d.config = &doctorConfig{cfg: cfg, err: err}
	}
	return d.config.cfg, d.config.err
}

// checkCLIBackends runs the `<path> --version` probe startup runs on every
// configured backend. A failed default is a fail (startup exits when no
// sibling answers; otherwise every default-bound spawn errors), a failed
// sibling is a warn. Paths resolve as the invoking user, not the service user.
func (d *doctor) checkCLIBackends() {
	cfg, err := d.loadConfig()
	if err != nil {
		d.add("cli backend", "pass", "skipped (config not loaded; see `naozhi config check`)")
		return
	}
	backend.EnsureDefaults()
	defaultID := cfg.DefaultBackendID()
	type result struct {
		id, path, version string
		known             bool
	}
	var results []result
	anyHealthy := false
	for _, b := range cfg.EnabledBackends() {
		profile, ok := backend.Get(b.ID)
		if !ok {
			results = append(results, result{id: b.ID, path: b.Path})
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), d.timeout)
		w := cli.NewWrapperLazy(b.Path, profile.NewProtocol(backend.ProtocolDeps{}), b.ID)
		v := w.Probe(ctx)
		cancel()
		anyHealthy = anyHealthy || v != ""
		results = append(results, result{id: b.ID, path: w.CLIPath, version: v, known: true})
	}
	for _, r := range results {
		category := "cli backend " + r.id
		path := r.path
		if path == "" {
			path = "(no path configured and none found on the install paths / $PATH)"
		}
		switch {
		case !r.known:
			d.add(category, "warn", "not a registered backend id — startup skips this entry")
		case r.version != "":
			d.add(category, "pass", r.version+" at "+path)
		case r.id != defaultID:
			d.add(category, "warn", "--version failed at "+path+" — sessions on this backend cannot start")
		case anyHealthy:
			d.add(category, "fail", "default backend: --version failed at "+path+" — startup continues on a sibling but default-bound sessions cannot start")
		default:
			d.add(category, "fail", "default backend: --version failed at "+path+" — startup will refuse to run")
		}
	}
}

// checkTranscribe checks what voice transcription needs beyond the config: an
// AWS credential chain and ffmpeg. Both warn rather than fail — startup runs
// without them and only voice messages break — and both resolve as the
// invoking user, whose environment can differ from the service's.
func (d *doctor) checkTranscribe() {
	cfg, err := d.loadConfig()
	switch {
	case err != nil:
		d.add("transcribe creds", "pass", "skipped (config not loaded)")
		d.add("transcribe ffmpeg", "pass", "skipped (config not loaded)")
		return
	case cfg.Transcribe == nil || !cfg.Transcribe.Enabled:
		d.add("transcribe creds", "pass", "skipped (transcribe disabled)")
		d.add("transcribe ffmpeg", "pass", "skipped (transcribe disabled)")
		return
	}
	checkCreds := d.awsCredentials
	if checkCreds == nil {
		checkCreds = transcribe.CheckCredentials
	}
	ctx, cancel := context.WithTimeout(context.Background(), d.timeout)
	defer cancel()
	if source, err := checkCreds(ctx, cfg.Transcribe.Region); err != nil {
		d.add("transcribe creds", "warn", "no AWS credentials for the invoking user: "+osutil.SanitizeForLog(err.Error(), 300)+" — voice messages fail if the service user has none either")
	} else {
		d.add("transcribe creds", "pass", "AWS credentials from "+source)
	}

	ffmpegPath := d.ffmpegPath
	if ffmpegPath == nil {
		ffmpegPath = transcribe.FFmpegPath
	}
	if path, err := ffmpegPath(); err != nil {
		d.add("transcribe ffmpeg", "warn", "ffmpeg not found for the invoking user ("+err.Error()+") — voice formats other than ogg/flac/pcm cannot be transcribed")
	} else {
		d.add("transcribe ffmpeg", "pass", path)
	}
}
