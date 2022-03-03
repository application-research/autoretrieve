package endpoint

import (
	"context"

	"github.com/application-research/autoretrieve/filecoin"
	"github.com/filecoin-project/index-provider/metadata"
	finderhttpclient "github.com/filecoin-project/storetheindex/api/v0/finder/client/http"
	"github.com/ipfs/go-cid"
)

type IndexerEndpoint struct {
	*finderhttpclient.Client
}

func NewIndexerEndpoint(url string) (*IndexerEndpoint, error) {
	client, err := finderhttpclient.New(url)
	if err != nil {
		return nil, err
	}
	return &IndexerEndpoint{
		Client: client,
	}, nil
}

func (idxf *IndexerEndpoint) FindCandidates(ctx context.Context, cid cid.Cid) ([]filecoin.RetrievalCandidate, error) {
	parsedResp, err := idxf.Client.Find(ctx, cid.Hash())
	if err != nil {
		return nil, err
	}
	hash := string(cid.Hash())
	// turn parsedResp into records.
	var matches []filecoin.RetrievalCandidate
	for _, multihashResult := range parsedResp.MultihashResults {
		if !(string(multihashResult.Multihash) == hash) {
			continue
		}
		for _, val := range multihashResult.ProviderResults {
			// filter out any results that aren't filecoin graphsync
			dtm, err := metadata.FromIndexerMetadata(val.Metadata)
			if err != nil {
				continue
			}

			if dtm.ExchangeFormat() != metadata.FilecoinV1 {
				continue
			}

			matches = append(matches, filecoin.RetrievalCandidate{
				RootCid:   cid,
				MinerPeer: val.Provider,
			})
		}
	}
	return matches, nil
}
