#ifndef NVF_OBSERVER_OLED_H
#define NVF_OBSERVER_OLED_H
// The status display's application side: what the receiver, the board task and the network
// know, gathered into the display's model (components/oled), and the USB console's `oled`
// commands. Every call is a no-op on a board without a display header.
#include <stdbool.h>
#include "esp_err.h"

// Starts the display service on a board whose row has a display header, after
// observer_board_start. reset_armed says whether the configuration reset is armed, for the
// warning that takes the display while it is. ESP_ERR_NOT_SUPPORTED on any other board.
esp_err_t observer_oled_start(bool (*reset_armed)(void));
// The board is in setup: its setup network's name (NULL when the network could not start)
// and why it is in setup. The setup password is never given to the display.
void observer_oled_setup(const char *network, const char *reason);
// The board is provisioned: its collector ("host:port"), the station name it reports as, and
// whether it joins a Wi-Fi network.
void observer_oled_provisioned(const char *collector, const char *station, bool wifi);
// A button press: the next page, held for a minute.
void observer_oled_next_page(void);
// The console's commands, after "oled ": status | rotate <0|180> | off | on | dump [page].
// Each answers with NVF-OLED-... lines.
void observer_oled_console(char *command);
#endif
