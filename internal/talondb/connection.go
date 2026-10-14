package talondb

import (
	"database/sql"
	"fmt"
	"net"
	"time"

	"github.com/go-sql-driver/mysql"
)

type Config struct {
	Addr     string `koanf:"addr"`
	Socket   string `koanf:"socket"`
	DB       string `koanf:"db"`
	User     string `koanf:"user"`
	Password string `koanf:"password"`
}

func (c Config) Validate() error {
	if c.DB == "" || c.User == "" || c.Password == "" {
		return fmt.Errorf("database.db, database.user and database.password are required")
	}
	if c.Socket == "" {
		if _, _, err := net.SplitHostPort(c.Addr); err != nil {
			return fmt.Errorf("database.addr must be host:port: %w", err)
		}
	}
	return nil
}

// Open uses only the dedicated account supplied by Talonflame's config.
func Open(c Config) (*sql.DB, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	cfg := mysql.NewConfig()
	cfg.DBName, cfg.User, cfg.Passwd = c.DB, c.User, c.Password
	cfg.Net, cfg.Addr = "tcp", c.Addr
	if c.Socket != "" {
		cfg.Net, cfg.Addr = "unix", c.Socket
	}
	cfg.Timeout = 10 * time.Second
	cfg.ReadTimeout = 10 * time.Minute
	cfg.WriteTimeout = 10 * time.Minute
	db, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(4)
	db.SetConnMaxLifetime(5 * time.Minute)
	return db, nil
}
