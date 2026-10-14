package talonconfig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
)

func TestExampleAndOverrides(t *testing.T) {
	cfg, err := Load("../../config.example.toml")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Tuning.Analyzer() != Defaults().Tuning.Analyzer() || cfg.HTTP != Defaults().HTTP || cfg.Logging != Defaults().Logging {
		t.Fatal("example does not match defaults")
	}
	path := filepath.Join(t.TempDir(), "config.toml")
	content := `[database]
socket = "/run/mysqld/mysqld.sock"
password = "test-only"
[http]
addr = "127.0.0.1:9999"
[logging]
level = "debug"
[tuning]
min_confirmations = 8
change_window = "5m"
evidence_retention = "24h"
backfill_batch_size = 10000
`
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err = Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Database.Socket != "/run/mysqld/mysqld.sock" || cfg.Database.DB != "golbat" || cfg.HTTP.Addr != "127.0.0.1:9999" || cfg.Tuning.MinConfirmations != 8 || cfg.Tuning.ChangeWindow != 5*time.Minute || cfg.Tuning.EvidenceRetention != 24*time.Hour || cfg.Tuning.BackfillBatchSize != 10000 || cfg.Logger().GetLevel() != logrus.DebugLevel {
		t.Fatal("configuration overrides were not applied", cfg)
	}
}

func TestInvalidConfig(t *testing.T) {
	for _, test := range []struct{ name, content, want string }{
		{"credentials", "[database]\npassword = \"\"\n", "database.password"},
		{"duration", "[tuning]\nchange_window = \"bad\"\n", "decode"},
		{"threshold", "[tuning]\nagreement = 0.4\n", "tuning"},
		{"batch", "[tuning]\nbackfill_batch_size = 0\n", "backfill_batch_size"},
		{"logging", "[logging]\nlevel = \"bad\"\n", "logging.level"},
		{"address", "[http]\naddr = \"bad\"\n", "http.addr"},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			base := "[database]\npassword = \"test-only\"\n"
			if test.name == "credentials" {
				base = ""
			}
			if err := os.WriteFile(path, []byte(base+test.content), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(path); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatal("unexpected validation result", err)
			}
		})
	}
}
