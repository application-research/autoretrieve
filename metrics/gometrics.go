package metrics

import (
	"context"

	gometrics "github.com/ipfs/go-metrics-interface"
	gometricsprometheus "github.com/ipfs/go-metrics-prometheus"
	peer "github.com/libp2p/go-libp2p-core/peer"
)

type GoMetrics struct {
	activeRetrievals        gometrics.Gauge
	retrievalSuccesses      gometrics.Counter
	retrievalFailures       gometrics.Counter
	averageSuccessDurations gometrics.Histogram
	averageFailureDurations gometrics.Histogram
}

func GoMetricsInjectPrometheus() error {
	return gometricsprometheus.Inject()
}

func NewGoMetrics(ctx context.Context) *GoMetrics {

	scope := gometrics.CtxScope(ctx, "autoretrieve")
	metrics := &GoMetrics{
		activeRetrievals: gometrics.
			NewCtx(scope, "active_retrievals", "active retrieval count").
			Gauge(),
		retrievalSuccesses: gometrics.
			NewCtx(scope, "retrieval_successes", "retrieval success count").
			Counter(),
		retrievalFailures: gometrics.
			NewCtx(scope, "retrieval_failures", "retrieval failure count").
			Counter(),
		averageSuccessDurations: gometrics.
			NewCtx(scope, "average_success_durations", "average success durations in seconds").
			Histogram([]float64{1, 5, 10, 30, 60, 300, 600, 1800, 3600, 7200}),
		averageFailureDurations: gometrics.
			NewCtx(scope, "average_failure_durations", "average failure durations in seconds").
			Histogram([]float64{1, 5, 10, 30, 60, 300, 600, 1800, 3600, 7200}),
	}

	return metrics
}

// Resets all session data like active retrievals
func (gometrics *GoMetrics) NewSession() {
	gometrics.activeRetrievals.Set(0)
}

func (metrics *GoMetrics) RecordWallet(info WalletInfo) {}

func (metrics *GoMetrics) RecordGetCandidatesResult(info RequestInfo, result GetCandidatesResult) {}

func (metrics *GoMetrics) RecordQuery(info CandidateInfo) {}

func (metrics *GoMetrics) RecordQueryResult(info CandidateInfo, result QueryResult) {}

func (metrics *GoMetrics) RecordRetrieval(info CandidateInfo) {
	metrics.activeRetrievals.Inc()
}

func (metrics *GoMetrics) RecordRetrievalResult(info CandidateInfo, result RetrievalResult) {
	metrics.activeRetrievals.Dec()

	if result.Err != nil {
		metrics.retrievalFailures.Inc()
		metrics.averageFailureDurations.Observe(result.Duration.Seconds())
	} else {
		metrics.retrievalSuccesses.Inc()
		metrics.averageSuccessDurations.Observe(result.Duration.Seconds())
	}
}

func (metrics *GoMetrics) RecordMinerConnection(peer peer.ID) {}

func (metrics *GoMetrics) RecordMinerDisconnection(peer peer.ID) {}

func (metrics *GoMetrics) RecordClientConnection(peer peer.ID) {}

func (metrics *GoMetrics) RecordClientDisconnection(peer peer.ID) {}
