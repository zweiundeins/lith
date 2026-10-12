package backup

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Collector returns a prometheus.Collector of the Manager's state, read from
// Status on each scrape. Register it even when the Manager is disabled:
// <namespace>_backup_enabled 0 is the alert for a process without a backup.
func (m *Manager) Collector(namespace string) prometheus.Collector {
	desc := func(name, help string, labels ...string) *prometheus.Desc {
		return prometheus.NewDesc(prometheus.BuildFQName(namespace, "backup", name), help, labels, nil)
	}
	return &collector{
		m:              m,
		enabled:        desc("enabled", "1 if the process replicates its databases, else 0."),
		replicas:       desc("replicas", "Databases attached for replication."),
		retention:      desc("retention_window_seconds", "How far back a restore can reach; older history is pruned."),
		lastSync:       desc("replica_last_sync_timestamp_seconds", "Unix time of the replica's last successful sync, 0 before the first.", "replica"),
		lastVerify:     desc("replica_last_verify_timestamp_seconds", "Unix time of the replica's last restore verification, 0 if none ran.", "replica"),
		verifyOK:       desc("replica_verify_ok", "1 if the replica's last restore verification passed, 0 if it failed or never ran.", "replica"),
		lastSnapshot:   desc("replica_last_snapshot_timestamp_seconds", "Unix time of the replica's newest snapshot at the last maintenance pass, 0 if none.", "replica"),
		replicaObjects: desc("replica_objects", "Objects in the replica at the last maintenance pass.", "replica"),
		replicaBytes:   desc("replica_bytes", "Bytes in the replica at the last maintenance pass.", "replica"),
	}
}

type collector struct {
	m                                                                          *Manager
	enabled, replicas, retention                                               *prometheus.Desc
	lastSync, lastVerify, verifyOK, lastSnapshot, replicaObjects, replicaBytes *prometheus.Desc
}

func (c *collector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{c.enabled, c.replicas, c.retention, c.lastSync, c.lastVerify, c.verifyOK, c.lastSnapshot, c.replicaObjects, c.replicaBytes} {
		ch <- d
	}
}

func (c *collector) Collect(ch chan<- prometheus.Metric) {
	gauge := func(d *prometheus.Desc, v float64, labels ...string) {
		ch <- prometheus.MustNewConstMetric(d, prometheus.GaugeValue, v, labels...)
	}
	gauge(c.enabled, boolValue(c.m.Enabled()))
	gauge(c.retention, c.m.RetentionWindow().Seconds())
	st := c.m.Status()
	gauge(c.replicas, float64(len(st)))
	for _, s := range st {
		gauge(c.lastSync, unix(s.LastSyncAt), s.Name)
		gauge(c.lastVerify, unix(s.LastVerifyAt), s.Name)
		gauge(c.verifyOK, boolValue(s.VerifyOK), s.Name)
		gauge(c.lastSnapshot, unix(s.LastSnapshotAt), s.Name)
		gauge(c.replicaObjects, float64(s.ReplicaObjects), s.Name)
		gauge(c.replicaBytes, float64(s.ReplicaBytes), s.Name)
	}
}

func boolValue(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

func unix(t time.Time) float64 {
	if t.IsZero() {
		return 0
	}
	return float64(t.Unix())
}
