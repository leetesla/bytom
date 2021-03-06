package node

import (
	"context"
	"net/http"
	_ "net/http/pprof"
	"time"

	log "github.com/sirupsen/logrus"
	cmn "github.com/tendermint/tmlibs/common"
	dbm "github.com/tendermint/tmlibs/db"
	browser "github.com/toqueteos/webbrowser"

	"github.com/bytom/accesstoken"
	"github.com/bytom/account"
	"github.com/bytom/api"
	"github.com/bytom/asset"
	"github.com/bytom/blockchain/pseudohsm"
	"github.com/bytom/blockchain/txfeed"
	cfg "github.com/bytom/config"
	"github.com/bytom/consensus"
	"github.com/bytom/database/leveldb"
	"github.com/bytom/env"
	"github.com/bytom/mining/cpuminer"
	"github.com/bytom/mining/miningpool"
	"github.com/bytom/netsync"
	"github.com/bytom/protocol"
	"github.com/bytom/protocol/bc"
	"github.com/bytom/types"
	w "github.com/bytom/wallet"
)

const (
	webAddress               = "http://127.0.0.1:9888"
	expireReservationsPeriod = time.Second
	maxNewBlockChSize        = 1024
)

type Node struct {
	cmn.BaseService

	// config
	config *cfg.Config

	syncManager *netsync.SyncManager

	evsw types.EventSwitch // pub/sub for services
	//bcReactor    *bc.BlockchainReactor
	wallet       *w.Wallet
	accessTokens *accesstoken.CredentialStore
	api          *api.API
	chain        *protocol.Chain
	txfeed       *txfeed.Tracker
	cpuMiner     *cpuminer.CPUMiner
	miningPool   *miningpool.MiningPool
	miningEnable bool
}

func NewNode(config *cfg.Config) *Node {
	ctx := context.Background()
	initActiveNetParams(config)
	// Get store
	txDB := dbm.NewDB("txdb", config.DBBackend, config.DBDir())
	store := leveldb.NewStore(txDB)

	tokenDB := dbm.NewDB("accesstoken", config.DBBackend, config.DBDir())
	accessTokens := accesstoken.NewStore(tokenDB)

	// Make event switch
	eventSwitch := types.NewEventSwitch()
	_, err := eventSwitch.Start()
	if err != nil {
		cmn.Exit(cmn.Fmt("Failed to start switch: %v", err))
	}

	txPool := protocol.NewTxPool()
	chain, err := protocol.NewChain(store, txPool)
	if err != nil {
		cmn.Exit(cmn.Fmt("Failed to create chain structure: %v", err))
	}

	var accounts *account.Manager = nil
	var assets *asset.Registry = nil
	var wallet *w.Wallet = nil
	var txFeed *txfeed.Tracker = nil

	txFeedDB := dbm.NewDB("txfeeds", config.DBBackend, config.DBDir())
	txFeed = txfeed.NewTracker(txFeedDB, chain)

	if err = txFeed.Prepare(ctx); err != nil {
		log.WithField("error", err).Error("start txfeed")
		return nil
	}

	hsm, err := pseudohsm.New(config.KeysDir())
	if err != nil {
		cmn.Exit(cmn.Fmt("initialize HSM failed: %v", err))
	}

	if !config.Wallet.Disable {
		walletDB := dbm.NewDB("wallet", config.DBBackend, config.DBDir())
		accounts = account.NewManager(walletDB, chain)
		assets = asset.NewRegistry(walletDB, chain)
		wallet, err = w.NewWallet(walletDB, accounts, assets, hsm, chain)
		if err != nil {
			log.WithField("error", err).Error("init NewWallet")
		}

		// Clean up expired UTXO reservations periodically.
		go accounts.ExpireReservations(ctx, expireReservationsPeriod)
	}
	newBlockCh := make(chan *bc.Hash, maxNewBlockChSize)

	syncManager, _ := netsync.NewSyncManager(config, chain, txPool, newBlockCh)

	// run the profile server
	profileHost := config.ProfListenAddress
	if profileHost != "" {
		// Profiling bytomd programs.see (https://blog.golang.org/profiling-go-programs)
		// go tool pprof http://profileHose/debug/pprof/heap
		go func() {
			http.ListenAndServe(profileHost, nil)
		}()
	}

	node := &Node{
		config:       config,
		syncManager:  syncManager,
		evsw:         eventSwitch,
		accessTokens: accessTokens,
		wallet:       wallet,
		chain:        chain,
		txfeed:       txFeed,
		miningEnable: config.Mining,
	}

	node.cpuMiner = cpuminer.NewCPUMiner(chain, accounts, txPool, newBlockCh)
	node.miningPool = miningpool.NewMiningPool(chain, accounts, txPool, newBlockCh)

	node.BaseService = *cmn.NewBaseService(nil, "Node", node)

	return node
}

func initActiveNetParams(config *cfg.Config) {
	var exist bool
	consensus.ActiveNetParams, exist = consensus.NetParams[config.ChainID]
	if !exist {
		cmn.Exit(cmn.Fmt("chain_id[%v] don't exist", config.ChainID))
	}
}

// Lanch web broser or not
func lanchWebBroser() {
	log.Info("Launching System Browser with :", webAddress)
	if err := browser.Open(webAddress); err != nil {
		log.Error(err.Error())
		return
	}
}

func (n *Node) initAndstartApiServer() {
	n.api = api.NewAPI(n.syncManager, n.wallet, n.txfeed, n.cpuMiner, n.miningPool, n.chain, n.config, n.accessTokens)

	listenAddr := env.String("LISTEN", n.config.ApiAddress)
	env.Parse()
	n.api.StartServer(*listenAddr)
}

func (n *Node) OnStart() error {
	if n.miningEnable {
		n.cpuMiner.Start()
	}
	n.syncManager.Start()
	n.initAndstartApiServer()
	if !n.config.Web.Closed {
		lanchWebBroser()
	}

	return nil
}

func (n *Node) OnStop() {
	n.BaseService.OnStop()
	if n.miningEnable {
		n.cpuMiner.Stop()
	}
	n.syncManager.Stop()
	log.Info("Stopping Node")
	// TODO: gracefully disconnect from peers.
}

func (n *Node) RunForever() {
	// Sleep forever and then...
	cmn.TrapSignal(func() {
		n.Stop()
	})
}

func (n *Node) EventSwitch() types.EventSwitch {
	return n.evsw
}

func (n *Node) SyncManager() *netsync.SyncManager {
	return n.syncManager
}

func (n *Node) MiningPool() *miningpool.MiningPool {
	return n.miningPool
}
