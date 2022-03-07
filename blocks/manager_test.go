package blocks

import (
	"context"
	"testing"

	"github.com/ipfs/go-datastore"
	blockstore "github.com/ipfs/go-ipfs-blockstore"
	blocksutil "github.com/ipfs/go-ipfs-blocksutil"
)

func TestPutBlock(t *testing.T) {
	ds := datastore.NewMapDatastore()
	bs := blockstore.NewBlockstore(ds)
	manager := newManager(bs)
	ctx := context.Background()
	blockGenerator := blocksutil.NewBlockGenerator()
	blk := blockGenerator.Next()
	err := manager.Put(ctx, blk)
	if err != nil {
		t.Fatal("wasn't able to put to blockstore")
	}
}
