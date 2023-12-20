package watchtower

import (
	"github.com/btcsuite/btclog"
	"github.com/voltagecloud/lnd/build"
	"github.com/voltagecloud/lnd/watchtower/lookout"
	"github.com/voltagecloud/lnd/watchtower/wtclient"
	"github.com/voltagecloud/lnd/watchtower/wtdb"
	"github.com/voltagecloud/lnd/watchtower/wtserver"
)

// log is a logger that is initialized with no output filters.  This
// means the package will not perform any logging by default until the caller
// requests it.
var log btclog.Logger

// The default amount of logging is none.
func init() {
	UseLogger(build.NewSubLogger("WTWR", nil))
}

// DisableLog disables all library log output.  Logging output is disabled
// by default until UseLogger is called.
func DisableLog() {
	UseLogger(btclog.Disabled)
}

// UseLogger uses a specified Logger to output package logging info.
// This should be used in preference to SetLogWriter if the caller is also
// using btclog.
func UseLogger(logger btclog.Logger) {
	log = logger
	lookout.UseLogger(logger)
	wtserver.UseLogger(logger)
	wtclient.UseLogger(logger)
	wtdb.UseLogger(logger)
}
