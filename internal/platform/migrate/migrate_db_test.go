package migrate_test

import (
	"context"
	"strings"
	"testing"

	"github.com/shubham-kukreja/core-jobs-service/internal/platform/migrate"
	"github.com/shubham-kukreja/core-jobs-service/internal/platform/testdb"
)

func TestUpDownUp(t *testing.T) {
	db := testdb.New(t)
	ctx := context.Background()
	if err := migrate.DownAll(ctx, db.Owner); err != nil {
		t.Fatalf("down: %v", err)
	}
	if v, err := migrate.Status(ctx, db.Owner); err != nil || v != 0 {
		t.Fatalf("after down: version=%d err=%v", v, err)
	}
	if err := migrate.Up(ctx, db.Owner, discard()); err != nil {
		t.Fatalf("up again: %v", err)
	}
}

// TestRuntimePrivileges pins the least-privilege contract: the runtime role
// can never rewrite facts, delete catalog rows or run DDL.
func TestRuntimePrivileges(t *testing.T) {
	db := testdb.New(t)
	ctx := context.Background()
	mustExec(t, db, `
INSERT INTO catalog.sources(key,kind,name) VALUES ('analyst','analyst','Analyst');
INSERT INTO catalog.series(key,name,layer,value_type,frequency) VALUES ('pe.nifty100','Nifty 100 P/E','raw','num','monthly');
INSERT INTO dp.datapoints(series_id,obs_date,known_at,value_num,source_id,batch_id)
  SELECT s.id,'2026-10-31',now(),22.5,src.id,gen_random_uuid() FROM catalog.series s, catalog.sources src;
INSERT INTO dp.outbox(topic,payload) VALUES ('datapoints.written','{}');`)

	denied := []string{
		`UPDATE dp.datapoints SET value_num = 1`,
		`DELETE FROM dp.datapoints`,
		`DELETE FROM audit.audit_log`,
		`UPDATE audit.audit_log SET action = 'x'`,
		`UPDATE dp.outbox SET topic = 'x'`,
		`DELETE FROM catalog.series`,
		`DELETE FROM auth.users`,
		`CREATE TABLE public.x (i int)`,
		`CREATE TABLE dp.x (i int)`,
		`TRUNCATE dp.datapoints`,
	}
	for _, q := range denied {
		if _, err := db.Runtime.Exec(ctx, q); err == nil || !strings.Contains(err.Error(), "permission denied") {
			t.Errorf("%s: want permission denied, got %v", q, err)
		}
	}

	allowed := []string{
		`SELECT count(*) FROM dp.datapoints`,
		`UPDATE dp.outbox SET dispatched_at = now()`,
		`INSERT INTO audit.audit_log(actor_kind,entity,entity_id,action) VALUES ('system','test','1','create')`,
		`SELECT count(*) FROM river.river_job`,
	}
	for _, q := range allowed {
		if _, err := db.Runtime.Exec(ctx, q); err != nil {
			t.Errorf("%s: %v", q, err)
		}
	}
}

// TestAppendOnlyForOwner checks history is protected even from the owner role.
func TestAppendOnlyForOwner(t *testing.T) {
	db := testdb.New(t)
	ctx := context.Background()
	mustExec(t, db, `INSERT INTO audit.audit_log(actor_kind,entity,entity_id,action) VALUES ('system','test','1','create')`)
	for _, q := range []string{`UPDATE audit.audit_log SET action = 'x'`, `DELETE FROM audit.audit_log`, `TRUNCATE audit.audit_log`} {
		if _, err := db.Owner.Exec(ctx, q); err == nil || !strings.Contains(err.Error(), "append-only") {
			t.Errorf("%s: want append-only error, got %v", q, err)
		}
	}
}

func TestConstraints(t *testing.T) {
	db := testdb.New(t)
	ctx := context.Background()
	rejected := map[string]string{
		"secret in source config": `INSERT INTO catalog.sources(key,kind,name,config) VALUES ('fred','external_api','FRED','{"api_key":"x"}')`,
		"malformed key":           `INSERT INTO catalog.series(key,name,layer,value_type,frequency) VALUES ('Bad Key','x','raw','num','monthly')`,
		"score without formula":   `INSERT INTO catalog.series(key,name,layer,value_type,frequency) VALUES ('equity.mid::relvalue','x','score','num','monthly')`,
		"raw with formula":        `INSERT INTO catalog.series(key,name,layer,value_type,frequency,derivation,derivation_ver) VALUES ('pe.x','x','raw','num','monthly','{}',1)`,
		"plaintext password":      `INSERT INTO auth.users(email,name,role) VALUES ('a@b.co','A','admin'); INSERT INTO auth.password_credentials(user_id,hash) SELECT id,'hunter2' FROM auth.users`,
		"short session hash":      `INSERT INTO auth.users(email,name,role) VALUES ('c@d.co','C','admin'); INSERT INTO auth.sessions(token_hash,user_id,csrf_hash,idle_expires_at,expires_at) SELECT '\x01',id,'\x01',now(),now() FROM auth.users WHERE email='c@d.co'`,
	}
	for name, q := range rejected {
		t.Run(name, func(t *testing.T) {
			if _, err := db.Owner.Exec(ctx, q); err == nil {
				t.Fatal("expected constraint violation")
			}
		})
	}
	mustExec(t, db, `INSERT INTO catalog.series(key,name,layer,value_type,frequency,derivation,derivation_ver)
VALUES ('equity.large::relvalue','Large Cap - Relative value','score','num','monthly','{"fn":"const"}',1)`)
}

func mustExec(t *testing.T, db *testdb.DB, sql string) {
	t.Helper()
	if _, err := db.Owner.Exec(context.Background(), sql); err != nil {
		t.Fatalf("exec: %v", err)
	}
}
