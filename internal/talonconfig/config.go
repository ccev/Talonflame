package talonconfig

import (
	"fmt"
	"net"
	"os"
	"time"

	"github.com/knadh/koanf/parsers/toml"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/providers/structs"
	"github.com/knadh/koanf/v2"
	"github.com/sirupsen/logrus"

	"github.com/UnownHash/Fletchling/internal/talondb"
	"github.com/UnownHash/Fletchling/internal/talonflame"
)

type Config struct {
	Database talondb.Config `koanf:"database"`
	HTTP     HTTP           `koanf:"http"`
	Logging  Logging        `koanf:"logging"`
	Tuning   Tuning         `koanf:"tuning"`
}

type HTTP struct {
	Addr string `koanf:"addr"`
}
type Logging struct {
	Level string `koanf:"level"`
}
type Tuning struct {
	MinConfirmations  int           `koanf:"min_confirmations"`
	Agreement         float64       `koanf:"agreement"`
	MismatchThreshold int           `koanf:"mismatch_threshold"`
	ChangeWindow      time.Duration `koanf:"change_window"`
	EvidenceRetention time.Duration `koanf:"evidence_retention"`
	ClusterRadius     float64       `koanf:"cluster_radius"`
	BackfillBatchSize int           `koanf:"backfill_batch_size"`
}

func (t Tuning) Analyzer() talonflame.Config {
	return talonflame.Config{Minimum: t.MinConfirmations, Agreement: t.Agreement, MismatchMinimum: t.MismatchThreshold, ChangeWindow: t.ChangeWindow, Retention: t.EvidenceRetention, ClusterRadius: t.ClusterRadius}
}

func Defaults() Config {
	a := talonflame.DefaultConfig()
	return Config{
		Database: talondb.Config{Addr: "127.0.0.1:3306", DB: "golbat", User: "talon"},
		HTTP:     HTTP{Addr: "127.0.0.1:9042"},
		Logging:  Logging{Level: "info"},
		Tuning:   Tuning{MinConfirmations: a.Minimum, Agreement: a.Agreement, MismatchThreshold: a.MismatchMinimum, ChangeWindow: a.ChangeWindow, EvidenceRetention: a.Retention, ClusterRadius: a.ClusterRadius, BackfillBatchSize: 1000},
	}
}

func Load(path string) (Config, error) {
	cfg := Defaults()
	k := koanf.New(".")
	if err := k.Load(structs.Provider(cfg, "koanf"), nil); err != nil {
		return cfg, err
	}
	if err := k.Load(file.Provider(path), toml.Parser()); err != nil {
		return cfg, fmt.Errorf("load %s: %w", path, err)
	}
	if err := k.Unmarshal("", &cfg); err != nil {
		return cfg, fmt.Errorf("decode %s: %w", path, err)
	}
	if err := cfg.Database.Validate(); err != nil {
		return cfg, err
	}
	if _, _, err := net.SplitHostPort(cfg.HTTP.Addr); err != nil {
		return cfg, fmt.Errorf("http.addr must be host:port: %w", err)
	}
	if _, err := logrus.ParseLevel(cfg.Logging.Level); err != nil {
		return cfg, fmt.Errorf("logging.level: %w", err)
	}
	if err := cfg.Tuning.Analyzer().Validate(); err != nil {
		return cfg, fmt.Errorf("tuning: %w", err)
	}
	if cfg.Tuning.BackfillBatchSize < 1 || cfg.Tuning.BackfillBatchSize > 10000 {
		return cfg, fmt.Errorf("tuning.backfill_batch_size must be between 1 and 10000")
	}
	return cfg, nil
}

func (c Config) Logger() *logrus.Logger {
	logger := logrus.New()
	logger.SetOutput(os.Stdout)
	logger.SetFormatter(&logrus.TextFormatter{DisableColors: true, FullTimestamp: true})
	level, _ := logrus.ParseLevel(c.Logging.Level)
	logger.SetLevel(level)
	return logger
}
