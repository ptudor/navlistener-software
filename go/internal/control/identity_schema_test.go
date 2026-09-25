package control

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// v1Devices is the navl_devices table as first released: two identity kinds,
// with PostgreSQL's unnamed check constraints.
const v1Devices = `CREATE TABLE IF NOT EXISTS navl_devices (
    observer_id text PRIMARY KEY,
    manufacturer_authority_id text,
    board_uid text CHECK(board_uid ~ '^[0-9a-f]+$'),
    board_uid_kind text CHECK(board_uid_kind IN ('microchip_eui64','microchip_cs128')),
    UNIQUE(board_uid_kind,board_uid),
    CHECK ((board_uid IS NULL) = (board_uid_kind IS NULL)),
    CHECK ((board_uid_kind='microchip_eui64' AND length(board_uid)=16) OR (board_uid_kind IN ('microchip_cs128') AND length(board_uid)=32) OR board_uid_kind IS NULL),
    atecc_serial text UNIQUE CHECK(atecc_serial ~ '^[0-9a-f]{18}$'),
    rtc_eui64 text UNIQUE CHECK(rtc_eui64 ~ '^[0-9a-f]{16}$'),
    rtc_model_id integer NOT NULL DEFAULT 0 CHECK(rtc_model_id BETWEEN 0 AND 65535),
    hardware_product integer NOT NULL DEFAULT 0 CHECK(hardware_product BETWEEN 0 AND 65535),
    hardware_revision integer NOT NULL DEFAULT 0 CHECK(hardware_revision BETWEEN 0 AND 65535),
    core_record bytea, core_attestation_fingerprint text NOT NULL DEFAULT '',
    core_signer_spki text NOT NULL DEFAULT '',
    commissioning_record bytea, commissioning_generation bigint NOT NULL DEFAULT 0,
    commissioning_signer_spki text NOT NULL DEFAULT '',
    current_enrollment_id text NOT NULL,
    CHECK ((manufacturer_authority_id IS NULL) = (board_uid IS NULL)),
    CHECK ((board_uid IS NULL) = (atecc_serial IS NULL)),
    CHECK ((board_uid IS NULL AND core_record IS NULL AND commissioning_record IS NULL AND rtc_eui64 IS NULL AND rtc_model_id=0)
        OR (board_uid IS NOT NULL AND observer_id='board-' || CASE board_uid_kind WHEN 'microchip_eui64' THEN '0001' WHEN 'microchip_cs128' THEN '0003' END || '-' || board_uid
        AND core_record IS NOT NULL AND octet_length(core_record)=72
        AND commissioning_record IS NOT NULL AND octet_length(commissioning_record)=252))
);
`

// v2Upgrade is the released block that named those constraints and added ST.
const v2Upgrade = `DO $upgrade$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conrelid='navl_devices'::regclass
                   AND conname='navl_devices_uid_v2_kind') THEN
        ALTER TABLE navl_devices
            DROP CONSTRAINT navl_devices_board_uid_kind_check,
            DROP CONSTRAINT navl_devices_check1,
            DROP CONSTRAINT navl_devices_check4,
            ADD CONSTRAINT navl_devices_uid_v2_kind CHECK
                (board_uid_kind IN ('microchip_eui64','microchip_cs128','st_uid128')),
            ADD CONSTRAINT navl_devices_uid_v2_length CHECK
                ((board_uid_kind='microchip_eui64' AND length(board_uid)=16)
                 OR (board_uid_kind IN ('microchip_cs128','st_uid128') AND length(board_uid)=32)
                 OR board_uid_kind IS NULL),
            ADD CONSTRAINT navl_devices_uid_v2_evidence CHECK
                ((board_uid IS NULL AND core_record IS NULL AND commissioning_record IS NULL
                  AND rtc_eui64 IS NULL AND rtc_model_id=0)
                 OR (board_uid IS NOT NULL AND observer_id='board-' || CASE board_uid_kind
                     WHEN 'microchip_eui64' THEN '0001' WHEN 'microchip_cs128' THEN '0003'
                     WHEN 'st_uid128' THEN '0004' END || '-' || board_uid
                     AND core_record IS NOT NULL AND octet_length(core_record)=72
                     AND commissioning_record IS NOT NULL AND octet_length(commissioning_record)=252));
    END IF;
END $upgrade$;
`

// v3Upgrade is the released block that named the kinds after esp_hardware_discovery.
const v3Upgrade = `DO $upgrade$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conrelid='navl_devices'::regclass
                   AND conname='navl_devices_uid_v3_kind') THEN
        ALTER TABLE navl_devices
            DROP CONSTRAINT IF EXISTS navl_devices_board_uid_kind_check,
            DROP CONSTRAINT IF EXISTS navl_devices_check1,
            DROP CONSTRAINT IF EXISTS navl_devices_check4,
            DROP CONSTRAINT IF EXISTS navl_devices_uid_v2_kind,
            DROP CONSTRAINT IF EXISTS navl_devices_uid_v2_length,
            DROP CONSTRAINT IF EXISTS navl_devices_uid_v2_evidence;
        UPDATE navl_devices SET board_uid_kind = CASE board_uid_kind
            WHEN 'microchip_eui64' THEN 'eui64' WHEN 'microchip_cs128' THEN 'serial128' END
            WHERE board_uid_kind IN ('microchip_eui64','microchip_cs128');
        ALTER TABLE navl_devices
            ADD CONSTRAINT navl_devices_uid_v3_kind CHECK
                (board_uid_kind IN ('eui64','serial128','st_uid128')),
            ADD CONSTRAINT navl_devices_uid_v3_length CHECK
                ((board_uid_kind='eui64' AND length(board_uid)=16)
                 OR (board_uid_kind IN ('serial128','st_uid128') AND length(board_uid)=32)
                 OR board_uid_kind IS NULL),
            ADD CONSTRAINT navl_devices_uid_v3_evidence CHECK
                ((board_uid IS NULL AND core_record IS NULL AND commissioning_record IS NULL
                  AND rtc_eui64 IS NULL AND rtc_model_id=0)
                 OR (board_uid IS NOT NULL AND observer_id='board-' || CASE board_uid_kind
                     WHEN 'eui64' THEN '0001' WHEN 'serial128' THEN '0003'
                     WHEN 'st_uid128' THEN '0004' END || '-' || board_uid
                     AND core_record IS NOT NULL AND octet_length(core_record)=72
                     AND commissioning_record IS NOT NULL AND octet_length(commissioning_record)=252));
    END IF;
END $upgrade$;
`

const insertDevice = `INSERT INTO navl_devices(observer_id,manufacturer_authority_id,board_uid,
        board_uid_kind,atecc_serial,core_record,commissioning_record,current_enrollment_id)
        VALUES($1,'ab',$2,$3,$4,$5,$6,'test')`

// historicalSchema is today's schema up to its upgrade block, with navl_devices
// replaced by the released table and the released upgrades through version applied.
func historicalSchema(t *testing.T, version int) string {
	t.Helper()
	prefix := strings.Split(schema, "-- Upgrade typed identity")[0]
	start := strings.Index(prefix, "CREATE TABLE IF NOT EXISTS navl_devices (")
	if start < 0 {
		t.Fatal("navl_devices table not found in schema")
	}
	end := strings.Index(prefix[start:], "\n);\n")
	if end < 0 {
		t.Fatal("navl_devices table end not found in schema")
	}
	old := prefix[:start] + v1Devices + prefix[start+end+len("\n);\n"):]
	if version >= 2 {
		old += v2Upgrade
	}
	if version >= 3 {
		old += v3Upgrade
	}
	return old
}

func insertIdentity(ctx context.Context, db *pgxpool.Pool, observer, uid, kind string, n int) error {
	_, err := db.Exec(ctx, insertDevice, observer, uid, kind, fmt.Sprintf("0123456789abcdef%02x", n),
		make([]byte, 72), make([]byte, 252))
	return err
}

// assertIdentityConstraints checks the v4 constraint set and what it admits.
func assertIdentityConstraints(t *testing.T, db *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	rows, err := db.Query(ctx, `SELECT conname FROM pg_constraint WHERE conrelid='navl_devices'::regclass
        AND (conname LIKE 'navl_devices_uid_%' OR conname IN ('navl_devices_board_uid_kind_check'))`)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	rows.Close()
	sort.Strings(names)
	if want := []string{"navl_devices_uid_v4_evidence", "navl_devices_uid_v4_kind", "navl_devices_uid_v4_length"}; strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("identity constraints %v, want %v", names, want)
	}
	var rtcUnique bool
	if err := db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_constraint WHERE conrelid='navl_devices'::regclass
        AND contype='u' AND conkey=ARRAY[(SELECT attnum FROM pg_attribute WHERE attrelid='navl_devices'::regclass
        AND attname='rtc_eui64')])`).Scan(&rtcUnique); err != nil || rtcUnique {
		t.Fatalf("recorded RTC EUI-64 is still unique: %v", err)
	}
	uid := "fedcba9876543210fedcba9876543210"
	for i, tc := range []struct{ observer, value, kind string }{
		{"board-0003-" + uid, uid, "microchip_cs128"},
		{"board-0001-" + uid[:16], uid[:16], "microchip_eui64"},
		{"board-0001-" + uid[:16], uid[:16], "eui64"},
		{"board-0001-" + uid, uid, "eui64"},
		{"board-0004-" + uid, uid, "serial128"},
		{"board-0003-" + uid[:16], uid[:16], "serial128"},
		{"board-0005-" + uid, uid, "future_uid"},
	} {
		if err := insertIdentity(ctx, db, tc.observer, tc.value, tc.kind, 0x40+i); err == nil {
			t.Fatalf("accepted invalid typed identity %v", tc)
		}
	}
	for i, tc := range []struct{ observer, value, kind string }{
		{"board-0003-" + uid, uid, "serial128"},
		{"board-0004-" + "20e00eff" + uid[8:], "20e00eff" + uid[8:], "st_uid128"},
	} {
		if err := insertIdentity(ctx, db, tc.observer, tc.value, tc.kind, 0x50+i); err != nil {
			t.Fatalf("refused %v: %v", tc, err)
		}
	}
}

func TestIdentitySchemaUpgrade(t *testing.T) {
	serial, st := "0123456789abcdef0123456789abcdef", "20e00eff0123456789abcdef01234567"
	codes := map[string]string{"microchip_cs128": "0003", "serial128": "0003", "st_uid128": "0004"}
	for _, tc := range []struct {
		name    string
		version int
		rows    [][2]string // released kind name, value
		want    []string    // kind after the upgrade, same order
	}{
		{"v1", 1, [][2]string{{"microchip_cs128", serial}}, []string{"serial128"}},
		{"v2", 2, [][2]string{{"microchip_cs128", serial}, {"st_uid128", st}}, []string{"serial128", "st_uid128"}},
		{"v3", 3, [][2]string{{"serial128", serial}, {"st_uid128", st}}, []string{"serial128", "st_uid128"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := historicalDatabase(t, tc.version)
			ctx := context.Background()
			for i, row := range tc.rows {
				if err := insertIdentity(ctx, db, "board-"+codes[row[0]]+"-"+row[1], row[1], row[0], i); err != nil {
					t.Fatalf("released schema refused %s: %v", row[0], err)
				}
			}
			b := newBench(t)
			b.service.DB = db
			for i := 0; i < 2; i++ {
				if err := b.service.Initialize(ctx, b.config); err != nil {
					t.Fatal(err)
				}
			}
			for i, row := range tc.rows {
				var kind string
				if err := db.QueryRow(ctx, `SELECT board_uid_kind FROM navl_devices WHERE board_uid=$1`, row[1]).Scan(&kind); err != nil {
					t.Fatal(err)
				}
				if kind != tc.want[i] {
					t.Fatalf("%s became %q, want %q", row[0], kind, tc.want[i])
				}
			}
			assertIdentityConstraints(t, db)
		})
	}
}

// A board identity is never 64 bits. A database that still holds one refuses the
// upgrade and names the devices, rather than keeping or silently dropping them.
func TestIdentitySchemaUpgradeRefuses64BitIdentities(t *testing.T) {
	eui := "0004a3aabbccddee"
	for version, kind := range map[int]string{1: "microchip_eui64", 2: "microchip_eui64", 3: "eui64"} {
		t.Run(fmt.Sprintf("v%d", version), func(t *testing.T) {
			db := historicalDatabase(t, version)
			ctx := context.Background()
			observer := "board-0001-" + eui
			if err := insertIdentity(ctx, db, observer, eui, kind, 0); err != nil {
				t.Fatalf("released schema refused %s: %v", kind, err)
			}
			b := newBench(t)
			b.service.DB = db
			err := b.service.Initialize(ctx, b.config)
			if err == nil || !strings.Contains(err.Error(), "64-bit board identities") || !strings.Contains(err.Error(), observer) {
				t.Fatalf("upgrade with a 64-bit identity: %v", err)
			}
			var kept string
			if err := db.QueryRow(ctx, `SELECT board_uid_kind FROM navl_devices WHERE observer_id=$1`, observer).Scan(&kept); err != nil || kept != kind {
				t.Fatalf("refused upgrade changed the row: %q %v", kept, err)
			}
		})
	}
}

func historicalDatabase(t *testing.T, version int) *pgxpool.Pool {
	t.Helper()
	db := testDatabase(t)
	ctx := context.Background()
	conn, err := db.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, err = conn.Conn().PgConn().Exec(ctx, historicalSchema(t, version)).ReadAll()
	conn.Release()
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func TestFreshIdentitySchema(t *testing.T) {
	db := testDatabase(t)
	b := newBench(t)
	b.service.DB = db
	for i := 0; i < 2; i++ {
		if err := b.service.Initialize(context.Background(), b.config); err != nil {
			t.Fatal(err)
		}
	}
	assertIdentityConstraints(t, db)
}
