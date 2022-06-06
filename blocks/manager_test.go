package blocks

import (
	"context"
	"testing"

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
	receivedBlk := make(chan cid.Cid)
	errChan := make(chan error, 1)
	go func() {
		err := manager.GetAwait(ctx, blk.Cid(), func(blk Block) {
			receivedBlk <- blk.Cid()
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
	case <-receivedBlk:
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
			err := manager.GetAwait(ctx, blk.Cid(), func(blk Block) {
				receivedBlocks <- blk.Cid()
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
