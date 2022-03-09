package metrics

import peer "github.com/libp2p/go-libp2p-core/peer"

type Multi struct {
	inner []Metrics
}

func NewMulti(inner ...Metrics) *Multi {
	return &Multi{
		inner: inner,
	}
}

func NewNoop() *Multi {
	return &Multi{}
}

// Run something on all the inner handlers
func (metrics *Multi) dispatch(f func(Metrics)) {
	for _, inner := range metrics.inner {
		f(inner)
	}
}

func (metrics *Multi) RecordWallet(info WalletInfo) {
	metrics.dispatch(func(m Metrics) { m.RecordWallet(info) })
}

func (metrics *Multi) RecordGetCandidatesResult(info RequestInfo, result GetCandidatesResult) {
	metrics.dispatch(func(m Metrics) { m.RecordGetCandidatesResult(info, result) })
}

func (metrics *Multi) RecordQuery(info CandidateInfo) {
	metrics.dispatch(func(m Metrics) { m.RecordQuery(info) })
}

func (metrics *Multi) RecordQueryResult(info CandidateInfo, result QueryResult) {
	metrics.dispatch(func(m Metrics) { m.RecordQueryResult(info, result) })
}

func (metrics *Multi) RecordRetrieval(info CandidateInfo) {
	metrics.dispatch(func(m Metrics) { m.RecordRetrieval(info) })
}

func (metrics *Multi) RecordRetrievalResult(info CandidateInfo, result RetrievalResult) {
	metrics.dispatch(func(m Metrics) { m.RecordRetrievalResult(info, result) })
}

func (metrics *Multi) RecordMinerConnection(peer peer.ID) {
	metrics.dispatch(func(m Metrics) { m.RecordMinerConnection(peer) })
}

func (metrics *Multi) RecordMinerDisconnection(peer peer.ID) {
	metrics.dispatch(func(m Metrics) { m.RecordMinerDisconnection(peer) })
}

func (metrics *Multi) RecordClientConnection(peer peer.ID) {
	metrics.dispatch(func(m Metrics) { m.RecordClientConnection(peer) })
}

func (metrics *Multi) RecordClientDisconnection(peer peer.ID) {
	metrics.dispatch(func(m Metrics) { m.RecordClientDisconnection(peer) })
}
