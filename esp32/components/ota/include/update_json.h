#pragma once
#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>
#define UJ_MAX_BYTES 12288u

// Bounded JSON for authenticated metadata. Integers retain all 64 bits; duplicate
// keys, floats, invalid UTF-8, embedded NULs and excessive nesting are rejected.
typedef enum { UJ_OBJECT, UJ_ARRAY, UJ_STRING, UJ_UINT, UJ_BOOL, UJ_NULL } uj_kind;
typedef struct { uj_kind kind; unsigned first, next; const char *text; uint64_t number; } uj_node;
typedef struct { uj_node *nodes; char *strings; unsigned used, limit; size_t stored; } uj_doc;
bool uj_parse(uj_doc *doc, const char *data, size_t length);
void uj_free(uj_doc *doc);
const uj_node *uj_root(const uj_doc *doc);
const uj_node *uj_get(const uj_doc *doc, const uj_node *object, const char *name);
const char *uj_string(const uj_node *node);
bool uj_uint(const uj_node *node, uint64_t *value);
bool uj_fields(const uj_doc *doc, const uj_node *object, const char *const *allowed, size_t count);
char *uj_canonical(const uj_doc *doc, const uj_node *node, size_t *length);
