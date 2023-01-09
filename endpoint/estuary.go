package endpoint

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"

	"github.com/application-research/autoretrieve/minerpeergetter"
	"github.com/filecoin-project/go-address"
	lassieretriever "github.com/filecoin-project/lassie/pkg/retriever"
	"github.com/ipfs/go-cid"
)

var (
	ErrInvalidEndpointURL    = errors.New("invalid endpoint URL")
	ErrEndpointRequestFailed = errors.New("endpoint request failed")
	ErrEndpointBodyInvalid   = errors.New("endpoint body invalid")
)

type EstuaryEndpoint struct {
	url string
	mpg *minerpeergetter.MinerPeerGetter
}

type retrievalCandidate struct {
	Miner   address.Address
	RootCid cid.Cid
	DealID  uint
}

func NewEstuaryEndpoint(url string, mpg *minerpeergetter.MinerPeerGetter) *EstuaryEndpoint {
	return &EstuaryEndpoint{
		url: url,
		mpg: mpg,
	}
}

func (ee *EstuaryEndpoint) FindCandidates(ctx context.Context, cid cid.Cid) ([]lassieretriever.RetrievalCandidate, error) {
	// Create URL with CID
	endpointURL, err := url.Parse(ee.url)
	if err != nil {
		return nil, fmt.Errorf("%w: '%s'", ErrInvalidEndpointURL, ee.url)
	}
	endpointURL.Path = path.Join(endpointURL.Path, cid.String())

	// Request candidates from endpoint
	resp, err := http.Get(endpointURL.String())
	if err != nil {
		return nil, fmt.Errorf("%w: HTTP request failed: %v", ErrEndpointRequestFailed, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: %s sent status %v", ErrEndpointRequestFailed, endpointURL, resp.StatusCode)
	}

	// Read candidate list from response body
	var unfiltered []retrievalCandidate
	if err := json.NewDecoder(resp.Body).Decode(&unfiltered); err != nil {
		return nil, ErrEndpointBodyInvalid
	}

	converted := make([]lassieretriever.RetrievalCandidate, 0, len(unfiltered))
	for _, original := range unfiltered {
		minerPeer, err := ee.mpg.MinerPeer(ctx, original.Miner)
		if err != nil {
			return nil, fmt.Errorf("%w: failed to get miner peer: %v", ErrEndpointRequestFailed, err)
		}
		converted = append(converted, lassieretriever.RetrievalCandidate{
			MinerPeer: minerPeer,
			RootCid:   original.RootCid,
		})
	}
	return converted, nil
}
