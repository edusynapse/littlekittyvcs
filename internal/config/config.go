// Package config reads the process environment once at startup.
// Error text names variables and never includes a value.
package config

import (
	"errors"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/edusynapse/littlekittyvcs/internal/blob"
	"github.com/edusynapse/littlekittyvcs/internal/objcrypt"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Database is the PostgreSQL connection.
type Database struct {
	URL string
}

// Backup is the optional second bucket. Enabled false means the other fields
// are zero and were not read from the environment.
type Backup struct {
	Enabled  bool
	Interval time.Duration // zero when BACKUP_INTERVAL is unset
	S3       blob.S3Config
}

// Storage is what restore needs: the database, the master key, and the bucket.
// It has no String or Format method. Callers log individual non-secret fields.
type Storage struct {
	Database
	MasterKey [32]byte
	S3        blob.S3Config
	Backup    Backup
}

// Serve is every setting the server needs.
// It has no String or Format method. Callers log individual non-secret fields.
type Serve struct {
	Storage
	Port               int
	PublicURL          string
	AnonymousRead      bool
	DenyNonFastForward bool
	AutoCreate         bool
}

// Getenv is os.Getenv in production and a map lookup in tests.
type Getenv func(string) string

// LoadDatabase reads DATABASE_URL.
func LoadDatabase(getenv Getenv) (Database, error) {
	var probs problems
	url := databaseURL(getenv, &probs)
	if err := probs.err(); err != nil {
		return Database{}, err
	}
	return Database{URL: url}, nil
}

// LoadStorage reads the database, the master key, and the bucket.
// One error lists every problem, sorted by variable name.
func LoadStorage(getenv Getenv) (Storage, error) {
	var probs problems
	cfg := fillStorage(getenv, &probs)
	if err := probs.err(); err != nil {
		return Storage{}, err
	}
	return cfg, nil
}

// LoadServe reads every setting. One error lists every problem, sorted by variable name.
func LoadServe(getenv Getenv) (Serve, error) {
	var probs problems
	cfg := Serve{Storage: fillStorage(getenv, &probs)}
	cfg.Port = port(getenv, &probs)
	cfg.PublicURL = httpEndpoint(getenv, "PUBLIC_URL", false, &probs)
	cfg.AnonymousRead = boolean(getenv, "ANONYMOUS_READ", false, &probs)
	cfg.DenyNonFastForward = boolean(getenv, "DENY_NON_FAST_FORWARD", true, &probs)
	cfg.AutoCreate = boolean(getenv, "AUTO_CREATE", false, &probs)
	if err := probs.err(); err != nil {
		return Serve{}, err
	}
	return cfg, nil
}

func fillStorage(getenv Getenv, probs *problems) Storage {
	var cfg Storage
	cfg.URL = databaseURL(getenv, probs)
	if v := getenv("MASTER_KEY"); v == "" {
		probs.add("MASTER_KEY", "required")
	} else if key, err := objcrypt.ParseMasterKey(v); err != nil {
		probs.add("MASTER_KEY", "must be base64 of exactly 32 bytes")
	} else {
		cfg.MasterKey = key
	}
	cfg.S3.Endpoint = httpEndpoint(getenv, "S3_ENDPOINT", true, probs)
	cfg.S3.Region = required(getenv, "S3_REGION", probs)
	cfg.S3.Bucket = required(getenv, "S3_BUCKET", probs)
	cfg.S3.AccessKey = required(getenv, "S3_ACCESS_KEY", probs)
	cfg.S3.SecretKey = required(getenv, "S3_SECRET_KEY", probs)
	cfg.S3.PathStyle = boolean(getenv, "S3_PATH_STYLE", false, probs)
	cfg.Backup = fillBackup(getenv, cfg.S3, probs)
	return cfg
}

// fillBackup reads the backup bucket only when BACKUP_ENABLED is true.
// Otherwise the _BACKUP variables and BACKUP_INTERVAL are left unread.
func fillBackup(getenv Getenv, primary blob.S3Config, probs *problems) Backup {
	var b Backup
	b.Enabled = boolean(getenv, "BACKUP_ENABLED", false, probs)
	if !b.Enabled {
		return b
	}
	b.S3.Endpoint = httpEndpoint(getenv, "S3_ENDPOINT_BACKUP", true, probs)
	b.S3.Region = required(getenv, "S3_REGION_BACKUP", probs)
	b.S3.Bucket = required(getenv, "S3_BUCKET_BACKUP", probs)
	b.S3.AccessKey = required(getenv, "S3_ACCESS_KEY_BACKUP", probs)
	b.S3.SecretKey = required(getenv, "S3_SECRET_KEY_BACKUP", probs)
	b.S3.PathStyle = boolean(getenv, "S3_PATH_STYLE_BACKUP", false, probs)
	if v := getenv("BACKUP_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			probs.add("BACKUP_INTERVAL", "must be a duration greater than zero")
		} else {
			b.Interval = d
		}
	}
	if b.S3.Endpoint != "" && b.S3.Bucket != "" &&
		b.S3.Endpoint == primary.Endpoint && b.S3.Bucket == primary.Bucket {
		probs.add("S3_BUCKET_BACKUP", "must not be the same bucket as the primary")
	}
	return b
}

func databaseURL(getenv Getenv, probs *problems) string {
	v := getenv("DATABASE_URL")
	if v == "" {
		probs.add("DATABASE_URL", "required")
		return ""
	}
	// pgx quotes the connection string in a parse error, with the password masked
	// and the host, user, and database name still visible. The reason stays fixed.
	if _, err := pgxpool.ParseConfig(v); err != nil {
		probs.add("DATABASE_URL", "not a valid PostgreSQL connection string")
		return ""
	}
	return v
}

func required(getenv Getenv, name string, probs *problems) string {
	v := getenv(name)
	if v == "" {
		probs.add(name, "required")
	}
	return v
}

func boolean(getenv Getenv, name string, fallback bool, probs *problems) bool {
	v := getenv(name)
	if v == "" {
		return fallback
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		probs.add(name, "must be a boolean")
		return fallback
	}
	return b
}

func port(getenv Getenv, probs *problems) int {
	v := getenv("PORT")
	if v == "" {
		return 8080
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 || n > 65535 {
		probs.add("PORT", "must be an integer from 1 to 65535")
		return 0
	}
	return n
}

func httpEndpoint(getenv Getenv, name string, required bool, probs *problems) string {
	v := getenv(name)
	if v == "" {
		if required {
			probs.add(name, "required")
		}
		return ""
	}
	if !absoluteHTTP(v) {
		probs.add(name, "must be an absolute http or https URL with a host")
		return ""
	}
	return v
}

func absoluteHTTP(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}
	return u.Host != ""
}

type problem struct {
	name   string
	reason string
}

type problems struct {
	list []problem
}

func (p *problems) add(name, reason string) {
	p.list = append(p.list, problem{name: name, reason: reason})
}

func (p *problems) err() error {
	if len(p.list) == 0 {
		return nil
	}
	sort.Slice(p.list, func(i, j int) bool { return p.list[i].name < p.list[j].name })
	var b strings.Builder
	b.WriteString("config: ")
	for i, item := range p.list {
		if i > 0 {
			b.WriteString("; ")
		}
		b.WriteString(item.name)
		b.WriteString(": ")
		b.WriteString(item.reason)
	}
	return errors.New(b.String())
}
