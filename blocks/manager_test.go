package blocks

import (
	"context"
	"testing"
	"time"

	blocks "github.com/ipfs/go-block-format"
	"github.com/ipfs/go-cid"
	"github.com/ipfs/go-datastore"
	"github.com/ipfs/go-datastore/sync"
	blockstore "github.com/ipfs/go-ipfs-blockstore"
	blocksutil "github.com/ipfs/go-ipfs-blocksutil"
)

func TestPutBlock(t *testing.T) {
	ctx := context.Background()
	ds := sync.MutexWrap(datastore.NewMapDatastore())
	bs := blockstore.NewBlockstore(ds)
	manager := NewManager(bs, 0)
	blockGenerator := blocksutil.NewBlockGenerator()
	blk := blockGenerator.Next()
	receivedBlk := make(chan Block)
	errChan := make(chan error, 1)
	go func() {
		err := manager.AwaitBlock(ctx, blk.Cid(), func(blk Block) {
			receivedBlk <- blk
		})
		if err != nil {
			errChan <- err
		}
	}()
	err := manager.Put(ctx, blk)
	if err != nil {
		t.Fatal("wasn't able to put to blockstore")
	}
	select {
	case <-ctx.Done():
		t.Fatal("not enough succcesful blocks fetched")
	case err := <-errChan:
		t.Fatalf("received error getting block: %s", err)
	case received := <-receivedBlk:
		if received.Cid != blk.Cid() && received.Size != len(blk.RawData()) {
			t.Fatalf("received block bit with incorrect parameters")
		}
	}
}

func TestPutMany(t *testing.T) {
	ctx := context.Background()
	ds := sync.MutexWrap(datastore.NewMapDatastore())
	bs := blockstore.NewBlockstore(ds)
	manager := NewManager(bs, 0)
	blockGenerator := blocksutil.NewBlockGenerator()
	var blks []blocks.Block
	for i := 0; i < 20; i++ {
		blks = append(blks, blockGenerator.Next())
	}
	receivedBlocks := make(chan cid.Cid, 20)
	errChan := make(chan error, 20)
	go func() {
		for _, blk := range blks {
			err := manager.AwaitBlock(ctx, blk.Cid(), func(blk Block) {
				receivedBlocks <- blk.Cid
			})
			if err != nil {
				errChan <- err
			}
		}
	}()
	err := manager.PutMany(ctx, blks)
	if err != nil {
		t.Fatal("wasn't able to put to blockstore")
	}
	for i := 0; i < 20; i++ {
		select {
		case <-ctx.Done():
			t.Fatal("not enough succcesful blocks fetched")
		case err := <-errChan:
			t.Fatalf("received error getting block: %s", err)
		case <-receivedBlocks:
		}
	}
}

func TestCleanup(t *testing.T) {
	ctx := context.Background()
	ds := sync.MutexWrap(datastore.NewMapDatastore())
	bs := blockstore.NewBlockstore(ds)
	manager := NewManager(bs, time.Second)

	testCount := 1000000

	gen := blocksutil.NewBlockGenerator()
	for i := 0; i < testCount; i++ {
		cid := gen.Next().Cid()
		manager.AwaitBlock(ctx, cid, func(b Block) { t.Fatalf("This should not happen") })
	}

	// manager.waitListLk.Lock()
	// addedCount := len(manager.waitList)
	// manager.waitListLk.Unlock()

	// if addedCount != testCount {
	// 	t.Fatalf("Only %d wait callbacks got registered", addedCount)
	// }

	time.Sleep(time.Second * 2)

	manager.waitListLk.Lock()
	cleanedCount := len(manager.waitList)
	manager.waitListLk.Unlock()

	if cleanedCount != 0 {
		t.Fatalf("%d block wait callbacks were still present after cleanup deadline", cleanedCount)
	}
}
