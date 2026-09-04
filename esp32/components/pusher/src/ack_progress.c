#include "ack_progress.h"

bool ack_progress_stalled(ack_progress_t *p, uint64_t acked, uint64_t sent, int64_t now_us)
{
    if (acked > p->acked || sent <= acked || !p->outstanding) {
        p->since_us = now_us;
    }
    if (acked > p->acked) p->acked = acked;
    p->outstanding = sent > acked;
    return p->outstanding && now_us - p->since_us >= ACK_STALL_US;
}
