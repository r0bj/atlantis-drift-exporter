package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const (
	defaultListenAddress   = ":8080"
	defaultVCSType         = "Github"
	defaultRefreshInterval = 5 * time.Minute
	defaultRefreshTimeout  = 30 * time.Second
	maxResponseBytes       = 10 << 20
	shutdownTimeout        = 5 * time.Second
)

var supportedVCSTypes = []string{"Github", "Gitlab", "Gitea"}

type config struct {
	AtlantisURL     string
	APISecret       string
	Repositories    []string
	VCSType         string
	Ref             string
	ListenAddr      string
	RefreshInterval time.Duration
	RefreshTimeout  time.Duration
}

type apiEnvelope struct {
	Success bool          `json:"success"`
	Data    *driftStatus  `json:"data"`
	Error   *apiErrorBody `json:"error"`
}

type apiErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type driftStatus struct {
	Repository string         `json:"repository"`
	Projects   []driftProject `json:"projects"`
}

type driftProject struct {
	ProjectName string        `json:"project_name"`
	Directory   string        `json:"directory"`
	Workspace   string        `json:"workspace"`
	Ref         string        `json:"ref"`
	BaseBranch  string        `json:"base_branch"`
	HasDrift    bool          `json:"has_drift"`
	Drift       *driftDetails `json:"drift"`
	LastChecked time.Time     `json:"last_checked"`
	Error       string        `json:"error"`
}

type driftDetails struct {
	ToAdd     int `json:"to_add"`
	ToChange  int `json:"to_change"`
	ToDestroy int `json:"to_destroy"`
	ToImport  int `json:"to_import"`
	ToForget  int `json:"to_forget"`
}

func newConfigFromEnv() (*config, error) {
	cfg := &config{
		AtlantisURL:     strings.TrimRight(strings.TrimSpace(os.Getenv("ATLANTIS_URL")), "/"),
		APISecret:       strings.TrimSpace(os.Getenv("ATLANTIS_API_SECRET")),
		VCSType:         strings.TrimSpace(os.Getenv("ATLANTIS_VCS_TYPE")),
		Ref:             strings.TrimSpace(os.Getenv("ATLANTIS_REF")),
		ListenAddr:      strings.TrimSpace(os.Getenv("LISTEN_ADDRESS")),
		RefreshInterval: defaultRefreshInterval,
		RefreshTimeout:  defaultRefreshTimeout,
	}

	if cfg.VCSType == "" {
		cfg.VCSType = defaultVCSType
	}
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = defaultListenAddress
	}

	for name, target := range map[string]*time.Duration{
		"REFRESH_INTERVAL": &cfg.RefreshInterval,
		"REFRESH_TIMEOUT":  &cfg.RefreshTimeout,
	} {
		value := strings.TrimSpace(os.Getenv(name))
		if value == "" {
			continue
		}
		d, err := time.ParseDuration(value)
		if err != nil {
			return nil, fmt.Errorf("invalid %s: %w", name, err)
		}
		if d <= 0 {
			return nil, fmt.Errorf("%s must be positive", name)
		}
		*target = d
	}
	if cfg.RefreshTimeout > cfg.RefreshInterval {
		return nil, errors.New("REFRESH_TIMEOUT must not exceed REFRESH_INTERVAL")
	}

	for repository := range strings.SplitSeq(os.Getenv("ATLANTIS_REPOSITORIES"), ",") {
		repository = strings.TrimSpace(repository)
		if repository != "" && !slices.Contains(cfg.Repositories, repository) {
			cfg.Repositories = append(cfg.Repositories, repository)
		}
	}

	if cfg.AtlantisURL == "" {
		return nil, errors.New("missing ATLANTIS_URL")
	}
	parsed, err := url.Parse(cfg.AtlantisURL)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, errors.New("ATLANTIS_URL must be an absolute http or https URL")
	}
	if cfg.APISecret == "" {
		return nil, errors.New("missing ATLANTIS_API_SECRET")
	}
	if len(cfg.Repositories) == 0 {
		return nil, errors.New("missing ATLANTIS_REPOSITORIES")
	}
	if !slices.Contains(supportedVCSTypes, cfg.VCSType) {
		return nil, fmt.Errorf("invalid ATLANTIS_VCS_TYPE %q: must be one of %s", cfg.VCSType, strings.Join(supportedVCSTypes, ", "))
	}

	return cfg, nil
}

type atlantisClient struct {
	baseURL   string
	apiSecret string
	vcsType   string
	ref       string
	client    *http.Client
}

func newAtlantisClient(cfg *config) *atlantisClient {
	return &atlantisClient{
		baseURL:   cfg.AtlantisURL,
		apiSecret: cfg.APISecret,
		vcsType:   cfg.VCSType,
		ref:       cfg.Ref,
		client: &http.Client{
			Timeout: cfg.RefreshTimeout,
			// Go keeps custom headers on redirects, which would leak the API token.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

func (c *atlantisClient) driftStatus(ctx context.Context, repository string) (*driftStatus, error) {
	query := url.Values{
		"repository": {repository},
		"type":       {c.vcsType},
	}
	if c.ref != "" {
		query.Set("ref", c.ref)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/api/drift/status?"+query.Encode(), nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("X-Atlantis-Token", c.apiSecret)
	req.Header.Set("Accept", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if len(body) > maxResponseBytes {
		return nil, fmt.Errorf("response exceeds %d bytes", maxResponseBytes)
	}

	var envelope apiEnvelope
	decodeErr := json.Unmarshal(body, &envelope)

	if resp.StatusCode != http.StatusOK {
		if decodeErr == nil && envelope.Error != nil {
			return nil, fmt.Errorf("unexpected status %d: %s: %s", resp.StatusCode, envelope.Error.Code, envelope.Error.Message)
		}
		return nil, fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	if decodeErr != nil {
		return nil, fmt.Errorf("decode response: %w", decodeErr)
	}
	if !envelope.Success {
		if envelope.Error != nil {
			return nil, fmt.Errorf("api error: %s: %s", envelope.Error.Code, envelope.Error.Message)
		}
		return nil, errors.New("api reported failure")
	}
	if envelope.Data == nil {
		return nil, errors.New("response has no data")
	}

	return envelope.Data, nil
}

// repositoryState is the latest refresh result for a repository. Projects and
// lastSuccess keep their values when a refresh fails.
type repositoryState struct {
	refreshed   bool
	up          bool
	lastSuccess time.Time
	projects    []driftProject
}

type exporter struct {
	client       *atlantisClient
	repositories []string
	timeout      time.Duration
	now          func() time.Time

	mu     sync.RWMutex
	states map[string]repositoryState

	up          *prometheus.Desc
	lastSuccess *prometheus.Desc
	hasDrift    *prometheus.Desc
	changes     *prometheus.Desc
	lastChecked *prometheus.Desc
	checkFailed *prometheus.Desc
}

func newExporter(client *atlantisClient, repositories []string, timeout time.Duration) *exporter {
	projectLabels := []string{"repository", "project", "directory", "workspace", "ref", "base_branch"}

	return &exporter{
		client:       client,
		repositories: repositories,
		timeout:      timeout,
		now:          time.Now,
		states:       make(map[string]repositoryState, len(repositories)),
		up: prometheus.NewDesc(
			"atlantis_drift_exporter_up",
			"Whether the last read of the Atlantis drift status API succeeded (1) or failed (0).",
			[]string{"repository"}, nil,
		),
		lastSuccess: prometheus.NewDesc(
			"atlantis_drift_exporter_last_success_timestamp_seconds",
			"Unix timestamp of the last successful read of the Atlantis drift status API.",
			[]string{"repository"}, nil,
		),
		hasDrift: prometheus.NewDesc(
			"atlantis_drift_has_drift",
			"Whether the last successful check found drift. Not exposed for projects whose last check failed.",
			projectLabels, nil,
		),
		changes: prometheus.NewDesc(
			"atlantis_drift_changes",
			"Number of resource changes found by the last successful check, by action.",
			append(slices.Clone(projectLabels), "action"), nil,
		),
		lastChecked: prometheus.NewDesc(
			"atlantis_drift_last_checked_timestamp_seconds",
			"Unix timestamp of the last drift check, whether it succeeded or failed.",
			projectLabels, nil,
		),
		checkFailed: prometheus.NewDesc(
			"atlantis_drift_check_failed",
			"Whether the last drift check for the project failed (1) or succeeded (0).",
			projectLabels, nil,
		),
	}
}

func (e *exporter) Describe(ch chan<- *prometheus.Desc) {
	ch <- e.up
	ch <- e.lastSuccess
	ch <- e.hasDrift
	ch <- e.changes
	ch <- e.lastChecked
	ch <- e.checkFailed
}

// run refreshes the drift status immediately and then on every interval.
func (e *exporter) run(ctx context.Context, interval time.Duration) {
	e.refresh(ctx)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			e.refresh(ctx)
		}
	}
}

func (e *exporter) refresh(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()

	var wg sync.WaitGroup
	for _, repository := range e.repositories {
		wg.Go(func() {
			e.refreshRepository(ctx, repository)
		})
	}
	wg.Wait()
}

func (e *exporter) refreshRepository(ctx context.Context, repository string) {
	status, err := e.client.driftStatus(ctx, repository)
	if err != nil {
		log.Printf("failed to read drift status for %s: %v", repository, err)
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	state := e.states[repository]
	state.refreshed = true
	state.up = err == nil
	if err == nil {
		state.lastSuccess = e.now()
		state.projects = status.Projects
	}
	e.states[repository] = state
}

// ready reports whether every repository has completed a refresh attempt.
func (e *exporter) ready() bool {
	e.mu.RLock()
	defer e.mu.RUnlock()

	for _, repository := range e.repositories {
		if !e.states[repository].refreshed {
			return false
		}
	}
	return true
}

func (e *exporter) Collect(ch chan<- prometheus.Metric) {
	e.mu.RLock()
	states := maps.Clone(e.states)
	e.mu.RUnlock()

	for repository, state := range states {
		ch <- prometheus.MustNewConstMetric(e.up, prometheus.GaugeValue, boolToFloat(state.up), repository)
		if !state.lastSuccess.IsZero() {
			ch <- prometheus.MustNewConstMetric(e.lastSuccess, prometheus.GaugeValue, float64(state.lastSuccess.UnixNano())/1e9, repository)
		}
		for _, project := range state.projects {
			e.collectProject(repository, project, ch)
		}
	}
}

func (e *exporter) collectProject(repository string, project driftProject, ch chan<- prometheus.Metric) {
	labels := []string{repository, project.ProjectName, project.Directory, project.Workspace, project.Ref, project.BaseBranch}

	if !project.LastChecked.IsZero() {
		ch <- prometheus.MustNewConstMetric(e.lastChecked, prometheus.GaugeValue, float64(project.LastChecked.UnixNano())/1e9, labels...)
	}

	// Atlantis reports has_drift=false for failed checks, so the drift state is unknown.
	if project.Error != "" {
		ch <- prometheus.MustNewConstMetric(e.checkFailed, prometheus.GaugeValue, 1, labels...)
		return
	}
	ch <- prometheus.MustNewConstMetric(e.checkFailed, prometheus.GaugeValue, 0, labels...)
	ch <- prometheus.MustNewConstMetric(e.hasDrift, prometheus.GaugeValue, boolToFloat(project.HasDrift), labels...)

	details := driftDetails{}
	if project.Drift != nil {
		details = *project.Drift
	}
	for _, change := range []struct {
		action string
		count  int
	}{
		{"add", details.ToAdd},
		{"change", details.ToChange},
		{"destroy", details.ToDestroy},
		{"import", details.ToImport},
		{"forget", details.ToForget},
	} {
		ch <- prometheus.MustNewConstMetric(e.changes, prometheus.GaugeValue, float64(change.count), append(slices.Clone(labels), change.action)...)
	}
}

func boolToFloat(value bool) float64 {
	if value {
		return 1
	}
	return 0
}

func newHandler(reg *prometheus.Registry, ready func() bool) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !ready() {
			http.Error(w, "waiting for first refresh", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("atlantis drift exporter\n\nEndpoints:\n  /metrics\n  /healthz\n  /readyz\n"))
	})
	return mux
}

func main() {
	cfg, err := newConfigFromEnv()
	if err != nil {
		log.Fatalf("invalid configuration: %v", err)
	}

	exp := newExporter(newAtlantisClient(cfg), cfg.Repositories, cfg.RefreshTimeout)

	reg := prometheus.NewRegistry()
	reg.MustRegister(exp)

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           newHandler(reg, exp.ready),
		ReadHeaderTimeout: 5 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go exp.run(ctx, cfg.RefreshInterval)

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			log.Printf("shutdown failed: %v", err)
		}
	}()

	log.Printf("listening on %s", cfg.ListenAddr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("server failed: %v", err)
	}
}
