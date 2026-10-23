package backup

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Collector returns a prometheus.Collector of the Manager's state, read from
// Status on each scrape. Register it even when the Manager is disabled:
// <namespace>_backup_enabled 0 is the alert for a process without a backup.
// Series with a replica label cover the attached replicas; maintenance of
// the others counts in the detached totals, so that a long
// MaintenanceOptions.Names adds no series. The pruned counts are close, not
// exact: a batch delete that fails in part counts nothing, and a key already
// gone counts as pruned. <namespace>_backup_detached_last_pass_timestamp_seconds
// older than twice the detached interval is the alert for a loop over the
// names that does not run, or whose Names keeps failing. It reads 0 until
// the first pass ends, about ten minutes after the start, so the alert needs
// "and ... > 0" or a "for:". A pass in which every name failed still sets
// it: the failures counter and the error log are the signal for that.
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
		verifyAttempts: desc("replica_verify_attempts", "Restore verifications of the replica in the verify loop's last run: 1 if it passed at once, 0 before a run finished.", "replica"),
		verifyRetried:  desc("replica_verify_retried_total", "Verify loop runs in which the replica failed and then passed on a retry.", "replica"),
		lastSnapshot:   desc("replica_last_snapshot_timestamp_seconds", "Unix time of the replica's newest snapshot at the last maintenance pass, 0 if none.", "replica"),
		replicaObjects: desc("replica_objects", "Objects in the replica at the last maintenance pass.", "replica"),
		replicaBytes:   desc("replica_bytes", "Bytes in the replica at the last maintenance pass.", "replica"),
		maintPasses:    desc("replica_maintenance_passes_total", "Maintenance passes that completed on the replica while it was attached.", "replica"),
		maintPruned:    desc("replica_pruned_objects_total", "Objects the maintenance passes pruned from the replica while it was attached, approximately.", "replica"),
		detPasses:      desc("detached_maintenance_passes_total", "Maintenance passes that completed on replicas not attached at the time, from MaintenanceOptions.Names."),
		detFailures:    desc("detached_maintenance_failures_total", "Maintenance passes that failed on replicas not attached at the time."),
		detPruned:      desc("detached_pruned_objects_total", "Objects the maintenance passes pruned from replicas not attached at the time, approximately."),
		detLastPass:    desc("detached_last_pass_timestamp_seconds", "Unix time the last pass over MaintenanceOptions.Names ended, even if every name failed, 0 before the first; a pass whose Names failed does not count."),
	}
}

type collector struct {
	m                                                                          *Manager
	enabled, replicas, retention                                               *prometheus.Desc
	lastSync, lastVerify, verifyOK, lastSnapshot, replicaObjects, replicaBytes *prometheus.Desc
	verifyAttempts, verifyRetried                                              *prometheus.Desc
	maintPasses, maintPruned, detPasses, detFailures, detPruned, detLastPass   *prometheus.Desc
}

func (c *collector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{c.enabled, c.replicas, c.retention, c.lastSync, c.lastVerify, c.verifyOK, c.lastSnapshot, c.replicaObjects, c.replicaBytes, c.verifyAttempts, c.verifyRetried, c.maintPasses, c.maintPruned, c.detPasses, c.detFailures, c.detPruned, c.detLastPass} {
		ch <- d
	}
}

func (c *collector) Collect(ch chan<- prometheus.Metric) {
	gauge := func(d *prometheus.Desc, v float64, labels ...string) {
		ch <- prometheus.MustNewConstMetric(d, prometheus.GaugeValue, v, labels...)
	}
	counter := func(d *prometheus.Desc, v int, labels ...string) {
		ch <- prometheus.MustNewConstMetric(d, prometheus.CounterValue, float64(v), labels...)
	}
	gauge(c.enabled, boolValue(c.m.Enabled()))
	gauge(c.retention, c.m.RetentionWindow().Seconds())
	st := c.m.Status()
	gauge(c.replicas, float64(len(st)))
	for _, s := range st {
		gauge(c.lastSync, unix(s.LastSyncAt), s.Name)
		gauge(c.lastVerify, unix(s.LastVerifyAt), s.Name)
		gauge(c.verifyOK, boolValue(s.VerifyOK), s.Name)
		gauge(c.verifyAttempts, float64(s.VerifyAttempts), s.Name)
		counter(c.verifyRetried, s.VerifyRetried, s.Name)
		gauge(c.lastSnapshot, unix(s.LastSnapshotAt), s.Name)
		gauge(c.replicaObjects, float64(s.ReplicaObjects), s.Name)
		gauge(c.replicaBytes, float64(s.ReplicaBytes), s.Name)
		counter(c.maintPasses, s.MaintenancePasses, s.Name)
		counter(c.maintPruned, s.PrunedObjects, s.Name)
	}
	c.m.mu.Lock()
	det := c.m.detachedMaint
	c.m.mu.Unlock()
	counter(c.detPasses, det.passes)
	counter(c.detFailures, det.failures)
	counter(c.detPruned, det.pruned)
	gauge(c.detLastPass, unix(det.lastPass))
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
