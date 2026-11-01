package config

import (
	"fmt"
	"strings"
	"testing"
)

func env(m map[string]string) lookupFunc {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

func validEnv() map[string]string {
	return map[string]string{
		"DATABASE_URL": "postgres://app:pw@localhost/app",
		"SECRET_KEY":   strings.Repeat("k", 32),
	}
}

func TestLoadDefaults(t *testing.T) {
	c, err := load(env(validEnv()))
	if err != nil {
		t.Fatal(err)
	}
	if c.Env != EnvDevelopment || c.HTTPAddr != ":8080" || c.Timezone.String() != "Asia/Kolkata" {
		t.Fatalf("unexpected defaults: %+v", c)
	}
	if c.MigrationDatabaseURL != c.DatabaseURL {
		t.Fatal("migration URL should default to runtime URL")
	}
}

func TestLoadRejectsInvalid(t *testing.T) {
	cases := map[string]map[string]string{
		"missing db":                {"SECRET_KEY": strings.Repeat("k", 32)},
		"short secret":              {"DATABASE_URL": "x", "SECRET_KEY": "short"},
		"wildcard cors":             {"DATABASE_URL": "x", "SECRET_KEY": strings.Repeat("k", 32), "CORS_ORIGINS": "*"},
		"http cors in prd":          {"DATABASE_URL": "x", "SECRET_KEY": strings.Repeat("k", 32), "APP_ENV": "production", "CORS_ORIGINS": "http://a.com"},
		"bad env":                   {"DATABASE_URL": "x", "SECRET_KEY": strings.Repeat("k", 32), "APP_ENV": "prod"},
		"prod without verified tls": {"DATABASE_URL": "postgres://u:p@db/app?sslmode=require", "SECRET_KEY": strings.Repeat("k", 32), "APP_ENV": "production"},
		"bad ttl":                   {"DATABASE_URL": "x", "SECRET_KEY": strings.Repeat("k", 32), "SESSION_IDLE_TTL": "2h", "SESSION_ABSOLUTE_TTL": "1h"},
	}
	for name, m := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := load(env(m)); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestProductionAcceptsVerifiedTLS(t *testing.T) {
	_, err := load(env(map[string]string{
		"DATABASE_URL": "postgres://u:p@db/app?sslmode=verify-full",
		"SECRET_KEY":   strings.Repeat("k", 32),
		"APP_ENV":      "production",
	}))
	if err != nil {
		t.Fatal(err)
	}
}

func TestSecretRedacted(t *testing.T) {
	c, err := load(env(validEnv()))
	if err != nil {
		t.Fatal(err)
	}
	out := fmt.Sprintf("%v %+v %#v %s", c, c, c.SecretKey, c.DatabaseURL)
	if strings.Contains(out, "kkkk") || strings.Contains(out, "pw@") {
		t.Fatalf("secret leaked: %s", out)
	}
}
