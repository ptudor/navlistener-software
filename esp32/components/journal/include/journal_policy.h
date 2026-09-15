#pragma once
#include <stdbool.h>
#include <stdint.h>
typedef struct { uint64_t next_checkpoint_ms; } journal_policy_t;
#define JOURNAL_CHECKPOINT_MS UINT64_C(3600000)
// First health sample after one minute; subsequent writes once an hour.
static inline bool journal_checkpoint_due(const journal_policy_t *p, uint64_t now)
{ return now >= (p->next_checkpoint_ms ? p->next_checkpoint_ms : UINT64_C(60000)); }
static inline void journal_checkpoint_saved(journal_policy_t *p, uint64_t now)
{ p->next_checkpoint_ms=now+JOURNAL_CHECKPOINT_MS; }
