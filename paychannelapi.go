package main

import (
	"context"

	"github.com/application-research/filclient"
	"github.com/filecoin-project/go-address"
	"github.com/filecoin-project/go-state-types/crypto"
	"github.com/filecoin-project/lotus/api"
	"github.com/filecoin-project/lotus/chain/types"
	"github.com/filecoin-project/lotus/chain/wallet"
)

type payChannelApiProvider struct {
	api.Gateway
	wallet *wallet.LocalWallet
	mp     *filclient.MsgPusher
}

func (a *payChannelApiProvider) MpoolPushMessage(ctx context.Context, msg *types.Message, maxFee *api.MessageSendSpec) (*types.SignedMessage, error) {
	return a.mp.MpoolPushMessage(ctx, msg, maxFee)
}

func (a *payChannelApiProvider) WalletHas(ctx context.Context, addr address.Address) (bool, error) {
	return a.wallet.WalletHas(ctx, addr)
}

func (a *payChannelApiProvider) WalletSign(ctx context.Context, addr address.Address, data []byte) (*crypto.Signature, error) {
	return a.wallet.WalletSign(ctx, addr, data, api.MsgMeta{Type: api.MTUnknown})
}
