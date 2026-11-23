#ifndef OLED_GNSS_H
#define OLED_GNSS_H
// The receiver's status as the display's pages show it. Pure C; host tests in test/.
#include "gnss_status.h"
#include "oled_pages.h"
// Fills out from the receiver status as it stands at now_ms. Reports older than 15 s are
// left out, as on the LED panel: the display never shows a stale count as current.
void oled_gnss_from_status(oled_gnss_t *out, const gnss_status_t *status, int64_t now_ms);
#endif
