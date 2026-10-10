// Package bootstrap provides env-driven factory functions for persistence backends.
package bootstrap

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	aigateway "github.com/ferro-labs/ai-gateway"
	"github.com/ferro-labs/ai-gateway/config"
	"github.com/ferro-labs/ai-gateway/internal/admin/handlers"
	"github.com/ferro-labs/ai-gateway/internal/admin/repository"
	"github.com/ferro-labs/ai-gateway/internal/requestlog"
	"github.com/ferro-labs/ai-gateway/internal/sqlitefile"
	"github.com/ferro-labs/ai-gateway/pkg/logger"
)

// Backend name constants returned alongside created stores.
const (
	BackendMemory      = "memory"
	BackendSQLite      = "sqlite"
	BackendPostgres    = "postgres"
	backendPostgresSQL = "postgresql"
	backendInMemory    = "in-memory"
	backendInMemoryAlt = "inmemory"
)

// CreateKeyStoreFromEnv builds an admin key store from API_KEY_STORE_BACKEND / API_KEY_STORE_DSN env vars.
func CreateKeyStoreFromEnv(ctx context.Context) (repository.Store, string, error) {
	backend := strings.ToLower(strings.TrimSpace(os.Getenv("API_KEY_STORE_BACKEND")))
	if backend == "" {
		backend = BackendMemory
	}

	storeDSN := strings.TrimSpace(os.Getenv("API_KEY_STORE_DSN"))

	switch backend {
	case BackendMemory, backendInMemory, backendInMemoryAlt:
		return repository.NewKeyStore(), BackendMemory, nil
	case BackendSQLite:
		logSQLiteFileSet(storeDSN)
		store, err := repository.NewSQLiteStore(ctx, storeDSN)
		if err != nil {
			return nil, "", err
		}
		return store, BackendSQLite, nil
	case BackendPostgres, backendPostgresSQL:
		store, err := repository.NewPostgresStore(ctx, storeDSN)
		if err != nil {
			return nil, "", err
		}
		return store, BackendPostgres, nil
	default:
		return nil, "", fmt.Errorf("unsupported API key store backend %q", backend)
	}
}

// CreateSessionStoreFromEnv builds a dashboard session store using the same
// backend as the key store, so an operator configures persistence once.
//
// The memory backend is the default and is a real implementation, not a
// fallback: sessions must work on a gateway with no database, they simply do
// not survive a restart there.
func CreateSessionStoreFromEnv(ctx context.Context) (repository.SessionStore, string, error) {
	backend := strings.ToLower(strings.TrimSpace(os.Getenv("API_KEY_STORE_BACKEND")))
	if backend == "" {
		backend = BackendMemory
	}
	storeDSN := strings.TrimSpace(os.Getenv("API_KEY_STORE_DSN"))

	switch backend {
	case BackendMemory, backendInMemory, backendInMemoryAlt:
		return repository.NewSessionStore(), BackendMemory, nil
	case BackendSQLite:
		store, err := repository.NewSQLiteSessionStore(ctx, sessionDSN(storeDSN))
		if err != nil {
			return nil, "", err
		}
		return store, BackendSQLite, nil
	case BackendPostgres, backendPostgresSQL:
		store, err := repository.NewPostgresSessionStore(ctx, storeDSN)
		if err != nil {
			return nil, "", err
		}
		return store, BackendPostgres, nil
	default:
		return nil, "", fmt.Errorf("unsupported session store backend %q", backend)
	}
}

// CreateAuditStoreFromEnv builds the admin audit store on the same backend as
// the key store, so persistence is configured once for all four admin stores.
//
// The memory backend is the default and a real implementation, not a fallback:
// the audit trail must work on a gateway with no database — it simply keeps only
// the most recent actions there and does not survive a restart. A deployment
// that needs the full history configures a SQL store.
func CreateAuditStoreFromEnv(ctx context.Context) (repository.AuditStore, string, error) {
	backend := strings.ToLower(strings.TrimSpace(os.Getenv("API_KEY_STORE_BACKEND")))
	if backend == "" {
		backend = BackendMemory
	}
	storeDSN := strings.TrimSpace(os.Getenv("API_KEY_STORE_DSN"))

	switch backend {
	case BackendMemory, backendInMemory, backendInMemoryAlt:
		return repository.NewMemoryAuditStore(), BackendMemory, nil
	case BackendSQLite:
		store, err := repository.NewSQLiteAuditStore(ctx, derivedSQLiteDSN(storeDSN, "-audit.db"))
		if err != nil {
			return nil, "", err
		}
		return store, BackendSQLite, nil
	case BackendPostgres, backendPostgresSQL:
		store, err := repository.NewPostgresAuditStore(ctx, storeDSN)
		if err != nil {
			return nil, "", err
		}
		return store, BackendPostgres, nil
	default:
		return nil, "", fmt.Errorf("unsupported audit store backend %q", backend)
	}
}

// sessionDSN and auditDSN keep the SQLite session and audit tables out of the
// key database. Postgres shares one DSN because every schema carries its own
// migration ledger and their version sequences cannot collide; SQLite is a
// file, so sharing it would put revocable session rows and the audit trail in
// the file operators copy around as their key store.
func sessionDSN(keyDSN string) string {
	// Empty stays empty: repository.NewSQLiteSessionStore supplies ferrogw-sessions.db.
	return derivedSQLiteDSN(keyDSN, "-sessions.db")
}

// Default SQLite filenames, restated here only so the startup line can name the
// files an empty DSN produces. Each store's constructor remains the authority.
const (
	defaultKeyFile     = "ferrogw-keys.db"
	defaultSessionFile = "ferrogw-sessions.db"
	defaultAuditFile   = "ferrogw-audit.db"
)

// logSQLiteFileSet names every file API_KEY_STORE_DSN produces.
//
// One DSN yields three databases, and neither of the two ways that surprises an
// operator produces an error: bind-mounting the single configured *file* into a
// container leaves the sessions and audit databases on the container's ephemeral
// layer, so the audit trail — a security control — is discarded on every
// restart while the keys persist; and a backup script pointed at the configured
// DSN captures one database of three.
//
// The layout itself is right and is left alone: session rows and the audit trail
// have no business travelling inside the file operators copy between machines
// as their key store. Consolidating them would undo that, and changing the
// derivation would silently repoint every existing deployment at an empty key
// store. What was missing is that nothing ever said there were three files.
// Saying so at startup is what turns "I mounted keys.db and lost my audit log"
// into a line in the log that named all three. Mount the containing directory,
// not the file.
func logSQLiteFileSet(keyDSN string) {
	if sqlitefile.IsInMemory(keyDSN) {
		return
	}
	named := func(dsn, fallback string) string {
		if dsn == "" {
			return fallback
		}
		base, _ := sqlitefile.SplitQuery(dsn)
		return base
	}
	logger.Default().Info("sqlite admin stores; back up and mount all three, not only the configured one",
		"keys", named(keyDSN, defaultKeyFile),
		"sessions", named(sessionDSN(keyDSN), defaultSessionFile),
		"audit", named(derivedSQLiteDSN(keyDSN, "-audit.db"), defaultAuditFile))
}

// derivedSQLiteDSN swaps the key DSN's file extension for suffix, so a store
// derives its own file beside the key store's rather than sharing it.
//
// A "?query" suffix (busy_timeout, journal_mode, ...) must survive onto the
// derived DSN, so it is split off before the extension swap and reattached
// after — using sqlitefile's own split rather than re-deriving it, so the two
// never drift. An empty DSN is returned unchanged so each store's constructor
// applies its own default filename. An in-memory DSN (":memory:", a file: URI
// whose path is ":memory:", or a "mode=memory" query) is also returned
// unchanged: an ephemeral key store is deliberate, and deriving a real filename
// from it would give the derived store a persistent file the key store itself
// does not have.
func derivedSQLiteDSN(keyDSN, suffix string) string {
	if keyDSN == "" || sqlitefile.IsInMemory(keyDSN) {
		return keyDSN
	}
	base, query := sqlitefile.SplitQuery(keyDSN)
	derived := strings.TrimSuffix(base, filepath.Ext(base)) + suffix
	if query != "" {
		derived += "?" + query
	}
	return derived
}

// CreateRequestLogReaderFromEnv builds a request log reader from REQUEST_LOG_STORE_BACKEND / REQUEST_LOG_STORE_DSN env vars.
func CreateRequestLogReaderFromEnv(ctx context.Context) (requestlog.Reader, requestlog.Maintainer, string, error) {
	backend := strings.ToLower(strings.TrimSpace(os.Getenv("REQUEST_LOG_STORE_BACKEND")))
	if backend == "" {
		return nil, nil, "disabled", nil
	}

	dsn := strings.TrimSpace(os.Getenv("REQUEST_LOG_STORE_DSN"))

	switch backend {
	case BackendSQLite:
		reader, err := requestlog.NewSQLiteWriter(ctx, dsn)
		if err != nil {
			return nil, nil, "", err
		}
		return reader, reader, BackendSQLite, nil
	case BackendPostgres, backendPostgresSQL:
		reader, err := requestlog.NewPostgresWriter(ctx, dsn)
		if err != nil {
			return nil, nil, "", err
		}
		return reader, reader, BackendPostgres, nil
	default:
		return nil, nil, "", fmt.Errorf("unsupported request log store backend %q", backend)
	}
}

// CreateConfigManagerFromEnv builds a config manager from CONFIG_STORE_BACKEND / CONFIG_STORE_DSN env vars.
func CreateConfigManagerFromEnv(ctx context.Context, gw *aigateway.Gateway) (handlers.ConfigManager, string, error) {
	store, backend, err := createConfigStoreFromEnv(ctx)
	if err != nil {
		return nil, "", err
	}
	manager, err := newConfigManager(gw, store)
	if err != nil {
		return nil, "", err
	}
	return manager, backend, nil
}

// configStore is a persistent config store, closed by whoever owns it: the
// startup sequence until a config manager is built over it, the manager after.
type configStore interface {
	repository.ConfigStore
	Close() error
}

// createConfigStoreFromEnv opens the persistent config store that
// CONFIG_STORE_BACKEND names. The memory backend keeps no store, so it returns
// a nil store.
//
// It is separate from the config manager because the manager needs a gateway,
// and part of what the store holds is read before the gateway exists: a
// persisted config supersedes the file whole, observability included, and the
// tracing pipeline is built ahead of the gateway (see buildServer).
func createConfigStoreFromEnv(ctx context.Context) (configStore, string, error) {
	backend := strings.ToLower(strings.TrimSpace(os.Getenv("CONFIG_STORE_BACKEND")))
	if backend == "" {
		backend = BackendMemory
	}

	dsn := strings.TrimSpace(os.Getenv("CONFIG_STORE_DSN"))

	switch backend {
	case BackendMemory, backendInMemory, backendInMemoryAlt:
		return nil, BackendMemory, nil
	case BackendSQLite:
		store, err := repository.NewSQLiteConfigStore(ctx, dsn)
		if err != nil {
			return nil, "", err
		}
		return store, BackendSQLite, nil
	case BackendPostgres, backendPostgresSQL:
		store, err := repository.NewPostgresConfigStore(ctx, dsn)
		if err != nil {
			return nil, "", err
		}
		return store, BackendPostgres, nil
	default:
		return nil, "", fmt.Errorf("unsupported config store backend %q", backend)
	}
}

// newConfigManager builds the config manager over store, which may be nil for
// the memory backend. The manager owns store from here on: it is closed here
// when the manager cannot be built, and by the manager's Close otherwise.
func newConfigManager(gw *aigateway.Gateway, store configStore) (handlers.ConfigManager, error) {
	var persistent repository.ConfigStore
	if store != nil {
		persistent = store
	}
	manager, err := repository.NewGatewayConfigManager(gw, persistent)
	if err != nil {
		if store != nil {
			_ = store.Close()
		}
		return nil, err
	}
	return manager, nil
}

// startupObservability returns the observability section of the config the
// gateway is about to run. A config persisted in the store supersedes the file
// as a whole when the config manager adopts it, so its section is the one in
// force; the file's applies only when the store holds none.
//
// Reading the file's section regardless built the tracing pipeline from a
// config the gateway was not running: an exporter, endpoint or sampler set
// through PUT /admin/config — accepted with a warning that it takes effect on
// the next restart — never took effect on any restart, while GET /admin/config
// reported it.
func startupObservability(ctx context.Context, fileCfg *config.Config, store configStore) (config.ObservabilityConfig, error) {
	if store != nil {
		persisted, ok, err := store.Load(ctx)
		if err != nil {
			return config.ObservabilityConfig{}, err
		}
		if ok {
			return persisted.Observability, nil
		}
	}
	if fileCfg != nil {
		return fileCfg.Observability, nil
	}
	return config.ObservabilityConfig{}, nil
}
