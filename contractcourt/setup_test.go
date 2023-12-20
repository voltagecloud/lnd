package contractcourt

import (
	"testing"

	"github.com/voltagecloud/lnd/kvdb"
)

func TestMain(m *testing.M) {
	kvdb.RunTests(m)
}
