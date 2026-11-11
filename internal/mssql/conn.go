// Package mssql — read-only T-SQL core for mcp-mssql-readonly.
//
// Mirror of internal/pg for SQL Server: the database role (ai_reader,
// SELECT-only) is the security boundary; this package adds the belt-and-
// suspenders posture (single-statement precheck, per-call context timeouts,
// output caps via internal/result) and wires the bidirectional anonymization
// choke point from internal/anon on both ends. SQL Server has no READ ONLY
// transaction mode, so unlike the PG sibling there is no RO txn wrapper —
// the role ACLs plus the precheck carry that side, and --selftest verifies
// the grants through sys.database_permissions.
package mssql

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"net/url"
	"time"

	mssql "github.com/microsoft/go-mssqldb" // driver "sqlserver" + Connector
)

// Config carries the connection parameters NewDB builds the DSN from.
type Config struct {
	Host     string
	Port     string
	User     string
	Password string
	Database string
	Encrypt  string // encrypt= DSN value: disable | false | true | strict
}

// NewDB opens the single-connection pool. MaxOpenConns=1 mirrors the PG pool
// discipline: tool calls in one stdio session are serial, and a larger pool
// would starve other concurrent sessions sharing the role's connection
// limit. There is no server-side statement_timeout knob via the driver —
// cancellation is client-side context (see ro.go).
func NewDB(cfg Config) (*sql.DB, error) {
	q := url.Values{}
	q.Set("database", cfg.Database)
	q.Set("app name", "mcp-mssql-readonly")
	if cfg.Encrypt != "" {
		q.Set("encrypt", cfg.Encrypt)
	}
	dsn := &url.URL{
		Scheme:   "sqlserver",
		User:     url.UserPassword(cfg.User, cfg.Password),
		Host:     net.JoinHostPort(cfg.Host, cfg.Port),
		RawQuery: q.Encode(),
	}
	// Connector instead of sql.Open: SessionInitSQL runs on EVERY physical
	// (re)connection, pinning the session language. The DWH login's default
	// language is Swedish, which made server error texts arrive in Swedish
	// (live finding 2026-09-04, report №12) — us_english keeps them readable
	// and predictable for masking; the URL DSN has no language parameter.
	connector, err := mssql.NewConnector(dsn.String())
	if err != nil {
		return nil, fmt.Errorf("connector: %w", err)
	}
	connector.SessionInitSQL = "SET LANGUAGE us_english"
	db := sql.OpenDB(connector)
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxIdleTime(5 * time.Minute)

	pingCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	return db, nil
}
