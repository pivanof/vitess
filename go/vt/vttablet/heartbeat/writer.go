package heartbeat

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/youtube/vitess/go/vt/dbconfigs"
	"github.com/youtube/vitess/go/vt/dbconnpool"
	"github.com/youtube/vitess/go/vt/logutil"
	"github.com/youtube/vitess/go/vt/proto/topodata"
	"github.com/youtube/vitess/go/vt/topo"
	"github.com/youtube/vitess/go/vt/vttablet/tabletserver/tabletenv"

	log "github.com/golang/glog"
	"github.com/youtube/vitess/go/sqldb"
	"github.com/youtube/vitess/go/stats"
	"github.com/youtube/vitess/go/vt/sqlparser"
	"github.com/youtube/vitess/go/vt/vttablet/tabletserver/connpool"
)

const (
	sqlCreateSidecarDB      = "create database if not exists %s"
	sqlCreateHeartbeatTable = `CREATE TABLE IF NOT EXISTS %s.heartbeat (
  ts bigint NOT NULL,
  master_uid int unsigned NOT NULL PRIMARY KEY
        ) engine=InnoDB`
	sqlInsertInitialRow = "INSERT INTO %s.heartbeat (ts, master_uid) VALUES (%d, %d) ON DUPLICATE KEY UPDATE ts=VALUES(ts)"
	sqlUpdateHeartbeat  = "UPDATE %v.heartbeat SET ts=%d WHERE master_uid=%d"
)

type mySQLChecker interface {
	CheckMySQL()
}

// Writer runs on master tablets and writes heartbeats to the _vt.heartbeat
// table at a regular interval, defined by heartbeat_interval.
type Writer struct {
	topoServer  topo.Server
	tabletAlias topodata.TabletAlias
	now         func() time.Time
	errorLog    *logutil.ThrottledLogger

	mu     sync.Mutex
	isOpen bool
	cancel context.CancelFunc
	wg     sync.WaitGroup
	pool   *connpool.Pool
	dbName string
}

// NewWriter creates a new Writer.
func NewWriter(topoServer topo.Server, alias topodata.TabletAlias, checker mySQLChecker, config tabletenv.TabletConfig) *Writer {
	return &Writer{
		topoServer:  topoServer,
		tabletAlias: alias,
		now:         time.Now,
		errorLog:    logutil.NewThrottledLogger("HeartbeatWriter", 60*time.Second),
		pool:        connpool.New(config.PoolNamePrefix+"HeartbeatWritePool", 1, time.Duration(config.IdleTimeout*1e9), checker),
	}
}

// Open sets up the Writer's db connection and launches the goroutine
// responsible for periodically writing to the heartbeat table.
func (w *Writer) Open(dbc dbconfigs.DBConfigs) error {
	if !*enableHeartbeat {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.isOpen {
		return nil
	}

	w.dbName = sqlparser.Backtick(dbc.SidecarDBName)
	w.pool.Open(&dbc.App, &dbc.Dba)

	ctx, cancel := context.WithCancel(tabletenv.LocalContext())
	w.cancel = cancel
	w.wg.Add(1)
	go w.run(ctx, &dbc.Dba)

	w.isOpen = true
	return nil
}

// Close closes the Writer's db connection, cancels the goroutine, and
// waits for the goroutine to finish.
func (w *Writer) Close() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.isOpen {
		return
	}
	w.cancel()
	w.wg.Wait()
	w.pool.Close()
	w.isOpen = false
}

// run is the main goroutine of the Writer. It initializes
// the heartbeat table then writes heartbeat at the heartbeat_interval
// until cancelled.
func (w *Writer) run(ctx context.Context, initParams *sqldb.ConnParams) {
	defer w.wg.Done()
	defer tabletenv.LogError()

	w.waitForTables(ctx, initParams)

	log.Info("Beginning heartbeat writes")
	for {
		if err := w.writeHeartbeat(ctx); err != nil {
			w.recordError(err)
		}

		if waitOrExit(ctx, *interval) {
			log.Info("Stopped heartbeat writes.")
			return
		}
	}
}

// waitForTables continually attempts to create the heartbeat tables, until
// success or cancellation.
func (w *Writer) waitForTables(ctx context.Context, cp *sqldb.ConnParams) {
	log.Info("Initializing heartbeat table")
	for {
		err := w.initializeTables(cp)
		if err == nil {
			return
		}

		w.recordError(err)
		if waitOrExit(ctx, 10*time.Second) {
			return
		}
	}
}

// initializeTables attempts to create the heartbeat tables exactly once.
func (w *Writer) initializeTables(cp *sqldb.ConnParams) error {
	conn, err := dbconnpool.NewDBConnection(cp, stats.NewTimings(""))
	if err != nil {
		return fmt.Errorf("Failed to create connection for heartbeat: %v", err)
	}
	defer conn.Close()
	statements := []string{
		fmt.Sprintf(sqlCreateSidecarDB, w.dbName),
		fmt.Sprintf(sqlCreateHeartbeatTable, w.dbName),
		fmt.Sprintf(sqlInsertInitialRow, w.dbName, w.now().UnixNano(), w.tabletAlias.Uid),
	}
	for _, s := range statements {
		if _, err := conn.ExecuteFetch(s, 0, false); err != nil {
			return fmt.Errorf("Failed to execute heartbeat init query: %v", err)
		}
	}
	return nil
}

// writeHeartbeat writes exactly one heartbeat record to _vt.heartbeat.
func (w *Writer) writeHeartbeat(ctx context.Context) error {
	err := w.exec(ctx, fmt.Sprintf(sqlUpdateHeartbeat, w.dbName, w.now().UnixNano(), w.tabletAlias.Uid))
	if err != nil {
		return fmt.Errorf("Failed to execute update query: %v", err)
	}
	writes.Add(1)
	return nil
}

func (w *Writer) exec(ctx context.Context, query string) error {
	conn, err := w.pool.Get(ctx)
	if err != nil {
		return err
	}
	defer conn.Recycle()
	_, err = conn.Exec(ctx, query, 0, false)
	if err != nil {
		return err
	}
	return nil
}

func (w *Writer) recordError(err error) {
	w.errorLog.Errorf("%v", err)
	errors.Add(1)
}