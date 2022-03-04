package endpoint

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/application-research/autoretrieve/filecoin"
	"github.com/filecoin-project/index-provider/metadata"
	"github.com/filecoin-project/storetheindex/api/v0/finder/model"
	"github.com/ipfs/go-cid"
)

type IndexerEndpoint struct {
	c       *http.Client
	baseUrl string
}

func NewIndexerEndpoint(url string) *IndexerEndpoint {
	return &IndexerEndpoint{
		c: &http.Client{
			Timeout: time.Minute,
		},
		baseUrl: url,
	}
}

func (idxf *IndexerEndpoint) sendRequest(req *http.Request) (*model.FindResponse, error) {
	req.Header.Set("Content-Type", "application/json")
	resp, err := idxf.c.Do(req)
	if err != nil {
		return nil, err
	}
	// Handle failed requests
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == http.StatusNotFound {
			return &model.FindResponse{}, nil
		}
		return nil, fmt.Errorf("batch find query failed: %v", http.StatusText(resp.StatusCode))
	}

	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	return model.UnmarshalFindResponse(b)
}

func (idxf *IndexerEndpoint) FindCandidates(ctx context.Context, cid cid.Cid) ([]filecoin.RetrievalCandidate, error) {
	u := fmt.Sprint(idxf.baseUrl, "/multihash/", cid.Hash().B58String())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}

	parsedResp, err := idxf.sendRequest(req)
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
