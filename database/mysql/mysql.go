// This is a copy of github.com/pulumi/golang-migrate/v4/cmd/database/mysql/mysql.go
// It has been extended to support x-metadata-lock-timeouts and x-metadata-lock-retry-delay
// See this design doc for more on the problem we are trying to solve:
//  https://docs.google.com/document/d/1TfctnknUy3YHTpt0LmzmWuUHK2Qp3_hGUR446hCZx74/edit

package mysql

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	nurl "net/url"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/pulumi/golang-migrate/v4/database"
)

var _ database.Driver = (*Mysql)(nil) // explicit compile time type check

func init() {
	database.Register("mysql", &Mysql{})
}

var DefaultMigrationsTable = "schema_migrations"

var (
	ErrDatabaseDirty    = fmt.Errorf("database is dirty")
	ErrNilConfig        = fmt.Errorf("no config")
	ErrNoDatabaseName   = fmt.Errorf("no database name")
	ErrAppendPEM        = fmt.Errorf("failed to append PEM")
	ErrTLSCertKeyConfig = fmt.Errorf("to use TLS client authentication, both x-tls-cert and x-tls-key must not be empty")
)

type Config struct {
	MigrationsTable  string
	DatabaseName     string
	NoLock           bool
	StatementTimeout time.Duration

	// MetadataLockTimeouts is the lock_wait_timeout, in seconds, for each attempt at a
	// migration file: an attempt that times out on a metadata lock (ER_LOCK_WAIT_TIMEOUT)
	// moves on to the next entry, and the file fails once the list is exhausted. Empty
	// leaves the server's lock_wait_timeout alone and makes a single attempt.
	MetadataLockTimeouts []uint
	// MetadataLockRetryDelay is the minimum pause between attempts; each pause is drawn
	// uniformly from [delay, 2*delay).
	MetadataLockRetryDelay time.Duration
}

type Mysql struct {
	// mysql RELEASE_LOCK must be called from the same conn, so
	// just do everything over a single conn anyway.
	conn     *sql.Conn
	db       *sql.DB
	isLocked atomic.Bool

	config *Config

	// sleep and jitter are swapped out by tests; nil means time.Sleep and rand.N.
	sleep  func(time.Duration)
	jitter func(time.Duration) time.Duration
}

// connection instance must have `multiStatements` set to true
func WithConnection(ctx context.Context, conn *sql.Conn, config *Config) (*Mysql, error) {
	if config == nil {
		return nil, ErrNilConfig
	}

	if err := conn.PingContext(ctx); err != nil {
		return nil, err
	}

	mx := &Mysql{
		conn:   conn,
		db:     nil,
		config: config,
	}

	if config.DatabaseName == "" {
		query := `SELECT DATABASE()`
		var databaseName sql.NullString
		if err := conn.QueryRowContext(ctx, query).Scan(&databaseName); err != nil {
			return nil, &database.Error{OrigErr: err, Query: []byte(query)}
		}

		if len(databaseName.String) == 0 {
			return nil, ErrNoDatabaseName
		}

		config.DatabaseName = databaseName.String
	}

	if len(config.MigrationsTable) == 0 {
		config.MigrationsTable = DefaultMigrationsTable
	}

	if err := mx.ensureVersionTable(); err != nil {
		return nil, err
	}

	return mx, nil
}

// instance must have `multiStatements` set to true
func WithInstance(instance *sql.DB, config *Config) (database.Driver, error) {
	ctx := context.Background()

	if err := instance.Ping(); err != nil {
		return nil, err
	}

	conn, err := instance.Conn(ctx)
	if err != nil {
		return nil, err
	}

	mx, err := WithConnection(ctx, conn, config)
	if err != nil {
		return nil, err
	}

	mx.db = instance

	return mx, nil
}

// extractCustomQueryParams extracts the custom query params (ones that start with "x-") from
// mysql.Config.Params (connection parameters) as to not interfere with connecting to MySQL
func extractCustomQueryParams(c *mysql.Config) (map[string]string, error) {
	if c == nil {
		return nil, ErrNilConfig
	}
	customQueryParams := map[string]string{}

	for k, v := range c.Params {
		if strings.HasPrefix(k, "x-") {
			customQueryParams[k] = v
			delete(c.Params, k)
		}
	}
	return customQueryParams, nil
}

func urlToMySQLConfig(url string) (*mysql.Config, error) {
	// Need to parse out custom TLS parameters and call
	// mysql.RegisterTLSConfig() before mysql.ParseDSN() is called
	// which consumes the registered tls.Config
	// Fixes: https://github.com/golang-migrate/migrate/issues/411
	//
	// Can't use url.Parse() since it fails to parse MySQL DSNs
	// mysql.ParseDSN() also searches for "?" to find query parameters:
	// https://github.com/go-sql-driver/mysql/blob/46351a8/dsn.go#L344
	if idx := strings.LastIndex(url, "?"); idx > 0 {
		rawParams := url[idx+1:]
		parsedParams, err := nurl.ParseQuery(rawParams)
		if err != nil {
			return nil, err
		}

		ctls := parsedParams.Get("tls")
		if len(ctls) > 0 {
			if _, isBool := readBool(ctls); !isBool && strings.ToLower(ctls) != "skip-verify" {
				rootCertPool := x509.NewCertPool()
				pem, err := os.ReadFile(parsedParams.Get("x-tls-ca"))
				if err != nil {
					return nil, err
				}

				if ok := rootCertPool.AppendCertsFromPEM(pem); !ok {
					return nil, ErrAppendPEM
				}

				clientCert := make([]tls.Certificate, 0, 1)
				if ccert, ckey := parsedParams.Get("x-tls-cert"), parsedParams.Get("x-tls-key"); ccert != "" || ckey != "" {
					if ccert == "" || ckey == "" {
						return nil, ErrTLSCertKeyConfig
					}
					certs, err := tls.LoadX509KeyPair(ccert, ckey)
					if err != nil {
						return nil, err
					}
					clientCert = append(clientCert, certs)
				}

				insecureSkipVerify := false
				insecureSkipVerifyStr := parsedParams.Get("x-tls-insecure-skip-verify")
				if len(insecureSkipVerifyStr) > 0 {
					x, err := strconv.ParseBool(insecureSkipVerifyStr)
					if err != nil {
						return nil, err
					}
					insecureSkipVerify = x
				}

				err = mysql.RegisterTLSConfig(ctls, &tls.Config{
					RootCAs:            rootCertPool,
					Certificates:       clientCert,
					InsecureSkipVerify: insecureSkipVerify,
				})
				if err != nil {
					return nil, err
				}
			}
		}
	}

	config, err := mysql.ParseDSN(strings.TrimPrefix(url, "mysql://"))
	if err != nil {
		return nil, err
	}

	config.MultiStatements = true

	// Keep backwards compatibility from when we used net/url.Parse() to parse the DSN.
	// net/url.Parse() would automatically unescape it for us.
	// See: https://play.golang.org/p/q9j1io-YICQ
	user, err := nurl.QueryUnescape(config.User)
	if err != nil {
		return nil, err
	}
	config.User = user

	password, err := nurl.QueryUnescape(config.Passwd)
	if err != nil {
		return nil, err
	}
	config.Passwd = password

	return config, nil
}

func (m *Mysql) Open(url string) (database.Driver, error) {
	config, err := urlToMySQLConfig(url)
	if err != nil {
		return nil, err
	}

	customParams, err := extractCustomQueryParams(config)
	if err != nil {
		return nil, err
	}

	noLockParam, noLock := customParams["x-no-lock"], false
	if noLockParam != "" {
		noLock, err = strconv.ParseBool(noLockParam)
		if err != nil {
			return nil, fmt.Errorf("could not parse x-no-lock as bool: %w", err)
		}
	}

	statementTimeoutParam := customParams["x-statement-timeout"]
	statementTimeout := 0
	if statementTimeoutParam != "" {
		statementTimeout, err = strconv.Atoi(statementTimeoutParam)
		if err != nil {
			return nil, fmt.Errorf("could not parse x-statement-timeout as float: %w", err)
		}
	}

	metadataLockTimeouts, metadataLockRetryDelay, err := metadataLockScheduleFromParams(customParams)
	if err != nil {
		return nil, err
	}

	db, err := sql.Open("mysql", config.FormatDSN())
	if err != nil {
		return nil, err
	}

	mx, err := WithInstance(db, &Config{
		DatabaseName:           config.DBName,
		MigrationsTable:        customParams["x-migrations-table"],
		NoLock:                 noLock,
		StatementTimeout:       time.Duration(statementTimeout) * time.Millisecond,
		MetadataLockTimeouts:   metadataLockTimeouts,
		MetadataLockRetryDelay: metadataLockRetryDelay,
	})
	if err != nil {
		return nil, err
	}

	return mx, nil
}

// metadataLockScheduleFromParams reads x-metadata-lock-timeouts and x-metadata-lock-retry-delay.
// The older x-metadata-lock-timeout and x-metadata-lock-retries pair is still accepted and
// expands to its original flat schedule: retries+1 attempts of the same timeout, about 1s apart.
func metadataLockScheduleFromParams(params map[string]string) ([]uint, time.Duration, error) {
	legacyTimeout, legacyRetries := params["x-metadata-lock-timeout"], params["x-metadata-lock-retries"]
	if legacyTimeout != "" || legacyRetries != "" {
		if params["x-metadata-lock-timeouts"] != "" || params["x-metadata-lock-retry-delay"] != "" {
			return nil, 0, fmt.Errorf("x-metadata-lock-timeout and x-metadata-lock-retries cannot be combined with x-metadata-lock-timeouts or x-metadata-lock-retry-delay")
		}
		if legacyTimeout == "" {
			return nil, 0, nil
		}
		timeout, err := strconv.ParseUint(legacyTimeout, 10, 32)
		if err != nil {
			return nil, 0, fmt.Errorf("could not parse x-metadata-lock-timeout as uint: %w", err)
		}
		retries := uint64(0)
		if legacyRetries != "" {
			retries, err = strconv.ParseUint(legacyRetries, 10, 32)
			if err != nil {
				return nil, 0, fmt.Errorf("could not parse x-metadata-lock-retries as uint: %w", err)
			}
		}
		if timeout == 0 {
			return nil, 0, nil
		}
		timeouts := make([]uint, retries+1)
		for i := range timeouts {
			timeouts[i] = uint(timeout)
		}
		return timeouts, time.Second / 2, nil
	}

	timeouts, err := ParseMetadataLockTimeouts(params["x-metadata-lock-timeouts"])
	if err != nil {
		return nil, 0, fmt.Errorf("could not parse x-metadata-lock-timeouts: %w", err)
	}
	delay := time.Duration(0)
	if raw := params["x-metadata-lock-retry-delay"]; raw != "" {
		delay, err = time.ParseDuration(raw)
		if err != nil {
			return nil, 0, fmt.Errorf("could not parse x-metadata-lock-retry-delay as duration: %w", err)
		}
	}
	return timeouts, delay, nil
}

func (m *Mysql) Close() error {
	connErr := m.conn.Close()
	var dbErr error
	if m.db != nil {
		dbErr = m.db.Close()
	}

	if connErr != nil || dbErr != nil {
		return fmt.Errorf("conn: %v, db: %v", connErr, dbErr)
	}
	return nil
}

func (m *Mysql) Lock() error {
	return database.CasRestoreOnErr(&m.isLocked, false, true, database.ErrLocked, func() error {
		if m.config.NoLock {
			return nil
		}
		aid, err := database.GenerateAdvisoryLockId(
			fmt.Sprintf("%s:%s", m.config.DatabaseName, m.config.MigrationsTable))
		if err != nil {
			return err
		}

		query := "SELECT GET_LOCK(?, 10)"
		var success bool
		if err := m.conn.QueryRowContext(context.Background(), query, aid).Scan(&success); err != nil {
			return &database.Error{OrigErr: err, Err: "try lock failed", Query: []byte(query)}
		}

		if !success {
			return database.ErrLocked
		}

		return nil
	})
}

func (m *Mysql) Unlock() error {
	return database.CasRestoreOnErr(&m.isLocked, true, false, database.ErrNotLocked, func() error {
		if m.config.NoLock {
			return nil
		}

		aid, err := database.GenerateAdvisoryLockId(
			fmt.Sprintf("%s:%s", m.config.DatabaseName, m.config.MigrationsTable))
		if err != nil {
			return err
		}

		query := `SELECT RELEASE_LOCK(?)`
		if _, err := m.conn.ExecContext(context.Background(), query, aid); err != nil {
			return &database.Error{OrigErr: err, Query: []byte(query)}
		}

		// NOTE: RELEASE_LOCK could return NULL or (or 0 if the code is changed),
		// in which case isLocked should be true until the timeout expires -- synchronizing
		// these states is likely not worth trying to do; reconsider the necessity of isLocked.

		return nil
	})
}

func (m *Mysql) Run(migration io.Reader) error {
	migr, err := io.ReadAll(migration)
	if err != nil {
		return err
	}

	ctx := context.Background()
	if m.config.StatementTimeout != 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, m.config.StatementTimeout)
		defer cancel()
	}

	exec := func() error {
		if _, err := m.conn.ExecContext(ctx, string(migr)); err != nil {
			return database.Error{OrigErr: err, Err: "migration failed", Query: migr}
		}
		return nil
	}

	if len(m.config.MetadataLockTimeouts) == 0 {
		return exec()
	}
	return m.runWithMetadataLockRetries(migr, func(timeout uint) error {
		query := "SET lock_wait_timeout=?"
		if _, err := m.conn.ExecContext(ctx, query, timeout); err != nil {
			return &database.Error{OrigErr: err, Query: []byte(query)}
		}
		return exec()
	})
}

func (m *Mysql) runWithMetadataLockRetries(migr []byte, attempt func(timeout uint) error) error {
	timeouts := m.config.MetadataLockTimeouts
	for i, timeout := range timeouts {
		err := attempt(timeout)
		if !isLockWaitTimeout(err) {
			return err
		}
		if i == len(timeouts)-1 {
			break
		}
		pause := m.retryPause()
		log.Printf("Failed to grab metadata lock within %ds (attempt %d of %d). Retrying in %s with a %ds timeout...",
			timeout, i+1, len(timeouts), pause.Round(100*time.Millisecond), timeouts[i+1])
		m.doSleep(pause)
	}
	return database.Error{
		Err:   fmt.Sprintf("couldn't obtain metadata lock in %d attempts with timeouts of %v seconds", len(timeouts), timeouts),
		Query: migr,
	}
}

func isLockWaitTimeout(err error) bool {
	var mysqlErr *mysql.MySQLError
	return errors.As(err, &mysqlErr) && mysqlErr.Number == 1205
}

func (m *Mysql) retryPause() time.Duration {
	delay := m.config.MetadataLockRetryDelay
	if delay <= 0 {
		return 0
	}
	jitter := m.jitter
	if jitter == nil {
		jitter = rand.N[time.Duration]
	}
	return delay + jitter(delay)
}

func (m *Mysql) doSleep(d time.Duration) {
	if m.sleep != nil {
		m.sleep(d)
		return
	}
	time.Sleep(d)
}

// ParseMetadataLockTimeouts parses a comma-separated list of whole seconds, such as
// "1,1,4,10", into a Config.MetadataLockTimeouts schedule. An empty string yields nil.
func ParseMetadataLockTimeouts(s string) ([]uint, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	parts := strings.Split(s, ",")
	timeouts := make([]uint, 0, len(parts))
	for _, part := range parts {
		timeout, err := strconv.ParseUint(strings.TrimSpace(part), 10, 32)
		if err != nil {
			return nil, fmt.Errorf("%q is not a whole number of seconds: %w", part, err)
		}
		if timeout == 0 {
			return nil, fmt.Errorf("timeouts must be at least 1 second, got %q", s)
		}
		timeouts = append(timeouts, uint(timeout))
	}
	return timeouts, nil
}

func (m *Mysql) SetVersion(version int, dirty bool) error {
	tx, err := m.conn.BeginTx(context.Background(), &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return &database.Error{OrigErr: err, Err: "transaction start failed"}
	}

	query := "DELETE FROM `" + m.config.MigrationsTable + "` LIMIT 1"
	if _, err := tx.ExecContext(context.Background(), query); err != nil {
		if errRollback := tx.Rollback(); errRollback != nil {
			err = errors.Join(err, errRollback)
		}
		return &database.Error{OrigErr: err, Query: []byte(query)}
	}

	// Also re-write the schema version for nil dirty versions to prevent
	// empty schema version for failed down migration on the first migration
	// See: https://github.com/golang-migrate/migrate/issues/330
	if version >= 0 || (version == database.NilVersion && dirty) {
		query := "INSERT INTO `" + m.config.MigrationsTable + "` (version, dirty) VALUES (?, ?)"
		if _, err := tx.ExecContext(context.Background(), query, version, dirty); err != nil {
			if errRollback := tx.Rollback(); errRollback != nil {
				err = errors.Join(err, errRollback)
			}
			return &database.Error{OrigErr: err, Query: []byte(query)}
		}
	}

	if err := tx.Commit(); err != nil {
		return &database.Error{OrigErr: err, Err: "transaction commit failed"}
	}

	return nil
}

func (m *Mysql) Version() (version int, dirty bool, err error) {
	query := "SELECT version, dirty FROM `" + m.config.MigrationsTable + "` LIMIT 1"
	err = m.conn.QueryRowContext(context.Background(), query).Scan(&version, &dirty)
	switch {
	case err == sql.ErrNoRows:
		return database.NilVersion, false, nil

	case err != nil:
		if e, ok := err.(*mysql.MySQLError); ok {
			if e.Number == 0 {
				return database.NilVersion, false, nil
			}
		}
		return 0, false, &database.Error{OrigErr: err, Query: []byte(query)}

	default:
		return version, dirty, nil
	}
}

func (m *Mysql) Drop() (err error) {
	// select all tables
	query := `SHOW TABLES LIKE '%'`
	tables, err := m.conn.QueryContext(context.Background(), query)
	if err != nil {
		return &database.Error{OrigErr: err, Query: []byte(query)}
	}
	defer func() {
		if errClose := tables.Close(); errClose != nil {
			err = errors.Join(err, errClose)
		}
	}()

	// delete one table after another
	tableNames := make([]string, 0)
	for tables.Next() {
		var tableName string
		if err := tables.Scan(&tableName); err != nil {
			return err
		}
		if len(tableName) > 0 {
			tableNames = append(tableNames, tableName)
		}
	}
	if err := tables.Err(); err != nil {
		return &database.Error{OrigErr: err, Query: []byte(query)}
	}

	if len(tableNames) > 0 {
		// disable checking foreign key constraints until finished
		query = `SET foreign_key_checks = 0`
		if _, err := m.conn.ExecContext(context.Background(), query); err != nil {
			return &database.Error{OrigErr: err, Query: []byte(query)}
		}

		defer func() {
			// enable foreign key checks
			_, _ = m.conn.ExecContext(context.Background(), `SET foreign_key_checks = 1`)
		}()

		// delete one by one ...
		for _, t := range tableNames {
			query = "DROP TABLE IF EXISTS `" + t + "`"
			if _, err := m.conn.ExecContext(context.Background(), query); err != nil {
				return &database.Error{OrigErr: err, Query: []byte(query)}
			}
		}
	}

	return nil
}

// ensureVersionTable checks if versions table exists and, if not, creates it.
// Note that this function locks the database, which deviates from the usual
// convention of "caller locks" in the Mysql type.
func (m *Mysql) ensureVersionTable() (err error) {
	if err = m.Lock(); err != nil {
		return err
	}

	defer func() {
		if e := m.Unlock(); e != nil {
			err = errors.Join(err, e)
		}
	}()

	// check if migration table exists
	var result string
	query := `SHOW TABLES LIKE '` + m.config.MigrationsTable + `'`
	if err := m.conn.QueryRowContext(context.Background(), query).Scan(&result); err != nil {
		if err != sql.ErrNoRows {
			return &database.Error{OrigErr: err, Query: []byte(query)}
		}
	} else {
		return nil
	}

	// if not, create the empty migration table
	query = "CREATE TABLE `" + m.config.MigrationsTable + "` (version bigint not null primary key, dirty boolean not null)"
	if _, err := m.conn.ExecContext(context.Background(), query); err != nil {
		return &database.Error{OrigErr: err, Query: []byte(query)}
	}
	return nil
}

// Returns the bool value of the input.
// The 2nd return value indicates if the input was a valid bool value
// See https://github.com/go-sql-driver/mysql/blob/a059889267dc7170331388008528b3b44479bffb/utils.go#L71
func readBool(input string) (value bool, valid bool) {
	switch input {
	case "1", "true", "TRUE", "True":
		return true, true
	case "0", "false", "FALSE", "False":
		return false, true
	}

	// Not a valid bool value
	return
}
