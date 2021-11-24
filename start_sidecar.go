package lnd

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/btcsuite/btcd/btcec"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/pool/acceptor"
	"github.com/lightninglabs/pool/auctioneer"
	"github.com/lightninglabs/pool/clientdb"
	"github.com/lightninglabs/pool/funding"
	"github.com/lightninglabs/pool/order"
	"github.com/lightningnetwork/lnd/lnrpc"

	"google.golang.org/grpc"
)

func StartSidecarAcceptor(cfg *Config, macBytes []byte) (*acceptor.SidecarAcceptor, error) {
	opts, err := AdminAuthOptions(cfg, false, true, macBytes)
	if err != nil {
		return nil, err
	}

	host := "127.0.0.1:10009"
	conn, err := grpc.Dial(host, opts...)
	if err != nil {
		return nil, fmt.Errorf("unable to connect to RPC server: %v", err)
	}

	network := lndclient.Network(cfg.ActiveNetParams.Params.Name)

	ctxc, cancel := context.WithCancel(context.Background())
	defer cancel()

	lndServices, err := lndclient.NewLndServices(&lndclient.LndServicesConfig{
		LndAddress:            host,
		Network:               network,
		TLSPath:               cfg.TLSCertPath,
		Insecure:              true,
		CustomMacaroonHex:     hex.EncodeToString(macBytes),
		BlockUntilChainSynced: false,
		BlockUntilUnlocked:    true,
		CallerCtx:             ctxc,
	})
	if err != nil {
		return nil, err
	}

	db, err := clientdb.New(cfg.PoolDir, clientdb.DBFilename)
	if err != nil {
		return nil, err
	}

	// Parse our lnd node's public key.
	nodePubKey, err := btcec.ParsePubKey(
		lndServices.NodePubkey[:], btcec.S256(),
	)
	if err != nil {
		return nil, fmt.Errorf("unable to parse node pubkey: %v", err)
	}

	lnClient := lnrpc.NewLightningClient(conn)
	channelAcceptor := acceptor.NewChannelAcceptor(lndServices.Client)
	fundingManager := funding.NewManager(&funding.ManagerConfig{
		DB:                db,
		WalletKit:         lndServices.WalletKit,
		LightningClient:   lndServices.Client,
		SignerClient:      lndServices.Signer,
		BaseClient:        lnClient,
		NodePubKey:        nodePubKey,
		BatchStepTimeout:  order.DefaultBatchStepTimeout,
		NewNodesOnly:      false,
		NotifyShimCreated: channelAcceptor.ShimRegistered,
	})

	var auctionServer string
	// Use the default addresses for mainnet and testnet auction servers.
	switch {
	case cfg.Bitcoin.MainNet:
		auctionServer = "pool.lightning.finance:12010"
	case cfg.Bitcoin.TestNet3:
		auctionServer = "test.pool.lightning.finance:12010"
	default:
		return nil, errors.New("no auction server address specified")
	}

	clientCfg := &auctioneer.Config{
		ServerAddress: auctionServer,
		ProxyAddress:  "",
		Insecure:      false,
		TLSPathServer: "",
		DialOpts:      make([]grpc.DialOption, 0),
		Signer:        lndServices.Signer,
		MinBackoff:    time.Millisecond * 100,
		MaxBackoff:    time.Minute,
		BatchSource:   db,
		BatchCleaner:  fundingManager,
		GenUserAgent: func(ctx context.Context) string {
			return acceptor.UserAgent(acceptor.InitiatorFromContext(ctx))
		},
	}

	acceptor := acceptor.NewSidecarAcceptor(&acceptor.SidecarAcceptorConfig{
		SidecarDB:       db,
		AcctDB:          &acceptor.AccountStore{DB: db},
		BaseClient:      lnClient,
		Acceptor:        channelAcceptor,
		Signer:          lndServices.Signer,
		Wallet:          lndServices.WalletKit,
		NodePubKey:      nodePubKey,
		ClientCfg:       *clientCfg,
		FundingManager:  fundingManager,
		FetchSidecarBid: db.SidecarBidTemplate,
	})

	acceptor.FundingManager = fundingManager

	return acceptor, nil
}
