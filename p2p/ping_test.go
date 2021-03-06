package p2p

import (
	"context"
	"github.com/qri-io/qri/p2p/test"
	"testing"
)

func TestPing(t *testing.T) {
	ctx := context.Background()
	testPeers, err := p2ptest.NewTestNetwork(ctx, t, 3, NewTestQriNode)
	if err != nil {
		t.Errorf("error creating network: %s", err.Error())
		return
	}

	// Convert from test nodes to non-test nodes.
	peers := make([]*QriNode, len(testPeers))
	for i, arg := range testPeers {
		peers[i] = arg.(*QriNode)
	}

	if err := p2ptest.ConnectNodes(ctx, testPeers); err != nil {
		t.Errorf("error connecting peers: %s", err.Error())
	}

	for i, p1 := range peers {
		for _, p2 := range peers[i+1:] {
			lat, err := p1.Ping(p2.ID)
			if err != nil {
				t.Errorf("%s -> %s error: %s", p1.ID.Pretty(), p2.ID.Pretty(), err.Error())
				return
			}
			t.Logf("%s Ping: %s: %s", p1.ID, p2.ID, lat)
		}
	}
}
