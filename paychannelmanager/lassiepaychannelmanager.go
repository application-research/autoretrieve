package paychannelmanager

import (
	"context"
	"fmt"

	"github.com/filecoin-project/go-address"
	"github.com/filecoin-project/go-state-types/big"
	"github.com/filecoin-project/go-state-types/builtin/v8/paych"
	"github.com/filecoin-project/lotus/chain/stmgr"
	"github.com/filecoin-project/lotus/chain/types"
	"github.com/filecoin-project/lotus/paychmgr"
)

type LassiePayChannelManager struct {
	*paychmgr.Manager
	clientAddress address.Address
}

func NewLassiePayChannelManager(ctx context.Context, shutdown func(), stateManagerAPI stmgr.StateManagerAPI, payChanStore *paychmgr.Store, payChanApi paychmgr.PaychAPI) *LassiePayChannelManager {
	payChanMgr := paychmgr.NewManager(ctx, shutdown, stateManagerAPI, payChanStore, payChanApi)
	return &LassiePayChannelManager{
		Manager: payChanMgr,
	}
}

func (lpcm *LassiePayChannelManager) CreateVoucher(ctx context.Context, ch address.Address, voucher paych.SignedVoucher) (*paych.SignedVoucher, big.Int, error) {
	result, err := lpcm.Manager.CreateVoucher(ctx, ch, voucher)
	return result.Voucher, result.Shortfall, err
}

func (lpcm *LassiePayChannelManager) GetPayChannelWithMinFunds(ctx context.Context, dest address.Address) (address.Address, error) {
	avail, err := lpcm.Manager.AvailableFundsByFromTo(ctx, lpcm.clientAddress, dest)
	if err != nil {
		return address.Undef, err
	}

	reqBalance, err := types.ParseFIL("0.01")
	if err != nil {
		return address.Undef, err
	}
	fmt.Println("available", avail.ConfirmedAmt)

	if types.BigCmp(avail.ConfirmedAmt, types.BigInt(reqBalance)) >= 0 {
		return *avail.Channel, nil
	}

	amount := types.BigMul(types.BigInt(reqBalance), types.NewInt(2))

	fmt.Println("getting payment channel: ", lpcm.clientAddress, dest, amount)
	pchaddr, mcid, err := lpcm.Manager.GetPaych(ctx, lpcm.clientAddress, dest, amount, paychmgr.GetOpts{
		Reserve:  false,
		OffChain: false,
	})
	if err != nil {
		return address.Undef, fmt.Errorf("failed to get payment channel: %w", err)
	}

	fmt.Println("got payment channel: ", pchaddr, mcid)
	if !mcid.Defined() {
		if pchaddr == address.Undef {
			return address.Undef, fmt.Errorf("GetPaych returned nothing")
		}

		return pchaddr, nil
	}

	return lpcm.Manager.GetPaychWaitReady(ctx, mcid)
}
