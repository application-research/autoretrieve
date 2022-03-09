package metrics

import (
	"time"

	"github.com/filecoin-project/go-address"
	"github.com/filecoin-project/lotus/chain/types"
	"github.com/ipfs/go-cid"
	peer "github.com/libp2p/go-libp2p-core/peer"
)

type WalletInfo struct {
	Err  error
	Addr address.Address
}

type RequestInfo struct {
	RequestCid cid.Cid
}

// Can be used to identify a specific candidate within a request
type CandidateInfo struct {
	RequestInfo

	// The root CID that will be retrieved from the miner
	RootCid cid.Cid

	// Peer ID to retrieve from
	PeerID peer.ID
}

type GetCandidatesResult struct {
	Err   error
	Count uint
}

type QueryResult struct {
	// Fill out whole struct even with non-nil error
	Err error
}

type RetrievalResult struct {
	Duration      time.Duration
	BytesReceived uint64
	TotalPayment  types.FIL

	// Fill out whole struct even with non-nil error
	Err error
}

type RequestResult struct {
	Duration     time.Duration
	TotalPayment types.FIL

	// Fill out whole struct even with non-nil error
	Err error
}

type Metrics interface {
	// Whenever the wallet is set
	RecordWallet(WalletInfo)

	// Called once, after getting candidates
	RecordGetCandidatesResult(RequestInfo, GetCandidatesResult)

	// Before each query
	RecordQuery(CandidateInfo)

	// After each query is finished
	RecordQueryResult(CandidateInfo, QueryResult)

	// Before each retrieval attempt
	RecordRetrieval(CandidateInfo)

	// After each retrieval attempt
	RecordRetrievalResult(CandidateInfo, RetrievalResult)

	// When a service provider connects
	RecordMinerConnection(peer.ID)

	// When a service provider disconnects
	RecordMinerDisconnection(peer.ID)

	// When a client connects
	RecordClientConnection(peer.ID)

	// When a client disconnects
	RecordClientDisconnection(peer.ID)
}
