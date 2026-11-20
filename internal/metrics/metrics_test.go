package metrics_test

import (
	"testing"

	"github.com/april/shorten-link/internal/metrics"
)

// The DeadLettered alert uses increase(), which needs the series to exist at 0
// before the first dead-letter.
func TestWorkerMessages_DLQSeriesPreCreated(t *testing.T) {
	mfs, err := metrics.Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]float64{}
	for _, mf := range mfs {
		if mf.GetName() != "shortlink_worker_messages_total" {
			continue
		}
		for _, m := range mf.GetMetric() {
			labels := map[string]string{}
			for _, l := range m.GetLabel() {
				labels[l.GetName()] = l.GetValue()
			}
			if labels["result"] == "dlq" {
				found[labels["queue"]] = m.GetCounter().GetValue()
			}
		}
	}
	for _, q := range []string{"clicks", "notifications"} {
		v, ok := found[q]
		if !ok {
			t.Errorf("no dlq series for queue %q", q)
		} else if v != 0 {
			t.Errorf("dlq series for %q = %v, want 0", q, v)
		}
	}
}
