package contractcourt

import (
	"github.com/voltagecloud/channeldb/models"
	"github.com/voltagecloud/lnd/channeldb"
)

type mockHTLCNotifier struct {
	HtlcNotifier
}

func (m *mockHTLCNotifier) NotifyFinalHtlcEvent(key models.CircuitKey,
	info channeldb.FinalHtlcInfo) { //nolint:whitespace
}
