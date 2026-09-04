// Durable ACK watchdog, driven only by the caller's monotonic microsecond clock.
#ifndef ACK_PROGRESS_H
#define ACK_PROGRESS_H
#include <stdbool.h>
#include <stdint.h>
#define ACK_STALL_US (30LL * 1000000)
typedef struct {
    uint64_t acked;
    int64_t since_us;
    bool outstanding;
} ack_progress_t;
bool ack_progress_stalled(ack_progress_t *p, uint64_t acked, uint64_t sent, int64_t now_us);
#endif
