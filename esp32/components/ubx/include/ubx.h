// ubx — a u-blox UBX framer that turns raw receiver bytes into GNF1 records.
//
// Clean-room, authored from the u-blox interface description and the byte-for-byte
// counterpart ../../../feeder/navfeeder.c (our own Apache-2.0 code). It resynchronises on the
// 0xB5 0x62 sync, validates the Fletcher-8 checksum, and emits one GNF1 record per message of
// interest via a callback:
//   UBX-RXM-SFRBX  -> a raw-nav record (nav words re-serialized BIG-ENDIAN)
//   UBX-MON-RF     -> a JammingStats telemetry record (0x05)
//   UBX-MON-HW     -> a JammingStats telemetry record (0x05, single band)
//   UBX-NAV-SAT    -> a ReceptionData telemetry record (0x01)
// The edge does NOT decode nav content — it frames and forwards (docs/DESIGN.md §1).
//
// Push model: feed bytes as they arrive off the UART; complete messages fire the callback.
// I/O-free, so it is host-testable and differentially checkable against navfeeder.c over the
// same capture. Every length and index is bounds-checked (docs/INTEGRITY.md §9).

#ifndef UBX_H
#define UBX_H

#include <stddef.h>
#include <stdint.h>

#include "gnf1.h"

#ifdef __cplusplus
extern "C" {
#endif

// Largest UBX payload we buffer. SFRBX/MON-RF/MON-HW/NAV-SAT all fit well under this; a
// message declaring a larger length is skipped (we don't forward it) and the framer resyncs.
#define UBX_MAX_PAYLOAD 4096

// ubx_emit_fn receives one complete GNF1 record (GNF1_RECORD_HDR + body). It must copy the
// bytes if they outlive the call (they alias the parser's scratch buffer). ctx is opaque.
typedef void (*ubx_emit_fn)(const uint8_t *record, size_t record_len, void *ctx);

// ubx_now_fn returns the reception timestamp (unix nanoseconds) to stamp into each record.
// On device, pass a wall-clock source (RTC/SNTP); in host tests, a fixed clock makes records
// deterministic for differential comparison.
typedef uint64_t (*ubx_now_fn)(void);

typedef struct {
    ubx_emit_fn emit;
    ubx_now_fn now;
    void *ctx;

    // framer state machine
    int state;
    uint8_t cls, id;
    uint16_t len, idx;
    uint8_t ck_a, ck_b;         // computed Fletcher-8 over class..payload
    uint8_t exp_a, exp_b;       // received checksum
    uint8_t payload[UBX_MAX_PAYLOAD];
    uint8_t scratch[GNF1_RECORD_MAX]; // record assembly (emit target)

    // counters (for the display/telemetry)
    uint32_t frames_nav;        // SFRBX records emitted
    uint32_t frames_telem;      // MON-RF/MON-HW/NAV-SAT records emitted
    uint32_t bad_checksum;      // messages dropped on checksum
    uint32_t oversize;          // messages skipped for exceeding UBX_MAX_PAYLOAD
} ubx_parser_t;

// ubx_parser_init resets the framer and binds the emit + clock callbacks.
void ubx_parser_init(ubx_parser_t *p, ubx_emit_fn emit, ubx_now_fn now, void *ctx);

// ubx_parser_feed drives the framer over a chunk of receiver bytes.
void ubx_parser_feed(ubx_parser_t *p, const uint8_t *data, size_t len);

#ifdef __cplusplus
}
#endif

#endif // UBX_H
