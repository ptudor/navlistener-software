-- NavListen control plane v1. This is a fresh schema, with no prototype import.
CREATE TABLE IF NOT EXISTS navl_authorities (
    kind text NOT NULL CHECK (kind IN ('operational','manufacturer')),
    id text NOT NULL, enabled boolean NOT NULL, configuration jsonb NOT NULL,
    PRIMARY KEY(kind,id)
);
CREATE TABLE IF NOT EXISTS navl_authority_keys (
    spki text PRIMARY KEY CHECK (spki ~ '^[0-9a-f]{64}$'),
    kind text NOT NULL, authority_id text NOT NULL,
    role text NOT NULL CHECK(role IN ('issuing','manufacturer','registry')),
    FOREIGN KEY(kind,authority_id) REFERENCES navl_authorities(kind,id)
);
CREATE TABLE IF NOT EXISTS navl_authority_pairings (
    operational_authority_id text NOT NULL, manufacturer_authority_id text NOT NULL,
    PRIMARY KEY(operational_authority_id,manufacturer_authority_id)
);
CREATE TABLE IF NOT EXISTS navl_registry_floors (
    manufacturer_authority_id text PRIMARY KEY,
    sequence numeric(20,0) NOT NULL CHECK(sequence > 0)
);
CREATE TABLE IF NOT EXISTS navl_devices (
    observer_id text PRIMARY KEY,
    manufacturer_authority_id text,
    board_uid text CHECK(board_uid ~ '^[0-9a-f]+$'),
    board_uid_kind text CHECK(board_uid_kind IN ('microchip_eui64','microchip_cs128')),
    UNIQUE(board_uid_kind,board_uid),
    CHECK ((board_uid IS NULL) = (board_uid_kind IS NULL)),
    CHECK ((board_uid_kind='microchip_eui64' AND length(board_uid)=16) OR (board_uid_kind='microchip_cs128' AND length(board_uid)=32) OR board_uid_kind IS NULL),
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
CREATE TABLE IF NOT EXISTS navl_enrollments (
    id text PRIMARY KEY,
    observer_id text NOT NULL REFERENCES navl_devices(observer_id),
    operational_authority_id text NOT NULL,
    manufacturer_authority_id text,
    organization_id text NOT NULL, collector_instance_id text NOT NULL,
    token_sha256 text NOT NULL UNIQUE CHECK(token_sha256 ~ '^[0-9a-f]{64}$'),
    issuer_spki text NOT NULL DEFAULT '',
    credential_fingerprint text NOT NULL DEFAULT '',
    credential_tier text NOT NULL CHECK(credential_tier IN ('token','software_mtls','hardware_mtls')),
    certificate_pem text NOT NULL DEFAULT '', certificate_expires timestamptz,
    attestation_tier text NOT NULL CHECK(attestation_tier IN ('none','verified_v1_core')),
    snapshot jsonb NOT NULL,
    core_record bytea, commissioning_record bytea,
    registry_sequence numeric(20,0) NOT NULL CHECK(registry_sequence >= 0),
    registry_signer_spki text NOT NULL,
    feed_grants text[] NOT NULL,
    collection_ids text[] NOT NULL DEFAULT '{}',
    declared_capabilities text[] NOT NULL DEFAULT '{}',
    aggregate_use text NOT NULL, station_metadata text NOT NULL,
    event_visibility text NOT NULL, raw_export text NOT NULL,
    federation_peers text[] NOT NULL DEFAULT '{}', publish_signals text[] NOT NULL DEFAULT '{}',
    policy_revision text NOT NULL,
    active boolean NOT NULL DEFAULT true,
    created_at timestamptz NOT NULL DEFAULT now(), revoked_at timestamptz
);
CREATE UNIQUE INDEX IF NOT EXISTS navl_current_enrollment ON navl_enrollments(observer_id) WHERE active;
CREATE TABLE IF NOT EXISTS navl_service_events (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    observer_id text NOT NULL, enrollment_id text NOT NULL,
    kind text NOT NULL, operator_id text NOT NULL,
    recorded_at timestamptz NOT NULL DEFAULT now(), detail jsonb NOT NULL
);
-- Read credentials are administered separately from observer enrollment. No
-- observer or operator API bearer token implicitly grants historical reads.
CREATE TABLE IF NOT EXISTS navl_read_credentials (
    token_sha256 text PRIMARY KEY CHECK(token_sha256 ~ '^[0-9a-f]{64}$'),
    principal_id text NOT NULL, audience_grants text[] NOT NULL,
    revision text NOT NULL, enabled boolean NOT NULL DEFAULT false
);
CREATE OR REPLACE VIEW navlistener_read_authorization_v1 AS
SELECT token_sha256,principal_id,audience_grants,revision,enabled FROM navl_read_credentials;
CREATE OR REPLACE VIEW navlistener_observer_authorization_v3 AS
SELECT e.token_sha256, e.observer_id, e.organization_id, e.id AS enrollment_id,
 e.collector_instance_id, e.operational_authority_id, e.manufacturer_authority_id,
 e.issuer_spki, e.snapshot->>'CoreSignerSPKI' AS core_signer_spki,
 e.snapshot->>'CoreAttestationFingerprint' AS core_attestation_fingerprint,
 (e.snapshot->>'HardwareProduct')::integer AS hardware_product,
 (e.snapshot->>'HardwareRevision')::integer AS hardware_revision,
 e.collection_ids, e.feed_grants, e.declared_capabilities,
 e.credential_tier, e.credential_fingerprint, e.attestation_tier,
 e.aggregate_use, e.station_metadata, e.event_visibility, e.raw_export,
 e.federation_peers, e.publish_signals, e.policy_revision, true AS enabled
FROM navl_enrollments e
JOIN navl_authorities o ON o.kind='operational' AND o.id=e.operational_authority_id AND o.enabled
LEFT JOIN navl_authorities m ON m.kind='manufacturer' AND m.id=e.manufacturer_authority_id
WHERE e.active AND (e.certificate_expires IS NULL OR e.certificate_expires > now())
 AND (e.manufacturer_authority_id IS NULL OR (m.enabled AND EXISTS (
 SELECT 1 FROM navl_authority_pairings p WHERE p.operational_authority_id=e.operational_authority_id
 AND p.manufacturer_authority_id=e.manufacturer_authority_id)));
