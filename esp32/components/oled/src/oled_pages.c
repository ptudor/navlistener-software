#include "oled_pages.h"
#include "gnss_status.h"
#include <inttypes.h>
#include <stdio.h>
#include <string.h>

// The picture is drawn in 126 by 63 pixels, 21 characters wide, and oled_shift moves it
// within the panel's 128 by 64. The first line's glyphs start at y = 0, a rule follows at
// y = 8, and six lines start at y = 10, 19 ... 55, each nine pixels below the last.
#define PICTURE_W (OLED_COLUMNS * OLED_ADVANCE)
#define RULE_Y 8
#define LINE_Y(n) (10 + 9 * (n))
_Static_assert(PICTURE_W + OLED_SHIFT_X <= OLED_WIDTH && LINE_Y(5) + OLED_GLYPH_H + OLED_SHIFT_Y <= OLED_HEIGHT,
               "the shifted picture must stay on the panel");

// The constellations in the LED panel's column order, by UBX gnssId, with RINEX's letters:
// SBAS, GPS, Galileo, BeiDou, GLONASS, QZSS, NavIC.
static const uint8_t panel_gnss[7] = {1, 0, 2, 3, 6, 5, 7};
static const char panel_letter[7] = {'S', 'G', 'E', 'C', 'R', 'J', 'I'};
static const char *const band_names[OLED_BANDS] = {"L1", "L2", "L5", "E5b", "E6"};

static const char *const page_names[OLED_PAGE_COUNT] = {
    "now", "gnss", "signal", "network", "sensors", "system", "attention",
};
const char *oled_page_name(oled_page_t page) { return page < OLED_PAGE_COUNT ? page_names[page] : "unknown"; }
bool oled_page_parse(const char *name, oled_page_t *page)
{
    for (unsigned i = 0; name && i < OLED_PAGE_COUNT; i++)
        if (!strcmp(name, page_names[i])) {
            *page = (oled_page_t)i;
            return true;
        }
    return false;
}

// Text from x, cut at the picture's right edge.
static void put(oled_canvas_t *c, int x, int y, const char *s)
{
    char cut[OLED_TEXT];
    const int room = (PICTURE_W - x) / OLED_ADVANCE;
    if (room <= 0) return;
    snprintf(cut, sizeof cut, "%.*s", room > OLED_COLUMNS ? OLED_COLUMNS : room, s);
    (void)oled_text(c, x, y, 1, cut);
}
// Text ending at the picture's right edge; its last OLED_COLUMNS characters when longer.
static void put_right(oled_canvas_t *c, int y, const char *s)
{
    const size_t length = strlen(s);
    oled_text_right(c, PICTURE_W - 1, y, 1, length > OLED_COLUMNS ? s + length - OLED_COLUMNS : s);
}
// Text at the left and text ending at the right edge of one line. Where the two would meet,
// the right one is left out: a value that has outgrown its place is not drawn over another.
static void put_pair(oled_canvas_t *c, int y, const char *left, const char *right)
{
    put(c, 0, y, left);
    const size_t length = strlen(left);
    const int left_end = (int)(length > OLED_COLUMNS ? OLED_COLUMNS : length) * OLED_ADVANCE;
    if (left_end + oled_text_width(right, 1) <= PICTURE_W - 1) put_right(c, y, right);
}
static void put_centered(oled_canvas_t *c, int y, int scale, const char *s)
{
    (void)oled_text(c, (PICTURE_W - 1 - oled_text_width(s, scale)) / 2, y, scale, s);
}

// Hundredths as a decimal number with one or two places, rounded half away from zero.
static void centi(char *out, size_t size, int32_t value, unsigned places)
{
    const char *sign = value < 0 ? "-" : "";
    uint32_t magnitude = value < 0 ? (uint32_t)-(int64_t)value : (uint32_t)value;
    if (places == 1) {
        magnitude = (magnitude + 5) / 10;
        snprintf(out, size, "%s%" PRIu32 ".%" PRIu32, sign, magnitude / 10, magnitude % 10);
    } else {
        snprintf(out, size, "%s%" PRIu32 ".%02" PRIu32, sign, magnitude / 100, magnitude % 100);
    }
}
// A horizontal accuracy in the unit that reads best: "1.4cm", "82cm", "1.2m", "12m".
static void accuracy(char *out, size_t size, uint32_t mm)
{
    if (mm < 100) snprintf(out, size, "%" PRIu32 ".%" PRIu32 "cm", mm / 10, mm % 10);
    else if (mm < 1000) snprintf(out, size, "%" PRIu32 "cm", (mm + 5) / 10);
    else if (mm < 10000) snprintf(out, size, "%" PRIu32 ".%" PRIu32 "m", (mm + 50) / 1000, (mm + 50) % 1000 / 100);
    else if (mm < 1000000) snprintf(out, size, "%" PRIu32 "m", (mm + 500) / 1000);
    else snprintf(out, size, ">1km");
}
static void address(char *out, size_t size, const uint8_t ip[4])
{
    snprintf(out, size, "%u.%u.%u.%u", ip[0], ip[1], ip[2], ip[3]);
}

// The civil date and time of day of a count of seconds since 1970 (the proleptic Gregorian
// calendar, by the days-from-civil arithmetic that needs no table).
typedef struct { int year; unsigned month, day, hour, minute, second; } civil_t;
static civil_t civil(int64_t utc)
{
    int64_t days = utc / 86400, rest = utc % 86400;
    if (rest < 0) { rest += 86400; days--; }
    days += 719468;
    const int64_t era = (days >= 0 ? days : days - 146096) / 146097;
    const unsigned day_of_era = (unsigned)(days - era * 146097);
    const unsigned year_of_era = (day_of_era - day_of_era / 1460 + day_of_era / 36524 - day_of_era / 146096) / 365;
    const unsigned day_of_year = day_of_era - (365 * year_of_era + year_of_era / 4 - year_of_era / 100);
    const unsigned shifted_month = (5 * day_of_year + 2) / 153;
    civil_t t = {.day = day_of_year - (153 * shifted_month + 2) / 5 + 1,
                 .month = shifted_month < 10 ? shifted_month + 3 : shifted_month - 9,
                 .hour = (unsigned)(rest / 3600), .minute = (unsigned)(rest % 3600 / 60), .second = (unsigned)(rest % 60)};
    t.year = (int)(year_of_era + era * 400) + (t.month <= 2 ? 1 : 0);
    return t;
}

static const char *fix_name(oled_fix_t fix)
{
    switch (fix) {
    case OLED_FIX_TIME: return "TIME";
    case OLED_FIX_POSITION: return "FIX";
    case OLED_FIX_2D: return "2D FIX";
    case OLED_FIX_3D: return "3D FIX";
    case OLED_FIX_DEAD_RECKONING: return "3D+DR";
    case OLED_FIX_DGNSS: return "DGNSS";
    case OLED_FIX_RTK_FLOAT: return "RTK FLT";
    case OLED_FIX_RTK_FIXED: return "RTK FIX";
    case OLED_FIX_NONE: break;
    }
    return "NO FIX";
}
static bool has_position(oled_fix_t fix) { return fix != OLED_FIX_NONE && fix != OLED_FIX_TIME; }

unsigned oled_attention(const oled_model_t *m, char lines[OLED_ATTENTION_MAX][OLED_TEXT])
{
    // Most urgent first, as the firmware's log names them (gnss_status_attention_text).
    const struct { bool set; const char *text; } causes[] = {
        {m->receiver_attention & GNSS_ATTENTION_SPOOFING, "SPOOFING INDICATED"},
        {m->receiver_attention & GNSS_ATTENTION_JAMMING_CRITICAL, "JAMMING CRITICAL"},
        {m->receiver_attention & GNSS_ATTENTION_JAMMING, "JAMMING WARNING"},
        {m->receiver_attention & GNSS_ATTENTION_ANTENNA_SHORT, "ANTENNA SHORT"},
        {m->receiver_attention & GNSS_ATTENTION_ANTENNA_OPEN, "ANTENNA OPEN"},
        {m->receiver_attention & GNSS_ATTENTION_SILENT, "NO RECEIVER DATA"},
        {m->reception_alarm, "RECEPTION ALARM"},
        {m->sensors.rail_alert, "POWER RAIL ALERT"},
    };
    unsigned count = 0;
    for (size_t i = 0; i < sizeof causes / sizeof causes[0] && count < OLED_ATTENTION_MAX; i++) {
        if (!causes[i].set) continue;
        if (lines) snprintf(lines[count], OLED_TEXT, "%s", causes[i].text);
        count++;
    }
    return count;
}

static bool sensors_any(const oled_sensors_t *s)
{
    bool any = s->temperature_valid || s->humidity_valid || s->pressure_valid || s->rtc_temperature_valid;
    for (unsigned i = 0; i < OLED_RAILS; i++) any = any || s->rail[i].valid;
    return any;
}

bool oled_page_available(const oled_model_t *m, oled_page_t page)
{
    switch (page) {
    case OLED_PAGE_SIGNAL: return m->gnss.satellites_valid && m->gnss.bars;
    case OLED_PAGE_SENSORS: return sensors_any(&m->sensors);
    case OLED_PAGE_ATTENTION: return oled_attention(m, NULL) != 0;
    case OLED_PAGE_NOW: case OLED_PAGE_GNSS: case OLED_PAGE_NETWORK: case OLED_PAGE_SYSTEM: return true;
    case OLED_PAGE_COUNT: break;
    }
    return false;
}

// The first line: the time (or, with date set, the date), the page's title at the right,
// and between them a mark while anything needs attention. Under it the rule: dotted, and
// solid where the page stands among those available, so its length shows how many there are.
static void header(oled_canvas_t *c, const oled_model_t *m, oled_page_t page, const char *title, bool date)
{
    char left[OLED_TEXT];
    if (!m->time_valid) {
        snprintf(left, sizeof left, date ? "NO DATE" : "--:--:--");
    } else {
        const civil_t t = civil(m->utc);
        // A year outside 0..9999 is no date the display has room for.
        if (date && t.year >= 0 && t.year <= 9999) snprintf(left, sizeof left, "%04d-%02u-%02u", t.year, t.month, t.day);
        else if (date) snprintf(left, sizeof left, "NO DATE");
        else snprintf(left, sizeof left, "%02u:%02u:%02uZ", t.hour, t.minute, t.second);
    }
    put(c, 0, 0, left);
    put_right(c, 0, title);
    if (page != OLED_PAGE_ATTENTION && oled_attention(m, NULL)) {
        (void)oled_text(c, 64, 0, 1, "!");
        oled_invert(c, 63, 0, 7, OLED_GLYPH_H);
    }
    unsigned available = 0, position = 0;
    for (unsigned p = 0; p < OLED_PAGE_COUNT; p++) {
        if (!oled_page_available(m, (oled_page_t)p)) continue;
        if (p == (unsigned)page) position = available;
        available++;
    }
    for (int x = 0; x < PICTURE_W; x += 2) oled_pixel(c, x, RULE_Y, true);
    if (available) {
        const int from = (int)(position * PICTURE_W / available), to = (int)((position + 1) * PICTURE_W / available);
        oled_hline(c, from, RULE_Y, to - from, true);
    }
}

// "3D FIX   34 SV  +-0.82m", or what stands in for it.
static void fix_line(oled_canvas_t *c, int y, const oled_gnss_t *g)
{
    if (!g->receiving) {
        put(c, 0, y, "NO RECEIVER DATA");
        return;
    }
    // The fix in the first eight columns, the satellites from the ninth.
    char left[48], right[24] = "";
    const unsigned satellites = has_position(g->fix) && g->used ? g->used : g->tracked_total;
    if (g->satellites_valid || (has_position(g->fix) && g->used))
        snprintf(left, sizeof left, "%-8s%u SV", fix_name(g->fix), satellites);
    else
        snprintf(left, sizeof left, "%s", fix_name(g->fix));
    if (has_position(g->fix) && g->accuracy_valid) {
        char value[16];
        accuracy(value, sizeof value, g->h_acc_mm);
        snprintf(right, sizeof right, OLED_PLUS_MINUS "%s", value);
    }
    put_pair(c, y, left, right);
}

static void page_now(oled_canvas_t *c, const oled_model_t *m)
{
    const oled_network_t *n = &m->network;
    const oled_sensors_t *s = &m->sensors;
    char text[96];
    header(c, m, OLED_PAGE_NOW, "NOW", true);
    if (m->time_valid) {
        const civil_t t = civil(m->utc);
        snprintf(text, sizeof text, "%02u:%02u:%02u", t.hour, t.minute, t.second);
        (void)oled_text(c, 0, LINE_Y(0) + 1, 2, text);
        put(c, 17 * OLED_ADVANCE, LINE_Y(0) + 1, "UTC");
        put(c, 17 * OLED_ADVANCE, LINE_Y(1) + 1, m->time_source);
    } else {
        (void)oled_text(c, 0, LINE_Y(0) + 1, 2, "--:--:--");
        put(c, 17 * OLED_ADVANCE, LINE_Y(0) + 1, "NO");
        put(c, 17 * OLED_ADVANCE, LINE_Y(1) + 1, "TIME");
    }
    fix_line(c, LINE_Y(2), &m->gnss);

    if (!n->provisioned) {
        if (n->setup_network[0]) snprintf(text, sizeof text, "AP %.21s", n->setup_network);
        else snprintf(text, sizeof text, "NO SETUP NETWORK");
        put(c, 0, LINE_Y(3), text);
        put(c, 0, LINE_Y(4), "NOT PROVISIONED");
    } else {
        char ip[16];
        if (n->ethernet_up) { address(ip, sizeof ip, n->ethernet_ip); snprintf(text, sizeof text, "ETH %s", ip); }
        else if (n->wifi_up) { address(ip, sizeof ip, n->wifi_ip); snprintf(text, sizeof text, "WIFI %s", ip); }
        else snprintf(text, sizeof text, "NO UPLINK");
        put(c, 0, LINE_Y(3), text);
        snprintf(text, sizeof text, "SPOOL %" PRIu32, n->spool);
        put_pair(c, LINE_Y(4), !n->collector_up ? "LINK DOWN" : n->tunnel ? "TUNNEL UP" : "LINK UP", text);
    }

    // The weather in the box, or the board's name where it has no such sensors.
    size_t used = 0;
    text[0] = 0;
    char value[16];
    if (s->temperature_valid) {
        centi(value, sizeof value, s->centi_c, 1);
        used += (size_t)snprintf(text + used, sizeof text - used, "%s" OLED_DEGREE "C", value);
    }
    if (s->humidity_valid)
        used += (size_t)snprintf(text + used, sizeof text - used, "%s%u%%", used ? " " : "", (s->centi_rh + 50u) / 100u);
    if (s->pressure_valid)
        used += (size_t)snprintf(text + used, sizeof text - used, "%s%" PRIu32 "hPa", used ? " " : "",
                                 s->pressure_pa / 100 + (s->pressure_pa % 100 >= 50));
    put(c, 0, LINE_Y(5), used ? text : m->board);
}

static const char *antenna_name(oled_antenna_t antenna)
{
    return antenna == OLED_ANTENNA_OK ? "ANT OK" : antenna == OLED_ANTENNA_SHORT ? "ANT SHORT" :
           antenna == OLED_ANTENNA_OPEN ? "ANT OPEN" : "ANT ?";
}

static void page_gnss(oled_canvas_t *c, const oled_model_t *m)
{
    const oled_gnss_t *g = &m->gnss;
    char text[48], value[16];
    header(c, m, OLED_PAGE_GNSS, "GNSS", false);
    if (!g->receiving) {
        put(c, 0, LINE_Y(0), "NO RECEIVER DATA");
    } else {
        // Satellites in the solution over satellites tracked, where the receiver gives both.
        if (has_position(g->fix) && g->used) snprintf(text, sizeof text, "%u/%u SV", g->used, g->tracked_total);
        else if (g->satellites_valid) snprintf(text, sizeof text, "%u SV", g->tracked_total);
        else text[0] = 0;
        put_pair(c, LINE_Y(0), fix_name(g->fix), text);
    }
    if (has_position(g->fix) && g->accuracy_valid) {
        char dop[24];
        accuracy(value, sizeof value, g->h_acc_mm);
        snprintf(text, sizeof text, "HACC %s", value);
        centi(value, sizeof value, g->p_dop_centi, 2);
        snprintf(dop, sizeof dop, "PDOP %s", value);
        put_pair(c, LINE_Y(1), text, dop);
    } else {
        put_pair(c, LINE_Y(1), "HACC --", "PDOP --");
    }
    // One 18-pixel column for each constellation: its letter, and under it the satellites
    // tracked, or a dash where the receiver does not track that constellation.
    for (unsigned i = 0; i < 7; i++) {
        const int x = (int)i * 18;
        const char letter[2] = {panel_letter[i], 0};
        (void)oled_text(c, x + 6, LINE_Y(2), 1, letter);
        if (!(g->supported & (1u << panel_gnss[i]))) snprintf(text, sizeof text, "-");
        else if (!g->satellites_valid) snprintf(text, sizeof text, "?");
        else snprintf(text, sizeof text, "%u", g->tracked[panel_gnss[i]]);
        (void)oled_text(c, x + (17 - oled_text_width(text, 1)) / 2, LINE_Y(3), 1, text);
    }
    put(c, 0, LINE_Y(4), antenna_name(g->antenna));
    put(c, 12 * OLED_ADVANCE, LINE_Y(4), g->jam == OLED_RF_NONE ? "JAM NONE" : g->jam == OLED_RF_WARNING ? "JAM WARN" :
                                         g->jam == OLED_RF_CRITICAL ? "JAM CRIT" : "JAM ?");
    put(c, 0, LINE_Y(5), g->spoof == OLED_RF_NONE ? "SPOOF NONE" : g->spoof == OLED_RF_WARNING ? "SPOOF YES" :
                         g->spoof == OLED_RF_CRITICAL ? "SPOOF MULTI" : "SPOOF ?");
    if (m->sensors.pulse_known) put(c, 12 * OLED_ADVANCE, LINE_Y(5), m->sensors.pulse ? "PPS OK" : "PPS NONE");
}

// The bar chart's scale: 15 dB-Hz at the baseline, 55 dB-Hz at its top.
#define BAR_BASE_Y (LINE_Y(4) + 8)
#define BAR_ROOM 26
static int bar_height(unsigned cno)
{
    if (cno <= 15) return 1;
    const int height = (int)(cno - 15) * BAR_ROOM / 40;
    return height < 1 ? 1 : height > BAR_ROOM ? BAR_ROOM : height;
}

static void page_signal(oled_canvas_t *c, const oled_model_t *m)
{
    const oled_gnss_t *g = &m->gnss;
    char text[48];
    header(c, m, OLED_PAGE_SIGNAL, "SIGNAL", false);
    // Signals locked per band, three seven-character cells to a line, the bands with none
    // left out; the strongest satellite's C/N0 ends the second line.
    unsigned cell = 0;
    for (unsigned b = 0; g->signals_valid && b < OLED_BANDS && cell < 5; b++) {
        if (!g->band[b]) continue;
        snprintf(text, sizeof text, "%s %u", band_names[b], g->band[b]);
        put(c, (int)(cell % 3) * 7 * OLED_ADVANCE, LINE_Y(cell / 3), text);
        cell++;
    }
    if (!cell) {
        snprintf(text, sizeof text, "TRACKED %u", g->tracked_total);
        put(c, 0, LINE_Y(0), text);
    }
    snprintf(text, sizeof text, "MAX %u", g->cno_max);
    put_right(c, LINE_Y(1), text);

    // One bar for each satellite, constellation by constellation, as wide as lets them all
    // in: three pixels and a gap for a few, down to one pixel and no gap for many.
    unsigned groups = 0;
    for (unsigned i = 0; i < g->bars; i++)
        if (!i || g->bar[i].gnss != g->bar[i - 1].gnss) groups++;
    int width = 3, gap = 1;
    while (width > 1 && (int)g->bars * (width + gap) + ((int)groups - 1) * 2 > PICTURE_W) width--;
    if ((int)g->bars * (width + gap) + ((int)groups - 1) * 2 > PICTURE_W) gap = 0;
    // Dotted lines at 30 and 40 dB-Hz, to read the bars against.
    for (int x = 0; x < PICTURE_W; x += 4) {
        oled_pixel(c, x, BAR_BASE_Y - bar_height(30), true);
        oled_pixel(c, x, BAR_BASE_Y - bar_height(40), true);
    }
    int x = 0, letter_end = -1;
    for (unsigned i = 0; i < g->bars; i++) {
        const bool group = !i || g->bar[i].gnss != g->bar[i - 1].gnss;
        if (i && group) x += 2;
        if (x + width > PICTURE_W) break; // more groups than the picture is wide: the rest are not drawn
        if (group) {
            // The group's letter, under its first bar, where the last letter left room.
            char letter[2] = {'?', 0};
            for (unsigned p = 0; p < 7; p++)
                if (panel_gnss[p] == g->bar[i].gnss) letter[0] = panel_letter[p];
            if (x > letter_end && x + OLED_GLYPH_W <= PICTURE_W) {
                (void)oled_text(c, x, LINE_Y(5), 1, letter);
                letter_end = x + OLED_GLYPH_W;
            }
        }
        const int height = bar_height(g->bar[i].cno);
        oled_fill(c, x, BAR_BASE_Y - height, width, height, true);
        x += width + gap;
    }
}

static void page_network(oled_canvas_t *c, const oled_model_t *m)
{
    const oled_network_t *n = &m->network;
    char text[96], ip[16];
    if (!n->provisioned) {
        // The password is on the device's label and in the paired app, nowhere on the board.
        header(c, m, OLED_PAGE_NETWORK, "SETUP", false);
        unsigned row = 0;
        if (n->setup_network[0]) {
            put(c, 0, LINE_Y(row++), "JOIN WI-FI NETWORK");
            put(c, 0, LINE_Y(row++), n->setup_network);
            put(c, 0, LINE_Y(row++), "PASSWORD ON THE LABEL");
            put(c, 0, LINE_Y(row++), "http://192.168.4.1");
        } else {
            put(c, 0, LINE_Y(row++), "NOT PROVISIONED");
            put(c, 0, LINE_Y(row++), "NO SETUP NETWORK");
        }
        // Why the board is in setup, over as many of the lines left as it needs, broken at
        // a space where a line has one.
        for (const char *rest = n->setup_reason; *rest && row < 6; row++) {
            size_t take = strlen(rest);
            if (take > OLED_COLUMNS) {
                take = OLED_COLUMNS;
                while (take && rest[take] != ' ') take--;
                if (!take) take = OLED_COLUMNS; // one long word: cut it at the edge
            }
            snprintf(text, sizeof text, "%.*s", (int)take, rest);
            put(c, 0, LINE_Y(row), text);
            rest += take;
            while (*rest == ' ') rest++;
        }
        return;
    }
    header(c, m, OLED_PAGE_NETWORK, "NETWORK", false);
    if (!n->ethernet) snprintf(text, sizeof text, "ETH none");
    else if (n->ethernet_up) { address(ip, sizeof ip, n->ethernet_ip); snprintf(text, sizeof text, "ETH %s", ip); }
    else snprintf(text, sizeof text, "ETH no address");
    put(c, 0, LINE_Y(0), text);
    if (!n->wifi) snprintf(text, sizeof text, "WIFI not configured");
    else if (n->wifi_up) { address(ip, sizeof ip, n->wifi_ip); snprintf(text, sizeof text, "WIFI %s", ip); }
    else snprintf(text, sizeof text, "WIFI no address");
    // The signal strength in dBm, where the address leaves room for it.
    char rssi[8] = "";
    if (n->wifi_rssi_valid) snprintf(rssi, sizeof rssi, "%d", n->wifi_rssi);
    put_pair(c, LINE_Y(1), text, rssi);
    put_pair(c, LINE_Y(2), n->collector_up ? "COLLECTOR UP" : "COLLECTOR DOWN", n->collector_up && n->tunnel ? "TUNNEL" : "");
    put(c, 0, LINE_Y(3), n->collector);
    char lost[24];
    snprintf(text, sizeof text, "SPOOL %" PRIu32, n->spool);
    if (n->dropped > 9999999) snprintf(lost, sizeof lost, "LOST >9999999");
    else snprintf(lost, sizeof lost, "LOST %" PRIu32, (uint32_t)n->dropped);
    put_pair(c, LINE_Y(4), text, lost);
    // A long station name is a board's serial: its end tells one board from another.
    put_right(c, LINE_Y(5), n->station);
}

static void page_sensors(oled_canvas_t *c, const oled_model_t *m)
{
    const oled_sensors_t *s = &m->sensors;
    char text[48], value[16];
    header(c, m, OLED_PAGE_SENSORS, "SENSORS", false);
    if (s->temperature_valid) {
        centi(value, sizeof value, s->centi_c, 2);
        snprintf(text, sizeof text, "%s" OLED_DEGREE "C", value);
    } else {
        snprintf(text, sizeof text, "--" OLED_DEGREE "C");
    }
    char right[24] = "";
    if (s->humidity_valid) {
        centi(value, sizeof value, s->centi_rh, 1);
        snprintf(right, sizeof right, "%s%%RH", value);
    } else {
        snprintf(right, sizeof right, "--%%RH");
    }
    put_pair(c, LINE_Y(0), text, right);
    // Pascals read as hundredths of a hectopascal; a pressure no int32_t holds is no pressure.
    if (s->pressure_valid && s->pressure_pa <= INT32_MAX - 5) {
        centi(value, sizeof value, (int32_t)s->pressure_pa, 1);
        snprintf(text, sizeof text, "%shPa", value);
    } else {
        snprintf(text, sizeof text, "--hPa");
    }
    right[0] = 0;
    if (s->rtc_temperature_valid) {
        centi(value, sizeof value, s->rtc_centi_c, 1);
        snprintf(right, sizeof right, "RTC %s" OLED_DEGREE "C", value);
    }
    put_pair(c, LINE_Y(1), text, right);
    for (unsigned i = 0; i < OLED_RAILS; i++) {
        if (!s->rail[i].name[0]) continue;
        const int y = LINE_Y(2 + i);
        put(c, 0, y, s->rail[i].name);
        if (!s->rail[i].valid) {
            put_right(c, y, "--");
            continue;
        }
        centi(value, sizeof value, (s->rail[i].bus_mv + (s->rail[i].bus_mv < 0 ? -5 : 5)) / 10, 2);
        snprintf(text, sizeof text, "%sV", value);
        oled_text_right(c, 15 * OLED_ADVANCE - 1, y, 1, text);
        snprintf(text, sizeof text, "%" PRId32 "mA", s->rail[i].ma);
        put_right(c, y, text);
    }
    put_pair(c, LINE_Y(5), !s->pulse_known ? "" : s->pulse ? "PPS OK" : "PPS NONE", s->rail_alert ? "RAIL ALERT" : "");
}

static void page_system(oled_canvas_t *c, const oled_model_t *m)
{
    char text[96];
    header(c, m, OLED_PAGE_SYSTEM, "SYSTEM", false);
    put(c, 0, LINE_Y(0), m->board[0] ? m->board : "BOARD unnamed");
    if (m->serial[0]) snprintf(text, sizeof text, "SERIAL ...%s", m->serial);
    else snprintf(text, sizeof text, "SERIAL unread");
    put(c, 0, LINE_Y(1), text);
    // The version alone when it is too long to carry its label.
    snprintf(text, sizeof text, "FW %s", m->firmware);
    put(c, 0, LINE_Y(2), strlen(text) > OLED_COLUMNS ? m->firmware : text);
    // Days, hours and minutes once there are days; hours, minutes and seconds before.
    char right[32] = "";
    const uint32_t days = m->uptime_s / 86400, rest = m->uptime_s % 86400;
    if (days) snprintf(text, sizeof text, "UP %" PRIu32 "d %02" PRIu32 ":%02" PRIu32, days, rest / 3600, rest % 3600 / 60);
    else snprintf(text, sizeof text, "UP %02" PRIu32 ":%02" PRIu32 ":%02" PRIu32, rest / 3600, rest % 3600 / 60, rest % 60);
    if (m->boot) snprintf(right, sizeof right, "BOOT %" PRIu32, m->boot);
    put_pair(c, LINE_Y(3), text, right);
    // The receiver, and the LNA mode the unit sets it to where it has one (esp32/README.md,
    // The ZED-X20P's LNA mode).
    snprintf(text, sizeof text, "RX %s", m->receiver[0] ? m->receiver : "none listed");
    put_pair(c, LINE_Y(4), text,
             m->receiver_lna == OLED_LNA_LOW ? "LNA LOW" : m->receiver_lna == OLED_LNA_NORMAL ? "LNA NORM" : "");
    snprintf(text, sizeof text, "HEAP %" PRIu32 "K", m->heap_free / 1024);
    right[0] = 0;
    if (m->time_valid && m->time_source[0]) snprintf(right, sizeof right, "TIME %.4s", m->time_source);
    put_pair(c, LINE_Y(5), text, right);
}

static void page_attention(oled_canvas_t *c, const oled_model_t *m)
{
    char lines[OLED_ATTENTION_MAX][OLED_TEXT];
    const unsigned count = oled_attention(m, lines);
    header(c, m, OLED_PAGE_ATTENTION, "ATTENTION", false);
    for (unsigned i = 0; i < count; i++) put(c, 0, LINE_Y(i), lines[i]);
    if (!count) put(c, 0, LINE_Y(0), "NOTHING TO REPORT");
    const oled_gnss_t *g = &m->gnss;
    if (!(m->receiver_attention & GNSS_ATTENTION_JAMMING) || !g->security_fresh || g->jam != OLED_RF_WARNING) return;
    unsigned row = count;
    char text[OLED_TEXT];
    if (g->solution_fresh && row < OLED_ATTENTION_MAX) {
        snprintf(text, sizeof text, "%s  %u SV USED", fix_name(g->fix), g->used);
        put(c, 0, LINE_Y(row++), text);
    }
    if (row < OLED_ATTENTION_MAX) {
        if (g->assessed) snprintf(text, sizeof text, "%u/%u FLAGGED", g->jammed, g->assessed);
        else snprintf(text, sizeof text, "NO CENTERS ASSESSED");
        put(c, 0, LINE_Y(row++), text);
    }
    const unsigned capacity = sizeof g->jammed_khz / sizeof *g->jammed_khz;
    const unsigned available = g->jammed < capacity ? g->jammed : capacity;
    const unsigned rows = OLED_ATTENTION_MAX - row;
    if (!available || !rows) return;
    const unsigned groups = (available + rows - 1) / rows;
    const unsigned start = (m->uptime_s / 4 % groups) * rows;
    for (unsigned i = start; i < available && row < OLED_ATTENTION_MAX; i++) {
        uint32_t khz = g->jammed_khz[i] & 0xffffffu;
        const char *band = "";
        switch (khz) {
        case 1575420: band = "L1/E1"; break;
        case 1227600: band = "L2"; break;
        case 1176450: band = "L5/E5a"; break;
        case 1207140: band = "E5b/B2"; break;
        case 1278750: band = "E6"; break;
        case 1561098: band = "B1I"; break;
        case 1268520: band = "B3I"; break;
        case 1602000: band = "G1"; break;
        case 1246000: band = "G2"; break;
        }
        snprintf(text, sizeof text, "%lu.%03lu MHz %s", (unsigned long)(khz / 1000),
                 (unsigned long)(khz % 1000), band);
        put(c, 0, LINE_Y(row++), text);
    }
}

void oled_draw_page(oled_canvas_t *c, const oled_model_t *m, oled_page_t page)
{
    switch (page) {
    case OLED_PAGE_NOW: page_now(c, m); break;
    case OLED_PAGE_GNSS: page_gnss(c, m); break;
    case OLED_PAGE_SIGNAL: page_signal(c, m); break;
    case OLED_PAGE_NETWORK: page_network(c, m); break;
    case OLED_PAGE_SENSORS: page_sensors(c, m); break;
    case OLED_PAGE_SYSTEM: page_system(c, m); break;
    case OLED_PAGE_ATTENTION: page_attention(c, m); break;
    case OLED_PAGE_COUNT: break;
    }
}

void oled_draw_splash(oled_canvas_t *c, const oled_model_t *m)
{
    char text[96];
    put_centered(c, 2, 2, "NavListen");
    oled_hline(c, 0, 21, PICTURE_W, true);
    put_centered(c, 26, 1, m->board[0] ? m->board : "GNSS observer");
    if (m->serial[0]) {
        snprintf(text, sizeof text, "SERIAL ...%s", m->serial);
        put_centered(c, 36, 1, text);
    }
    snprintf(text, sizeof text, "%.21s", m->firmware);
    put_centered(c, 46, 1, text);
}

void oled_draw_reset_armed(oled_canvas_t *c)
{
    put_centered(c, 2, 1, "CONFIG RESET ARMED");
    oled_invert(c, 0, 0, PICTURE_W, 11);
    put(c, 0, 17, "RELEASE BOOT TO ERASE");
    put(c, 0, 27, "THE NETWORK SETTINGS");
    put(c, 0, 43, "POWER CYCLE TO CANCEL");
}

static oled_page_t next_available(const oled_model_t *m, oled_page_t page)
{
    for (unsigned step = 1; step <= OLED_PAGE_COUNT; step++) {
        const oled_page_t candidate = (oled_page_t)(((unsigned)page + step) % OLED_PAGE_COUNT);
        if (oled_page_available(m, candidate)) return candidate;
    }
    return OLED_PAGE_NOW;
}

oled_screen_t oled_director_step(oled_director_t *d, const oled_model_t *m, int64_t now_ms, oled_page_t *page)
{
    if (!d->started) {
        *d = (oled_director_t){.started = true, .started_ms = now_ms, .page = OLED_PAGE_NOW,
                               .page_ms = now_ms + OLED_SPLASH_MS};
    }
    *page = d->page;
    // What needs attention now, and what of it is new since the last step.
    const uint16_t attention = (uint16_t)(m->receiver_attention | (m->reception_alarm ? 0x100 : 0) |
                                          (m->sensors.rail_alert ? 0x200 : 0));
    const bool news = (attention & ~d->attention) != 0;
    d->attention = attention;
    if (m->reset_armed) return OLED_SCREEN_RESET_ARMED;
    if (now_ms - d->started_ms < OLED_SPLASH_MS) return OLED_SCREEN_SPLASH;
    if (news && now_ms >= d->hold_until_ms && d->page != OLED_PAGE_ATTENTION) {
        d->page = OLED_PAGE_ATTENTION;
        d->page_ms = now_ms;
    } else if (!oled_page_available(m, d->page)) {
        // What the page showed is gone: move on at once, and a page held by hand is released.
        d->page = next_available(m, d->page);
        d->page_ms = now_ms;
        d->hold_until_ms = 0;
    } else if (now_ms >= d->hold_until_ms && now_ms - d->page_ms >= OLED_DWELL_MS) {
        d->page = next_available(m, d->page);
        d->page_ms = now_ms;
    }
    *page = d->page;
    return OLED_SCREEN_PAGE;
}

void oled_director_next(oled_director_t *d, const oled_model_t *m, int64_t now_ms)
{
    if (!d->started) return;
    d->page = next_available(m, d->page);
    d->page_ms = now_ms;
    d->hold_until_ms = now_ms + OLED_HOLD_MS;
}

void oled_shift(int64_t now_ms, int8_t *dx, int8_t *dy)
{
    // Round the 3 by 2 positions, one pixel at a time: along the top, down, and back.
    static const int8_t path[][2] = {{0, 0}, {1, 0}, {2, 0}, {2, 1}, {1, 1}, {0, 1}};
    _Static_assert(sizeof path / sizeof path[0] == (OLED_SHIFT_X + 1) * (OLED_SHIFT_Y + 1),
                   "the shift path must visit every position once");
    const uint64_t step = now_ms < 0 ? 0 : (uint64_t)now_ms / OLED_SHIFT_MS;
    *dx = path[step % (sizeof path / sizeof path[0])][0];
    *dy = path[step % (sizeof path / sizeof path[0])][1];
}

uint8_t oled_contrast(unsigned percent)
{
    if (percent < 1) percent = 1;
    if (percent > 100) percent = 100;
    return (uint8_t)(1 + (percent - 1) * 254 / 99);
}
