#ifndef NVF_BOARD_RESERVATIONS_H
#define NVF_BOARD_RESERVATIONS_H
// Pin and I2C-address allocation for the observer boards, compiled so the build
// checks it. The hardware repository's netlists and docs/HARDWARE-OBSERVER.md
// section 7 are the contract; this header is the copy a future edit collides with.
// Each board's map lists every ESP32-S3 module pad that board connects.
#include "sdkconfig.h"
#define NVF_PIN(n) (1ULL << (n))

// Common to every board: strap, receiver UART, shared I2C, eFuse fault, PPS, the
// TLC5916 chain's clock and latch, RTC interrupt/square wave, native USB, the gated
// rails, the debug UART, the GPIO46 strap and the two output enables. The chain's
// data pins are per board (led_sdi and led_panel_sdi in the rows below).
#define NVF_PINS_COMMON ( \
    NVF_PIN(0)  | /* ESP_BOOT, section 7.7            */ \
    NVF_PIN(4)  | /* GNSS_UART_TX, section 7.1        */ \
    NVF_PIN(5)  | /* GNSS_UART_RX, section 7.1        */ \
    NVF_PIN(6)  | /* I2C_SDA, section 7.5             */ \
    NVF_PIN(7)  | /* I2C_SCL, section 7.5             */ \
    NVF_PIN(9)  | /* EFUSE_FAULT_N, section 7.3.1     */ \
    NVF_PIN(10) | /* GNSS_PPS, section 7.2            */ \
    NVF_PIN(11) | /* LED_SCLK, section 2.1            */ \
    NVF_PIN(12) | /* LED_LATCH, section 2.1           */ \
    NVF_PIN(15) | /* RTC_MFP_N, 7.5; also XTAL32K_P   */ \
    NVF_PIN(19) | /* USB_DM, section 7.6              */ \
    NVF_PIN(20) | /* USB_DP, section 7.6              */ \
    NVF_PIN(21) | /* SENS_EN, section 7.3.1           */ \
    NVF_PIN(38) | /* GNSS_EN, section 7.3.1           */ \
    NVF_PIN(43) | /* U0TXD header, section 7.7        */ \
    NVF_PIN(44) | /* U0RXD header, section 7.7        */ \
    NVF_PIN(46) | /* Joint Download Boot strap, 7.7   */ \
    NVF_PIN(47) | /* LED_GREEN_OE_N, section 2.1      */ \
    NVF_PIN(48))  /* LED_YELLOW_OE_N, section 2.1     */

// NEO: the on-board TLC5916 chain, interrupts wired to GPIO, TLC readback,
// backup-cell sense.
#define NVF_PINS_NEO (NVF_PINS_COMMON | \
    NVF_PIN(1)  | /* BACKUP_BAT_SENSE path, section 7.3.1 */ \
    NVF_PIN(14) | /* LED_SDI, section 2.1             */ \
    NVF_PIN(8)  | /* TEMP_ALERT_N, section 7.5        */ \
    NVF_PIN(13) | /* LED_SDO readback, section 2.1    */ \
    NVF_PIN(39) | /* HUM_INT_N, section 7.5           */ \
    NVF_PIN(41))  /* BARO_INT_N, section 7.5          */

// ZED/X20, the 162mm mainboard with its required LED panel: W5500 on its own SPI
// bus, the panel's 24-bit chain on GPIO1 (it has no on-board chain), the console
// port's VBUS sense
// on GPIO14, the MCP23008 expander's interrupt on GPIO3, the brightness trimmer
// and the panel's preset button, the receiver's second UART and status pins.
#define NVF_PIN_ETH_SCLK       8
#define NVF_PIN_ETH_CS        13
#define NVF_PIN_ETH_MOSI      39
#define NVF_PIN_ETH_MISO      41
#define NVF_PIN_GNSS_UART2_TX 16
#define NVF_PIN_GNSS_UART2_RX 17
#define NVF_PIN_RTK_STAT      40
#define NVF_PIN_GEOFENCE_STAT 42
#define NVF_PIN_CONSOLE_VBUS  14  /* U32 buffers the console host's VBUS through R97 */
#define NVF_PIN_EXP_INT_N      3  /* U39 INT, R94 pull-up; a strap read only as an input */
#define NVF_PINS_ZED_X20_COMMON (NVF_PINS_COMMON | \
    NVF_PIN(1)  | /* LED_PANEL_SDI                    */ \
    NVF_PIN(2)  | /* BRIGHT_ADC trimmer wiper         */ \
    NVF_PIN(NVF_PIN_CONSOLE_VBUS) | NVF_PIN(NVF_PIN_EXP_INT_N) | \
    NVF_PIN(NVF_PIN_ETH_SCLK) | NVF_PIN(NVF_PIN_ETH_CS) | \
    NVF_PIN(NVF_PIN_ETH_MOSI) | NVF_PIN(NVF_PIN_ETH_MISO) | \
    NVF_PIN(NVF_PIN_GNSS_UART2_TX) | NVF_PIN(NVF_PIN_GNSS_UART2_RX) | \
    NVF_PIN(18) | /* BRIGHT_BUTTON presets            */ \
    NVF_PIN(NVF_PIN_RTK_STAT))
#define NVF_PINS_ZED_X20_A (NVF_PINS_ZED_X20_COMMON | NVF_PIN(NVF_PIN_GEOFENCE_STAT))
// Revision B reads GEOFENCE_STAT through U41 GP6, freeing ESP32 GPIO42.
#define NVF_PINS_ZED_X20_B NVF_PINS_ZED_X20_COMMON

// MAX: the MAX31856 thermocouple converter's SPI and data-ready, the IMU's two
// interrupts, the environmental interrupts, the brightness trimmer and buttons.
#define NVF_PIN_TC_DRDY_N      1
#define NVF_PIN_TC_CS_N       13
#define NVF_PIN_TC_SCK        40
#define NVF_PIN_TC_MOSI       41
#define NVF_PIN_TC_MISO       42
#define NVF_PIN_IMU_INT1      16
#define NVF_PIN_IMU_INT2      17
#define NVF_PINS_MAX (NVF_PINS_COMMON | \
    NVF_PIN(14) | /* LED_SDI, section 2.1             */ \
    NVF_PIN(2)  | /* BRIGHT_ADC trimmer wiper         */ \
    NVF_PIN(8)  | /* TEMP_ALERT_N                     */ \
    NVF_PIN(18) | /* BRIGHT_BUTTON presets            */ \
    NVF_PIN(39) | /* HUM_INT_N                        */ \
    NVF_PIN(NVF_PIN_TC_DRDY_N) | NVF_PIN(NVF_PIN_TC_CS_N) | NVF_PIN(NVF_PIN_TC_SCK) | \
    NVF_PIN(NVF_PIN_TC_MOSI) | NVF_PIN(NVF_PIN_TC_MISO) | \
    NVF_PIN(NVF_PIN_IMU_INT1) | NVF_PIN(NVF_PIN_IMU_INT2))

// P3: the mosaic-G5 P3 observer, on the 162mm card with the 162mm panel's 24-bit chain on
// GPIO14 (no on-board chain), the receiver's COM2 flow control and reset driver, the
// brightness trimmer and preset buttons, and the sensor and rail-monitor alert lines.
// P3_RESET_DRV drives Q1, which pulls the receiver's reset low while GPIO13 is high; R97 holds
// Q1 off until the firmware drives GPIO13 low, and it pulses it high only to recover a receiver
// that stops answering. ESP_UART_RTS_N (GPIO17) and ESP_UART_CTS_N (GPIO16) are COM2's flow
// control through U41 and U43: RTS runs from the start, CTS is honored once the receiver has
// confirmed RTS|CTS. TEMP_ALERT_N, HUM_INT_N and PWR_ALERT_N are open drain with pull-ups. PWR_ALERT_N
// (R107 to 3V3_SENS) interrupts on its falling edge; the other two are read only by sampling
// the parts.
#define NVF_PIN_P3_RESET_DRV  13
#define NVF_PIN_P3_UART_CTS_N 16
#define NVF_PIN_P3_UART_RTS_N 17
#define NVF_PIN_P3_PWR_ALERT_N 41
#define NVF_PINS_MOSAIC_P3 (NVF_PINS_COMMON | \
    NVF_PIN(2)  | /* BRIGHT_ADC trimmer wiper         */ \
    NVF_PIN(8)  | /* TEMP_ALERT_N, both MCP9808s      */ \
    NVF_PIN(14) | /* LED_SDI to the panel's chain     */ \
    NVF_PIN(18) | /* BRIGHT_BUTTON presets            */ \
    NVF_PIN(39) | /* HUM_INT_N                        */ \
    NVF_PIN(NVF_PIN_P3_PWR_ALERT_N) | /* INA3221 Warning and Critical */ \
    NVF_PIN(NVF_PIN_P3_RESET_DRV) | NVF_PIN(NVF_PIN_P3_UART_CTS_N) | NVF_PIN(NVF_PIN_P3_UART_RTS_N))

// Not pins on this part or consumed by the module: GPIO22-25 do not exist on the
// S3, GPIO26-32 serve the module's SPI flash, and GPIO35-37 are the N16R8's
// octal PSRAM interface (section 7.7).
#define NVF_PINS_UNAVAILABLE ((0x7FFULL << 22) | (0x7ULL << 35))

// Strapping pins that section 7.7 keeps clear of external reset-time pulls. GPIO3
// (JTAG_SEL) is the exception the ZED/X20 makes: it only reads the expander's
// interrupt, whose 10 kOhm pull-up is harmless while EFUSE_STRAP_JTAG_SEL stays
// unburned, which the firmware never changes.
#define NVF_PINS_STRAP_AVOID (NVF_PIN(45))
_Static_assert(((NVF_PINS_NEO | NVF_PINS_MAX | NVF_PINS_MOSAIC_P3) & NVF_PIN(3)) == 0,
               "GPIO3 is a strap; only the ZED/X20 reads it");

#define NVF_PINS_CHECK(map, name) \
    _Static_assert(((map) & NVF_PINS_UNAVAILABLE) == 0, \
        name " claims a pin the S3 or the N16R8 module does not offer"); \
    _Static_assert(((map) & NVF_PINS_STRAP_AVOID) == 0, name " lands on a strapping pin")
NVF_PINS_CHECK(NVF_PINS_NEO, "the NEO pin map");
NVF_PINS_CHECK(NVF_PINS_ZED_X20_A, "the revision-A ZED/X20 pin map");
NVF_PINS_CHECK(NVF_PINS_ZED_X20_B, "the revision-B ZED/X20 pin map");
NVF_PINS_CHECK(NVF_PINS_MAX, "the MAX pin map");
NVF_PINS_CHECK(NVF_PINS_MOSAIC_P3, "the P3 pin map");

// Shared-bus I2C addresses. Absence of another board's part is not a fault.
#define NVF_I2C_MAGNETOMETER  0x30 /* MMC34160PJ, MAX              */
#define NVF_I2C_TEMPERATURE   0x18 /* MCP9808                      */
#define NVF_I2C_REGULATOR_TEMPERATURE 0x1F /* MCP9808 on the LDL1117 spreader, P3 U47 */
#define NVF_I2C_HUMIDITY_HIH  0x27 /* HIH8121, P3                  */
#define NVF_I2C_HUMIDITY      0x40 /* HDC2080                      */
#define NVF_I2C_HUMIDITY_SHT3X 0x44 /* SHT35, P3, ADDR to GND      */
#define NVF_I2C_RAIL_MONITOR  0x41 /* INA3221, ZED/X20 and P3, A0 to VS */
#define NVF_I2C_PRESSURE_BMP5 0x46 /* BMP581, ZED/X20, SDO to GND  */
#define NVF_I2C_BOARD_EEPROM  0x50 /* 24CS128 main array; its 128-bit serial is at 0x58 */
#define NVF_I2C_BOARD_SERIAL  0x58 /* 24CS128 security interface   */
#define NVF_I2C_RTC_EUI       0x57 /* MCP79412 EEPROM, NEO, MAX and P3 */
#define NVF_I2C_CRYPTO        0x60 /* ATECC608C                    */
#define NVF_I2C_RTC_TCXO      0x68 /* DS3231M, ZED/X20 (MAX31328 on the superseded square list) */
#define NVF_I2C_IO_EXPANDER   0x24 /* MCP23008 U39, ZED/X20 interrupt collector */
#define NVF_I2C_RECEIVER_EXPANDER 0x25 /* MCP23008 U41, X20 revision B receiver service */
#define NVF_I2C_IMU           0x69 /* ICM-45686, MAX, strapped     */
#define NVF_I2C_RTC           0x6f /* MCP79412 RTCC, NEO, MAX and P3 */
#define NVF_I2C_PRESSURE      0x76 /* BMP388, NEO                  */
#define NVF_I2C_PRESSURE_ALT  0x77 /* MS5607, MAX and P3, CSB low  */

// U41 receiver-service functions. GP0-GP3 are only ever driven low or released;
// GP4-GP6 are inputs and GP7 is reserved at a test point.
#define NVF_RXEXP_SAFEBOOT_N   (1u << 0)
#define NVF_RXEXP_RESET_N      (1u << 1)
#define NVF_RXEXP_EXTINT       (1u << 2)
#define NVF_RXEXP_ANT_ENABLE   (1u << 3)
#define NVF_RXEXP_ANT_FAULT_N  (1u << 4)
#define NVF_RXEXP_TX_READY     (1u << 5)
#define NVF_RXEXP_GEOFENCE     (1u << 6)
#define NVF_RXEXP_SPARE        (1u << 7)
_Static_assert((NVF_RXEXP_SAFEBOOT_N | NVF_RXEXP_RESET_N | NVF_RXEXP_EXTINT | NVF_RXEXP_ANT_ENABLE |
                NVF_RXEXP_ANT_FAULT_N | NVF_RXEXP_TX_READY | NVF_RXEXP_GEOFENCE | NVF_RXEXP_SPARE) == 0xff,
               "the X20 receiver expander pin allocation must cover GP0-GP7 exactly once");

// Each board's function pins; -1 marks a function the board does not have. board.c holds
// every row, the image drives the boards it supports (Kconfig NVF_BOARD_SUPPORT_*), and
// the manifest's CAT_INTSAT entry picks the row at boot (observer_board_current()).
#include "manifest_board.h"
#include "nvf_board.h"
#include "regulator_thermal.h"
// An image's OTA board ID is the board's CAT_INTSAT ID; 0 is the universal image.
_Static_assert((int)NVF_BOARD_ID_NEO == (int)INTSAT_NEO && (int)NVF_BOARD_ID_ZED_X20 == (int)INTSAT_X20 &&
               (int)NVF_BOARD_ID_MAX == (int)INTSAT_MAX && (int)NVF_BOARD_ID_MOSAIC_P3 == (int)INTSAT_MOSAIC_P3,
               "OTA board IDs must equal the CAT_INTSAT IDs");
// An INA3221 channel: the rail it measures and its shunt; a shunt of 0 marks no channel.
// warn_ma is the Warning alert's limit on the averaged current and crit_ma the Critical
// alert's on each conversion; 0 sets no alert. The limit is mA x mOhm uV across the shunt.
typedef struct { const char *name; uint16_t shunt_mohm, warn_ma, crit_ma; } observer_rail_t;
// An MCP9808 on the system regulator's heat spreader, the regulator it watches, and the limits
// at which it throttles Wi-Fi (regulator_thermal.h); address 0 marks a board without one.
typedef struct { uint8_t address; const char *part; regulator_thermal_limits_t limits; } observer_regulator_t;
typedef struct {
    board_model_t model;
    const char *name;
    uint8_t intsat_id;          // the manifest's CAT_INTSAT ID, also the OTA board ID
    const char *family;         // the signed-release board family
    uint64_t pins;              // everything the board connects, as above
    bool boot_steps_brightness; // BOOT's short press steps the panel presets
    int led_sdi;                // the on-board TLC5916 chain's data pin, or none
    int led_panel_sdi;          // the front panel's chain: a mirror of the on-board one, or the only chain
    bool panel_status_byte;     // the panel chain has a third driver: 24 bits, status, amber, green
    int console_vbus;           // the console port's VBUS, read as an input (pull-down: low where unwired)
    int exp_int_n;              // the MCP23008's interrupt, read as an input
    int pwr_alert_n;            // the INA3221's alert line on its own pin (no expander): falling-edge interrupt
    int rx_rts, rx_cts;         // the receiver UART's flow control, or none
    int rx_reset;               // drives the receiver's reset, high to hold it, or none
    int rtk_stat;               // the receiver's RTK_STAT, low while RTK is fixed
    int bright_adc, bright_button;
    int eth_sclk, eth_cs, eth_mosi, eth_miso;
    int tc_sck, tc_mosi, tc_miso, tc_cs_n, tc_drdy_n;
    int imu_int1, imu_int2;
    observer_rail_t rails[3];   // INA3221 channels 1-3, where the board has the monitor
    observer_regulator_t regulator;
} observer_board_t;
#define NVF_PIN_ON(map, pin) ((pin) < 0 || ((map) & NVF_PIN((pin) < 0 ? 0 : (pin))) != 0)
#define NVF_NONE (-1)
#define NVF_BOARD_NEO_ROW {.model = BOARD_MODEL_NEO_A, .name = "NEO revision A", .intsat_id = INTSAT_NEO, \
    .family = "gnss-color-neo", .pins = NVF_PINS_NEO, .boot_steps_brightness = true, \
    .led_sdi = 14, .led_panel_sdi = NVF_NONE, .panel_status_byte = false, \
    .console_vbus = NVF_NONE, .exp_int_n = NVF_NONE, .pwr_alert_n = NVF_NONE, .rtk_stat = NVF_NONE, \
    .rx_rts = NVF_NONE, .rx_cts = NVF_NONE, .rx_reset = NVF_NONE, \
    .bright_adc = NVF_NONE, .bright_button = NVF_NONE, \
    .eth_sclk = NVF_NONE, .eth_cs = NVF_NONE, .eth_mosi = NVF_NONE, .eth_miso = NVF_NONE, \
    .tc_sck = NVF_NONE, .tc_mosi = NVF_NONE, .tc_miso = NVF_NONE, .tc_cs_n = NVF_NONE, .tc_drdy_n = NVF_NONE, \
    .imu_int1 = NVF_NONE, .imu_int2 = NVF_NONE}
// The X20's INA3221 channels. Its alert limits are engineering values to be confirmed on the
// bench, like the heater thresholds: +5V warns below the input eFuse's 1.45 A minimum limit,
// so the alert is seen before the eFuse acts; 3V3_GNSS allows the receiver and antenna feed
// (under 300 mA) against the ADM7150's 800 mA; 3V3_SYS allows the ESP32's Wi-Fi peaks and the
// panel logic against the LDL1117's 1.2 A.
#define NVF_X20_RAIL_5V   {.name = "+5V", .shunt_mohm = 20, .warn_ma = 1000, .crit_ma = 1300}
#define NVF_X20_RAIL_GNSS {.name = "3V3_GNSS", .shunt_mohm = 50, .warn_ma = 350, .crit_ma = 500}
#define NVF_X20_RAIL_SYS  {.name = "3V3_SYS", .shunt_mohm = 20, .warn_ma = 800, .crit_ma = 1000}
#define NVF_BOARD_X20_ROW(model_id, revision_name, pin_map) {.model = (model_id), .name = (revision_name), .intsat_id = INTSAT_X20, \
    .family = "gnss-color-zed-x20", .pins = (pin_map), .boot_steps_brightness = false, \
    .led_sdi = NVF_NONE, .led_panel_sdi = 1, .panel_status_byte = true, \
    .console_vbus = NVF_PIN_CONSOLE_VBUS, .exp_int_n = NVF_PIN_EXP_INT_N, .pwr_alert_n = NVF_NONE, \
    .rtk_stat = NVF_PIN_RTK_STAT, \
    .rx_rts = NVF_NONE, .rx_cts = NVF_NONE, .rx_reset = NVF_NONE, \
    .bright_adc = 2, .bright_button = 18, \
    .eth_sclk = NVF_PIN_ETH_SCLK, .eth_cs = NVF_PIN_ETH_CS, .eth_mosi = NVF_PIN_ETH_MOSI, .eth_miso = NVF_PIN_ETH_MISO, \
    .tc_sck = NVF_NONE, .tc_mosi = NVF_NONE, .tc_miso = NVF_NONE, .tc_cs_n = NVF_NONE, .tc_drdy_n = NVF_NONE, \
    .imu_int1 = NVF_NONE, .imu_int2 = NVF_NONE, \
    .rails = {NVF_X20_RAIL_5V, NVF_X20_RAIL_GNSS, NVF_X20_RAIL_SYS}} /* U37 through R74, R75, R76 */
#define NVF_BOARD_X20_A_ROW NVF_BOARD_X20_ROW(BOARD_MODEL_X20_A, "X20 revision A", NVF_PINS_ZED_X20_A)
#define NVF_BOARD_X20_B_ROW NVF_BOARD_X20_ROW(BOARD_MODEL_X20_B, "X20 revision B", NVF_PINS_ZED_X20_B)
#define NVF_BOARD_MAX_ROW {.model = BOARD_MODEL_MAX_A, .name = "MAX revision A", .intsat_id = INTSAT_MAX, \
    .family = "gnss-color-max", .pins = NVF_PINS_MAX, .boot_steps_brightness = false, \
    .led_sdi = 14, .led_panel_sdi = NVF_NONE, .panel_status_byte = false, \
    .console_vbus = NVF_NONE, .exp_int_n = NVF_NONE, .pwr_alert_n = NVF_NONE, .rtk_stat = NVF_NONE, \
    .rx_rts = NVF_NONE, .rx_cts = NVF_NONE, .rx_reset = NVF_NONE, \
    .bright_adc = 2, .bright_button = 18, \
    .eth_sclk = NVF_NONE, .eth_cs = NVF_NONE, .eth_mosi = NVF_NONE, .eth_miso = NVF_NONE, \
    .tc_sck = NVF_PIN_TC_SCK, .tc_mosi = NVF_PIN_TC_MOSI, .tc_miso = NVF_PIN_TC_MISO, .tc_cs_n = NVF_PIN_TC_CS_N, \
    .tc_drdy_n = NVF_PIN_TC_DRDY_N, .imu_int1 = NVF_PIN_IMU_INT1, .imu_int2 = NVF_PIN_IMU_INT2}
// The P3's INA3221 channels, through R89, R75 and R76 (the X20's shunt values). Engineering
// values to be confirmed on the bench, like the X20's: +5V warns above the 1.18 A simulated
// maximum load and trips Critical below the TPS259531 eFuse's 1.446 A minimum limit, so both
// alerts are seen before the eFuse acts; 3V3_GNSS allows the mosaic-G5 (570 mW) and the
// antenna feed against the ADM7150's 800 mA; 3V3_SYS allows the ESP32's Wi-Fi peaks and the
// panel logic against the LDL1117's 1.2 A.
#define NVF_P3_RAIL_5V   {.name = "+5V", .shunt_mohm = 20, .warn_ma = 1200, .crit_ma = 1400}
#define NVF_P3_RAIL_GNSS {.name = "3V3_GNSS", .shunt_mohm = 50, .warn_ma = 350, .crit_ma = 500}
#define NVF_P3_RAIL_SYS  {.name = "3V3_SYS", .shunt_mohm = 20, .warn_ma = 800, .crit_ma = 1000}
// The P3's regulator sensor, U47 on the LDL1117's spreader beside the tab. ST rates the LDL1117's
// junction to 125 C in operation, 15 C/W above its case (DocID030319 Tables 2 and 3): at the
// 0.72 W the board's supply simulation gives for continuous transmit, the junction sits about
// 11 C above the tab, and U47 reads no hotter than the tab. Throttling from 95 C and entirely
// from 105 C keeps the junction near 106-116 C, leaving 9 C for the tab-to-sensor gradient the
// closed-box bench run must measure; 85 C releases with 10 C of hysteresis. Engineering values
// to be confirmed on the bench, like the rails' alert limits.
#define NVF_P3_REGULATOR_THROTTLE_CENTI_C 9500
#define NVF_P3_REGULATOR_FULL_CENTI_C     10500
#define NVF_P3_REGULATOR_RELEASE_CENTI_C  8500
_Static_assert(NVF_P3_REGULATOR_RELEASE_CENTI_C < NVF_P3_REGULATOR_THROTTLE_CENTI_C &&
               NVF_P3_REGULATOR_THROTTLE_CENTI_C <= NVF_P3_REGULATOR_FULL_CENTI_C,
               "the P3 regulator limits must satisfy release < throttle <= full");
#define NVF_P3_REGULATOR {.address = NVF_I2C_REGULATOR_TEMPERATURE, .part = "LDL1117", \
    .limits = {.throttle_centi_c = NVF_P3_REGULATOR_THROTTLE_CENTI_C, .full_centi_c = NVF_P3_REGULATOR_FULL_CENTI_C, \
               .release_centi_c = NVF_P3_REGULATOR_RELEASE_CENTI_C}}
#define NVF_BOARD_P3_ROW {.model = BOARD_MODEL_P3_A, .name = "P3 revision A", .intsat_id = INTSAT_MOSAIC_P3, \
    .family = "gnss-color-mosaic-p3", .pins = NVF_PINS_MOSAIC_P3, .boot_steps_brightness = false, \
    .led_sdi = NVF_NONE, .led_panel_sdi = 14, .panel_status_byte = true, \
    .console_vbus = NVF_NONE, .exp_int_n = NVF_NONE, .pwr_alert_n = NVF_PIN_P3_PWR_ALERT_N, .rtk_stat = NVF_NONE, \
    .rx_rts = NVF_PIN_P3_UART_RTS_N, .rx_cts = NVF_PIN_P3_UART_CTS_N, .rx_reset = NVF_PIN_P3_RESET_DRV, \
    .bright_adc = 2, .bright_button = 18, \
    .eth_sclk = NVF_NONE, .eth_cs = NVF_NONE, .eth_mosi = NVF_NONE, .eth_miso = NVF_NONE, \
    .tc_sck = NVF_NONE, .tc_mosi = NVF_NONE, .tc_miso = NVF_NONE, .tc_cs_n = NVF_NONE, .tc_drdy_n = NVF_NONE, \
    .imu_int1 = NVF_NONE, .imu_int2 = NVF_NONE, \
    .rails = {NVF_P3_RAIL_5V, NVF_P3_RAIL_GNSS, NVF_P3_RAIL_SYS}, /* U37 through R89, R75, R76 */ \
    .regulator = NVF_P3_REGULATOR}
// Every function pin is on its board's map.
_Static_assert(NVF_PIN_ON(NVF_PINS_ZED_X20_A, NVF_PIN_GEOFENCE_STAT) &&
               !NVF_PIN_ON(NVF_PINS_ZED_X20_B, NVF_PIN_GEOFENCE_STAT),
               "only X20 revision A connects GEOFENCE_STAT to ESP32 GPIO42");
_Static_assert(NVF_PIN_ON(NVF_PINS_ZED_X20_B, 1) && NVF_PIN_ON(NVF_PINS_ZED_X20_B, 2) &&
               NVF_PIN_ON(NVF_PINS_ZED_X20_B, 18) && NVF_PIN_ON(NVF_PINS_ZED_X20_B, NVF_PIN_CONSOLE_VBUS) &&
               NVF_PIN_ON(NVF_PINS_ZED_X20_B, NVF_PIN_EXP_INT_N) && NVF_PIN_ON(NVF_PINS_ZED_X20_B, NVF_PIN_RTK_STAT),
               "an X20 function pin is missing from its map");
_Static_assert(NVF_PIN_ON(NVF_PINS_NEO, 14) && NVF_PIN_ON(NVF_PINS_MAX, 14), "the on-board chain's data pin is missing from its map");
_Static_assert(NVF_PIN_ON(NVF_PINS_MAX, 2) && NVF_PIN_ON(NVF_PINS_MAX, 18),
               "a MAX function pin is missing from its map");
_Static_assert(NVF_PIN_ON(NVF_PINS_MOSAIC_P3, 2) && NVF_PIN_ON(NVF_PINS_MOSAIC_P3, 14) &&
               NVF_PIN_ON(NVF_PINS_MOSAIC_P3, 18) && NVF_PIN_ON(NVF_PINS_MOSAIC_P3, NVF_PIN_P3_PWR_ALERT_N) &&
               NVF_PIN_ON(NVF_PINS_MOSAIC_P3, NVF_PIN_P3_UART_RTS_N) && NVF_PIN_ON(NVF_PINS_MOSAIC_P3, NVF_PIN_P3_UART_CTS_N) &&
               NVF_PIN_ON(NVF_PINS_MOSAIC_P3, NVF_PIN_P3_RESET_DRV),
               "a P3 function pin is missing from its map");
#endif
