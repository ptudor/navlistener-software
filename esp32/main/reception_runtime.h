#pragma once
#include "gnss_status.h"
#include "observer_report.h"
void reception_control(uint8_t type,const uint8_t *bytes,size_t length);
uint8_t reception_poll(const gnss_status_t *status,uint64_t now,uint64_t (*now_ns)(void));
bool reception_snapshot_due(uint64_t now);
size_t reception_snapshot_append(uint8_t *body,size_t length,size_t cap,
                                 const gnss_status_t *status,uint64_t now);
void reception_snapshot_queued(void);
