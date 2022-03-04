package endpoint

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"

	"github.com/application-research/autoretrieve/filecoin"
	"github.com/application-research/filclient"
	"github.com/filecoin-project/go-address"
	"github.com/ipfs/go-cid"
)

var (
	ErrInvalidEndpointURL    = errors.New("invalid endpoint URL")
	ErrEndpointRequestFailed = errors.New("endpoint request failed")
	ErrEndpointBodyInvalid   = errors.New("endpoint body invalid")
)

type EstuaryEndpoint struct {
	url string
	fc  *filclient.FilClient
}

type retrievalCandidate struct {
	Miner   address.Address
	RootCid cid.Cid
	DealID  uint
}

func NewEstuaryEndpoint(fc *filclient.FilClient, url string) *EstuaryEndpoint {
	return &EstuaryEndpoint{
		url: url,
		fc:  fc,
	}
}

func (ee *EstuaryEndpoint) FindCandidates(ctx context.Context, cid cid.Cid) ([]filecoin.RetrievalCandidate, error) {
	// Create URL with CID
	endpointURL, err := url.Parse(ee.url)
	if err != nil {
		return nil, fmt.Errorf("%w: '%s'", ErrInvalidEndpointURL, ee.url)
	}
	endpointURL.Path = path.Join(endpointURL.Path, cid.String())

	// Request candidates from endpoint
	resp, err := http.Get(endpointURL.String())
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrEndpointRequestFailed, err)
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

	converted := make([]filecoin.RetrievalCandidate, 0, len(unfiltered))
	for _, original := range unfiltered {
		minerPeer, err := ee.fc.MinerPeer(ctx, original.Miner)
		if err != nil {
			return nil, err
		}
		converted = append(converted, filecoin.RetrievalCandidate{
			MinerPeer: minerPeer,
			RootCid:   original.RootCid,
		})
	}
	return converted, nil
}
