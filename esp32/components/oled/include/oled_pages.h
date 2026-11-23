#ifndef OLED_PAGES_H
#define OLED_PAGES_H
// What the status display shows: a model of the observer's state, the pages drawn from it,
// and the order they are shown in. Pure C with host tests in test/; the task that fills the
// model and writes the frames to the display is oled_service.c.
//
// Every page has the UTC time, its own name and a mark when something needs attention in
// its first line, and six lines of 21 characters below. The pages follow one another every
// OLED_DWELL_MS:
//
//   NOW        the clock, the fix, the uplink, the collector link and the weather in the box
//   GNSS       the fix and its accuracy, satellites per constellation, antenna, interference
//   SIGNAL     signals locked per band and one C/N0 bar for every satellite tracked
//   NETWORK    addresses, the collector and the spool; SETUP on a board not yet provisioned
//   SENSORS    temperature, humidity, pressure, the supply rails, the RTC and the pulse
//   SYSTEM     the board, its serial, the firmware, the uptime and the receiver
//   ATTENTION  what the panel's ATTENTION lamp is lit for, in words; shown only while it is
//
// A page with nothing to show is left out, and ATTENTION is shown at once when something new
// joins it. Two screens take the whole display: the start screen for OLED_SPLASH_MS after
// power-up, and the configuration-reset warning for as long as the reset is armed.
#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>
#include "oled_canvas.h"

#define OLED_COLUMNS 21
#define OLED_TEXT (OLED_COLUMNS + 1) // a line and its terminator
#define OLED_BANDS 5                 // L1, L2, L5, E5b, E6
#define OLED_BARS 96
#define OLED_RAILS 3
#define OLED_ATTENTION_MAX 6
#define OLED_SPLASH_MS 4000
#define OLED_DWELL_MS 6000
// A page chosen by hand stays this long before the pages move on again.
#define OLED_HOLD_MS 60000
// The picture moves one pixel every OLED_SHIFT_MS through six positions (oled_shift).
#define OLED_SHIFT_MS 60000
#define OLED_SHIFT_X 2
#define OLED_SHIFT_Y 1

typedef enum {
    OLED_FIX_NONE, OLED_FIX_TIME, OLED_FIX_POSITION, OLED_FIX_2D, OLED_FIX_3D, OLED_FIX_DEAD_RECKONING,
    OLED_FIX_DGNSS, OLED_FIX_RTK_FLOAT, OLED_FIX_RTK_FIXED,
} oled_fix_t;
typedef enum { OLED_ANTENNA_UNKNOWN, OLED_ANTENNA_OK, OLED_ANTENNA_SHORT, OLED_ANTENNA_OPEN } oled_antenna_t;
// Jamming and spoofing, as the receivers report them: 0 unknown, 1 none, 2 a warning or one
// indication, 3 critical or several indications.
enum { OLED_RF_UNKNOWN, OLED_RF_NONE, OLED_RF_WARNING, OLED_RF_CRITICAL };

typedef struct {
    bool receiving;        // the receiver has reported within the last 15 s
    bool satellites_valid; // tracked[], bar[] and cno_max are that recent
    uint8_t supported;     // constellations the receiver tracks, bit n for UBX gnssId n
    uint8_t tracked[8];    // satellites tracked, by gnssId
    uint16_t tracked_total;
    oled_fix_t fix;
    uint8_t used;          // satellites in the solution; 0 when the receiver does not say
    bool accuracy_valid;   // h_acc_mm and p_dop_centi came with the fix
    uint32_t h_acc_mm;
    uint16_t p_dop_centi;
    bool signals_valid;    // band[] is recent
    uint8_t band[OLED_BANDS];
    uint8_t bars;          // satellites in bar[], in the page's constellation order
    struct { uint8_t gnss, cno; } bar[OLED_BARS];
    uint8_t cno_max;       // dB-Hz
    oled_antenna_t antenna;
    uint8_t jam, spoof;    // OLED_RF_*; jam without the crossings of a known periodic source
    uint8_t pulse_s;       // that source's period in seconds, 1 to 99; 0 when none is known
    bool solution_fresh, security_fresh;
    uint8_t assessed, jammed;
    uint32_t jammed_khz[16];
} oled_gnss_t;

typedef struct {
    bool provisioned;                 // the board has a network configuration and runs it
    char setup_network[OLED_TEXT];    // the setup access point's name, while it is up
    char setup_reason[2 * OLED_TEXT]; // why the board is in setup, in the firmware's words
    bool ethernet;                    // the board has a wired port and uses it
    bool ethernet_up;                 // it has an address
    uint8_t ethernet_ip[4];
    bool wifi;                        // a Wi-Fi network is configured
    bool wifi_up;                     // it has an address
    uint8_t wifi_ip[4];
    bool wifi_rssi_valid;
    int8_t wifi_rssi;
    bool collector_up, tunnel;        // the push link is up; it runs inside the tunnel
    char collector[64];               // host:port
    char station[64];                 // the name the board reports as
    uint32_t spool;                   // records waiting for the collector
    uint64_t dropped;                 // records lost to a full spool since start
} oled_network_t;

typedef struct {
    bool temperature_valid, humidity_valid, pressure_valid;
    int16_t centi_c;
    uint16_t centi_rh;
    uint32_t pressure_pa;
    struct {
        bool valid;
        char name[9];
        int16_t bus_mv;
        int32_t ma;
    } rail[OLED_RAILS];
    bool rail_alert;                  // the rail monitor holds an alert
    bool rtc_temperature_valid;
    int16_t rtc_centi_c;
    bool pulse_known, pulse;          // the GNSS pulse is captured; one arrived in the last seconds
} oled_sensors_t;

// The mode a unit sets its receiver's internal LNAs to, where the receiver has one to set.
enum { OLED_LNA_NONE, OLED_LNA_LOW, OLED_LNA_NORMAL };

typedef struct {
    bool time_valid;       // utc is a plausible UTC time
    int64_t utc;           // seconds since 1970
    uint16_t utc_millis;   // and milliseconds into that second, which times the next frame
    char time_source[5];   // "GNSS", "RTC", "NET", "SET", or empty when its origin is unknown
    uint32_t uptime_s;
    char board[OLED_TEXT]; // "X20 revision A"
    char serial[9];        // the last eight hex digits of the board's serial, or empty
    char firmware[33];
    char receiver[OLED_TEXT];
    uint8_t receiver_lna;  // OLED_LNA_*: the mode the unit sets its receiver's internal LNAs to
    uint32_t boot;         // this boot's number; 0 unknown
    uint32_t heap_free;    // bytes of internal heap
    uint8_t brightness;    // the LED panel's, 1 to 100 percent, which the contrast follows; 0 for full
    bool reset_armed;      // the configuration reset is armed: BOOT is held and release erases
    // What the panel's ATTENTION lamp is lit for, as the board task holds it: the receiver's
    // GNSS_ATTENTION_* flags (gnss_status.h), the reception alarm, and a rail alert
    // (sensors.rail_alert). The ATTENTION page and the mark in every header follow these.
    uint8_t receiver_attention;
    bool reception_alarm;
    oled_gnss_t gnss;
    oled_network_t network;
    oled_sensors_t sensors;
} oled_model_t;

typedef enum {
    OLED_PAGE_NOW, OLED_PAGE_GNSS, OLED_PAGE_SIGNAL, OLED_PAGE_NETWORK, OLED_PAGE_SENSORS, OLED_PAGE_SYSTEM,
    OLED_PAGE_ATTENTION, OLED_PAGE_COUNT,
} oled_page_t;
// "now", "gnss", "signal", "network", "sensors", "system", "attention": the console's names.
const char *oled_page_name(oled_page_t page);
// The page a console name stands for; false when it names none.
bool oled_page_parse(const char *name, oled_page_t *page);
// Whether the page has something to show for this model.
bool oled_page_available(const oled_model_t *m, oled_page_t page);
// What needs attention, one line each, most urgent first; returns how many (up to
// OLED_ATTENTION_MAX). lines may be NULL to count them. Trouble with the network is not
// among them: the panel's uplink lamp and the NOW and NETWORK pages show that.
unsigned oled_attention(const oled_model_t *m, char lines[OLED_ATTENTION_MAX][OLED_TEXT]);

// Draws one page into a cleared canvas. dx and dy are left as the caller set them.
void oled_draw_page(oled_canvas_t *c, const oled_model_t *m, oled_page_t page);
void oled_draw_splash(oled_canvas_t *c, const oled_model_t *m);
void oled_draw_reset_armed(oled_canvas_t *c);

typedef enum { OLED_SCREEN_SPLASH, OLED_SCREEN_RESET_ARMED, OLED_SCREEN_PAGE } oled_screen_t;
// Which screen is due. Zero it before the first oled_director_step.
typedef struct {
    bool started;
    int64_t started_ms, page_ms, hold_until_ms;
    oled_page_t page;
    uint16_t attention; // what needed attention at the last step
} oled_director_t;
// The screen to draw at now_ms, and for OLED_SCREEN_PAGE the page: the start screen first,
// the reset warning while it is armed, otherwise the available pages in turn. When something
// new needs attention its page is shown at once, unless a page is being held by hand.
oled_screen_t oled_director_step(oled_director_t *d, const oled_model_t *m, int64_t now_ms, oled_page_t *page);
// Moves to the next available page now and holds it for OLED_HOLD_MS: a button press.
void oled_director_next(oled_director_t *d, const oled_model_t *m, int64_t now_ms);
// The picture's offset at now_ms: one of (OLED_SHIFT_X + 1) * (OLED_SHIFT_Y + 1) positions,
// a pixel from the last, so that a lit pixel's wear is spread over its neighbours.
void oled_shift(int64_t now_ms, int8_t *dx, int8_t *dy);
// The contrast for a panel brightness of 1 to 100 percent.
uint8_t oled_contrast(unsigned percent);
#endif
