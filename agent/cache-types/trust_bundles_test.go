package cachetype

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/hashicorp/consul/agent/cache"
	"github.com/hashicorp/consul/proto/pbpeering"
)

func TestTrustBundles(t *testing.T) {
	client := NewMockTrustBundleLister(t)
	typ := &TrustBundles{Client: client}

	resp := &pbpeering.TrustBundleListByServiceResponse{
		Index: 48,
		Bundles: []*pbpeering.PeeringTrustBundle{
			{
				PeerName: "peer1",
				RootPEMs: []string{"peer1-roots"},
			},
		},
	}

	// Expect the proper call.
	// This also returns the canned response above.
	client.On("TrustBundleListByService", mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) {
			req := args.Get(1).(*pbpeering.TrustBundleListByServiceRequest)
			require.Equal(t, "foo", req.ServiceName)
		}).
		Return(resp, nil)

	// Fetch and assert against the result.
	result, err := typ.Fetch(cache.FetchOptions{}, &TrustBundleListRequest{
		Request: &pbpeering.TrustBundleListByServiceRequest{
			ServiceName: "foo",
		},
	})
	require.NoError(t, err)
	require.Equal(t, cache.FetchResult{
		Value: resp,
		Index: 48,
	}, result)
}

func TestTrustBundles_badReqType(t *testing.T) {
	client := pbpeering.NewPeeringServiceClient(nil)
	typ := &TrustBundles{Client: client}

	// Fetch
	_, err := typ.Fetch(cache.FetchOptions{}, cache.TestRequest(
		t, cache.RequestInfo{Key: "foo", MinIndex: 64}))
	require.Error(t, err)
	require.Contains(t, err.Error(), "wrong type")
}

// This test asserts that we can continuously poll this cache type, given that it doesn't support blocking.
func TestTrustBundles_MultipleUpdates(t *testing.T) {
	c := cache.New(cache.Options{})

	client := NewMockTrustBundleLister(t)

	// On each mock client call to TrustBundleList by service we will increment the index by 1
	// to simulate new data arriving.
	resp := &pbpeering.TrustBundleListByServiceResponse{
		Index: uint64(0),
	}

	client.On("TrustBundleListByService", mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) {
			req := args.Get(1).(*pbpeering.TrustBundleListByServiceRequest)
			require.Equal(t, "foo", req.ServiceName)

			// Increment on each call.
			resp.Index++
		}).
		Return(resp, nil)

	c.RegisterType(TrustBundleListName, &TrustBundles{Client: client})

	ch := make(chan cache.UpdateEvent)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	err := c.Notify(ctx, TrustBundleListName, &TrustBundleListRequest{
		Request: &pbpeering.TrustBundleListByServiceRequest{ServiceName: "foo"},
	}, "updates", ch)
	require.NoError(t, err)

	i := uint64(1)
	for {
		select {
		case <-ctx.Done():
			return
		case update := <-ch:
			// Expect to receive updates for increasing indexes serially.
			resp := update.Result.(*pbpeering.TrustBundleListByServiceResponse)
			require.Equal(t, i, resp.Index)
			i++

			if i > 3 {
				return
			}
		}
	}
}
