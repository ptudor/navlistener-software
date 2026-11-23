#include "oled_gnss.h"
#include "oled_pages.h"
#include <assert.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

// Usage: pages_test [directory]. With a directory, every screen drawn below is also written
// there as <name>.pbm, to look at the layout the assertions describe.
static const char *directory;

static void save(const char *name, const oled_canvas_t *c)
{
    if (!directory) return;
    char path[256];
    snprintf(path, sizeof path, "%s/%s.pbm", directory, name);
    FILE *f = fopen(path, "wb");
    assert(f);
    fprintf(f, "P4\n%d %d\n", OLED_WIDTH, OLED_HEIGHT);
    for (int y = 0; y < OLED_HEIGHT; y++)
        for (int x = 0; x < OLED_WIDTH; x += 8) {
            unsigned byte = 0;
            for (int bit = 0; bit < 8; bit++) byte = byte << 1 | oled_get(c, x + bit, y);
            fputc((int)byte, f);
        }
    assert(fclose(f) == 0);
}

static unsigned lit(const oled_canvas_t *c)
{
    unsigned count = 0;
    for (int y = 0; y < OLED_HEIGHT; y++)
        for (int x = 0; x < OLED_WIDTH; x++) count += oled_get(c, x, y);
    return count;
}
// Whether the canvas shows exactly this text with its top-left corner at (x, y): every
// cell of its glyphs, lit and dark, and the blank columns between them.
static bool shows(const oled_canvas_t *c, int x, int y, int scale, const char *text)
{
    static oled_canvas_t want;
    oled_clear(&want);
    want.dx = want.dy = 0;
    const int end = oled_text(&want, x, y, scale, text) - scale;
    for (int row = y; row < y + OLED_GLYPH_H * scale; row++)
        for (int column = x; column < end; column++)
            if (oled_get(c, column, row) != oled_get(&want, column, row)) return false;
    return true;
}
static bool shows_right(const oled_canvas_t *c, int y, const char *text)
{
    return shows(c, OLED_COLUMNS * OLED_ADVANCE - 1 - oled_text_width(text, 1), y, 1, text);
}
static bool blank(const oled_canvas_t *c, int x, int y, int w, int h)
{
    for (int row = y; row < y + h; row++)
        for (int column = x; column < x + w; column++)
            if (oled_get(c, column, row)) return false;
    return true;
}
#define LINE(n) (10 + 9 * (n))

static int64_t utc(int year, unsigned month, unsigned day, unsigned hour, unsigned minute, unsigned second)
{
    year -= month <= 2;
    const int64_t era = (year >= 0 ? year : year - 399) / 400;
    const unsigned year_of_era = (unsigned)(year - era * 400);
    const unsigned day_of_year = (153 * (month > 2 ? month - 3 : month + 9) + 2) / 5 + day - 1;
    const unsigned day_of_era = year_of_era * 365 + year_of_era / 4 - year_of_era / 100 + day_of_year;
    return (era * 146097 + (int64_t)day_of_era - 719468) * 86400 + hour * 3600 + minute * 60 + second;
}

// An X20 in service: a fix on four bands, wired and wireless uplinks, the collector reached.
static void in_service(oled_model_t *m)
{
    *m = (oled_model_t){.time_valid = true, .utc = utc(2026, 11, 22, 12, 34, 56), .time_source = "GNSS",
        .uptime_s = 3 * 86400 + 4 * 3600 + 5 * 60 + 6, .board = "X20 revision A", .serial = "89abcdef",
        .firmware = "0.1.0+1.0f7d658", .receiver = "ZED-X20P", .boot = 12, .heap_free = 131072};
    oled_gnss_t *g = &m->gnss;
    *g = (oled_gnss_t){.receiving = true, .satellites_valid = true, .supported = 0x2f, .fix = OLED_FIX_3D, .used = 34,
        .accuracy_valid = true, .h_acc_mm = 820, .p_dop_centi = 125, .signals_valid = true,
        .band = {28, 14, 19, 9, 6}, .antenna = OLED_ANTENNA_OK, .jam = OLED_RF_NONE, .spoof = OLED_RF_NONE};
    const uint8_t counts[8] = {[0] = 10, [1] = 2, [2] = 8, [3] = 11, [5] = 3};
    memcpy(g->tracked, counts, sizeof counts);
    static const uint8_t order[] = {1, 0, 2, 3, 6, 5, 7};
    for (unsigned o = 0; o < sizeof order; o++)
        for (unsigned i = 0; i < counts[order[o]]; i++) {
            const uint8_t cno = (uint8_t)(24 + (g->bars * 7) % 27);
            g->bar[g->bars].gnss = order[o];
            g->bar[g->bars].cno = cno;
            g->bars++;
            g->tracked_total++;
            if (cno > g->cno_max) g->cno_max = cno;
        }
    m->network = (oled_network_t){.provisioned = true, .ethernet = true, .ethernet_up = true, .ethernet_ip = {192, 0, 2, 23},
        .wifi = true, .wifi_up = true, .wifi_ip = {198, 51, 100, 7}, .wifi_rssi_valid = true, .wifi_rssi = -61,
        .collector_up = true, .collector = "collector.invalid:8443",
        .station = "board-0003-00112233445566778899aabb89abcdef"};
    m->sensors = (oled_sensors_t){.temperature_valid = true, .humidity_valid = true, .pressure_valid = true,
        .centi_c = 2412, .centi_rh = 5234, .pressure_pa = 101501, .rtc_temperature_valid = true, .rtc_centi_c = 2350,
        .pulse_known = true, .pulse = true,
        .rail = {{true, "+5V", 5064, 308}, {true, "3V3_GNSS", 3296, 92}, {true, "3V3_SYS", 3288, 136}}};
}
// The same board before anyone has configured it: its setup network is up.
static void in_setup(oled_model_t *m)
{
    in_service(m);
    m->network = (oled_network_t){.ethernet = true};
    snprintf(m->network.setup_network, sizeof m->network.setup_network, "navfeeder-A1B2C3");
    snprintf(m->network.setup_reason, sizeof m->network.setup_reason, "collector host is empty");
}

static oled_canvas_t canvas;
static const oled_canvas_t *draw(const oled_model_t *m, oled_page_t page, const char *name)
{
    oled_clear(&canvas);
    canvas.dx = canvas.dy = 0;
    oled_draw_page(&canvas, m, page);
    if (name) save(name, &canvas);
    return &canvas;
}

// Every page keeps to the 126 by 63 picture, and survives each position oled_shift gives it
// without losing a pixel: the picture moved is the picture.
static void stays_on_the_panel(const oled_model_t *m)
{
    static oled_canvas_t moved;
    for (unsigned screen = 0; screen < OLED_PAGE_COUNT + 2; screen++) {
        for (int dy = 0; dy <= OLED_SHIFT_Y; dy++)
            for (int dx = 0; dx <= OLED_SHIFT_X; dx++) {
                oled_clear(&moved);
                moved.dx = (int8_t)dx;
                moved.dy = (int8_t)dy;
                if (screen < OLED_PAGE_COUNT) oled_draw_page(&moved, m, (oled_page_t)screen);
                else if (screen == OLED_PAGE_COUNT) oled_draw_splash(&moved, m);
                else oled_draw_reset_armed(&moved);
                if (!dx && !dy) {
                    canvas = moved;
                    assert(lit(&canvas) > 0);
                    assert(blank(&canvas, OLED_COLUMNS * OLED_ADVANCE, 0, OLED_WIDTH - OLED_COLUMNS * OLED_ADVANCE, OLED_HEIGHT));
                    assert(blank(&canvas, 0, OLED_HEIGHT - 1, OLED_WIDTH, 1));
                }
                for (int y = 0; y < OLED_HEIGHT; y++)
                    for (int x = 0; x < OLED_WIDTH; x++)
                        assert(oled_get(&moved, x, y) == oled_get(&canvas, x - dx, y - dy));
            }
    }
}

static void pages(void)
{
    static oled_model_t m;
    in_service(&m);
    const oled_canvas_t *c = draw(&m, OLED_PAGE_NOW, "now");
    assert(shows(c, 0, 0, 1, "2026-11-22") && shows_right(c, 0, "NOW"));
    assert(shows(c, 0, 11, 2, "12:34:56") && shows(c, 102, 11, 1, "UTC") && shows(c, 102, 20, 1, "GNSS"));
    assert(shows(c, 0, LINE(2), 1, "3D FIX") && shows(c, 48, LINE(2), 1, "34 SV") && shows_right(c, LINE(2), OLED_PLUS_MINUS "82cm"));
    assert(shows(c, 0, LINE(3), 1, "ETH 192.0.2.23"));
    assert(shows(c, 0, LINE(4), 1, "LINK UP") && shows_right(c, LINE(4), "SPOOL 0"));
    assert(shows(c, 0, LINE(5), 1, "24.1" OLED_DEGREE "C 52% 1015hPa"));
    assert(blank(c, 63, 0, 7, 8)); // nothing needs attention: no mark
    // The rule is solid under the first of six pages and dotted along the rest.
    for (int x = 0; x < 126; x++) assert(oled_get(c, x, 8) == (x < 21 || x % 2 == 0));

    c = draw(&m, OLED_PAGE_GNSS, "gnss");
    assert(shows(c, 0, 0, 1, "12:34:56Z") && shows_right(c, 0, "GNSS"));
    assert(shows(c, 0, LINE(0), 1, "3D FIX") && shows_right(c, LINE(0), "34/34 SV"));
    assert(shows(c, 0, LINE(1), 1, "HACC 82cm") && shows_right(c, LINE(1), "PDOP 1.25"));
    // SBAS, GPS, Galileo, BeiDou, GLONASS, QZSS, NavIC: the LED panel's columns.
    static const char *const letters[] = {"S", "G", "E", "C", "R", "J", "I"};
    static const char *const counts[] = {"2", "10", "8", "11", "-", "3", "-"};
    for (int i = 0; i < 7; i++) {
        assert(shows(c, i * 18 + 6, LINE(2), 1, letters[i]));
        assert(shows(c, i * 18 + (17 - oled_text_width(counts[i], 1)) / 2, LINE(3), 1, counts[i]));
    }
    assert(shows(c, 0, LINE(4), 1, "ANT OK") && shows(c, 72, LINE(4), 1, "JAM NONE"));
    // A known periodic source reads as its period where there is no other jamming, and
    // gives way to jamming that is not its crossing.
    m.gnss.pulse_s = 12;
    assert(shows(draw(&m, OLED_PAGE_GNSS, "gnss-pulse"), 72, LINE(4), 1, "PULSE 12s"));
    m.gnss.pulse_s = 99;
    assert(shows(draw(&m, OLED_PAGE_GNSS, NULL), 72, LINE(4), 1, "PULSE 99s"));
    m.gnss.pulse_s = 12; m.gnss.jam = OLED_RF_WARNING;
    assert(shows(draw(&m, OLED_PAGE_GNSS, NULL), 72, LINE(4), 1, "JAM WARN"));
    m.gnss.pulse_s = 0; m.gnss.jam = OLED_RF_NONE;
    assert(shows(c, 0, LINE(5), 1, "SPOOF NONE") && shows(c, 72, LINE(5), 1, "PPS OK"));
    for (int x = 0; x < 126; x++) assert(oled_get(c, x, 8) == ((x >= 21 && x < 42) || x % 2 == 0));

    c = draw(&m, OLED_PAGE_SIGNAL, "signal");
    assert(shows_right(c, 0, "SIGNAL"));
    assert(shows(c, 0, LINE(0), 1, "L1 28") && shows(c, 42, LINE(0), 1, "L2 14") && shows(c, 84, LINE(0), 1, "L5 19"));
    assert(shows(c, 0, LINE(1), 1, "E5b 9") && shows(c, 42, LINE(1), 1, "E6 6") && shows_right(c, LINE(1), "MAX 50"));
    // 34 satellites in five groups: two-pixel bars. The first, SBAS at 24 dB-Hz, is five
    // pixels tall from the baseline at y = 53, with its letter beneath.
    assert(oled_get(c, 0, 53) && oled_get(c, 1, 53) && !oled_get(c, 2, 53) && oled_get(c, 0, 49) && !oled_get(c, 1, 48));
    assert(shows(c, 0, LINE(5), 1, "S") && blank(c, 0, 54, 126, 1));
    // The second group starts two bars and a two-pixel space along.
    assert(oled_get(c, 8, 53) && shows(c, 8, LINE(5), 1, "G"));

    c = draw(&m, OLED_PAGE_NETWORK, "network");
    assert(shows_right(c, 0, "NETWORK") && shows(c, 0, LINE(0), 1, "ETH 192.0.2.23"));
    assert(shows(c, 0, LINE(1), 1, "WIFI 198.51.100.7") && shows_right(c, LINE(1), "-61"));
    assert(shows(c, 0, LINE(2), 1, "COLLECTOR UP") && shows(c, 0, LINE(3), 1, "collector.invalid:844")); // cut at the edge
    assert(shows(c, 0, LINE(4), 1, "SPOOL 0") && shows_right(c, LINE(4), "LOST 0"));
    assert(shows_right(c, LINE(5), "566778899aabb89abcdef")); // the station name's last 21 characters

    c = draw(&m, OLED_PAGE_SENSORS, "sensors");
    assert(shows_right(c, 0, "SENSORS") && shows(c, 0, LINE(0), 1, "24.12" OLED_DEGREE "C") && shows_right(c, LINE(0), "52.3%RH"));
    assert(shows(c, 0, LINE(1), 1, "1015.0hPa") && shows_right(c, LINE(1), "RTC 23.5" OLED_DEGREE "C"));
    assert(shows(c, 0, LINE(2), 1, "+5V") && shows(c, 60, LINE(2), 1, "5.06V") && shows_right(c, LINE(2), "308mA"));
    assert(shows(c, 0, LINE(3), 1, "3V3_GNSS") && shows(c, 60, LINE(3), 1, "3.30V") && shows_right(c, LINE(3), "92mA"));
    assert(shows(c, 0, LINE(4), 1, "3V3_SYS") && shows(c, 60, LINE(4), 1, "3.29V") && shows_right(c, LINE(4), "136mA"));
    assert(shows(c, 0, LINE(5), 1, "PPS OK"));

    c = draw(&m, OLED_PAGE_SYSTEM, "system");
    assert(shows_right(c, 0, "SYSTEM") && shows(c, 0, LINE(0), 1, "X20 revision A"));
    assert(shows(c, 0, LINE(1), 1, "SERIAL ...89abcdef") && shows(c, 0, LINE(2), 1, "FW 0.1.0+1.0f7d658"));
    assert(shows(c, 0, LINE(3), 1, "UP 3d 04:05") && shows_right(c, LINE(3), "BOOT 12") && shows(c, 0, LINE(4), 1, "RX ZED-X20P"));
    // The LNA mode a unit sets its receiver to stands beside the receiver; a receiver without
    // one, and a receiver whose name leaves it no room, stand alone.
    assert(blank(c, 66, LINE(4), 60, 8));
    m.receiver_lna = OLED_LNA_LOW;
    c = draw(&m, OLED_PAGE_SYSTEM, "system-lna");
    assert(shows(c, 0, LINE(4), 1, "RX ZED-X20P") && shows_right(c, LINE(4), "LNA LOW"));
    m.receiver_lna = OLED_LNA_NORMAL;
    assert(shows_right(draw(&m, OLED_PAGE_SYSTEM, NULL), LINE(4), "LNA NORM"));
    snprintf(m.receiver, sizeof m.receiver, "%s", "A-LONGER-RECEIVER");
    c = draw(&m, OLED_PAGE_SYSTEM, NULL);
    assert(shows(c, 0, LINE(4), 1, "RX A-LONGER-RECEIVER") && blank(c, 120, LINE(4), 6, 8));
    snprintf(m.receiver, sizeof m.receiver, "%s", "ZED-X20P");
    c = draw(&m, OLED_PAGE_SYSTEM, NULL);
    m.uptime_s = 23 * 3600 + 59 * 60 + 58; // seconds until there are days
    assert(shows(draw(&m, OLED_PAGE_SYSTEM, NULL), 0, LINE(3), 1, "UP 23:59:58"));
    in_service(&m);
    assert(shows(c, 0, LINE(5), 1, "HEAP 128K") && shows_right(c, LINE(5), "TIME GNSS"));
    stays_on_the_panel(&m);

    // Negative temperatures, a float solution in centimetres, a tunnel, a backlog.
    m.sensors.centi_c = -1234;
    m.sensors.centi_rh = 10000;
    m.gnss.fix = OLED_FIX_RTK_FLOAT;
    m.gnss.h_acc_mm = 14;
    m.network.ethernet_up = false;
    m.network.tunnel = true;
    m.network.spool = 65536;
    c = draw(&m, OLED_PAGE_NOW, "now-float");
    assert(shows(c, 0, LINE(2), 1, "RTK FLT") && shows(c, 48, LINE(2), 1, "34 SV") && shows_right(c, LINE(2), OLED_PLUS_MINUS "1.4cm"));
    assert(shows(c, 0, LINE(3), 1, "WIFI 198.51.100.7") && shows(c, 0, LINE(4), 1, "TUNNEL UP") && shows_right(c, LINE(4), "SPOOL 65536"));
    assert(shows(c, 0, LINE(5), 1, "-12.3" OLED_DEGREE "C 100% 1015hPa"));
    c = draw(&m, OLED_PAGE_NETWORK, "network-tunnel");
    assert(shows(c, 0, LINE(0), 1, "ETH no address") && shows_right(c, LINE(2), "TUNNEL"));
    m.gnss.h_acc_mm = 1234;
    assert(shows_right(draw(&m, OLED_PAGE_NOW, NULL), LINE(2), OLED_PLUS_MINUS "1.2m"));
    m.gnss.h_acc_mm = 45600;
    assert(shows_right(draw(&m, OLED_PAGE_NOW, NULL), LINE(2), OLED_PLUS_MINUS "46m"));
    m.gnss.h_acc_mm = 2000000;
    assert(shows_right(draw(&m, OLED_PAGE_NOW, NULL), LINE(2), OLED_PLUS_MINUS ">1km"));
    stays_on_the_panel(&m);

    // The widest values every field can take still fit their places.
    in_service(&m);
    m.uptime_s = UINT32_MAX;
    m.boot = UINT32_MAX;
    m.heap_free = UINT32_MAX;
    m.sensors.centi_c = INT16_MIN;
    m.sensors.centi_rh = UINT16_MAX;
    m.sensors.pressure_pa = UINT32_MAX;
    m.sensors.rtc_centi_c = INT16_MIN;
    for (unsigned i = 0; i < OLED_RAILS; i++) { m.sensors.rail[i].bus_mv = INT16_MIN; m.sensors.rail[i].ma = INT32_MIN; }
    m.sensors.rail_alert = true;
    m.receiver_attention = 0xff;
    m.reception_alarm = true;
    m.network.spool = UINT32_MAX;
    m.network.dropped = UINT64_MAX;
    memset(m.network.ethernet_ip, 255, 4);
    memset(m.network.wifi_ip, 255, 4);
    m.network.wifi_rssi = -128;
    m.gnss.used = 255;
    m.gnss.tracked_total = 65535;
    m.gnss.h_acc_mm = UINT32_MAX;
    m.gnss.p_dop_centi = UINT16_MAX;
    m.gnss.cno_max = 255;
    memset(m.gnss.tracked, 255, sizeof m.gnss.tracked);
    memset(m.gnss.band, 255, sizeof m.gnss.band);
    m.gnss.supported = 0xff;
    m.gnss.bars = OLED_BARS;
    for (unsigned i = 0; i < OLED_BARS; i++) { m.gnss.bar[i].gnss = (uint8_t)(i % 8); m.gnss.bar[i].cno = 255; }
    memset(m.firmware, 'W', sizeof m.firmware - 1);
    memset(m.board, 'W', sizeof m.board - 1);
    memset(m.receiver, 'W', sizeof m.receiver - 1);
    memset(m.network.collector, 'W', sizeof m.network.collector - 1);
    memset(m.network.station, 'W', sizeof m.network.station - 1);
    m.utc = utc(9999, 12, 31, 23, 59, 59);
    c = draw(&m, OLED_PAGE_NOW, "widest-now");
    // A value that has outgrown its place is left out, not drawn over its neighbour.
    assert(shows(c, 0, LINE(4), 1, "LINK UP") && blank(c, 42, LINE(4), 84, 8));
    assert(shows(c, 0, LINE(2), 1, "3D FIX  255 SV") && shows_right(c, LINE(2), OLED_PLUS_MINUS ">1km"));
    save("widest-signal", draw(&m, OLED_PAGE_SIGNAL, NULL));
    stays_on_the_panel(&m);
    m.utc = INT64_MAX; // no date the display has room for
    assert(shows(draw(&m, OLED_PAGE_NOW, NULL), 0, 0, 1, "NO DATE"));
    stays_on_the_panel(&m);
    m.utc = INT64_MIN;
    stays_on_the_panel(&m);

    // Before the clock is set.
    in_service(&m);
    m.time_valid = false;
    c = draw(&m, OLED_PAGE_NOW, "now-no-time");
    assert(shows(c, 0, 0, 1, "NO DATE") && shows(c, 0, 11, 2, "--:--:--") && shows(c, 102, 11, 1, "NO") && shows(c, 102, 20, 1, "TIME"));
    assert(shows(draw(&m, OLED_PAGE_GNSS, NULL), 0, 0, 1, "--:--:--"));
    assert(blank(draw(&m, OLED_PAGE_SYSTEM, NULL), 60, LINE(5), 66, 8)); // no time source to name

    // A receiver that says nothing, and one without a fix.
    in_service(&m);
    m.gnss = (oled_gnss_t){.supported = 0x2f};
    c = draw(&m, OLED_PAGE_NOW, "now-no-receiver");
    assert(shows(c, 0, LINE(2), 1, "NO RECEIVER DATA") && blank(c, 63, 0, 7, 8)); // no mark until the board calls it a fault
    c = draw(&m, OLED_PAGE_GNSS, "gnss-no-receiver");
    assert(shows(c, 0, LINE(0), 1, "NO RECEIVER DATA") && shows(c, 0, LINE(1), 1, "HACC --") && shows_right(c, LINE(1), "PDOP --"));
    assert(shows(c, 6, LINE(3), 1, "?") && shows(c, 4 * 18 + 6, LINE(3), 1, "-")); // counts unknown, GLONASS not tracked
    assert(shows(c, 0, LINE(4), 1, "ANT ?") && shows(c, 72, LINE(4), 1, "JAM ?") && shows(c, 0, LINE(5), 1, "SPOOF ?"));
    assert(!oled_page_available(&m, OLED_PAGE_SIGNAL));
    m.gnss.receiving = m.gnss.satellites_valid = true;
    m.gnss.tracked[0] = 3;
    m.gnss.tracked_total = 3;
    c = draw(&m, OLED_PAGE_NOW, NULL);
    assert(shows(c, 0, LINE(2), 1, "NO FIX") && shows(c, 48, LINE(2), 1, "3 SV") && blank(c, 84, LINE(2), 42, 8));
    assert(shows_right(draw(&m, OLED_PAGE_GNSS, NULL), LINE(0), "3 SV"));
    // A receiver that reports its signals without naming their bands (a Septentrio's).
    m.gnss.bars = 3;
    m.gnss.cno_max = 44;
    for (unsigned i = 0; i < 3; i++) { m.gnss.bar[i].gnss = 0; m.gnss.bar[i].cno = (uint8_t)(40 + 2 * i); }
    c = draw(&m, OLED_PAGE_SIGNAL, "signal-few");
    assert(shows(c, 0, LINE(0), 1, "TRACKED 3") && shows_right(c, LINE(1), "MAX 44"));
    assert(oled_get(c, 0, 53) && oled_get(c, 2, 53) && !oled_get(c, 3, 53) && oled_get(c, 4, 53)); // three pixels and a gap

    // A board no one has configured: how to configure it, and why it is asking. The setup
    // password is on the label; no page shows it.
    in_setup(&m);
    c = draw(&m, OLED_PAGE_NOW, "now-setup");
    assert(shows(c, 0, LINE(3), 1, "AP navfeeder-A1B2C3") && shows(c, 0, LINE(4), 1, "NOT PROVISIONED"));
    c = draw(&m, OLED_PAGE_NETWORK, "setup");
    assert(shows_right(c, 0, "SETUP") && shows(c, 0, LINE(0), 1, "JOIN WI-FI NETWORK") && shows(c, 0, LINE(1), 1, "navfeeder-A1B2C3"));
    assert(shows(c, 0, LINE(2), 1, "PASSWORD ON THE LABEL") && shows(c, 0, LINE(3), 1, "http://192.168.4.1"));
    assert(shows(c, 0, LINE(4), 1, "collector host is") && blank(c, 102, LINE(4), 24, 8) && shows(c, 0, LINE(5), 1, "empty"));
    assert(!oled_attention(&m, NULL)); // setup is not a fault
    stays_on_the_panel(&m);
    m.network.setup_network[0] = 0; // the setup network could not start
    c = draw(&m, OLED_PAGE_NETWORK, NULL);
    assert(shows(c, 0, LINE(0), 1, "NOT PROVISIONED") && shows(c, 0, LINE(1), 1, "NO SETUP NETWORK") && shows(c, 0, LINE(2), 1, "collector host is"));
    assert(shows(draw(&m, OLED_PAGE_NOW, NULL), 0, LINE(3), 1, "NO SETUP NETWORK"));
    // A reason with no space to break at is cut at the edge, and one longer than the lines
    // left stops with the page.
    snprintf(m.network.setup_reason, sizeof m.network.setup_reason, "abcdefghijklmnopqrstuvwxyz0123456789ABCDE");
    c = draw(&m, OLED_PAGE_NETWORK, NULL);
    assert(shows(c, 0, LINE(2), 1, "abcdefghijklmnopqrstu") && shows(c, 0, LINE(3), 1, "vwxyz0123456789ABCDE"));
    in_setup(&m);
    snprintf(m.network.setup_reason, sizeof m.network.setup_reason, "one two three four five six seven eight nin");
    c = draw(&m, OLED_PAGE_NETWORK, "setup-long-reason");
    assert(shows(c, 0, LINE(4), 1, "one two three four") && shows(c, 0, LINE(5), 1, "five six seven eight"));

    // A board with no sensors names itself where the weather would be, and has no sensor page.
    in_service(&m);
    m.sensors = (oled_sensors_t){0};
    assert(shows(draw(&m, OLED_PAGE_NOW, NULL), 0, LINE(5), 1, "X20 revision A") && !oled_page_available(&m, OLED_PAGE_SENSORS));
    m.sensors.rail[1].valid = true; // one rail alone is a page
    snprintf(m.sensors.rail[1].name, sizeof m.sensors.rail[1].name, "3V3_GNSS");
    snprintf(m.sensors.rail[0].name, sizeof m.sensors.rail[0].name, "+5V");
    assert(oled_page_available(&m, OLED_PAGE_SENSORS));
    c = draw(&m, OLED_PAGE_SENSORS, "sensors-partial");
    assert(shows(c, 0, LINE(0), 1, "--" OLED_DEGREE "C") && shows_right(c, LINE(0), "--%RH") && shows(c, 0, LINE(1), 1, "--hPa"));
    assert(shows(c, 0, LINE(2), 1, "+5V") && shows_right(c, LINE(2), "--") && shows_right(c, LINE(3), "0mA") && blank(c, 0, LINE(4), 126, 17));

    in_service(&m);
    oled_clear(&canvas);
    canvas.dx = canvas.dy = 0;
    oled_draw_splash(&canvas, &m);
    save("splash", &canvas);
    assert(shows(&canvas, 9, 2, 2, "NavListen") && shows(&canvas, 21, 26, 1, "X20 revision A"));
    assert(shows(&canvas, 9, 36, 1, "SERIAL ...89abcdef") && shows(&canvas, 18, 46, 1, "0.1.0+1.0f7d658"));
    oled_clear(&canvas);
    oled_draw_reset_armed(&canvas);
    save("reset-armed", &canvas);
    assert(oled_get(&canvas, 0, 0) && oled_get(&canvas, 125, 10) && !oled_get(&canvas, 0, 11)); // the inverted banner
    assert(shows(&canvas, 0, 17, 1, "RELEASE BOOT TO ERASE") && shows(&canvas, 0, 27, 1, "THE NETWORK SETTINGS") &&
           shows(&canvas, 0, 43, 1, "POWER CYCLE TO CANCEL"));
}

static void attention(void)
{
    static oled_model_t m;
    char lines[OLED_ATTENTION_MAX][OLED_TEXT];
    in_service(&m);
    assert(oled_attention(&m, lines) == 0 && !oled_page_available(&m, OLED_PAGE_ATTENTION));

    // The lamp's causes, most urgent first. Six lines are all the page holds: with all eight
    // set (the two jamming states never are together) the last two are left to the mark.
    m.receiver_attention = GNSS_ATTENTION_SPOOFING | GNSS_ATTENTION_JAMMING_CRITICAL | GNSS_ATTENTION_ANTENNA_SHORT |
                           GNSS_ATTENTION_ANTENNA_OPEN | GNSS_ATTENTION_SILENT;
    m.reception_alarm = true;
    m.sensors.rail_alert = true;
    assert(oled_attention(&m, lines) == OLED_ATTENTION_MAX && oled_attention(&m, NULL) == OLED_ATTENTION_MAX);
    assert(!strcmp(lines[0], "SPOOFING INDICATED") && !strcmp(lines[1], "JAMMING CRITICAL") && !strcmp(lines[2], "ANTENNA SHORT"));
    assert(!strcmp(lines[3], "ANTENNA OPEN") && !strcmp(lines[4], "NO RECEIVER DATA") && !strcmp(lines[5], "RECEPTION ALARM"));
    const oled_canvas_t *c = draw(&m, OLED_PAGE_ATTENTION, "attention");
    assert(shows_right(c, 0, "ATTENTION") && shows(c, 0, LINE(0), 1, "SPOOFING INDICATED") && shows(c, 0, LINE(5), 1, "RECEPTION ALARM"));
    assert(blank(c, 63, 0, 7, 8)); // its own page carries no mark
    c = draw(&m, OLED_PAGE_GNSS, "gnss-attention");
    assert(oled_get(c, 63, 0) && oled_get(c, 69, 7) && !oled_get(c, 66, 1)); // the mark: a lit box, the stroke dark
    // Seven pages now: the rule's solid part is a seventh of it.
    for (int x = 0; x < 126; x++) assert(oled_get(c, x, 8) == ((x >= 18 && x < 36) || x % 2 == 0));
    stays_on_the_panel(&m);

    // Each cause alone.
    static const struct { uint8_t receiver; bool reception, rail; const char *line; } causes[] = {
        {GNSS_ATTENTION_SPOOFING, false, false, "SPOOFING INDICATED"}, {GNSS_ATTENTION_JAMMING_CRITICAL, false, false, "JAMMING CRITICAL"},
        {GNSS_ATTENTION_JAMMING, false, false, "JAMMING WARNING"}, {GNSS_ATTENTION_ANTENNA_SHORT, false, false, "ANTENNA SHORT"},
        {GNSS_ATTENTION_ANTENNA_OPEN, false, false, "ANTENNA OPEN"}, {GNSS_ATTENTION_SILENT, false, false, "NO RECEIVER DATA"},
        {0, true, false, "RECEPTION ALARM"}, {0, false, true, "POWER RAIL ALERT"},
    };
    for (unsigned i = 0; i < sizeof causes / sizeof causes[0]; i++) {
        in_service(&m);
        m.receiver_attention = causes[i].receiver;
        m.reception_alarm = causes[i].reception;
        m.sensors.rail_alert = causes[i].rail;
        assert(oled_attention(&m, lines) == 1 && !strcmp(lines[0], causes[i].line) && oled_page_available(&m, OLED_PAGE_ATTENTION));
        assert(shows(draw(&m, OLED_PAGE_ATTENTION, NULL), 0, LINE(0), 1, causes[i].line));
    }

    // The GNSS page words the receiver's states itself, from its own reading of them.
    in_service(&m);
    m.receiver_attention = GNSS_ATTENTION_JAMMING;
    m.gnss.jam = OLED_RF_WARNING;
    m.gnss.security_fresh = true;
    m.gnss.solution_fresh = true;
    m.gnss.assessed = 5;
    m.gnss.jammed = 4;
    memcpy(m.gnss.jammed_khz, (uint32_t[]){1575420, 1227600, 1176450, 1246000}, 4*sizeof(uint32_t));
    m.uptime_s = 0;
    c = draw(&m, OLED_PAGE_ATTENTION, NULL);
    assert(shows(c, 0, LINE(2), 1, "4/5 FLAGGED") && shows(c, 0, LINE(3), 1, "1575.420 MHz L1/E1"));
    m.uptime_s = 4;
    assert(shows(draw(&m, OLED_PAGE_ATTENTION, NULL), 0, LINE(3), 1, "1246.000 MHz G2"));
    m.gnss.assessed = m.gnss.jammed = 0;
    assert(shows(draw(&m, OLED_PAGE_ATTENTION, NULL), 0, LINE(2), 1, "NO CENTERS ASSESSED"));

    in_service(&m);
    m.gnss.antenna = OLED_ANTENNA_SHORT;
    m.gnss.jam = OLED_RF_CRITICAL;
    m.gnss.spoof = OLED_RF_WARNING;
    c = draw(&m, OLED_PAGE_GNSS, NULL);
    assert(shows(c, 0, LINE(4), 1, "ANT SHORT") && shows(c, 72, LINE(4), 1, "JAM CRIT") && shows(c, 0, LINE(5), 1, "SPOOF YES"));
    m.gnss.antenna = OLED_ANTENNA_OPEN;
    m.gnss.jam = OLED_RF_WARNING;
    m.gnss.spoof = OLED_RF_CRITICAL;
    c = draw(&m, OLED_PAGE_GNSS, NULL);
    assert(shows(c, 0, LINE(4), 1, "ANT OPEN") && shows(c, 72, LINE(4), 1, "JAM WARN") && shows(c, 0, LINE(5), 1, "SPOOF MULTI"));
    assert(shows(c, 72, LINE(5), 1, "PPS OK")); // the longest spoofing text leaves the pulse its place

    // Trouble with the network is not the lamp's business: the uplink lamp and the NOW and
    // NETWORK pages show it. Records lost to a full spool are counted on the NETWORK page.
    in_service(&m);
    m.network.ethernet_up = m.network.wifi_up = m.network.collector_up = false;
    m.network.dropped = 7;
    assert(oled_attention(&m, NULL) == 0);
    c = draw(&m, OLED_PAGE_NOW, NULL);
    assert(shows(c, 0, LINE(3), 1, "NO UPLINK") && shows(c, 0, LINE(4), 1, "LINK DOWN") && blank(c, 63, 0, 7, 8));
    c = draw(&m, OLED_PAGE_NETWORK, NULL);
    assert(shows(c, 0, LINE(2), 1, "COLLECTOR DOWN") && shows_right(c, LINE(4), "LOST 7"));
    m.network.dropped = UINT64_MAX;
    assert(shows_right(draw(&m, OLED_PAGE_NETWORK, NULL), LINE(4), "LOST >9999999"));
}

static void director(void)
{
    static oled_model_t m;
    oled_director_t d = {0};
    oled_page_t page = OLED_PAGE_COUNT;
    in_service(&m);
    // The start screen for four seconds from the first step, whenever that is.
    assert(oled_director_step(&d, &m, 100000, &page) == OLED_SCREEN_SPLASH);
    assert(oled_director_step(&d, &m, 100000 + OLED_SPLASH_MS - 1, &page) == OLED_SCREEN_SPLASH);
    // Then the pages in turn, six seconds each, the unavailable ones left out.
    static const oled_page_t order[] = {OLED_PAGE_NOW, OLED_PAGE_GNSS, OLED_PAGE_SIGNAL, OLED_PAGE_NETWORK,
                                        OLED_PAGE_SENSORS, OLED_PAGE_SYSTEM, OLED_PAGE_NOW};
    int64_t now = 100000 + OLED_SPLASH_MS;
    for (unsigned i = 0; i < sizeof order / sizeof order[0]; i++, now += OLED_DWELL_MS) {
        assert(oled_director_step(&d, &m, now, &page) == OLED_SCREEN_PAGE && page == order[i]);
        assert(oled_director_step(&d, &m, now + OLED_DWELL_MS - 1, &page) == OLED_SCREEN_PAGE && page == order[i]);
    }
    // now is when NOW's second showing ends, and GNSS is next.
    assert(oled_director_step(&d, &m, now, &page) == OLED_SCREEN_PAGE && page == OLED_PAGE_GNSS);

    // Something goes wrong a second into that page: ATTENTION is shown at once, for its six
    // seconds, and then takes its place in the round.
    m.receiver_attention = GNSS_ATTENTION_JAMMING;
    assert(oled_director_step(&d, &m, now + 1000, &page) == OLED_SCREEN_PAGE && page == OLED_PAGE_ATTENTION);
    assert(oled_director_step(&d, &m, now + 1000 + OLED_DWELL_MS - 1, &page) == OLED_SCREEN_PAGE && page == OLED_PAGE_ATTENTION);
    now += 1000 + OLED_DWELL_MS;
    static const oled_page_t with_attention[] = {OLED_PAGE_NOW, OLED_PAGE_GNSS, OLED_PAGE_SIGNAL, OLED_PAGE_NETWORK,
                                                 OLED_PAGE_SENSORS, OLED_PAGE_SYSTEM, OLED_PAGE_ATTENTION, OLED_PAGE_NOW};
    for (unsigned i = 0; i < sizeof with_attention / sizeof with_attention[0]; i++, now += OLED_DWELL_MS)
        assert(oled_director_step(&d, &m, now, &page) == OLED_SCREEN_PAGE && page == with_attention[i]);
    // A second cause is news too, and one that only stays is not: the page is not shown again
    // for it. now is when that NOW ends.
    assert(oled_director_step(&d, &m, now, &page) == OLED_SCREEN_PAGE && page == OLED_PAGE_GNSS);
    m.reception_alarm = true;
    assert(oled_director_step(&d, &m, now + 500, &page) == OLED_SCREEN_PAGE && page == OLED_PAGE_ATTENTION);
    now += 500 + OLED_DWELL_MS;
    assert(oled_director_step(&d, &m, now, &page) == OLED_SCREEN_PAGE && page == OLED_PAGE_NOW);
    m.receiver_attention = 0; // the jamming ends; the alarm stays: nothing new
    assert(oled_director_step(&d, &m, now + 100, &page) == OLED_SCREEN_PAGE && page == OLED_PAGE_NOW);
    // The page is left at once when nothing needs attention any more.
    for (unsigned i = 0; i < 6; i++) { now += OLED_DWELL_MS; (void)oled_director_step(&d, &m, now, &page); }
    assert(page == OLED_PAGE_ATTENTION);
    m.reception_alarm = false;
    assert(oled_director_step(&d, &m, now + 1, &page) == OLED_SCREEN_PAGE && page == OLED_PAGE_NOW);
    now += 1;

    // A press moves on at once and holds that page a minute; a second press moves on again.
    oled_director_next(&d, &m, now + 1000);
    assert(oled_director_step(&d, &m, now + 1000, &page) == OLED_SCREEN_PAGE && page == OLED_PAGE_GNSS);
    assert(oled_director_step(&d, &m, now + 1000 + OLED_HOLD_MS - 1, &page) == OLED_SCREEN_PAGE && page == OLED_PAGE_GNSS);
    oled_director_next(&d, &m, now + 2000);
    assert(oled_director_step(&d, &m, now + 2000, &page) == OLED_SCREEN_PAGE && page == OLED_PAGE_SIGNAL);
    // A page held by hand is not taken away by news: the mark in its header says there is some.
    m.sensors.rail_alert = true;
    assert(oled_director_step(&d, &m, now + 3000, &page) == OLED_SCREEN_PAGE && page == OLED_PAGE_SIGNAL);
    assert(oled_director_step(&d, &m, now + 2000 + OLED_HOLD_MS - 1, &page) == OLED_SCREEN_PAGE && page == OLED_PAGE_SIGNAL);
    assert(oled_director_step(&d, &m, now + 2000 + OLED_HOLD_MS, &page) == OLED_SCREEN_PAGE && page == OLED_PAGE_NETWORK);
    m.sensors.rail_alert = false;
    now += 2000 + OLED_HOLD_MS;
    // A held page that stops being available is released too.
    oled_director_next(&d, &m, now);     // SENSORS
    oled_director_next(&d, &m, now);     // SYSTEM
    oled_director_next(&d, &m, now);     // NOW
    oled_director_next(&d, &m, now);     // GNSS
    oled_director_next(&d, &m, now);     // SIGNAL, held
    assert(oled_director_step(&d, &m, now, &page) == OLED_SCREEN_PAGE && page == OLED_PAGE_SIGNAL);
    m.gnss.satellites_valid = false;
    assert(oled_director_step(&d, &m, now + 10, &page) == OLED_SCREEN_PAGE && page == OLED_PAGE_NETWORK);
    assert(oled_director_step(&d, &m, now + 10 + OLED_DWELL_MS, &page) == OLED_SCREEN_PAGE && page == OLED_PAGE_SENSORS);

    // The reset warning takes the display for as long as the reset is armed, the start screen
    // included, and the pages resume where they were.
    m.reset_armed = true;
    assert(oled_director_step(&d, &m, now + 20 + OLED_DWELL_MS, &page) == OLED_SCREEN_RESET_ARMED);
    m.reset_armed = false;
    assert(oled_director_step(&d, &m, now + 30 + OLED_DWELL_MS, &page) == OLED_SCREEN_PAGE && page == OLED_PAGE_SENSORS);
    oled_director_t fresh = {0};
    m.reset_armed = true;
    assert(oled_director_step(&fresh, &m, 0, &page) == OLED_SCREEN_RESET_ARMED);
    m.reset_armed = false;
    assert(oled_director_step(&fresh, &m, 1, &page) == OLED_SCREEN_SPLASH);
    // What already needed attention when the board started is not news once the start screen
    // ends: the round begins with NOW, and ATTENTION comes in its turn.
    oled_director_t alarmed = {0};
    m.receiver_attention = GNSS_ATTENTION_JAMMING;
    assert(oled_director_step(&alarmed, &m, 0, &page) == OLED_SCREEN_SPLASH);
    assert(oled_director_step(&alarmed, &m, OLED_SPLASH_MS, &page) == OLED_SCREEN_PAGE && page == OLED_PAGE_NOW);
    m.receiver_attention = 0;
    // A press before the first step does nothing.
    oled_director_t idle = {0};
    oled_director_next(&idle, &m, 5);
    assert(!idle.started);

    // The picture's six positions, a minute each, every step one pixel from the last.
    int8_t dx = 9, dy = 9, previous_x = 0, previous_y = 1;
    bool seen[OLED_SHIFT_X + 1][OLED_SHIFT_Y + 1] = {{false}};
    for (unsigned step = 0; step < 12; step++) {
        oled_shift((int64_t)step * OLED_SHIFT_MS, &dx, &dy);
        assert(dx >= 0 && dx <= OLED_SHIFT_X && dy >= 0 && dy <= OLED_SHIFT_Y);
        assert(abs(dx - previous_x) + abs(dy - previous_y) == 1);
        int8_t late_x, late_y;
        oled_shift((int64_t)step * OLED_SHIFT_MS + OLED_SHIFT_MS - 1, &late_x, &late_y);
        assert(late_x == dx && late_y == dy);
        seen[dx][dy] = true;
        previous_x = dx;
        previous_y = dy;
    }
    for (unsigned x = 0; x <= OLED_SHIFT_X; x++)
        for (unsigned y = 0; y <= OLED_SHIFT_Y; y++) assert(seen[x][y]);
    oled_shift(-5, &dx, &dy);
    assert(dx == 0 && dy == 0);

    assert(oled_contrast(0) == 1 && oled_contrast(1) == 1 && oled_contrast(100) == 255 && oled_contrast(1000) == 255);
    assert(oled_contrast(50) == 126 && oled_contrast(20) == 49 && oled_contrast(10) == 24);
    for (unsigned percent = 1; percent < 100; percent++) assert(oled_contrast(percent) < oled_contrast(percent + 1));

    oled_page_t parsed;
    for (unsigned p = 0; p < OLED_PAGE_COUNT; p++)
        assert(oled_page_parse(oled_page_name((oled_page_t)p), &parsed) && parsed == (oled_page_t)p);
    assert(!oled_page_parse("clock", &parsed) && !oled_page_parse(NULL, &parsed) && !strcmp(oled_page_name(OLED_PAGE_COUNT), "unknown"));
}

static void put16(uint8_t *p, unsigned v) { p[0] = (uint8_t)v; p[1] = (uint8_t)(v >> 8); }
static void put32(uint8_t *p, uint32_t v) { for (unsigned i = 0; i < 4; i++) p[i] = (uint8_t)(v >> (8 * i)); }
// A NAV-PVT with a fix of the given type and flags, 0.82 m and PDOP 1.25.
static void solution(gnss_status_t *s, uint8_t type, uint8_t flags, uint8_t used, int64_t now)
{
    uint8_t p[92] = {0};
    put16(p + 4, 2026); p[6] = 11; p[7] = 22; p[8] = 12; p[9] = 34; p[10] = 56; p[11] = 7;
    p[20] = type; p[21] = flags; p[23] = used;
    put32(p + 24, 10000000); put32(p + 28, 20000000); // 1 E, 2 N: nowhere in particular
    put32(p + 40, 820); put16(p + 76, 125);
    gnss_status_feed(s, 1, 7, p, sizeof p, now);
}

static void from_receiver(void)
{
    static gnss_status_t s;
    static oled_gnss_t g;
    s = (gnss_status_t){.supported = 0x2f};
    // Nothing received: nothing shown, and the 15 s after start are not mistaken for reports.
    oled_gnss_from_status(&g, &s, 5000);
    assert(!g.receiving && !g.satellites_valid && g.fix == OLED_FIX_NONE && !g.bars && g.supported == 0x2f);
    assert(g.antenna == OLED_ANTENNA_UNKNOWN && g.jam == OLED_RF_UNKNOWN && g.spoof == OLED_RF_UNKNOWN);

    // NAV-SAT: GPS 5 and 12, SBAS 131, BeiDou 20 and Galileo 3 tracked; GPS 9 only acquired.
    static const struct { uint8_t gnss, sv, cno, quality; } sats[] = {
        {0, 12, 38, 5}, {3, 20, 41, 7}, {0, 5, 45, 4}, {1, 131, 33, 4}, {2, 3, 47, 6}, {0, 9, 20, 3},
    };
    uint8_t sat[8 + 12 * 6] = {0};
    sat[4] = 1; sat[5] = 6;
    for (unsigned i = 0; i < 6; i++) {
        uint8_t *r = sat + 8 + 12 * i;
        r[0] = sats[i].gnss; r[1] = sats[i].sv; r[2] = sats[i].cno; r[3] = 30; r[8] = sats[i].quality;
    }
    gnss_status_feed(&s, 1, 0x35, sat, sizeof sat, 1000);
    oled_gnss_from_status(&g, &s, 1500);
    assert(g.receiving && g.satellites_valid && g.tracked_total == 5 && g.tracked[0] == 2 && g.tracked[1] == 1);
    assert(g.fix == OLED_FIX_NONE && g.used == 0 && !g.accuracy_valid);
    // The bars: SBAS, GPS by number, Galileo, BeiDou; the strongest is Galileo's 47 dB-Hz.
    assert(g.bars == 5 && g.cno_max == 47);
    static const struct { uint8_t gnss, cno; } bars[] = {{1, 33}, {0, 45}, {0, 38}, {2, 47}, {3, 41}};
    for (unsigned i = 0; i < 5; i++) assert(g.bar[i].gnss == bars[i].gnss && g.bar[i].cno == bars[i].cno);

    // NAV-PVT fix types and flags: gnssFixOK is bit 0, diffSoln bit 1, carrSoln bits 6 and 7.
    solution(&s, 3, 0x01, 4, 2000);
    oled_gnss_from_status(&g, &s, 2100);
    assert(g.fix == OLED_FIX_3D && g.used == 4 && g.accuracy_valid && g.h_acc_mm == 820 && g.p_dop_centi == 125);
    solution(&s, 2, 0x01, 3, 3000); oled_gnss_from_status(&g, &s, 3000); assert(g.fix == OLED_FIX_2D);
    solution(&s, 4, 0x01, 3, 4000); oled_gnss_from_status(&g, &s, 4000); assert(g.fix == OLED_FIX_DEAD_RECKONING);
    solution(&s, 3, 0x03, 9, 5000); oled_gnss_from_status(&g, &s, 5000); assert(g.fix == OLED_FIX_DGNSS);
    solution(&s, 3, 0x43, 9, 6000); oled_gnss_from_status(&g, &s, 6000); assert(g.fix == OLED_FIX_RTK_FLOAT);
    solution(&s, 3, 0x83, 9, 7000); oled_gnss_from_status(&g, &s, 7000); assert(g.fix == OLED_FIX_RTK_FIXED);
    // Time only, a fix the receiver does not vouch for, and none: no position, no accuracy.
    solution(&s, 5, 0x01, 1, 8000); oled_gnss_from_status(&g, &s, 8000);
    assert(g.fix == OLED_FIX_TIME && !g.accuracy_valid && g.used == 0 && s.h_acc_mm == 0 && s.p_dop == 0);
    solution(&s, 3, 0x00, 4, 9000); oled_gnss_from_status(&g, &s, 9000); assert(g.fix == OLED_FIX_NONE);
    solution(&s, 0, 0x00, 0, 10000); oled_gnss_from_status(&g, &s, 10000); assert(g.fix == OLED_FIX_NONE && g.receiving);
    // A position without a fix type is still a position: a Septentrio receiver's status.
    solution(&s, 3, 0x01, 4, 11000);
    memset(s.pvt_utc + 16, 0, 4);
    s.h_acc_mm = 0;
    oled_gnss_from_status(&g, &s, 11000);
    assert(g.fix == OLED_FIX_POSITION && g.used == 0 && !g.accuracy_valid);

    // MON-RF: two blocks, both OK; then one SHORT; then one OPEN; then one still initializing.
    uint8_t rf[4 + 2 * 24] = {0};
    rf[1] = 2; rf[4 + 1] = 1; rf[4 + 2] = 2; rf[28] = 1; rf[28 + 1] = 1; rf[28 + 2] = 2; // jamming flag 1: none
    gnss_status_feed(&s, 0x0a, 0x38, rf, sizeof rf, 12000);
    oled_gnss_from_status(&g, &s, 12000);
    assert(g.antenna == OLED_ANTENNA_OK && g.jam == OLED_RF_NONE);
    rf[28 + 2] = 3; rf[28 + 1] = 2;
    gnss_status_feed(&s, 0x0a, 0x38, rf, sizeof rf, 12100);
    oled_gnss_from_status(&g, &s, 12100);
    assert(g.antenna == OLED_ANTENNA_SHORT && g.jam == OLED_RF_WARNING);
    rf[28 + 2] = 4; rf[28 + 1] = 3;
    gnss_status_feed(&s, 0x0a, 0x38, rf, sizeof rf, 12200);
    oled_gnss_from_status(&g, &s, 12200);
    assert(g.antenna == OLED_ANTENNA_OPEN && g.jam == OLED_RF_CRITICAL);
    rf[28 + 2] = 0;
    gnss_status_feed(&s, 0x0a, 0x38, rf, sizeof rf, 12300);
    oled_gnss_from_status(&g, &s, 12300);
    assert(g.antenna == OLED_ANTENNA_UNKNOWN);
    // NAV-STATUS spoofing state, flags2 bits 3 and 4.
    uint8_t status[16] = {0};
    status[7] = 1 << 3;
    gnss_status_feed(&s, 1, 3, status, sizeof status, 12400);
    oled_gnss_from_status(&g, &s, 12400);
    assert(g.spoof == OLED_RF_NONE);
    status[7] = 3 << 3;
    gnss_status_feed(&s, 1, 3, status, sizeof status, 12500);
    oled_gnss_from_status(&g, &s, 12500);
    assert(g.spoof == OLED_RF_CRITICAL);

    // A periodic source: MON-RF's jamming flag for one report in ten. Once it is known, the
    // page's jamming state leaves its crossings out and its period stands beside it.
    static gnss_status_t radar;
    oled_gnss_t r;
    uint8_t turn[4 + 24] = {0};
    turn[1] = 1; turn[4 + 2] = 2;
    for (int64_t at = 1000; at <= 71000; at += 1000) {
        const bool crossing = at >= 21000 && (at - 21000) % 10000 == 0;
        turn[4 + 1] = crossing ? 2 : 1;
        gnss_status_feed(&radar, 0x0a, 0x38, turn, sizeof turn, at);
        oled_gnss_from_status(&r, &radar, at);
        if (at == 21000 || at == 51000) assert(r.jam == OLED_RF_WARNING && r.pulse_s == 0); // not yet known
        if (at == 61000 || at == 71000) assert(radar.jam == 2 && r.jam == OLED_RF_NONE && r.pulse_s == 10);
        if (at == 65000) assert(r.jam == OLED_RF_NONE && r.pulse_s == 10);
    }

    // NAV-SIG: GPS 5 on L1 C/A, L2C and L5, Galileo 3 on E1 and E6, BeiDou 20 on B2I (E5b).
    static const struct { uint8_t gnss, sv, sig; } signals[] = {{0, 5, 0}, {0, 5, 3}, {0, 5, 6}, {2, 3, 0}, {2, 3, 8}, {3, 20, 2}};
    uint8_t sig[8 + 16 * 6] = {0};
    sig[5] = 6;
    for (unsigned i = 0; i < 6; i++) {
        uint8_t *r = sig + 8 + 16 * i;
        r[0] = signals[i].gnss; r[1] = signals[i].sv; r[2] = signals[i].sig; r[6] = 40; r[7] = 5;
    }
    gnss_status_feed(&s, 1, 0x43, sig, sizeof sig, 12600);
    oled_gnss_from_status(&g, &s, 12600);
    assert(g.signals_valid && g.band[0] == 2 && g.band[1] == 1 && g.band[2] == 1 && g.band[3] == 1 && g.band[4] == 1);

    // Sixteen seconds on, every report is too old to show.
    oled_gnss_from_status(&g, &s, 12600 + 15001);
    assert(!g.receiving && !g.satellites_valid && !g.signals_valid && g.fix == OLED_FIX_NONE && !g.bars && !g.tracked_total);
    assert(g.antenna == OLED_ANTENNA_UNKNOWN && g.jam == OLED_RF_UNKNOWN && g.spoof == OLED_RF_UNKNOWN && g.cno_max == 0);

    // More satellites than the page has bars for: the first OLED_BARS, and every count kept.
    s = (gnss_status_t){.supported = 0x0f, .satellites_valid = true, .satellites_ms = 20000, .satellite_count = GNSS_OBS_MAX};
    for (unsigned i = 0; i < GNSS_OBS_MAX; i++) s.satellites[i] = (gnss_observation_t){.gnss = 0, .sv = (uint8_t)(i + 1), .cno = 30};
    s.tracked[0] = GNSS_OBS_MAX;
    oled_gnss_from_status(&g, &s, 20000);
    assert(g.bars == OLED_BARS && g.tracked_total == GNSS_OBS_MAX && g.bar[OLED_BARS - 1].cno == 30);
}

// Every glyph, sixteen to a line, to look the font over.
static void specimen(void)
{
    oled_clear(&canvas);
    canvas.dx = canvas.dy = 0;
    for (unsigned ch = 32; ch <= 128; ch++) {
        const char text[2] = {(char)ch, 0};
        (void)oled_text(&canvas, (int)((ch - 32) % 20) * OLED_ADVANCE, (int)((ch - 32) / 20) * 9, 1, text);
    }
    (void)oled_text(&canvas, 0, 48, 2, "0123456789");
    save("font", &canvas);
}

int main(int argc, char **argv)
{
    directory = argc > 1 ? argv[1] : NULL;
    pages();
    attention();
    director();
    from_receiver();
    specimen();
    puts("oled pages, director and receiver adapter: ok");
    return 0;
}
