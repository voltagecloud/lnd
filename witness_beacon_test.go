package lnd

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/voltagecloud/lnd/channeldb"
	"github.com/voltagecloud/lnd/htlcswitch"
	"github.com/voltagecloud/lnd/htlcswitch/hop"
	"github.com/voltagecloud/lnd/lntypes"
	"github.com/voltagecloud/lnd/lnwire"
)

// TestWitnessBeaconIntercept tests that the beacon passes on subscriptions to
// the interceptor correctly.
func TestWitnessBeaconIntercept(t *testing.T) {
	var interceptedFwd htlcswitch.InterceptedForward
	interceptor := func(fwd htlcswitch.InterceptedForward) error {
		interceptedFwd = fwd

		return nil
	}

	p := newPreimageBeacon(
		&mockWitnessCache{}, interceptor,
	)

	preimage := lntypes.Preimage{1, 2, 3}
	hash := preimage.Hash()

	subscription, err := p.SubscribeUpdates(
		lnwire.NewShortChanIDFromInt(1),
		&channeldb.HTLC{
			RHash: hash,
		},
		&hop.Payload{},
		[]byte{2},
	)
	require.NoError(t, err)
	t.Cleanup(subscription.CancelSubscription)

	require.NoError(t, interceptedFwd.Settle(preimage))

	update := <-subscription.WitnessUpdates
	require.Equal(t, preimage, update)
}

type mockWitnessCache struct {
	witnessCache
}

func (w *mockWitnessCache) AddSha256Witnesses(
	preimages ...lntypes.Preimage) error {

	return nil
}
