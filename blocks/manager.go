package blocks

import (
	"context"
	"errors"
	"sync"
	"time"

	blocks "github.com/ipfs/go-block-format"
	"github.com/ipfs/go-cid"
	blockstore "github.com/ipfs/go-ipfs-blockstore"
	ipld "github.com/ipfs/go-ipld-format"
	logging "github.com/ipfs/go-log/v2"
)

var log = logging.Logger("blockstore")

var ErrWaitTimeout = errors.New("wait timeout for block")

type Block struct {
	Cid  cid.Cid
	Size int
}

// Manager is a blockstore with thread safe notification hooking for put
// events.
type Manager struct {
	blockstore.Blockstore
	readyBlocks     chan Block
	waitList        map[cid.Cid][]waitListEntry
	waitListLk      sync.Mutex
	getAwaitTimeout time.Duration
}

type waitListEntry struct {
	callback     func(Block, error)
	registeredAt time.Time
}

func NewManager(inner blockstore.Blockstore, getAwaitTimeout time.Duration) *Manager {
	mgr := &Manager{
		Blockstore:      inner,
		readyBlocks:     make(chan Block, 10),
		waitList:        make(map[cid.Cid][]waitListEntry),
		getAwaitTimeout: getAwaitTimeout,
	}

	go mgr.startPollReadyBlocks()

	// Only do cleanups if a timeout is actually set
	if getAwaitTimeout != 0 {
		go mgr.startPollCleanup()
	}

	return mgr
}

func (mgr *Manager) AwaitBlock(ctx context.Context, cid cid.Cid, callback func(Block, error)) {
	// We need to lock the blockstore here to make sure the requested block
	// doesn't get added while being added to the waitlist
	mgr.waitListLk.Lock()

	size, err := mgr.GetSize(ctx, cid)

	// If we couldn't get the block, we add it to the waitlist - the block will
	// be populated later during a Put or PutMany event
	if err != nil {
		if !ipld.IsNotFound(err) {
			mgr.waitListLk.Unlock()
			callback(Block{}, err)
			return
		}

		mgr.waitList[cid] = append(mgr.waitList[cid], waitListEntry{
			callback:     callback,
			registeredAt: time.Now(),
		})

		mgr.waitListLk.Unlock()
		return
	}

	mgr.waitListLk.Unlock()

	// Otherwise, we can immediately run the callback
	callback(Block{cid, size}, nil)
}

func (mgr *Manager) Put(ctx context.Context, block blocks.Block) error {

	// We do this first since it should catch any errors with block being nil
	if err := mgr.Blockstore.Put(ctx, block); err != nil {
		log.Debugw("err save block", "cid", block.Cid(), "error", err)
		return err
	}

	select {
	case mgr.readyBlocks <- Block{block.Cid(), len(block.RawData())}:
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
		case mgr.readyBlocks <- Block{block.Cid(), len(block.RawData())}:
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	return nil
}

func (mgr *Manager) startPollReadyBlocks() {
	for block := range mgr.readyBlocks {
		mgr.notifyWaitCallbacks(block)
	}
}

func (mgr *Manager) notifyWaitCallbacks(block Block) {
	cid := block.Cid
	mgr.waitListLk.Lock()
	entries, ok := mgr.waitList[cid]
	if ok {
		delete(mgr.waitList, cid)
	}
	mgr.waitListLk.Unlock()
	if ok {
		for _, entry := range entries {
			entry.callback(block, nil)
		}
	}
}

func (mgr *Manager) startPollCleanup() {
	for range time.Tick(time.Second * 1) {
		mgr.waitListLk.Lock()
		for cid := range mgr.waitList {
			// For each element in the slice for this CID...
			for i := 0; i < len(mgr.waitList[cid]); i++ {
				// ...check if it's timed out...
				if time.Since(mgr.waitList[cid][i].registeredAt) > mgr.getAwaitTimeout {
					// ...and if so, delete this element by replacing it with
					// the last element of the slice and shrinking the length by
					// 1, and step the index back
					mgr.waitList[cid][i].callback(Block{}, ErrWaitTimeout)
					mgr.waitList[cid][i] = mgr.waitList[cid][len(mgr.waitList[cid])-1]
					mgr.waitList[cid] = mgr.waitList[cid][:len(mgr.waitList[cid])-1]
					i--
				}
			}

			// If the slice is empty now, remove it entirely from the waitList
			// map
			if len(mgr.waitList[cid]) == 0 {
				delete(mgr.waitList, cid)
			}
		}
		mgr.waitListLk.Unlock()
	}
}
