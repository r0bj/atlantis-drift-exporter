package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

const testToken = "test-token"

const statusResponse = `{
  "success": true,
  "data": {
    "repository": "acme/infra",
    "projects": [
      {
        "project_name": "drifted",
        "directory": "envs/prod",
        "workspace": "default",
        "ref": "main",
        "has_drift": true,
        "drift": {"to_add": 1, "to_change": 2, "to_destroy": 3, "to_import": 4, "to_forget": 5, "total_changes": 15},
        "last_checked": "2026-11-23T05:00:00Z"
      },
      {
        "project_name": "clean",
        "directory": "envs/dev",
        "workspace": "default",
        "ref": "main",
        "has_drift": false,
        "last_checked": "2026-11-23T05:00:01Z"
      },
      {
        "project_name": "broken",
        "directory": "envs/stage",
        "workspace": "default",
        "ref": "main",
        "has_drift": false,
        "last_checked": "2026-11-23T05:00:02Z",
        "error": "plan failed"
      }
    ]
  }
}`

func newTestExporter(baseURL string, repositories ...string) *exporter {
	cfg := &config{
		AtlantisURL:     baseURL,
		APISecret:       testToken,
		VCSType:         "Github",
		Ref:             "main",
		RefreshInterval: time.Minute,
		RefreshTimeout:  2 * time.Second,
	}
	return newExporter(newAtlantisClient(cfg), repositories, cfg.RefreshTimeout)
}

func TestCollectExposesProjectStates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/drift/status" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		if got := r.Header.Get("X-Atlantis-Token"); got != testToken {
			t.Errorf("unexpected token %q", got)
		}
		query := r.URL.Query()
		if query.Get("repository") != "acme/infra" || query.Get("type") != "Github" || query.Get("ref") != "main" {
			t.Errorf("unexpected query %q", r.URL.RawQuery)
		}
		_, _ = w.Write([]byte(statusResponse))
	}))
	defer srv.Close()

	expected := `
# HELP atlantis_drift_check_failed Whether the last drift check for the project failed (1) or succeeded (0).
# TYPE atlantis_drift_check_failed gauge
atlantis_drift_check_failed{base_branch="",directory="envs/dev",project="clean",ref="main",repository="acme/infra",workspace="default"} 0
atlantis_drift_check_failed{base_branch="",directory="envs/prod",project="drifted",ref="main",repository="acme/infra",workspace="default"} 0
atlantis_drift_check_failed{base_branch="",directory="envs/stage",project="broken",ref="main",repository="acme/infra",workspace="default"} 1
# HELP atlantis_drift_exporter_up Whether the last read of the Atlantis drift status API succeeded (1) or failed (0).
# TYPE atlantis_drift_exporter_up gauge
atlantis_drift_exporter_up{repository="acme/infra"} 1
# HELP atlantis_drift_has_drift Whether the last successful check found drift. Not exposed for projects whose last check failed.
# TYPE atlantis_drift_has_drift gauge
atlantis_drift_has_drift{base_branch="",directory="envs/dev",project="clean",ref="main",repository="acme/infra",workspace="default"} 0
atlantis_drift_has_drift{base_branch="",directory="envs/prod",project="drifted",ref="main",repository="acme/infra",workspace="default"} 1
`
	exp := newTestExporter(srv.URL, "acme/infra")
	exp.refresh(t.Context())

	err := testutil.CollectAndCompare(exp, strings.NewReader(expected),
		"atlantis_drift_check_failed", "atlantis_drift_exporter_up", "atlantis_drift_has_drift")
	if err != nil {
		t.Fatal(err)
	}

	expectedChanges := `
# HELP atlantis_drift_changes Number of resource changes found by the last successful check, by action.
# TYPE atlantis_drift_changes gauge
atlantis_drift_changes{action="add",base_branch="",directory="envs/dev",project="clean",ref="main",repository="acme/infra",workspace="default"} 0
atlantis_drift_changes{action="add",base_branch="",directory="envs/prod",project="drifted",ref="main",repository="acme/infra",workspace="default"} 1
atlantis_drift_changes{action="change",base_branch="",directory="envs/dev",project="clean",ref="main",repository="acme/infra",workspace="default"} 0
atlantis_drift_changes{action="change",base_branch="",directory="envs/prod",project="drifted",ref="main",repository="acme/infra",workspace="default"} 2
atlantis_drift_changes{action="destroy",base_branch="",directory="envs/dev",project="clean",ref="main",repository="acme/infra",workspace="default"} 0
atlantis_drift_changes{action="destroy",base_branch="",directory="envs/prod",project="drifted",ref="main",repository="acme/infra",workspace="default"} 3
atlantis_drift_changes{action="forget",base_branch="",directory="envs/dev",project="clean",ref="main",repository="acme/infra",workspace="default"} 0
atlantis_drift_changes{action="forget",base_branch="",directory="envs/prod",project="drifted",ref="main",repository="acme/infra",workspace="default"} 5
atlantis_drift_changes{action="import",base_branch="",directory="envs/dev",project="clean",ref="main",repository="acme/infra",workspace="default"} 0
atlantis_drift_changes{action="import",base_branch="",directory="envs/prod",project="drifted",ref="main",repository="acme/infra",workspace="default"} 4
`
	err = testutil.CollectAndCompare(exp, strings.NewReader(expectedChanges), "atlantis_drift_changes")
	if err != nil {
		t.Fatal(err)
	}
}

func TestCollectIsolatesRepositoryFailures(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("repository") == "acme/bad" {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"success":false,"error":{"code":"SERVICE_UNAVAILABLE","message":"drift detection is not enabled"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"success":true,"data":{"repository":"acme/good","projects":[]}}`))
	}))
	defer srv.Close()

	expected := `
# HELP atlantis_drift_exporter_up Whether the last read of the Atlantis drift status API succeeded (1) or failed (0).
# TYPE atlantis_drift_exporter_up gauge
atlantis_drift_exporter_up{repository="acme/bad"} 0
atlantis_drift_exporter_up{repository="acme/good"} 1
`
	exp := newTestExporter(srv.URL, "acme/good", "acme/bad")
	exp.refresh(t.Context())

	err := testutil.CollectAndCompare(exp, strings.NewReader(expected), "atlantis_drift_exporter_up")
	if err != nil {
		t.Fatal(err)
	}
}

func TestRefreshKeepsLastResultOnFailure(t *testing.T) {
	var failing atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if failing.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(statusResponse))
	}))
	defer srv.Close()

	exp := newTestExporter(srv.URL, "acme/infra")
	exp.now = func() time.Time { return time.Unix(1000, 0) }
	exp.refresh(t.Context())

	exp.now = func() time.Time { return time.Unix(2000, 0) }
	failing.Store(true)
	exp.refresh(t.Context())

	expected := `
# HELP atlantis_drift_exporter_last_success_timestamp_seconds Unix timestamp of the last successful read of the Atlantis drift status API.
# TYPE atlantis_drift_exporter_last_success_timestamp_seconds gauge
atlantis_drift_exporter_last_success_timestamp_seconds{repository="acme/infra"} 1000
# HELP atlantis_drift_exporter_up Whether the last read of the Atlantis drift status API succeeded (1) or failed (0).
# TYPE atlantis_drift_exporter_up gauge
atlantis_drift_exporter_up{repository="acme/infra"} 0
# HELP atlantis_drift_has_drift Whether the last successful check found drift. Not exposed for projects whose last check failed.
# TYPE atlantis_drift_has_drift gauge
atlantis_drift_has_drift{base_branch="",directory="envs/dev",project="clean",ref="main",repository="acme/infra",workspace="default"} 0
atlantis_drift_has_drift{base_branch="",directory="envs/prod",project="drifted",ref="main",repository="acme/infra",workspace="default"} 1
`
	err := testutil.CollectAndCompare(exp, strings.NewReader(expected),
		"atlantis_drift_exporter_last_success_timestamp_seconds", "atlantis_drift_exporter_up", "atlantis_drift_has_drift")
	if err != nil {
		t.Fatal(err)
	}
}

func TestRefreshClearsProjectsAfterEmptySuccess(t *testing.T) {
	var empty atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if empty.Load() {
			_, _ = w.Write([]byte(`{"success":true,"data":{"repository":"acme/infra","projects":[]}}`))
			return
		}
		_, _ = w.Write([]byte(statusResponse))
	}))
	defer srv.Close()

	exp := newTestExporter(srv.URL, "acme/infra")
	exp.refresh(t.Context())
	empty.Store(true)
	exp.refresh(t.Context())

	if got := testutil.CollectAndCount(exp, "atlantis_drift_has_drift"); got != 0 {
		t.Fatalf("expected no project series, got %d", got)
	}
}

func TestCollectDoesNotCallAtlantis(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		_, _ = w.Write([]byte(statusResponse))
	}))
	defer srv.Close()

	exp := newTestExporter(srv.URL, "acme/infra")
	exp.refresh(t.Context())
	for range 3 {
		testutil.CollectAndCount(exp)
	}

	if got := requests.Load(); got != 1 {
		t.Fatalf("expected 1 API request, got %d", got)
	}
}

func TestReadyAfterFirstRefreshAttempt(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	exp := newTestExporter(srv.URL, "acme/infra")
	handler := httptest.NewServer(newHandler(prometheus.NewRegistry(), exp.ready))
	defer handler.Close()

	assertStatus := func(want int) {
		t.Helper()
		resp, err := http.Get(handler.URL + "/readyz")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Fatalf("/readyz returned %d, want %d", resp.StatusCode, want)
		}
	}

	assertStatus(http.StatusServiceUnavailable)
	exp.refresh(t.Context())
	assertStatus(http.StatusOK)
}

func TestClientDoesNotFollowRedirects(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("redirect target received a request")
	}))
	defer target.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer srv.Close()

	if _, err := newTestExporter(srv.URL).client.driftStatus(t.Context(), "acme/infra"); err == nil {
		t.Fatal("expected an error for a redirect response")
	}
}

func TestClientRejectsInvalidResponses(t *testing.T) {
	for name, body := range map[string]string{
		"not json":     `<html></html>`,
		"missing data": `{"success":true}`,
		"not success":  `{"success":false,"error":{"code":"INTERNAL_ERROR","message":"boom"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(body))
			}))
			defer srv.Close()

			if _, err := newTestExporter(srv.URL).client.driftStatus(t.Context(), "acme/infra"); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestNewConfigFromEnv(t *testing.T) {
	setValid := func(t *testing.T) {
		t.Helper()
		t.Setenv("ATLANTIS_URL", "http://atlantis.atlantis.svc/")
		t.Setenv("ATLANTIS_API_SECRET", testToken)
		t.Setenv("ATLANTIS_REPOSITORIES", "acme/a, acme/b,acme/a,")
		t.Setenv("ATLANTIS_VCS_TYPE", "")
		t.Setenv("ATLANTIS_REF", "")
		t.Setenv("LISTEN_ADDRESS", "")
		t.Setenv("REFRESH_INTERVAL", "")
		t.Setenv("REFRESH_TIMEOUT", "")
	}

	t.Run("valid", func(t *testing.T) {
		setValid(t)
		cfg, err := newConfigFromEnv()
		if err != nil {
			t.Fatal(err)
		}
		if cfg.AtlantisURL != "http://atlantis.atlantis.svc" {
			t.Errorf("unexpected URL %q", cfg.AtlantisURL)
		}
		if got := strings.Join(cfg.Repositories, ","); got != "acme/a,acme/b" {
			t.Errorf("unexpected repositories %q", got)
		}
		if cfg.VCSType != defaultVCSType || cfg.ListenAddr != defaultListenAddress ||
			cfg.RefreshInterval != defaultRefreshInterval || cfg.RefreshTimeout != defaultRefreshTimeout {
			t.Errorf("unexpected defaults: %+v", cfg)
		}
	})

	for name, env := range map[string][2]string{
		"missing url":        {"ATLANTIS_URL", ""},
		"relative url":       {"ATLANTIS_URL", "atlantis.svc"},
		"missing secret":     {"ATLANTIS_API_SECRET", ""},
		"missing repository": {"ATLANTIS_REPOSITORIES", " , "},
		"invalid vcs":        {"ATLANTIS_VCS_TYPE", "github"},
		"invalid interval":   {"REFRESH_INTERVAL", "soon"},
		"negative interval":  {"REFRESH_INTERVAL", "-1s"},
		"invalid timeout":    {"REFRESH_TIMEOUT", "soon"},
		"negative timeout":   {"REFRESH_TIMEOUT", "-1s"},
		"timeout too long":   {"REFRESH_TIMEOUT", "6m"},
	} {
		t.Run(name, func(t *testing.T) {
			setValid(t)
			t.Setenv(env[0], env[1])
			if _, err := newConfigFromEnv(); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestHandlerServesMetrics(t *testing.T) {
	reg := prometheus.NewRegistry()
	srv := httptest.NewServer(newHandler(reg, func() bool { return true }))
	defer srv.Close()

	for _, path := range []string{"/", "/healthz", "/readyz", "/metrics"} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s returned %d", path, resp.StatusCode)
		}
	}
}
