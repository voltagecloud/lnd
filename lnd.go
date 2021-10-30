// Copyright (c) 2013-2017 The btcsuite developers
// Copyright (c) 2015-2016 The Decred developers
// Copyright (C) 2015-2022 The Lightning Network Developers

package lnd

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	_ "net/http/pprof" // nolint:gosec // used to set up profiling HTTP handlers.
	"os"
	"runtime/pprof"
	"strings"
	"sync"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	proxy "github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"github.com/lightningnetwork/lnd/autopilot"
	"github.com/lightningnetwork/lnd/build"
	"github.com/lightningnetwork/lnd/cert"
	"github.com/lightningnetwork/lnd/chanacceptor"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lncfg"
	"github.com/lightningnetwork/lnd/lnencrypt"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/macaroons"
	"github.com/lightningnetwork/lnd/monitoring"
	"github.com/lightningnetwork/lnd/rpcperms"
	"github.com/lightningnetwork/lnd/signal"
	"github.com/lightningnetwork/lnd/tor"
	"github.com/lightningnetwork/lnd/tor/onionfile"
	"github.com/lightningnetwork/lnd/walletunlocker"
	"github.com/lightningnetwork/lnd/watchtower"
	"golang.org/x/crypto/acme/autocert"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/protobuf/encoding/protojson"
	"gopkg.in/macaroon-bakery.v2/bakery"
	"gopkg.in/macaroon.v2"
)

const (
	// adminMacaroonFilePermissions is the file permission that is used for
	// creating the admin macaroon file.
	//
	// Why 640 is safe:
	// Assuming a reasonably secure Linux system, it will have a
	// separate group for each user. E.g. a new user lnd gets assigned group
	// lnd which nothing else belongs to. A system that does not do this is
	// inherently broken already.
	//
	// Since there is no other user in the group, no other user can read
	// admin macaroon unless the administrator explicitly allowed it. Thus
	// there's no harm allowing group read.
	adminMacaroonFilePermissions = 0640
)

// AdminAuthOptions returns a list of DialOptions that can be used to
// authenticate with the RPC server with admin capabilities.
// skipMacaroons=true should be set if we don't want to include macaroons with
// the auth options. This is needed for instance for the WalletUnlocker
// service, which must be usable also before macaroons are created.
//
// NOTE: This should only be called after the RPCListener has signaled it is
// ready.
func AdminAuthOptions(cfg *Config, skipMacaroons bool) ([]grpc.DialOption,
	error) {

	creds, err := credentials.NewClientTLSFromFile(cfg.TLSCertPath, "")
	if err != nil {
		return nil, fmt.Errorf("unable to read TLS cert: %v", err)
	}

	// Create a dial options array.
	opts := []grpc.DialOption{
		grpc.WithTransportCredentials(creds),
	}

	// Get the admin macaroon if macaroons are active.
	if !skipMacaroons && !cfg.NoMacaroons {
		// Load the adming macaroon file.
		macBytes, err := ioutil.ReadFile(cfg.AdminMacPath)
		if err != nil {
			return nil, fmt.Errorf("unable to read macaroon "+
				"path (check the network setting!): %v", err)
		}

		mac := &macaroon.Macaroon{}
		if err = mac.UnmarshalBinary(macBytes); err != nil {
			return nil, fmt.Errorf("unable to decode macaroon: %v",
				err)
		}

		// Now we append the macaroon credentials to the dial options.
		cred, err := macaroons.NewMacaroonCredential(mac)
		if err != nil {
			return nil, fmt.Errorf("error cloning mac: %v", err)
		}
		opts = append(opts, grpc.WithPerRPCCredentials(cred))
	}

	return opts, nil
}

// ListenerWithSignal is a net.Listener that has an additional Ready channel
// that will be closed when a server starts listening.
type ListenerWithSignal struct {
	net.Listener

	// Ready will be closed by the server listening on Listener.
	Ready chan struct{}

	// MacChan is an optional way to pass the admin macaroon to the program
	// that started lnd. The channel should be buffered to avoid lnd being
	// blocked on sending to the channel.
	MacChan chan []byte
}

// ListenerCfg is a wrapper around custom listeners that can be passed to lnd
// when calling its main method.
type ListenerCfg struct {
	// RPCListeners can be set to the listeners to use for the RPC server.
	// If empty a regular network listener will be created.
	RPCListeners []*ListenerWithSignal
}

var errStreamIsolationWithProxySkip = errors.New(
	"while stream isolation is enabled, the TOR proxy may not be skipped",
)

// Main is the true entry point for lnd. It accepts a fully populated and
// validated main configuration struct and an optional listener config struct.
// This function starts all main system components then blocks until a signal
// is received on the shutdownChan at which point everything is shut down again.
func Main(cfg *Config, lisCfg ListenerCfg, implCfg *ImplementationCfg,
	interceptor signal.Interceptor) error {

	defer func() {
		ltndLog.Info("Shutdown complete\n")
		err := cfg.LogWriter.Close()
		if err != nil {
			ltndLog.Errorf("Could not close log rotator: %v", err)
		}
	}()

	mkErr := func(format string, args ...interface{}) error {
		ltndLog.Errorf("Shutting down because error in main "+
			"method: "+format, args...)
		return fmt.Errorf(format, args...)
	}

	// Show version at startup.
	ltndLog.Infof("Version: %s commit=%s, build=%s, logging=%s, "+
		"debuglevel=%s", build.Version(), build.Commit,
		build.Deployment, build.LoggingType, cfg.DebugLevel)

	var network string
	switch {
	case cfg.Bitcoin.TestNet3 || cfg.Litecoin.TestNet3:
		network = "testnet"

	case cfg.Bitcoin.MainNet || cfg.Litecoin.MainNet:
		network = "mainnet"

	case cfg.Bitcoin.SimNet || cfg.Litecoin.SimNet:
		network = "simnet"

	case cfg.Bitcoin.RegTest || cfg.Litecoin.RegTest:
		network = "regtest"

	case cfg.Bitcoin.SigNet:
		network = "signet"
	}

	ltndLog.Infof("Active chain: %v (network=%v)",
		strings.Title(cfg.registeredChains.PrimaryChain().String()),
		network,
	)

	// Enable http profiling server if requested.
	if cfg.Profile != "" {
		go func() {
			profileRedirect := http.RedirectHandler("/debug/pprof",
				http.StatusSeeOther)
			http.Handle("/", profileRedirect)
			ltndLog.Infof("Pprof listening on %v", cfg.Profile)
			fmt.Println(http.ListenAndServe(cfg.Profile, nil))
		}()
	}

	// Write cpu profile if requested.
	if cfg.CPUProfile != "" {
		f, err := os.Create(cfg.CPUProfile)
		if err != nil {
			return mkErr("unable to create CPU profile: %v", err)
		}
		pprof.StartCPUProfile(f)
		defer f.Close()
		defer pprof.StopCPUProfile()
	}

	ctx := context.Background()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Run configuration dependent DB pre-initialization. Note that this
	// needs to be done early and once during the startup process, before
	// any DB access.
	if err := cfg.DB.Init(ctx, cfg.graphDatabaseDir()); err != nil {
		return mkErr("error initializing DBs: %v", err)
	}

	// The real KeyRing isn't available until after the wallet is unlocked,
	// but we need one now if --tlsencryptkey is true. Because we aren't
	// encrypting anything here it can be an empty KeyRing.
	var emptyKeyRing keychain.KeyRing

	// Only process macaroons if --no-macaroons isn't set.
	tlsManager := TLSManager{
		cfg:     cfg,
		keyRing: emptyKeyRing,
	}
	serverOpts,
		restDialOpts,
		restListen,
		cleanUp,
		err := tlsManager.getConfig()
	if err != nil {
		return mkErr("unable to load TLS credentials: %v", err)
	}
	defer cleanUp()

	// If we have chosen to start with a dedicated listener for the
	// rpc server, we set it directly.
	grpcListeners := append([]*ListenerWithSignal{}, lisCfg.RPCListeners...)
	if len(grpcListeners) == 0 {
		// Otherwise we create listeners from the RPCListeners defined
		// in the config.
		for _, grpcEndpoint := range cfg.RPCListeners {
			// Start a gRPC server listening for HTTP/2
			// connections.
			lis, err := lncfg.ListenOnAddress(grpcEndpoint)
			if err != nil {
				return mkErr("unable to listen on %s: %v",
					grpcEndpoint, err)
			}
			defer lis.Close()

			grpcListeners = append(
				grpcListeners, &ListenerWithSignal{
					Listener: lis,
					Ready:    make(chan struct{}),
				},
			)
		}
	}

	// Create a new RPC interceptor that we'll add to the GRPC server. This
	// will be used to log the API calls invoked on the GRPC server.
	interceptorChain := rpcperms.NewInterceptorChain(
		rpcsLog, cfg.NoMacaroons, cfg.RPCMiddleware.Mandatory,
	)
	if err := interceptorChain.Start(); err != nil {
		return mkErr("error starting interceptor chain: %v", err)
	}
	defer func() {
		err := interceptorChain.Stop()
		if err != nil {
			ltndLog.Warnf("error stopping RPC interceptor "+
				"chain: %v", err)
		}
	}()

	rpcServerOpts := interceptorChain.CreateServerOpts()
	serverOpts = append(serverOpts, rpcServerOpts...)
	serverOpts = append(
		serverOpts, grpc.MaxRecvMsgSize(lnrpc.MaxGrpcMsgSize),
	)

	grpcServer := grpc.NewServer(serverOpts...)
	defer grpcServer.Stop()

	// We'll also register the RPC interceptor chain as the StateServer, as
	// it can be used to query for the current state of the wallet.
	lnrpc.RegisterStateServer(grpcServer, interceptorChain)

	// Initialize, and register our implementation of the gRPC interface
	// exported by the rpcServer.
	rpcServer := newRPCServer(cfg, interceptorChain, implCfg, interceptor)
	err = rpcServer.RegisterWithGrpcServer(grpcServer)
	if err != nil {
		return mkErr("error registering gRPC server: %v", err)
	}

	// Now that both the WalletUnlocker and LightningService have been
	// registered with the GRPC server, we can start listening.
	err = startGrpcListen(cfg, grpcServer, grpcListeners)
	if err != nil {
		return mkErr("error starting gRPC listener: %v", err)
	}

	// Now start the REST proxy for our gRPC server above. We'll ensure
	// we direct LND to connect to its loopback address rather than a
	// wildcard to prevent certificate issues when accessing the proxy
	// externally.
	stopProxy, err := startRestProxy(
		cfg, rpcServer, restDialOpts, restListen,
	)
	if err != nil {
		return mkErr("error starting REST proxy: %v", err)
	}
	defer stopProxy()

	// Start leader election if we're running on etcd. Continuation will be
	// blocked until this instance is elected as the current leader or
	// shutting down.
	elected := false
	if cfg.Cluster.EnableLeaderElection {
		electionCtx, cancelElection := context.WithCancel(ctx)

		go func() {
			<-interceptor.ShutdownChannel()
			cancelElection()
		}()

		ltndLog.Infof("Using %v leader elector",
			cfg.Cluster.LeaderElector)

		leaderElector, err := cfg.Cluster.MakeLeaderElector(
			electionCtx, cfg.DB,
		)
		if err != nil {
			return err
		}

		defer func() {
			if !elected {
				return
			}

			ltndLog.Infof("Attempting to resign from leader role "+
				"(%v)", cfg.Cluster.ID)

			if err := leaderElector.Resign(); err != nil {
				ltndLog.Errorf("Leader elector failed to "+
					"resign: %v", err)
			}
		}()

		ltndLog.Infof("Starting leadership campaign (%v)",
			cfg.Cluster.ID)

		if err := leaderElector.Campaign(electionCtx); err != nil {
			return mkErr("leadership campaign failed: %v", err)
		}

		elected = true
		ltndLog.Infof("Elected as leader (%v)", cfg.Cluster.ID)
	}

	dbs, cleanUp, err := implCfg.DatabaseBuilder.BuildDatabase(ctx)
	switch {
	case err == channeldb.ErrDryRunMigrationOK:
		ltndLog.Infof("%v, exiting", err)
		return nil
	case err != nil:
		return mkErr("unable to open databases: %v", err)
	}

	defer cleanUp()

	partialChainControl, walletConfig, cleanUp, err := implCfg.BuildWalletConfig(
		ctx, dbs, interceptorChain, grpcListeners,
	)
	if err != nil {
		return mkErr("error creating wallet config: %v", err)
	}

	defer cleanUp()

	activeChainControl, cleanUp, err := implCfg.BuildChainControl(
		partialChainControl, walletConfig,
	)
	if err != nil {
		return mkErr("error loading chain control: %v", err)
	}

	defer cleanUp()

	// Finally before we start the server, we'll register the "holy
	// trinity" of interface for our current "home chain" with the active
	// chainRegistry interface.
	primaryChain := cfg.registeredChains.PrimaryChain()
	cfg.registeredChains.RegisterChain(primaryChain, activeChainControl)

	// TODO(roasbeef): add rotation
	idKeyDesc, err := activeChainControl.KeyRing.DeriveKey(
		keychain.KeyLocator{
			Family: keychain.KeyFamilyNodeKey,
			Index:  0,
		},
	)
	if err != nil {
		return mkErr("error deriving node key: %v", err)
	}

	if cfg.Tor.StreamIsolation && cfg.Tor.SkipProxyForClearNetTargets {
		return errStreamIsolationWithProxySkip
	}

	if cfg.Tor.Active {
		if cfg.Tor.SkipProxyForClearNetTargets {
			srvrLog.Info("Onion services are accessible via Tor! " +
				"NOTE: Traffic to clearnet services is not " +
				"routed via Tor.")
		} else {
			srvrLog.Infof("Proxying all network traffic via Tor "+
				"(stream_isolation=%v)! NOTE: Ensure the "+
				"backend node is proxying over Tor as well",
				cfg.Tor.StreamIsolation)
		}
	}

	// If tor is active and either v2 or v3 onion services have been
	// specified, make a tor controller and pass it into both the watchtower
	// server and the regular lnd server.
	var torController *tor.Controller
	if cfg.Tor.Active && (cfg.Tor.V2 || cfg.Tor.V3) {
		torController = tor.NewController(
			cfg.Tor.Control, cfg.Tor.TargetIPAddress,
			cfg.Tor.Password,
		)

		// Start the tor controller before giving it to any other
		// subsystems.
		if err := torController.Start(); err != nil {
			return mkErr("unable to initialize tor controller: %v",
				err)
		}
		defer func() {
			if err := torController.Stop(); err != nil {
				ltndLog.Errorf("error stopping tor "+
					"controller: %v", err)
			}
		}()
	}

	var tower *watchtower.Standalone
	if cfg.Watchtower.Active {
		towerKeyDesc, err := activeChainControl.KeyRing.DeriveKey(
			keychain.KeyLocator{
				Family: keychain.KeyFamilyTowerID,
				Index:  0,
			},
		)
		if err != nil {
			return mkErr("error deriving tower key: %v", err)
		}

		wtCfg := &watchtower.Config{
			BlockFetcher:   activeChainControl.ChainIO,
			DB:             dbs.TowerServerDB,
			EpochRegistrar: activeChainControl.ChainNotifier,
			Net:            cfg.net,
			NewAddress: func() (btcutil.Address, error) {
				return activeChainControl.Wallet.NewAddress(
					lnwallet.WitnessPubKey, false,
					lnwallet.DefaultAccountName,
				)
			},
			NodeKeyECDH: keychain.NewPubKeyECDH(
				towerKeyDesc, activeChainControl.KeyRing,
			),
			PublishTx: activeChainControl.Wallet.PublishTransaction,
			ChainHash: *cfg.ActiveNetParams.GenesisHash,
		}

		// If there is a tor controller (user wants auto hidden
		// services), then store a pointer in the watchtower config.
		if torController != nil {
			wtCfg.TorController = torController
			wtCfg.WatchtowerKeyPath = cfg.Tor.WatchtowerKeyPath
			wtCfg.EncryptKey = cfg.Tor.EncryptKey
			wtCfg.KeyRing = activeChainControl.KeyRing

			switch {
			case cfg.Tor.V2:
				wtCfg.Type = onionfile.V2
			case cfg.Tor.V3:
				wtCfg.Type = onionfile.V3
			}
		}

		wtConfig, err := cfg.Watchtower.Apply(
			wtCfg, lncfg.NormalizeAddresses,
		)
		if err != nil {
			return mkErr("unable to configure watchtower: %v", err)
		}

		tower, err = watchtower.New(wtConfig)
		if err != nil {
			return mkErr("unable to create watchtower: %v", err)
		}
	}

	// Initialize the ChainedAcceptor.
	chainedAcceptor := chanacceptor.NewChainedAcceptor()

	// Set up the core server which will listen for incoming peer
	// connections.
	server, err := newServer(
		cfg, cfg.Listeners, dbs, activeChainControl, &idKeyDesc,
		activeChainControl.Cfg.WalletUnlockParams.ChansToRestore,
		chainedAcceptor, torController,
	)
	if err != nil {
		return mkErr("unable to create server: %v", err)
	}

	// Set up an autopilot manager from the current config. This will be
	// used to manage the underlying autopilot agent, starting and stopping
	// it at will.
	atplCfg, err := initAutoPilot(
		server, cfg.Autopilot, activeChainControl.MinHtlcIn,
		cfg.ActiveNetParams,
	)
	if err != nil {
		return mkErr("unable to initialize autopilot: %v", err)
	}

	atplManager, err := autopilot.NewManager(atplCfg)
	if err != nil {
		return mkErr("unable to create autopilot manager: %v", err)
	}
	if err := atplManager.Start(); err != nil {
		return mkErr("unable to start autopilot manager: %v", err)
	}
	defer atplManager.Stop()

	// If --tlsencryptkey is set, we previously generated a throwaway TLSConfig
	// Now we want to remove that and load the persistent TLSConfig
	// The wallet is unlocked at this point so we can use the real KeyRing
	if cfg.TLSEncryptKey {
		// Update tlsManager with actual key.
		tlsManager.keyRing = activeChainControl.KeyRing
		err := tlsManager.reloadCertificate()
		if err != nil {
			err := fmt.Errorf("unable to reload TLS credentials: "+
				"%v", err)
			ltndLog.Error(err)
		}
	}

	// Now we have created all dependencies necessary to populate and
	// start the RPC server.
	err = rpcServer.addDeps(
		server, interceptorChain.MacaroonService(), cfg.SubRPCServers,
		atplManager, server.invoices, tower, chainedAcceptor,
	)
	if err != nil {
		return mkErr("unable to add deps to RPC server: %v", err)
	}
	if err := rpcServer.Start(); err != nil {
		return mkErr("unable to start RPC server: %v", err)
	}
	defer rpcServer.Stop()

	// We transition the RPC state to Active, as the RPC server is up.
	interceptorChain.SetRPCActive()

	if err := interceptor.Notifier.NotifyReady(true); err != nil {
		return mkErr("error notifying ready: %v", err)
	}

	// We'll wait until we're fully synced to continue the start up of the
	// remainder of the daemon. This ensures that we don't accept any
	// possibly invalid state transitions, or accept channels with spent
	// funds.
	_, bestHeight, err := activeChainControl.ChainIO.GetBestBlock()
	if err != nil {
		return mkErr("unable to determine chain tip: %v", err)
	}

	ltndLog.Infof("Waiting for chain backend to finish sync, "+
		"start_height=%v", bestHeight)

	for {
		if !interceptor.Alive() {
			return nil
		}

		synced, _, err := activeChainControl.Wallet.IsSynced()
		if err != nil {
			return mkErr("unable to determine if wallet is "+
				"synced: %v", err)
		}

		if synced {
			break
		}

		time.Sleep(time.Second * 1)
	}

	_, bestHeight, err = activeChainControl.ChainIO.GetBestBlock()
	if err != nil {
		return mkErr("unable to determine chain tip: %v", err)
	}

	ltndLog.Infof("Chain backend is fully synced (end_height=%v)!",
		bestHeight)

	// With all the relevant chains initialized, we can finally start the
	// server itself.
	if err := server.Start(); err != nil {
		return mkErr("unable to start server: %v", err)
	}
	defer server.Stop()

	// We transition the server state to Active, as the server is up.
	interceptorChain.SetServerActive()

	// Now that the server has started, if the autopilot mode is currently
	// active, then we'll start the autopilot agent immediately. It will be
	// stopped together with the autopilot service.
	if cfg.Autopilot.Active {
		if err := atplManager.StartAgent(); err != nil {
			return mkErr("unable to start autopilot agent: %v", err)
		}
	}

	if cfg.Watchtower.Active {
		if err := tower.Start(); err != nil {
			return mkErr("unable to start watchtower: %v", err)
		}
		defer tower.Stop()
	}

	// Wait for shutdown signal from either a graceful server stop or from
	// the interrupt handler.
	<-interceptor.ShutdownChannel()
	return nil
}

// TLSManager generates/renews a TLS certificate if needed and returns the
// certificate configuration options needed for gRPC and REST.
type TLSManager struct {
	cfg *Config

	certBytes  []byte
	keyBytes   []byte
	certData   *tls.Certificate
	parsedCert *x509.Certificate
	certPath   string
	keyRing    keychain.KeyRing

	tlsCfg      *tls.Config
	tlsReloader *cert.TlsReloader
}

// getConfig returns a TLS configuration for the gRPC server and credentials
// and a proxy destination for the REST reverse proxy.
func (t *TLSManager) getConfig() ([]grpc.ServerOption, []grpc.DialOption,
	func(net.Addr) (net.Listener, error), func(), error) {

	err := t.generateOrRenewCert()
	if err != nil {
		return nil, nil, nil, nil, err
	}

	tlsr, err := cert.NewTLSReloader(t.certBytes, t.keyBytes)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	t.tlsReloader = tlsr
	tlsCfg := cert.TLSConfFromCert(*t.certData)
	t.tlsCfg = tlsCfg
	t.tlsCfg.GetCertificate = tlsr.GetCertificateFunc()

	// Now that we know that we have a ceritificate, let's generate the
	// required config options.
	restCreds, err := credentials.NewClientTLSFromFile(t.certPath, "")
	if err != nil {
		return nil, nil, nil, nil, err
	}

        cleanUp, err := t.setUpLetsEncrypt(tlsCfg)
        if err != nil {
                return nil, nil, nil, nil, err
        }

	serverCreds := credentials.NewTLS(t.tlsCfg)
	serverOpts := []grpc.ServerOption{grpc.Creds(serverCreds)}

	// For our REST dial options, we'll still use TLS, but also increase
	// the max message size that we'll decode to allow clients to hit
	// endpoints which return more data such as the DescribeGraph call.
	// We set this to 200MiB atm. Should be the same value as maxMsgRecvSize
	// in cmd/lncli/main.go.
	restDialOpts := []grpc.DialOption{
		grpc.WithTransportCredentials(restCreds),
		grpc.WithDefaultCallOptions(
			grpc.MaxCallRecvMsgSize(lnrpc.MaxGrpcMsgSize),
		),
	}

	// Return a function closure that can be used to listen on a given
	// address with the current TLS config.
	restListen := func(addr net.Addr) (net.Listener, error) {
		// For restListen we will call ListenOnAddress if TLS is
		// disabled.
		if t.cfg.DisableRestTLS {
			return lncfg.ListenOnAddress(addr)
		}

		return lncfg.TLSListenOnAddress(addr, t.tlsCfg)
	}

	return serverOpts, restDialOpts, restListen, cleanUp, nil
}

// generateOrRenewCert generates a new TLS certificate if we're not using one
// yet or renews it if it's outdated.
func (t *TLSManager) generateOrRenewCert() error {
	// Genereate a TLS pair if we don't have one yet.
	err := t.createTLSPair()
	if err != nil {
		return err
	}

	certData, parsedCert, err := cert.LoadCert(
		t.certBytes, t.keyBytes,
	)
	if err != nil {
		return err
	}
	t.certData = &certData
	t.parsedCert = parsedCert

	// Check to see if the certificate needs to be renewed.
	err = t.certMaintenance()
	if err != nil {
		return err
	}

	return nil
}

// If a TLS pair doesn't exist yet, we create them and write them to disk. And
// we encrypt the pair if needed. If the TLS pair already exists, we load it
// into memory.
func (t *TLSManager) createTLSPair() error {
	// If TLS Key Encryption is on but the keyring is empty then we need to
	// generate a temporary certificate.
	var emptyKeyRing keychain.KeyRing
	if t.cfg.TLSEncryptKey && (t.keyRing == emptyKeyRing) {
		rpcsLog.Infof("Generating ephemeral TLS certificates...")

		tmpValidity := 24 * time.Hour
		// Append .tmp to the end of the cert for differentiation.
		tmpCertPath := t.cfg.TLSCertPath + ".tmp"
		t.certPath = tmpCertPath
		// Pass in a blank string for the key path so the
		// function doesn't write them to disk.
		certBytes, keyBytes, err := cert.GenCertPair(
			"lnd temporary autogenerated cert", tmpCertPath,
			"", t.cfg.TLSExtraIPs, t.cfg.TLSExtraDomains,
			t.cfg.TLSDisableAutofill, tmpValidity,
		)
		if err != nil {
			return err
		}
		t.certBytes = certBytes
		t.keyBytes = keyBytes

		rpcsLog.Infof("Done generating ephemeral TLS certificates")

	} else {
		// Ensure we create TLS key and certificate if they don't exist.
		if !fileExists(t.cfg.TLSCertPath) && !fileExists(t.cfg.TLSKeyPath) {
			rpcsLog.Infof("Generating TLS certificates...")

			certBytes, keyBytes, err := cert.GenCertPair(
				"lnd autogenerated cert", t.cfg.TLSCertPath,
				t.cfg.TLSKeyPath, t.cfg.TLSExtraIPs,
				t.cfg.TLSExtraDomains, t.cfg.TLSDisableAutofill,
				t.cfg.TLSCertDuration,
			)
			if err != nil {
				return err
			}
			t.certBytes = certBytes
			t.keyBytes = keyBytes

			if t.cfg.TLSEncryptKey {
				keyBuf := bytes.NewBuffer(t.keyBytes)
				var b bytes.Buffer
				err := lnencrypt.EncryptPayloadToWriter(
					*keyBuf, &b, t.keyRing,
				)
				if err != nil {
					return err
				}
				if err = ioutil.WriteFile(t.cfg.TLSKeyPath, b.Bytes(),
					0600); err != nil {
					return err
				}
				t.keyBytes = b.Bytes()

			} else {
				keyBuf := bytes.NewBuffer(t.keyBytes)
				if err := ioutil.WriteFile(t.cfg.TLSKeyPath, keyBuf.Bytes(),
					0600); err != nil {
					return err
				}
			}

		} else {
			rpcsLog.Info("Getting the existing cert data")
			certBytes, keyBytes, err := cert.GetCertBytesFromPath(t.cfg.TLSCertPath, t.cfg.TLSKeyPath)
			if err != nil {
				return err
			}
			t.certBytes = certBytes
			t.keyBytes = keyBytes

			certData, parsedCert, err := cert.LoadCert(
				t.certBytes, t.keyBytes,
			)
			if err != nil {
				return err
			}
			t.certData = &certData
			t.parsedCert = parsedCert
		}
		t.certPath = t.cfg.TLSCertPath

		rpcsLog.Infof("Done generating TLS certificates")

		err := t.getTLSKey()
		if err != nil {
			return err
		}
	}

	return nil
}

// getTLSKey gets the TLS key, whether it's encrypted on disk or not.
func (t *TLSManager) getTLSKey() error {
	// We check to see if the private key is encrypted or plaintext.
	// If it's encrypted we need to try to decrypt it so we can use it
	// in the gRPC server.
	privateKeyPrefix := []byte("-----BEGIN EC PRIVATE KEY-----")
	if !bytes.HasPrefix(t.keyBytes, privateKeyPrefix) {
		rpcsLog.Info("Saw that the private key is encrypted")
		// If the private key is encrypted but the user didn't pass
		// --tlsencryptkey we error out. This is because the wallet is
		// not unlocked yet and we don't have access to the keys yet
		// for decrypt.
		if !t.cfg.TLSEncryptKey {
			return fmt.Errorf("it appears the TLS key is " +
				"encrypted but you didn't pass the " +
				"--tlsencryptkey flag. Please restart lnd " +
				"with the --tlsencryptkey flag or delete the " +
				"TLS files for regeneration")
		}
		reader := bytes.NewReader(t.keyBytes)
		keyBytes, err := lnencrypt.DecryptPayloadFromReader(
			reader, t.keyRing,
		)
		if err != nil {
			return err
		}
		t.keyBytes = keyBytes

	} else if t.cfg.TLSEncryptKey {
		// If the user requests an encrypted key but the key is in plaintext
		// we encrypt the key before writing to disk.
		keyBuf := bytes.NewBuffer(t.keyBytes)
		var b bytes.Buffer
		err := lnencrypt.EncryptPayloadToWriter(*keyBuf, &b, t.keyRing)
		if err != nil {
			return err
		}
		if err = ioutil.WriteFile(t.cfg.TLSKeyPath, b.Bytes(), 0600); err != nil {
			return err
		}
	}

	return nil
}

// certMaintenance checks if the certificate IP and domains matches the config,
// and renews the certificate if either this data is outdated or the
// certificate is expired.
func (t *TLSManager) certMaintenance() error {
	// We check whether the certificate we have on disk match the IPs and
	// domains specified by the config. If the extra IPs or domains have
	// changed from when the certificate was created, we will refresh the
	// certificate if auto refresh is active.
	refresh := false
	var err error
	if t.cfg.TLSAutoRefresh {
		refresh, err = cert.IsOutdated(
			t.parsedCert, t.cfg.TLSExtraIPs,
			t.cfg.TLSExtraDomains, t.cfg.TLSDisableAutofill,
		)
		if err != nil {
			return err
		}
	}

	// If the certificate expired or it was outdated, delete it and the TLS
	// key and generate a new pair.
	if time.Now().After(t.parsedCert.NotAfter) || refresh {
		ltndLog.Info("TLS certificate is expired or outdated, " +
			"generating a new one")

		err := os.Remove(t.cfg.TLSCertPath)
		if err != nil {
			return err
		}

		err = os.Remove(t.cfg.TLSKeyPath)
		if err != nil {
			return err
		}

		rpcsLog.Infof("Renewing TLS certificates...")
		certBytes, keyBytes, err := cert.GenCertPair(
			"lnd autogenerated cert", t.cfg.TLSCertPath,
			t.cfg.TLSKeyPath, t.cfg.TLSExtraIPs,
			t.cfg.TLSExtraDomains, t.cfg.TLSDisableAutofill,
			t.cfg.TLSCertDuration,
		)
		if err != nil {
			return err
		}
		t.certBytes = certBytes
		t.keyBytes = keyBytes
		rpcsLog.Infof("Done renewing TLS certificates")

		// Reload the certificate data.
		reloadedCertData, _, err := cert.LoadCert(
			certBytes, keyBytes,
		)
		if err != nil {
			return err
		}
		t.certData = &reloadedCertData
	}

	return nil
}

// setUpLetsEncrypt automatically generates a Let's Encrypt certificate if the
// option is set.
func (t *TLSManager) setUpLetsEncrypt(tlsCfg *tls.Config) (func(), error) {
	// If Let's Encrypt is enabled, instantiate autocert to request/renew
	// the certificates.
	cleanUp := func() {}
	if t.cfg.LetsEncryptDomain != "" {
		ltndLog.Infof("Using Let's Encrypt certificate for domain %v",
			t.cfg.LetsEncryptDomain)

		manager := autocert.Manager{
			Cache:      autocert.DirCache(t.cfg.LetsEncryptDir),
			Prompt:     autocert.AcceptTOS,
			HostPolicy: autocert.HostWhitelist(t.cfg.LetsEncryptDomain),
		}

		srv := &http.Server{
			Addr:    t.cfg.LetsEncryptListen,
			Handler: manager.HTTPHandler(nil),
		}
		shutdownCompleted := make(chan struct{})
		cleanUp = func() {
			err := srv.Shutdown(context.Background())
			if err != nil {
				ltndLog.Errorf("Autocert listener shutdown "+
					" error: %v", err)

				return
			}
			<-shutdownCompleted
			ltndLog.Infof("Autocert challenge listener stopped")
		}

		go func() {
			ltndLog.Infof("Autocert challenge listener started "+
				"at %v", t.cfg.LetsEncryptListen)

			err := srv.ListenAndServe()
			if err != http.ErrServerClosed {
				ltndLog.Errorf("autocert http: %v", err)
			}
			close(shutdownCompleted)
		}()

		getCertificate := func(h *tls.ClientHelloInfo) (
			*tls.Certificate, error) {

			lecert, err := manager.GetCertificate(h)
			if err != nil {
				ltndLog.Errorf("GetCertificate: %v", err)
				return t.certData, nil
			}

			return lecert, err
		}

		// The self-signed tls.cert remains available as fallback.
		tlsCfg.GetCertificate = getCertificate
	}

	return cleanUp, nil
}

// reloadCertificate deletes the temporary certificate file and generates a new
// one with the real keyring.
func (t *TLSManager) reloadCertificate() error {
	tmpCertPath := t.cfg.TLSCertPath + ".tmp"
	err := os.Remove(tmpCertPath)
	if err != nil {
		ltndLog.Warn("unable to delete temp cert at %v", tmpCertPath)
	}

	// Ensure the persistent TLS credentials are created
	_, _, _, _, err = t.getConfig()
	if err != nil {
		err := fmt.Errorf("unable to load TLS credentials: %v", err)
		ltndLog.Error(err)
		return err
	}
	certBytes, encryptedKeyBytes, err := cert.GetCertBytesFromPath(
		t.cfg.TLSCertPath, t.cfg.TLSKeyPath,
	)
	if err != nil {
		return err
	}
	t.certBytes = certBytes

	reader := bytes.NewReader(encryptedKeyBytes)
	keyBytes, err := lnencrypt.DecryptPayloadFromReader(reader, t.keyRing)
	if err != nil {
		return err
	}
	t.keyBytes = keyBytes

	// Switch the server's TLS certificate to the persistent one
	err = t.tlsReloader.AttemptReload(t.certBytes, t.keyBytes)
	if err != nil {
		return err
	}

	return nil
}

// fileExists reports whether the named file or directory exists.
// This function is taken from https://github.com/btcsuite/btcd
func fileExists(name string) bool {
	if _, err := os.Stat(name); err != nil {
		if os.IsNotExist(err) {
			return false
		}
	}
	return true
}

// bakeMacaroon creates a new macaroon with newest version and the given
// permissions then returns it binary serialized.
func bakeMacaroon(ctx context.Context, svc *macaroons.Service,
	permissions []bakery.Op) ([]byte, error) {

	mac, err := svc.NewMacaroon(
		ctx, macaroons.DefaultRootKeyID, permissions...,
	)
	if err != nil {
		return nil, err
	}

	return mac.M().MarshalBinary()
}

// genMacaroons generates three macaroon files; one admin-level, one for
// invoice access and one read-only. These can also be used to generate more
// granular macaroons.
func genMacaroons(ctx context.Context, svc *macaroons.Service,
	admFile, roFile, invoiceFile string) error {

	// First, we'll generate a macaroon that only allows the caller to
	// access invoice related calls. This is useful for merchants and other
	// services to allow an isolated instance that can only query and
	// modify invoices.
	invoiceMacBytes, err := bakeMacaroon(ctx, svc, invoicePermissions)
	if err != nil {
		return err
	}
	err = ioutil.WriteFile(invoiceFile, invoiceMacBytes, 0644)
	if err != nil {
		_ = os.Remove(invoiceFile)
		return err
	}

	// Generate the read-only macaroon and write it to a file.
	roBytes, err := bakeMacaroon(ctx, svc, readPermissions)
	if err != nil {
		return err
	}
	if err = ioutil.WriteFile(roFile, roBytes, 0644); err != nil {
		_ = os.Remove(roFile)
		return err
	}

	// Generate the admin macaroon and write it to a file.
	admBytes, err := bakeMacaroon(ctx, svc, adminPermissions())
	if err != nil {
		return err
	}

	err = ioutil.WriteFile(admFile, admBytes, adminMacaroonFilePermissions)
	if err != nil {
		_ = os.Remove(admFile)
		return err
	}

	return nil
}

// adminPermissions returns a list of all permissions in a safe way that doesn't
// modify any of the source lists.
func adminPermissions() []bakery.Op {
	admin := make([]bakery.Op, len(readPermissions)+len(writePermissions))
	copy(admin[:len(readPermissions)], readPermissions)
	copy(admin[len(readPermissions):], writePermissions)
	return admin
}

// createWalletUnlockerService creates a WalletUnlockerService from the passed
// config.
func createWalletUnlockerService(cfg *Config) *walletunlocker.UnlockerService {
	// The macaroonFiles are passed to the wallet unlocker so they can be
	// deleted and recreated in case the root macaroon key is also changed
	// during the change password operation.
	macaroonFiles := []string{
		cfg.AdminMacPath, cfg.ReadMacPath, cfg.InvoiceMacPath,
	}

	return walletunlocker.New(
		cfg.ActiveNetParams.Params, macaroonFiles,
		cfg.ResetWalletTransactions, nil,
	)
}

// startGrpcListen starts the GRPC server on the passed listeners.
func startGrpcListen(cfg *Config, grpcServer *grpc.Server,
	listeners []*ListenerWithSignal) error {

	// Use a WaitGroup so we can be sure the instructions on how to input the
	// password is the last thing to be printed to the console.
	var wg sync.WaitGroup

	for _, lis := range listeners {
		wg.Add(1)
		go func(lis *ListenerWithSignal) {
			rpcsLog.Infof("RPC server listening on %s", lis.Addr())

			// Close the ready chan to indicate we are listening.
			close(lis.Ready)

			wg.Done()
			_ = grpcServer.Serve(lis)
		}(lis)
	}

	// If Prometheus monitoring is enabled, start the Prometheus exporter.
	if cfg.Prometheus.Enabled() {
		err := monitoring.ExportPrometheusMetrics(
			grpcServer, cfg.Prometheus,
		)
		if err != nil {
			return err
		}
	}

	// Wait for gRPC servers to be up running.
	wg.Wait()

	return nil
}

// startRestProxy starts the given REST proxy on the listeners found in the
// config.
func startRestProxy(cfg *Config, rpcServer *rpcServer, restDialOpts []grpc.DialOption,
	restListen func(net.Addr) (net.Listener, error)) (func(), error) {

	// We use the first RPC listener as the destination for our REST proxy.
	// If the listener is set to listen on all interfaces, we replace it
	// with localhost, as we cannot dial it directly.
	restProxyDest := cfg.RPCListeners[0].String()
	switch {
	case strings.Contains(restProxyDest, "0.0.0.0"):
		restProxyDest = strings.Replace(
			restProxyDest, "0.0.0.0", "127.0.0.1", 1,
		)

	case strings.Contains(restProxyDest, "[::]"):
		restProxyDest = strings.Replace(
			restProxyDest, "[::]", "[::1]", 1,
		)
	}

	var shutdownFuncs []func()
	shutdown := func() {
		for _, shutdownFn := range shutdownFuncs {
			shutdownFn()
		}
	}

	// Start a REST proxy for our gRPC server.
	ctx := context.Background()
	ctx, cancel := context.WithCancel(ctx)
	shutdownFuncs = append(shutdownFuncs, cancel)

	// We'll set up a proxy that will forward REST calls to the GRPC
	// server.
	//
	// The default JSON marshaler of the REST proxy only sets OrigName to
	// true, which instructs it to use the same field names as specified in
	// the proto file and not switch to camel case. What we also want is
	// that the marshaler prints all values, even if they are falsey.
	customMarshalerOption := proxy.WithMarshalerOption(
		proxy.MIMEWildcard, &proxy.JSONPb{
			MarshalOptions: protojson.MarshalOptions{
				UseProtoNames:   true,
				EmitUnpopulated: true,
			},
		},
	)
	mux := proxy.NewServeMux(
		customMarshalerOption,

		// Don't allow falling back to other HTTP methods, we want exact
		// matches only. The actual method to be used can be overwritten
		// by setting X-HTTP-Method-Override so there should be no
		// reason for not specifying the correct method in the first
		// place.
		proxy.WithDisablePathLengthFallback(),
	)

	// Register our services with the REST proxy.
	err := lnrpc.RegisterStateHandlerFromEndpoint(
		ctx, mux, restProxyDest, restDialOpts,
	)
	if err != nil {
		return nil, err
	}

	err = rpcServer.RegisterWithRestProxy(
		ctx, mux, restDialOpts, restProxyDest,
	)
	if err != nil {
		return nil, err
	}

	// Wrap the default grpc-gateway handler with the WebSocket handler.
	restHandler := lnrpc.NewWebSocketProxy(
		mux, rpcsLog, cfg.WSPingInterval, cfg.WSPongWait,
		lnrpc.LndClientStreamingURIs,
	)

	// Use a WaitGroup so we can be sure the instructions on how to input the
	// password is the last thing to be printed to the console.
	var wg sync.WaitGroup

	// Now spin up a network listener for each requested port and start a
	// goroutine that serves REST with the created mux there.
	for _, restEndpoint := range cfg.RESTListeners {
		lis, err := restListen(restEndpoint)
		if err != nil {
			ltndLog.Errorf("gRPC proxy unable to listen on %s",
				restEndpoint)
			return nil, err
		}

		shutdownFuncs = append(shutdownFuncs, func() {
			err := lis.Close()
			if err != nil {
				rpcsLog.Errorf("Error closing listener: %v",
					err)
			}
		})

		wg.Add(1)
		go func() {
			rpcsLog.Infof("gRPC proxy started at %s", lis.Addr())

			// Create our proxy chain now. A request will pass
			// through the following chain:
			// req ---> CORS handler --> WS proxy --->
			//   REST proxy --> gRPC endpoint
			corsHandler := allowCORS(restHandler, cfg.RestCORS)

			wg.Done()
			err := http.Serve(lis, corsHandler)
			if err != nil && !lnrpc.IsClosedConnError(err) {
				rpcsLog.Error(err)
			}
		}()
	}

	// Wait for REST servers to be up running.
	wg.Wait()

	return shutdown, nil
}
