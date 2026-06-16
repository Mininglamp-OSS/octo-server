package messages_search

import "testing"

// Step 5 — the read-only drift gauges reflect the latest reconciliation report.
// octo-server only stores/exposes the report (the authoritative recon runs in
// the indexer repo); these assertions pin the signed-drift math and the
// machine-checkable failure thresholds (drift!=0 => unhealthy).
func TestPublishReconReport_SignedDrift(t *testing.T) {
	cases := []struct {
		name   string
		report ReconReport
		want   int64
	}{
		{"healthy", ReconReport{ESDocCount: 1000, MySQLRowCount: 1000}, 0},
		{"es_extra_delete_miss", ReconReport{ESDocCount: 1005, MySQLRowCount: 1000}, 5},
		{"es_missing_index_miss", ReconReport{ESDocCount: 990, MySQLRowCount: 1000}, -10},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.report.DocDrift(); got != tc.want {
				t.Fatalf("DocDrift() = %d, want %d", got, tc.want)
			}
			if got := PublishReconReport(tc.report); got != tc.want {
				t.Fatalf("PublishReconReport() = %d, want %d", got, tc.want)
			}
		})
	}
}
