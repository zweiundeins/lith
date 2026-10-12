package backup

import (
	"path/filepath"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

func gather(t *testing.T, c prometheus.Collector) map[string]*dto.MetricFamily {
	t.Helper()
	reg := prometheus.NewPedanticRegistry()
	reg.MustRegister(c)
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]*dto.MetricFamily{}
	for _, mf := range mfs {
		out[mf.GetName()] = mf
	}
	return out
}

// gauge returns the value of the series of name for replica, or the only
// series when replica is empty.
func gauge(t *testing.T, mfs map[string]*dto.MetricFamily, name, replica string) float64 {
	t.Helper()
	for _, m := range mfs[name].GetMetric() {
		if replica == "" {
			return m.GetGauge().GetValue()
		}
		for _, l := range m.GetLabel() {
			if l.GetName() == "replica" && l.GetValue() == replica {
				return m.GetGauge().GetValue()
			}
		}
	}
	t.Fatalf("no %s series for %q", name, replica)
	return 0
}

func TestMetricsOfADisabledManager(t *testing.T) {
	m, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	mfs := gather(t, m.Collector("app"))
	if v := gauge(t, mfs, "app_backup_enabled", ""); v != 0 {
		t.Errorf("enabled = %v, want 0", v)
	}
	if v := gauge(t, mfs, "app_backup_replicas", ""); v != 0 {
		t.Errorf("replicas = %v, want 0", v)
	}
}

func TestMetricsOfAReplica(t *testing.T) {
	t.Parallel()
	m := newManager(t)
	db, detach := replicated(t, m, "app", filepath.Join(t.TempDir(), "app.db"))
	defer shutdown(t, db, detach)

	mfs := gather(t, m.Collector("app"))
	if v := gauge(t, mfs, "app_backup_enabled", ""); v != 1 {
		t.Errorf("enabled = %v, want 1", v)
	}
	if v := gauge(t, mfs, "app_backup_retention_window_seconds", ""); v != DefaultRetentionWindow.Seconds() {
		t.Errorf("retention = %v, want the default", v)
	}
	if v := gauge(t, mfs, "app_backup_replica_last_sync_timestamp_seconds", "app"); v <= 0 {
		t.Errorf("last sync = %v, want a time", v)
	}
	if v := gauge(t, mfs, "app_backup_replica_verify_ok", "app"); v != 0 {
		t.Errorf("verify_ok = %v before any verification, want 0", v)
	}

	if err := m.VerifyRestore(bg, "app"); err != nil {
		t.Fatal(err)
	}
	mfs = gather(t, m.Collector("app"))
	if v := gauge(t, mfs, "app_backup_replica_verify_ok", "app"); v != 1 {
		t.Errorf("verify_ok = %v after a verification, want 1", v)
	}
	if v := gauge(t, mfs, "app_backup_replica_last_verify_timestamp_seconds", "app"); v <= 0 {
		t.Errorf("last verify = %v, want a time", v)
	}
}
