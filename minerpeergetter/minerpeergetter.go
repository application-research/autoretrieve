package minerpeergetter

import (
	"context"
	"fmt"

	"github.com/filecoin-project/go-address"
	lotusapi "github.com/filecoin-project/lotus/api"
	"github.com/filecoin-project/lotus/chain/types"
	peer "github.com/libp2p/go-libp2p/core/peer"
	"github.com/multiformats/go-multiaddr"
)

type MinerPeerGetter struct {
	api lotusapi.Gateway
}

func NewMinerPeerGetter(api lotusapi.Gateway) *MinerPeerGetter {
	return &MinerPeerGetter{api: api}
}

func (mpg *MinerPeerGetter) MinerPeer(ctx context.Context, miner address.Address) (peer.AddrInfo, error) {
	minfo, err := mpg.api.StateMinerInfo(ctx, miner, types.EmptyTSK)
	if err != nil {
		return peer.AddrInfo{}, err
	}

	if minfo.PeerId == nil {
		return peer.AddrInfo{}, fmt.Errorf("miner %s has no peer ID set", miner)
	}

	var maddrs []multiaddr.Multiaddr
	for _, mma := range minfo.Multiaddrs {
		ma, err := multiaddr.NewMultiaddrBytes(mma)
		if err != nil {
			return peer.AddrInfo{}, fmt.Errorf("miner %s had invalid multiaddrs in their info: %w", miner, err)
		}
		maddrs = append(maddrs, ma)
	}

	return peer.AddrInfo{
		ID:    *minfo.PeerId,
		Addrs: maddrs,
	}, nil
}
