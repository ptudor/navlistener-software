package control

import (
	"context"
	"strings"
	"testing"
)

func TestSTIdentitySchemaUpgrade(t *testing.T) {
	db := testDatabase(t)
	ctx := context.Background()
	// Start with the original two-kind schema, including PostgreSQL check names.
	old := strings.Split(schema, "-- Upgrade typed identity")[0]
	old = strings.ReplaceAll(old, ",'st_uid128'", "")
	old = strings.ReplaceAll(old, " WHEN 'st_uid128' THEN '0004'", "")
	conn, err := db.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, err = conn.Conn().PgConn().Exec(ctx, old).ReadAll()
	conn.Release()
	if err != nil {
		t.Fatal(err)
	}
	b := newBench(t)
	b.service.DB = db
	for i := 0; i < 2; i++ {
		if err := b.service.Initialize(ctx, b.config); err != nil {
			t.Fatal(err)
		}
	}
	insert := `INSERT INTO navl_devices(observer_id,manufacturer_authority_id,board_uid,
        board_uid_kind,atecc_serial,core_record,commissioning_record,current_enrollment_id)
        VALUES($1,'ab',$2,$3,'0123456789abcdef01',$4,$5,'test')`
	uid := "20e00eff0123456789abcdef01234567"
	for _, tc := range []struct{ observer, value, kind string }{
		{"board-0003-" + uid, uid, "st_uid128"},
		{"board-0004-" + uid[:16], uid[:16], "st_uid128"},
		{"board-0005-" + uid, uid, "future_uid"},
	} {
		if _, err := db.Exec(ctx, insert, tc.observer, tc.value, tc.kind, make([]byte, 72), make([]byte, 252)); err == nil {
			t.Fatal("accepted invalid typed identity", tc)
		}
	}
	if _, err := db.Exec(ctx, insert, "board-0004-"+uid, uid, "st_uid128", make([]byte, 72), make([]byte, 252)); err != nil {
		t.Fatal(err)
	}
}
