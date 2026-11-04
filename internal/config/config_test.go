package config

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"
)

func validEnv() map[string]string {
	key := base64.StdEncoding.EncodeToString(bytes32())
	return map[string]string{
		"DATABASE_URL":          "postgres://lkv:lkvtest@127.0.0.1:5432/lkv?sslmode=disable",
		"MASTER_KEY":            key,
		"S3_ENDPOINT":           "https://fra1.digitaloceanspaces.com",
		"S3_REGION":             "fra1",
		"S3_BUCKET":             "lkv",
		"S3_ACCESS_KEY":         "access",
		"S3_SECRET_KEY":         "secret",
		"S3_PATH_STYLE":         "true",
		"PORT":                  "9090",
		"PUBLIC_URL":            "https://git.example.com",
		"ANONYMOUS_READ":        "true",
		"DENY_NON_FAST_FORWARD": "false",
		"AUTO_CREATE":           "true",
	}
}

func bytes32() []byte {
	b := make([]byte, 32)
	for i := range b {
		b[i] = 0x11
	}
	return b
}

func mapGetenv(m map[string]string) Getenv {
	return func(k string) string { return m[k] }
}

func TestLoadServeValid(t *testing.T) {
	cfg, err := LoadServe(mapGetenv(validEnv()))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.URL != validEnv()["DATABASE_URL"] {
		t.Fatalf("url %q", cfg.URL)
	}
	if cfg.MasterKey != bytes32Array() {
		t.Fatal("master key")
	}
	if cfg.S3.Endpoint != "https://fra1.digitaloceanspaces.com" || cfg.S3.Region != "fra1" || cfg.S3.Bucket != "lkv" {
		t.Fatalf("s3 %+v", cfg.S3)
	}
	if cfg.S3.AccessKey != "access" || cfg.S3.SecretKey != "secret" || !cfg.S3.PathStyle {
		t.Fatalf("s3 credentials %+v", cfg.S3)
	}
	if cfg.Port != 9090 || cfg.PublicURL != "https://git.example.com" {
		t.Fatalf("port %d url %q", cfg.Port, cfg.PublicURL)
	}
	if !cfg.AnonymousRead || cfg.DenyNonFastForward || !cfg.AutoCreate {
		t.Fatalf("flags read %v deny %v create %v", cfg.AnonymousRead, cfg.DenyNonFastForward, cfg.AutoCreate)
	}

	env := validEnv()
	for _, k := range []string{"S3_PATH_STYLE", "PORT", "PUBLIC_URL", "ANONYMOUS_READ", "DENY_NON_FAST_FORWARD", "AUTO_CREATE"} {
		delete(env, k)
	}
	cfg, err = LoadServe(mapGetenv(env))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Port != 8080 || cfg.PublicURL != "" || cfg.S3.PathStyle || cfg.AnonymousRead || !cfg.DenyNonFastForward || cfg.AutoCreate {
		t.Fatalf("defaults port %d url %q path %v read %v deny %v create %v",
			cfg.Port, cfg.PublicURL, cfg.S3.PathStyle, cfg.AnonymousRead, cfg.DenyNonFastForward, cfg.AutoCreate)
	}
}

func bytes32Array() [32]byte {
	var k [32]byte
	copy(k[:], bytes32())
	return k
}

func TestLoadServeEverythingMissing(t *testing.T) {
	_, err := LoadServe(func(string) string { return "" })
	if err == nil {
		t.Fatal("expected error")
	}
	want := []string{"DATABASE_URL", "MASTER_KEY", "S3_ACCESS_KEY", "S3_BUCKET", "S3_ENDPOINT", "S3_REGION", "S3_SECRET_KEY"}
	text := err.Error()
	prev := 0
	for _, name := range want {
		at := strings.Index(text, name+": required")
		if at < prev {
			t.Fatalf("%s missing or out of order in %s", name, text)
		}
		prev = at + 1
	}
}

func TestLoadServeBadValues(t *testing.T) {
	short := base64.StdEncoding.EncodeToString(make([]byte, 31))
	cases := []struct {
		name string
		set  map[string]string
		want string
	}{
		{"master key", map[string]string{"MASTER_KEY": short}, "MASTER_KEY"},
		{"port zero", map[string]string{"PORT": "0"}, "PORT"},
		{"port high", map[string]string{"PORT": "65536"}, "PORT"},
		{"port text", map[string]string{"PORT": "abc"}, "PORT"},
		{"path style", map[string]string{"S3_PATH_STYLE": "yes"}, "S3_PATH_STYLE"},
		{"endpoint", map[string]string{"S3_ENDPOINT": "fra1.digitaloceanspaces.com"}, "S3_ENDPOINT"},
		{"public url", map[string]string{"PUBLIC_URL": "ftp://x"}, "PUBLIC_URL"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := validEnv()
			for k, v := range tc.set {
				env[k] = v
			}
			_, err := LoadServe(mapGetenv(env))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v", err)
			}
		})
	}
}

func TestLoadServeNoSecretInError(t *testing.T) {
	const (
		master = "SentinelMasterKeyValueShouldNotLeak"
		secret = "SentinelS3SecretValueShouldNotLeak"
		access = "SentinelS3AccessValueShouldNotLeak"
		db     = "postgres://sentinel-user:SentinelDbPassword@sentinel-host.example:5432/sentineldb?sslmode=disable"
		badDB  = "SentinelDatabaseUrlNotAUrl"
	)
	env := validEnv()
	env["MASTER_KEY"] = master
	env["S3_SECRET_KEY"] = secret
	env["S3_ACCESS_KEY"] = access
	env["DATABASE_URL"] = db
	env["PORT"] = "abc"
	_, err := LoadServe(mapGetenv(env))
	if err == nil {
		t.Fatal("expected error")
	}
	for _, s := range []string{master, secret, access, db, "SentinelDbPassword", "sentinel-host.example", "sentinel-user", "sentineldb"} {
		if strings.Contains(err.Error(), s) {
			t.Fatalf("error contains %q: %s", s, err)
		}
	}
	env["DATABASE_URL"] = badDB
	_, err = LoadServe(mapGetenv(env))
	if err == nil || strings.Contains(err.Error(), badDB) {
		t.Fatalf("got %v", err)
	}
}

func TestBackupDisabledIgnoresVariables(t *testing.T) {
	env := validEnv()
	env["BACKUP_INTERVAL"] = "soon"
	env["S3_ENDPOINT_BACKUP"] = "not-a-url"
	env["S3_REGION_BACKUP"] = "nope"
	env["S3_BUCKET_BACKUP"] = "same"
	env["S3_ACCESS_KEY_BACKUP"] = "access"
	env["S3_SECRET_KEY_BACKUP"] = "SentinelBackupSecret"
	env["S3_PATH_STYLE_BACKUP"] = "yes"
	cfg, err := LoadServe(mapGetenv(env))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Backup.Enabled || cfg.Backup.Interval != 0 || cfg.Backup.S3.Endpoint != "" || cfg.Backup.S3.Bucket != "" || cfg.Backup.S3.SecretKey != "" {
		t.Fatalf("backup was read: %+v", cfg.Backup)
	}
	env["BACKUP_ENABLED"] = "false"
	cfg, err = LoadServe(mapGetenv(env))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Backup.Enabled || cfg.Backup.Interval != 0 || cfg.Backup.S3.Bucket != "" || cfg.Backup.S3.SecretKey != "" {
		t.Fatalf("disabled backup %+v", cfg.Backup)
	}
}

func TestBackupEnabled(t *testing.T) {
	env := backupEnv()
	cfg, err := LoadStorage(mapGetenv(env))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Backup.Enabled || cfg.Backup.Interval != 6*time.Hour {
		t.Fatalf("enabled %+v", cfg.Backup)
	}
	if cfg.Backup.S3.Endpoint != "https://backup.example.com" || cfg.Backup.S3.Region != "ams3" || cfg.Backup.S3.Bucket != "lkv-backup" {
		t.Fatalf("s3 %+v", cfg.Backup.S3)
	}
	if cfg.Backup.S3.AccessKey != "baccess" || cfg.Backup.S3.SecretKey != "bsecret" || cfg.Backup.S3.PathStyle {
		t.Fatalf("path style and keys %+v", cfg.Backup.S3)
	}
	env["S3_BUCKET_BACKUP"] = "other-bucket"
	delete(env, "BACKUP_INTERVAL")
	delete(env, "S3_PATH_STYLE_BACKUP")
	cfg, err = LoadStorage(mapGetenv(env))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Backup.Interval != 0 || cfg.Backup.S3.PathStyle || cfg.Backup.S3.Bucket != "other-bucket" {
		t.Fatalf("defaults %+v", cfg.Backup)
	}
}

func TestBackupEnabledMissing(t *testing.T) {
	env := validEnv()
	env["BACKUP_ENABLED"] = "true"
	_, err := LoadServe(mapGetenv(env))
	if err == nil {
		t.Fatal("expected error")
	}
	text := err.Error()
	want := []string{"S3_ACCESS_KEY_BACKUP", "S3_BUCKET_BACKUP", "S3_ENDPOINT_BACKUP", "S3_REGION_BACKUP", "S3_SECRET_KEY_BACKUP"}
	prev := 0
	for _, name := range want {
		at := strings.Index(text, name+": required")
		if at < prev {
			t.Fatalf("%s missing or out of order in %s", name, text)
		}
		prev = at + 1
	}
	if strings.Contains(text, "bsecret") || strings.Contains(text, "https://") {
		t.Fatalf("value in error %s", text)
	}
}

func TestBackupSameBucketRefused(t *testing.T) {
	env := backupEnv()
	env["S3_ENDPOINT_BACKUP"] = env["S3_ENDPOINT"]
	env["S3_BUCKET_BACKUP"] = env["S3_BUCKET"]
	_, err := LoadServe(mapGetenv(env))
	if err == nil || !strings.Contains(err.Error(), "S3_BUCKET_BACKUP") {
		t.Fatalf("got %v", err)
	}
	env["S3_BUCKET_BACKUP"] = "other-bucket"
	if _, err := LoadServe(mapGetenv(env)); err != nil {
		t.Fatal(err)
	}
}

func TestBackupInterval(t *testing.T) {
	for _, bad := range []string{"0", "-1h", "soon"} {
		env := backupEnv()
		env["BACKUP_INTERVAL"] = bad
		_, err := LoadServe(mapGetenv(env))
		if err == nil || !strings.Contains(err.Error(), "BACKUP_INTERVAL") {
			t.Fatalf("%s: %v", bad, err)
		}
	}
	env := backupEnv()
	env["BACKUP_INTERVAL"] = "6h"
	cfg, err := LoadServe(mapGetenv(env))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Backup.Interval.String() != "6h0m0s" {
		t.Fatalf("interval %s", cfg.Backup.Interval)
	}
}

func backupEnv() map[string]string {
	env := validEnv()
	env["BACKUP_ENABLED"] = "true"
	env["BACKUP_INTERVAL"] = "6h"
	env["S3_ENDPOINT_BACKUP"] = "https://backup.example.com"
	env["S3_REGION_BACKUP"] = "ams3"
	env["S3_BUCKET_BACKUP"] = "lkv-backup"
	env["S3_ACCESS_KEY_BACKUP"] = "baccess"
	env["S3_SECRET_KEY_BACKUP"] = "bsecret"
	return env
}

func TestLoadDatabase(t *testing.T) {
	env := map[string]string{
		"DATABASE_URL": validEnv()["DATABASE_URL"],
	}
	got, err := LoadDatabase(mapGetenv(env))
	if err != nil {
		t.Fatal(err)
	}
	if got.URL != env["DATABASE_URL"] {
		t.Fatalf("url %q", got.URL)
	}
	_, err = LoadDatabase(func(string) string { return "" })
	if err == nil || !strings.Contains(err.Error(), "DATABASE_URL") || strings.Contains(err.Error(), "MASTER_KEY") {
		t.Fatalf("got %v", err)
	}
}
