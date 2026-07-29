package cardtmpl

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/Mininglamp-OSS/octo-server/internal/carddispatch"
	"github.com/Mininglamp-OSS/octo-server/pkg/cardmsg"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

type metricMutationGateway struct {
	snapshot CardMutationSnapshot
}

type CardMutationSnapshot = carddispatch.CardMutationSnapshot

func (g *metricMutationGateway) Snapshot(context.Context, carddispatch.CardMutationTarget) (carddispatch.CardMutationSnapshot, error) {
	return g.snapshot, nil
}

func (*metricMutationGateway) Mutate(context.Context, carddispatch.CardMutationRequest) (carddispatch.CardMutationResult, error) {
	return carddispatch.CardMutationResult{Applied: true}, nil
}

func TestMetricsExposeCallbackAndUpdateCounters(t *testing.T) {
	registry := prometheus.NewRegistry()
	metrics := NewMetrics(registry)
	metrics.callbackResult("docs.access-request", "0.3.0", "docs-access-approve", "ok")
	metrics.updateResult("docs.access-request", "0.3.0", "ok")
	metrics.callbackResult("docs.access-request", "0.3.0", "docs-access-approve", "future")
	metrics.updateResult("docs.access-request", "0.3.0", "future")

	if got := testutil.ToFloat64(metrics.callback.WithLabelValues(
		"docs.access-request", "0.3.0", "docs-access-approve", "ok")); got != 1 {
		t.Fatalf("callback counter = %v, want 1", got)
	}
	if got := testutil.ToFloat64(metrics.update.WithLabelValues("docs.access-request", "0.3.0", "ok")); got != 1 {
		t.Fatalf("update counter = %v, want 1", got)
	}
	if got := testutil.ToFloat64(metrics.callback.WithLabelValues(
		"docs.access-request", "0.3.0", "docs-access-approve", "error")); got != 1 {
		t.Fatalf("bounded callback error counter = %v, want 1", got)
	}
	if got := testutil.ToFloat64(metrics.update.WithLabelValues("docs.access-request", "0.3.0", "error")); got != 1 {
		t.Fatalf("bounded update error counter = %v, want 1", got)
	}
}

func TestUpdateMetricsOnlyUseRegisteredTemplateLabels(t *testing.T) {
	t.Run("replace unknown template", func(t *testing.T) {
		promRegistry := prometheus.NewRegistry()
		metrics := NewMetrics(promRegistry)
		SetGlobalMetrics(metrics)
		t.Cleanup(func() { SetGlobalMetrics(nil) })

		catalog, catalogErr := NewStaticCatalog(NewRegistry())
		if catalogErr != nil {
			t.Fatal(catalogErr)
		}
		updater, err := NewCardUpdater(catalog, &metricMutationGateway{})
		if err != nil {
			t.Fatal(err)
		}
		err = updater.ReplaceView(context.Background(), UpdateTarget{
			Target:    carddispatch.Target{SpaceID: "space-1", ChannelID: "user-b", ChannelType: 1},
			SenderUID: "notification", MessageID: "1001", CardSeq: 1,
		}, ID("untrusted.template"), "999.999.999", State("done"), json.RawMessage(`{}`), BuildEnv{SpaceID: "space-1"})
		if !errors.Is(err, ErrTemplateUnknown) {
			t.Fatalf("ReplaceView(unknown) error = %v, want %v", err, ErrTemplateUnknown)
		}
		if got := testutil.CollectAndCount(metrics.update); got != 0 {
			t.Fatalf("unknown template created %d update metric series, want 0", got)
		}
	})

	t.Run("append unregistered metadata preserves compatibility", func(t *testing.T) {
		promRegistry := prometheus.NewRegistry()
		metrics := NewMetrics(promRegistry)
		SetGlobalMetrics(metrics)
		t.Cleanup(func() { SetGlobalMetrics(nil) })

		envelope, err := json.Marshal(map[string]any{
			"type": cardmsg.InteractiveCard.Int(), "card_version": cardmsg.CardVersion,
			"profile": cardmsg.ProfileV1, "space_id": "space-1",
			"card": map[string]any{
				"type": "AdaptiveCard", "version": cardmsg.CardVersion,
				"body": []any{map[string]any{"type": "TextBlock", "text": "before"}},
				"metadata": map[string]any{"octo": map[string]any{
					"protocol": Protocol, "template": map[string]any{"id": "untrusted.template", "version": "999.999.999"},
				}},
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		gateway := &metricMutationGateway{snapshot: carddispatch.CardMutationSnapshot{Envelope: envelope}}
		catalog, catalogErr := NewStaticCatalog(NewRegistry())
		if catalogErr != nil {
			t.Fatal(catalogErr)
		}
		updater, err := NewCardUpdater(catalog, gateway)
		if err != nil {
			t.Fatal(err)
		}
		err = updater.Append(context.Background(), UpdateTarget{
			Target:    carddispatch.Target{SpaceID: "space-1", ChannelID: "user-b", ChannelType: 1},
			SenderUID: "notification", MessageID: "1001", CardSeq: 1,
		}, json.RawMessage(`{"type":"TextBlock","text":"after"}`))
		if err != nil {
			t.Fatalf("Append(unregistered metadata) error = %v, want nil", err)
		}
		if got := testutil.CollectAndCount(metrics.update); got != 0 {
			t.Fatalf("unregistered metadata created %d update metric series, want 0", got)
		}
	})
}
