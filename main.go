// Copyright (c) 2017, Jonathan Chappelow
// See LICENSE for details.

package main

import (
	"database/sql"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime/pprof"
	"strings"
	"sync"
	"time"

	"github.com/HcashOrg/hcexplorer/blockdata"
	"github.com/HcashOrg/hcexplorer/db/dbtypes"
	"github.com/HcashOrg/hcexplorer/db/hcpg"
	"github.com/HcashOrg/hcexplorer/db/hcsqlite"
	"github.com/HcashOrg/hcexplorer/explorer"
	"github.com/HcashOrg/hcexplorer/mempool"
	"github.com/HcashOrg/hcexplorer/rpcutils"
	"github.com/HcashOrg/hcexplorer/semver"
	"github.com/HcashOrg/hcexplorer/txhelpers"
	"github.com/HcashOrg/hcd/chaincfg/chainhash"
	"github.com/HcashOrg/hcrpcclient"
	"github.com/go-chi/chi"
)

// mainCore does all the work. Deferred functions do not run after os.Exit(),
// so main wraps this function, which returns a code.
func mainCore() error {
	// Parse the configuration file, and setup logger.
	cfg, err := loadConfig()
	if err != nil {
		fmt.Printf("Failed to load hcexplorer config: %s\n", err.Error())
		return err
	}
	defer func() {
		if logRotator != nil {
			logRotator.Close()
		}
	}()

	// PostgreSQL
	usePG := !cfg.LiteMode
	var db *hcpg.ChainDB
	if usePG {
		pgHost, pgPort := cfg.PGHost, ""
		if !strings.HasPrefix(pgHost, "/") {
			pgHost, pgPort, err = net.SplitHostPort(cfg.PGHost)
			if err != nil {
				return fmt.Errorf("SplitHostPort failed: %v", err)
			}
		}
		dbi := hcpg.DBInfo{
			Host:   pgHost,
			Port:   pgPort,
			User:   cfg.PGUser,
			Pass:   cfg.PGPass,
			DBName: cfg.PGDBName,
		}
		db, err = hcpg.NewChainDB(&dbi, activeChain)
		if db != nil {
			defer db.Close()
		}
		if err != nil {
			return err
		}

		if err = db.SetupTables(); err != nil {
			return err
		}
	}

	if cfg.CPUProfile != "" {
		var f *os.File
		f, err = os.Create(cfg.CPUProfile)
		if err != nil {
			return err
		}
		pprof.StartCPUProfile(f)
		defer pprof.StopCPUProfile()
	}

	// Start with version info
	log.Infof(appName+" version %s", ver.String())

	//log.Debugf("Output folder: %v", cfg.OutFolder)
	log.Debugf("Log folder: %v", cfg.LogDir)

	if usePG {
		log.Info(`Running in full-functionality mode with PostgreSQL backend enabled.`)
	} else {
		log.Info(`Running in "Lite" mode with only SQLite backend and limited functionality.`)
	}

	// // Create data output folder if it does not already exist
	// if err = os.MkdirAll(cfg.OutFolder, 0750); err != nil {
	// 	log.Errorf("Failed to create data output folder %s. Error: %s\n",
	// 		cfg.OutFolder, err.Error())
	// 	return 2
	// }

	// Connect to hcd RPC server using websockets

	// Set up the notification handler to deliver blocks through a channel.
	makeNtfnChans(cfg)

	// Daemon client connection
	ntfnHandlers, collectionQueue := makeNodeNtfnHandlers(cfg)
	hcdClient, nodeVer, err := connectNodeRPC(cfg, ntfnHandlers)
	if err != nil || hcdClient == nil {
		return fmt.Errorf("Connection to hcd failed: %v", err)
	}

	defer func() {
		// Closing these channels should be unnecessary if quit was handled right
		closeNtfnChans()

		if hcdClient != nil {
			log.Infof("Closing connection to hcd.")
			hcdClient.Shutdown()
		}

		log.Infof("Bye!")
		time.Sleep(250 * time.Millisecond)
	}()

	// Display connected network
	curnet, err := hcdClient.GetCurrentNet()
	if err != nil {
		return fmt.Errorf("Unable to get current network from hcd: %v", err)
	}
	log.Infof("Connected to hcd (JSON-RPC API v%s) on %v",
		nodeVer.String(), curnet.String())

	// Another (horrible) example of saving to a map in memory
	// blockDataMapSaver := NewBlockDataToMemdb()
	// blockDataSavers = append(blockDataSavers, blockDataMapSaver)

	// Sqlite output
	dbInfo := hcsqlite.DBInfo{FileName: cfg.DBFileName}
	//sqliteDB, err := hcsqlite.InitDB(&dbInfo)
	sqliteDB, cleanupDB, err := hcsqlite.InitWiredDB(&dbInfo,
		ntfnChans.updateStatusDBHeight, hcdClient, activeChain)
	defer cleanupDB()
	if err != nil {
		return fmt.Errorf("Unable to initialize SQLite database: %v", err)
	}
	log.Infof("SQLite DB successfully opened: %s", cfg.DBFileName)
	defer sqliteDB.Close()

	// Ctrl-C to shut down.
	// Nothing should be sent the quit channel.  It should only be closed.
	quit := make(chan struct{})
	// Only accept a single CTRL+C
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)

	// Start waiting for the interrupt signal
	go func() {
		<-c
		signal.Stop(c)
		// Close the channel so multiple goroutines can get the message
		log.Infof("CTRL+C hit.  Closing goroutines.")
		close(quit)
	}()

	_, height, err := hcdClient.GetBestBlock()
	if err != nil {
		return fmt.Errorf("Unable to get block from node: %v", err)
	}

	var newPGIndexes, updateAllAddresses bool
	if usePG {
		heightDB, err := db.HeightDB()
		if err != nil {
			if err != sql.ErrNoRows {
				return fmt.Errorf("Unable to get height from PostgreSQL DB: %v", err)
			}
			heightDB = 0
		}
		blocksBehind := height - int64(heightDB)
		if blocksBehind < 0 {
			return fmt.Errorf("Node is still syncing. Node height = %d, "+
				"DB height = %d", height, heightDB)
		}
		if blocksBehind > 7500 {
			log.Infof("Setting PSQL sync to rebuild address table after large "+
				"import (%d blocks).", blocksBehind)
			updateAllAddresses = true
			if blocksBehind > 40000 {
				log.Infof("Setting PSQL sync to drop indexes prior to bulk data "+
					"import (%d blocks).", blocksBehind)
				newPGIndexes = true
			}
		}
	}

	// Simultaneously synchronize the ChainDB (PostgreSQL) and the block/stake
	// info DB (sqlite). They don't communicate, so we'll just ensure they exit
	// with the same best block height by calling them repeatedly in a loop.
	var sqliteHeight, pgHeight int64
	sqliteSyncRes := make(chan dbtypes.SyncResult)
	pgSyncRes := make(chan dbtypes.SyncResult)
	for {
		// Launch the sync functions for both DBs
		go sqliteDB.SyncDBAsync(sqliteSyncRes, quit)
		go db.SyncChainDBAsync(pgSyncRes, hcdClient, quit,
			newPGIndexes, updateAllAddresses)

		// Wait for the results
		sqliteRes := <-sqliteSyncRes
		sqliteHeight = sqliteRes.Height
		log.Infof("SQLite sync ended at height %d", sqliteHeight)

		pgRes := <-pgSyncRes
		pgHeight = pgRes.Height
		if usePG {
			log.Infof("PostgreSQL sync ended at height %d", pgHeight)
		}

		// See if there was a SIGINT (CTRL+C)
		select {
		case <-quit:
			log.Info("Quit signal received during DB sync.")
			return nil
		default:
		}

		// Check for errors and combine if necessary
		if sqliteRes.Error != nil {
			if usePG && pgRes.Error != nil {
				log.Error("hcsqlite.SyncDBAsync AND hcpg.SyncChainDBAsync "+
					"failed at heights %d and %d, respectively.",
					sqliteRes.Height, pgRes.Height)
				errCombined := fmt.Sprintln(sqliteRes.Error, ", ", pgRes.Error)
				return errors.New(errCombined)
			}
			log.Errorf("hcsqlite.SyncDBAsync failed at height %d.", sqliteRes.Height)
			return sqliteRes.Error
		} else if usePG && pgRes.Error != nil {
			log.Errorf("hcpg.SyncChainDBAsync failed at height %d.", pgRes.Height)
			return pgRes.Error
		}

		// Break loop to continue starting hcexplorer.
		if !usePG || pgHeight == sqliteHeight {
			break
		}
		log.Infof("Restarting sync with PostgreSQL at %d, SQLite at %d.",
			pgHeight, sqliteHeight)
		updateAllAddresses, newPGIndexes = false, false
	}

	// Block data collector
	collector := blockdata.NewCollector(hcdClient, activeChain, sqliteDB.GetStakeDB())
	if collector == nil {
		return fmt.Errorf("Failed to create block data collector")
	}

	// Build a slice of each required saver type for each data source
	var blockDataSavers []blockdata.BlockDataSaver
	var mempoolSavers []mempool.MempoolDataSaver

	blockDataSavers = append(blockDataSavers, db)

	// For example, dumping all mempool fees with a custom saver
	if cfg.DumpAllMPTix {
		log.Debugf("Dumping all mempool tickets to file in %s.\n", cfg.OutFolder)
		mempoolFeeDumper := mempool.NewMempoolFeeDumper(cfg.OutFolder, "mempool-fees")
		mempoolSavers = append(mempoolSavers, mempoolFeeDumper)
	}

	blockDataSavers = append(blockDataSavers, &sqliteDB)
	mempoolSavers = append(mempoolSavers, sqliteDB.MPC)

	// Web template data. WebUI implements BlockDataSaver interface
	webUI := NewWebUI(&sqliteDB,activeChain)
	if webUI == nil {
		return fmt.Errorf("Failed to start WebUI. Missing HTML resources?")
	}
	defer webUI.StopWebsocketHub()
	webUI.UseSIGToReloadTemplates()
	blockDataSavers = append(blockDataSavers, webUI)
	mempoolSavers = append(mempoolSavers, webUI)

	// Start the explorer system
	explore := explorer.New(&sqliteDB, db, cfg.UseRealIP)
	explore.UseSIGToReloadTemplates()
	defer explore.StopWebsocketHub()
	blockDataSavers = append(blockDataSavers, explore)

	// Initial data summary for web ui
	blockData, _, err := collector.Collect()
	if err != nil {
		return fmt.Errorf("Block data collection for initial summary failed: %v",
			err.Error())
	}

	if err = webUI.Store(blockData, nil); err != nil {
		return fmt.Errorf("Failed to store initial block data for main page: %v", err.Error())
	}

	if err = explore.Store(blockData, nil); err != nil {
		return fmt.Errorf("Failed to store initial block data for explorer pages: %v", err.Error())
	}
	// WaitGroup for the monitor goroutines
	var wg sync.WaitGroup

	// Blockchain monitor for the collector
	addrMap := make(map[string]txhelpers.TxAction) // for support of watched addresses
	// On reorg, only update web UI since hcsqlite's own reorg handler will
	// deal with patching up the block info database.
	reorgBlockDataSavers := []blockdata.BlockDataSaver{webUI, explore}
	wsChainMonitor := blockdata.NewChainMonitor(collector, blockDataSavers,
		reorgBlockDataSavers, quit, &wg, addrMap,
		ntfnChans.connectChan, ntfnChans.recvTxBlockChan,
		ntfnChans.reorgChanBlockData)
	wg.Add(2)
	go wsChainMonitor.BlockConnectedHandler()
	// The blockdata reorg handler disables collection during reorg, leaving
	// hcsqlite to do the switch, except for the last block which gets
	// collected and stored via reorgBlockDataSavers.
	go wsChainMonitor.ReorgHandler()

	// Blockchain monitor for the stake DB
	sdbChainMonitor := sqliteDB.NewStakeDBChainMonitor(quit, &wg,
		ntfnChans.connectChanStakeDB, ntfnChans.reorgChanStakeDB)
	wg.Add(2)
	go sdbChainMonitor.BlockConnectedHandler()
	go sdbChainMonitor.ReorgHandler()

	// Blockchain monitor for the wired sqlite DB
	wiredDBChainMonitor := sqliteDB.NewChainMonitor(collector, quit, &wg,
		ntfnChans.connectChanWiredDB, ntfnChans.reorgChanWiredDB)
	wg.Add(2)
	// hcsqlite does not handle new blocks except during reorg
	go wiredDBChainMonitor.BlockConnectedHandler()
	go wiredDBChainMonitor.ReorgHandler()

	// Setup the synchronous handler functions called by the collectionQueue via
	// OnBlockConnected.
	collectionQueue.SetSynchronousHandlers([]func(*chainhash.Hash){
		sdbChainMonitor.BlockConnectedSync,     // 1. Stake DB for pool info
		wsChainMonitor.BlockConnectedSync,      // 2. blockdata for regular block data collection and storage
		wiredDBChainMonitor.BlockConnectedSync, // 3. hcsqlite for sqlite DB reorg handling
	})

	if cfg.MonitorMempool {
		mpoolCollector := mempool.NewMempoolDataCollector(hcdClient, activeChain)
		if mpoolCollector == nil {
			return fmt.Errorf("Failed to create mempool data collector")
		}

		mpData, err := mpoolCollector.Collect()
		if err != nil {
			return fmt.Errorf("Mempool info collection failed while gathering"+
				" initial data: %v", err.Error())
		}

		// Store initial MP data
		if err = sqliteDB.MPC.StoreMPData(mpData, time.Now()); err != nil {
			return fmt.Errorf("Failed to store initial mempool data (wiredDB): %v",
				err.Error())
		}

		// Store initial MP data to webUI
		if err = webUI.StoreMPData(mpData, time.Now()); err != nil {
			return fmt.Errorf("Failed to store initial mempool data (WebUI): %v",
				err.Error())
		}

		// Setup monitor
		mpi := &mempool.MempoolInfo{
			CurrentHeight:               mpData.GetHeight(),
			NumTicketPurchasesInMempool: mpData.GetNumTickets(),
			NumTicketsSinceStatsReport:  0,
			LastCollectTime:             time.Now(),
		}

		newTicketLimit := int32(cfg.MPTriggerTickets)
		mini := time.Duration(cfg.MempoolMinInterval) * time.Second
		maxi := time.Duration(cfg.MempoolMaxInterval) * time.Second

		mpm := mempool.NewMempoolMonitor(mpoolCollector, mempoolSavers,
			ntfnChans.newTxChan, quit, &wg, newTicketLimit, mini, maxi, mpi)
		wg.Add(1)
		go mpm.TxHandler(hcdClient)
	}

	select {
	case <-quit:
		return nil
	default:
	}

	// Register for notifications now that the monitors are listening
	cerr := registerNodeNtfnHandlers(hcdClient)
	if cerr != nil {
		return fmt.Errorf("RPC client error: %v (%v)", cerr.Error(), cerr.Cause())
	}

	// Start web API
	app := newContext(hcdClient, &sqliteDB, cfg.IndentJSON)
	// Start notification hander to keep /status up-to-date
	wg.Add(1)
	go app.StatusNtfnHandler(&wg, quit)
	// Initial setting of db_height. Subsequently, Store() will send this.
	ntfnChans.updateStatusDBHeight <- uint32(sqliteDB.GetHeight())

	apiMux := newAPIRouter(app, cfg.UseRealIP)

	webMux := chi.NewRouter()
	webMux.Get("/", webUI.RootPage)
	webMux.Get("/ws", webUI.WSBlockUpdater)
	webMux.Get("/favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "./public/images/favicon.ico")
	})
	cacheControlMaxAge := int64(cfg.CacheControlMaxAge)
	FileServer(webMux, "/js", http.Dir("./public/js"), cacheControlMaxAge)
	FileServer(webMux, "/css", http.Dir("./public/css"), cacheControlMaxAge)
	FileServer(webMux, "/fonts", http.Dir("./public/fonts"), cacheControlMaxAge)
	FileServer(webMux, "/images", http.Dir("./public/images"), cacheControlMaxAge)
	webMux.With(SearchPathCtx).Get("/error/{search}", webUI.ErrorPage)
	webMux.NotFound(webUI.ErrorPage)
	webMux.Mount("/api", apiMux.Mux)
	webMux.Mount("/explorer", explore.Mux)
	if err = listenAndServeProto(cfg.APIListen, cfg.APIProto, webMux); err != nil {
		log.Criticalf("listenAndServeProto: %v", err)
		close(quit)
	}

	// Wait for notification handlers to quit
	wg.Wait()

	return nil
}

func main() {
	if err := mainCore(); err != nil {
		if logRotator != nil {
			log.Error(err)
		}
		os.Exit(1)
	}
	os.Exit(0)
}

func connectNodeRPC(cfg *config, ntfnHandlers *hcrpcclient.NotificationHandlers) (*hcrpcclient.Client, semver.Semver, error) {
	return rpcutils.ConnectNodeRPC(cfg.HcdServ, cfg.HcdUser, cfg.HcdPass,
		cfg.HcdCert, cfg.DisableDaemonTLS, ntfnHandlers)
}

func listenAndServeProto(listen, proto string, mux http.Handler) error {
	// Try to bind web server
	errChan := make(chan error)
	if proto == "https" {
		go func() {
			errChan <- http.ListenAndServeTLS(listen, "hcexplorer.cert", "hcexplorer.key", mux)
		}()
	} else {
		go func() {
			errChan <- http.ListenAndServe(listen, mux)
		}()
	}

	// Briefly wait for an error and then return
	t := time.NewTimer(3 * time.Second)
	select {
	case err := <-errChan:
		return fmt.Errorf("Failed to bind web server: %v", err)
	case <-t.C:
		apiLog.Infof("Now serving on %s://%v/", proto, listen)
		return nil
	}
}
