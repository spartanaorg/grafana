package sqlstore

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-xorm/xorm"
	"github.com/grafana/grafana/pkg/bus"
	"github.com/grafana/grafana/pkg/infra/localcache"
	"github.com/grafana/grafana/pkg/infra/log"
	m "github.com/grafana/grafana/pkg/models"
	"github.com/grafana/grafana/pkg/registry"
	"github.com/grafana/grafana/pkg/services/annotations"
	"github.com/grafana/grafana/pkg/services/sqlstore/migrations"
	"github.com/grafana/grafana/pkg/services/sqlstore/migrator"
	"github.com/grafana/grafana/pkg/services/sqlstore/sqlutil"
	"github.com/grafana/grafana/pkg/setting"
	_ "github.com/grafana/grafana/pkg/tsdb/mssql"
)

var (
	x       *xorm.Engine
	dialect migrator.Dialect

	sqlog log.Logger = log.New("sqlstore")
)

const ContextSessionName = "db-session"

func init() {
	registry.Register(&registry.Descriptor{
		Name:         "SqlStore",
		Instance:     &SqlStore{},
		InitPriority: registry.High,
	})
}

type SqlStore struct {
	Cfg          *setting.Cfg             `inject:""`
	Bus          bus.Bus                  `inject:""`
	CacheService *localcache.CacheService `inject:""`

	dbCfg           DatabaseConfig
	engine          *xorm.Engine
	log             log.Logger
	Dialect         migrator.Dialect
	skipEnsureAdmin bool
}

func (ss *SqlStore) Init() error {
	ss.log = log.New("sqlstore")
	ss.readConfig()

	engine, err := ss.getEngine()

	if err != nil {
		return fmt.Errorf("Fail to connect to database: %v", err)
	}

	ss.engine = engine
	ss.Dialect = migrator.NewDialect(ss.engine)

	// temporarily still set global var
	x = engine
	dialect = ss.Dialect

	migrator := migrator.NewMigrator(x)
	migrations.AddMigrations(migrator)

	for _, descriptor := range registry.GetServices() {
		sc, ok := descriptor.Instance.(registry.DatabaseMigrator)
		if ok {
			sc.AddMigration(migrator)
		}
	}

	if err := migrator.Start(); err != nil {
		return fmt.Errorf("Migration failed err: %v", err)
	}

	// Init repo instances
	annotations.SetRepository(&SqlAnnotationRepo{})
	ss.Bus.SetTransactionManager(ss)

	// Register handlers
	ss.addUserQueryAndCommandHandlers()

	// ensure admin user
	if ss.skipEnsureAdmin {
		return nil
	}

	return ss.ensureAdminUser()
}

func (ss *SqlStore) ensureAdminUser() error {
	systemUserCountQuery := m.GetSystemUserCountStatsQuery{}

	err := ss.InTransaction(context.Background(), func(ctx context.Context) error {

		err := bus.DispatchCtx(ctx, &systemUserCountQuery)
		if err != nil {
			return fmt.Errorf("Could not determine if admin user exists: %v", err)
		}

		if systemUserCountQuery.Result.Count > 0 {
			return nil
		}

		cmd := m.CreateUserCommand{}
		cmd.Login = setting.AdminUser
		cmd.Email = setting.AdminUser + "@localhost"
		cmd.Password = setting.AdminPassword
		cmd.IsAdmin = true

		if err := bus.DispatchCtx(ctx, &cmd); err != nil {
			return fmt.Errorf("Failed to create admin user: %v", err)
		}

		ss.log.Info("Created default admin", "user", setting.AdminUser)

		return nil
	})

	return err
}

func (ss *SqlStore) buildExtraConnectionString(sep rune) string {
	if ss.dbCfg.UrlQueryParams == nil {
		return ""
	}

	var sb strings.Builder
	for key, values := range ss.dbCfg.UrlQueryParams {
		for _, value := range values {
			sb.WriteRune(sep)
			sb.WriteString(key)
			sb.WriteRune('=')
			sb.WriteString(value)
		}
	}
	return sb.String()
}

func (ss *SqlStore) buildConnectionString() (string, error) {
	cnnstr := ss.dbCfg.ConnectionString

	// special case used by integration tests
	if cnnstr != "" {
		return cnnstr, nil
	}

	switch ss.dbCfg.Type {
	case migrator.SQLITE:
		// special case for tests
		if !filepath.IsAbs(ss.dbCfg.Path) {
			ss.dbCfg.Path = filepath.Join(ss.Cfg.DataPath, ss.dbCfg.Path)
		}
		if err := os.MkdirAll(path.Dir(ss.dbCfg.Path), os.ModePerm); err != nil {
			return "", err
		}

		cnnstr = fmt.Sprintf("file:%s?cache=%s&mode=rwc", ss.dbCfg.Path, ss.dbCfg.CacheMode)
		cnnstr += ss.buildExtraConnectionString('&')
	default:
		return "", fmt.Errorf("Unknown database type: %s", ss.dbCfg.Type)
	}

	return cnnstr, nil
}

func (ss *SqlStore) getEngine() (*xorm.Engine, error) {
	connectionString, err := ss.buildConnectionString()

	if err != nil {
		return nil, err
	}

	sqlog.Info("Connecting to DB", "dbtype", ss.dbCfg.Type)
	engine, err := xorm.NewEngine(ss.dbCfg.Type, connectionString)
	if err != nil {
		return nil, err
	}

	engine.SetMaxOpenConns(ss.dbCfg.MaxOpenConn)
	engine.SetMaxIdleConns(ss.dbCfg.MaxIdleConn)
	engine.SetConnMaxLifetime(time.Second * time.Duration(ss.dbCfg.ConnMaxLifetime))

	// configure sql logging
	debugSql := ss.Cfg.Raw.Section("database").Key("log_queries").MustBool(false)
	if !debugSql {
		engine.SetLogger(&xorm.DiscardLogger{})
	} else {
		engine.SetLogger(NewXormLogger(log.LvlInfo, log.New("sqlstore.xorm")))
		engine.ShowSQL(true)
		engine.ShowExecTime(true)
	}

	return engine, nil
}

func (ss *SqlStore) readConfig() {
	sec := ss.Cfg.Raw.Section("database")

	ss.dbCfg.Type = sec.Key("type").String()
	ss.dbCfg.Name = sec.Key("name").String()
	ss.dbCfg.User = sec.Key("user").String()
	ss.dbCfg.ConnectionString = sec.Key("connection_string").String()
	ss.dbCfg.Pwd = sec.Key("password").String()

	ss.dbCfg.MaxOpenConn = sec.Key("max_open_conn").MustInt(0)
	ss.dbCfg.MaxIdleConn = sec.Key("max_idle_conn").MustInt(2)
	ss.dbCfg.ConnMaxLifetime = sec.Key("conn_max_lifetime").MustInt(14400)

	ss.dbCfg.CaCertPath = sec.Key("ca_cert_path").String()
	ss.dbCfg.ClientKeyPath = sec.Key("client_key_path").String()
	ss.dbCfg.ClientCertPath = sec.Key("client_cert_path").String()
	ss.dbCfg.ServerCertName = sec.Key("server_cert_name").String()
	ss.dbCfg.Path = sec.Key("path").MustString("data/grafana.db")

	ss.dbCfg.CacheMode = sec.Key("cache_mode").MustString("private")
}

// Interface of arguments for testing db
type ITestDB interface {
	Helper()
	Fatalf(format string, args ...interface{})
}

// InitTestDB initiliaze test DB
func InitTestDB(t ITestDB) *SqlStore {
	t.Helper()
	sqlstore := &SqlStore{}
	sqlstore.skipEnsureAdmin = true
	sqlstore.Bus = bus.New()
	sqlstore.CacheService = localcache.New(5*time.Minute, 10*time.Minute)

	dbType := migrator.SQLITE

	// environment variable present for test db?
	if db, present := os.LookupEnv("GRAFANA_TEST_DB"); present {
		dbType = db
	}

	// set test db config
	sqlstore.Cfg = setting.NewCfg()
	sec, err := sqlstore.Cfg.Raw.NewSection("database")
	if err != nil {
		t.Fatalf("Failed to create section: %s", err)
	}
	if _, err := sec.NewKey("type", dbType); err != nil {
		t.Fatalf("Failed to create key: %s", err)
	}

	if _, err := sec.NewKey("connection_string", sqlutil.TestDB_Sqlite3.ConnStr); err != nil {
		t.Fatalf("Failed to create key: %s", err)
	}

	// need to get engine to clean db before we init
	engine, err := xorm.NewEngine(dbType, sec.Key("connection_string").String())
	if err != nil {
		t.Fatalf("Failed to init test database: %v", err)
	}

	sqlstore.Dialect = migrator.NewDialect(engine)

	// temp global var until we get rid of global vars
	dialect = sqlstore.Dialect

	if err := dialect.CleanDB(); err != nil {
		t.Fatalf("Failed to clean test db %v", err)
	}

	if err := sqlstore.Init(); err != nil {
		t.Fatalf("Failed to init test database: %v", err)
	}

	sqlstore.engine.DatabaseTZ = time.UTC
	sqlstore.engine.TZLocation = time.UTC

	return sqlstore
}

type DatabaseConfig struct {
	Type             string
	Name             string
	User             string
	Pwd              string
	Path             string
	CaCertPath       string
	ClientKeyPath    string
	ClientCertPath   string
	ServerCertName   string
	ConnectionString string
	MaxOpenConn      int
	MaxIdleConn      int
	ConnMaxLifetime  int
	CacheMode        string
	UrlQueryParams   map[string][]string
}
