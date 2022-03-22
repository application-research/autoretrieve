package blocks

import (
	"context"
	"errors"
	"sync"

	blocks "github.com/ipfs/go-block-format"
	"github.com/ipfs/go-cid"
	blockstore "github.com/ipfs/go-ipfs-blockstore"
	logging "github.com/ipfs/go-log/v2"
)

var log = logging.Logger("blockstore")

type Block = blocks.Block

// Manager is a blockstore with thread safe notification hooking for put
// events.
type Manager struct {
	blockstore.Blockstore
	readyBlocks chan Block
	waitList    map[cid.Cid][]func(Block)
	waitListLk  sync.Mutex
}

func NewManager(inner blockstore.Blockstore) *Manager {
	mgr := &Manager{
		Blockstore:  inner,
		readyBlocks: make(chan Block, 10),
		waitList:    make(map[cid.Cid][]func(Block)),
	}

	go mgr.pollNotifyWaitCallbacks()

	return mgr
}

func (mgr *Manager) GetAwait(ctx context.Context, cid cid.Cid, callback func(Block)) error {
	// We need to lock the blockstore here to make sure the requested block
	// doesn't get added while being added to the waitlist
	mgr.waitListLk.Lock()

	block, err := mgr.Get(ctx, cid)

	// If we couldn't get the block, we add it to the waitlist - the block will
	// be populated later during a Put or PutMany event
	if err != nil {
		if !errors.Is(err, blockstore.ErrNotFound) {
			mgr.waitListLk.Unlock()
			return err
		}

		mgr.waitList[cid] = append(mgr.waitList[cid], callback)

		mgr.waitListLk.Unlock()

		return nil
	}

	mgr.waitListLk.Unlock()

	// Otherwise, we can immediately run the callback
	callback(block)

	return nil
}

func (mgr *Manager) Put(ctx context.Context, block blocks.Block) error {

	// We do this first since it should catch any errors with block being nil
	if err := mgr.Blockstore.Put(ctx, block); err != nil {
		log.Debugw("err save block", "cid", block.Cid(), "error", err)
		return err
	}

	select {
	case mgr.readyBlocks <- block:
	case <-ctx.Done():
		return ctx.Err()
	}

	log.Debugw("finish save block", "cid", block.Cid())

	return nil
}

func (mgr *Manager) PutMany(ctx context.Context, blocks []blocks.Block) error {

	if err := mgr.Blockstore.PutMany(ctx, blocks); err != nil {
		return err
	}

	for _, block := range blocks {
		select {
		case mgr.readyBlocks <- block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	return nil
}

func (mgr *Manager) pollNotifyWaitCallbacks() {
	for block := range mgr.readyBlocks {
		mgr.notifyWaitCallbacks(block)
	}
}

func (mgr *Manager) notifyWaitCallbacks(block Block) {
	cid := block.Cid()

	mgr.waitListLk.Lock()
	if blockChans, ok := mgr.waitList[cid]; ok {
		delete(mgr.waitList, cid)
		for _, callback := range blockChans {
			callback(block)
		}
	}
	mgr.waitListLk.Unlock()
}
