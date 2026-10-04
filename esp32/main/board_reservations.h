#ifndef NVF_BOARD_RESERVATIONS_H
#define NVF_BOARD_RESERVATIONS_H
// Pin and I2C-address allocation for the observer boards, compiled so the build
// checks it. The hardware repository's netlists and docs/HARDWARE-OBSERVER.md
// section 7 are the contract; this header is the copy a future edit collides with.
// Each board's map lists every ESP32-S3 module pad that board connects.
#include "sdkconfig.h"
#define NVF_PIN(n) (1ULL << (n))

// Common to every board: strap, receiver UART, shared I2C, eFuse fault, PPS, the
// main TLC5916 chain, RTC interrupt/square wave, native USB, the gated rails,
// the debug UART, the GPIO46 strap and the two panel output enables.
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
    NVF_PIN(14) | /* LED_SDI, section 2.1             */ \
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

// NEO: interrupts wired to GPIO, TLC readback, backup-cell sense.
#define NVF_PINS_NEO (NVF_PINS_COMMON | \
    NVF_PIN(1)  | /* BACKUP_BAT_SENSE path, section 7.3.1 */ \
    NVF_PIN(8)  | /* TEMP_ALERT_N, section 7.5        */ \
    NVF_PIN(13) | /* LED_SDO readback, section 2.1    */ \
    NVF_PIN(39) | /* HUM_INT_N, section 7.5           */ \
    NVF_PIN(41))  /* BARO_INT_N, section 7.5          */

// ZED/X20: W5500 on its own SPI bus, the optional panel's data line, the
// brightness trimmer and preset buttons, the receiver's second UART and status.
#define NVF_PIN_ETH_SCLK       8
#define NVF_PIN_ETH_CS        13
#define NVF_PIN_ETH_MOSI      39
#define NVF_PIN_ETH_MISO      41
#define NVF_PIN_GNSS_UART2_TX 16
#define NVF_PIN_GNSS_UART2_RX 17
#define NVF_PIN_RTK_STAT      40
#define NVF_PIN_GEOFENCE_STAT 42
#define NVF_PINS_ZED_X20 (NVF_PINS_COMMON | \
    NVF_PIN(1)  | /* LED_PANEL_SDI                    */ \
    NVF_PIN(2)  | /* BRIGHT_ADC trimmer wiper         */ \
    NVF_PIN(NVF_PIN_ETH_SCLK) | NVF_PIN(NVF_PIN_ETH_CS) | \
    NVF_PIN(NVF_PIN_ETH_MOSI) | NVF_PIN(NVF_PIN_ETH_MISO) | \
    NVF_PIN(NVF_PIN_GNSS_UART2_TX) | NVF_PIN(NVF_PIN_GNSS_UART2_RX) | \
    NVF_PIN(18) | /* BRIGHT_BUTTON presets            */ \
    NVF_PIN(NVF_PIN_RTK_STAT) | NVF_PIN(NVF_PIN_GEOFENCE_STAT))

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
    NVF_PIN(2)  | /* BRIGHT_ADC trimmer wiper         */ \
    NVF_PIN(8)  | /* TEMP_ALERT_N                     */ \
    NVF_PIN(18) | /* BRIGHT_BUTTON presets            */ \
    NVF_PIN(39) | /* HUM_INT_N                        */ \
    NVF_PIN(NVF_PIN_TC_DRDY_N) | NVF_PIN(NVF_PIN_TC_CS_N) | NVF_PIN(NVF_PIN_TC_SCK) | \
    NVF_PIN(NVF_PIN_TC_MOSI) | NVF_PIN(NVF_PIN_TC_MISO) | \
    NVF_PIN(NVF_PIN_IMU_INT1) | NVF_PIN(NVF_PIN_IMU_INT2))

// Not pins on this part or consumed by the module: GPIO22-25 do not exist on the
// S3, GPIO26-32 serve the module's SPI flash, and GPIO35-37 are the N16R8's
// octal PSRAM interface (section 7.7).
#define NVF_PINS_UNAVAILABLE ((0x7FFULL << 22) | (0x7ULL << 35))

// Strapping pins that section 7.7 keeps clear of external reset-time pulls.
#define NVF_PINS_STRAP_AVOID (NVF_PIN(3) | NVF_PIN(45))

#define NVF_PINS_CHECK(map, name) \
    _Static_assert(((map) & NVF_PINS_UNAVAILABLE) == 0, \
        name " claims a pin the S3 or the N16R8 module does not offer"); \
    _Static_assert(((map) & NVF_PINS_STRAP_AVOID) == 0, name " lands on a strapping pin")
NVF_PINS_CHECK(NVF_PINS_NEO, "the NEO pin map");
NVF_PINS_CHECK(NVF_PINS_ZED_X20, "the ZED/X20 pin map");
NVF_PINS_CHECK(NVF_PINS_MAX, "the MAX pin map");

// Shared-bus I2C addresses. Absence of another board's part is not a fault.
#define NVF_I2C_MAGNETOMETER  0x30 /* MMC34160PJ, MAX              */
#define NVF_I2C_TEMPERATURE   0x18 /* MCP9808                      */
#define NVF_I2C_HUMIDITY      0x40 /* HDC2080                      */
#define NVF_I2C_RAIL_MONITOR  0x41 /* INA3221, ZED/X20, A0 to VS   */
#define NVF_I2C_PRESSURE_BMP5 0x46 /* BMP581, ZED/X20, SDO to GND  */
#define NVF_I2C_BOARD_EEPROM  0x50 /* 24CS128 main array; its 128-bit serial is at 0x58 */
#define NVF_I2C_BOARD_SERIAL  0x58 /* 24CS128 security interface   */
#define NVF_I2C_RTC_EUI       0x57 /* MCP79412 EEPROM, NEO and MAX */
#define NVF_I2C_CRYPTO        0x60 /* ATECC608C                    */
#define NVF_I2C_RTC_TCXO      0x68 /* MAX31328, ZED/X20            */
#define NVF_I2C_IMU           0x69 /* ICM-45686, MAX, strapped     */
#define NVF_I2C_RTC           0x6f /* MCP79412 RTCC, NEO and MAX   */
#define NVF_I2C_PRESSURE      0x76 /* BMP388, NEO                  */
#define NVF_I2C_PRESSURE_ALT  0x77 /* MS5607, MAX, CSB low         */

// Each board's function pins; -1 marks a function the board does not have. board.c holds
// every row, the image drives the boards it supports (Kconfig NVF_BOARD_SUPPORT_*), and
// the manifest's CAT_INTSAT entry picks the row at boot (observer_board_current()).
#include "manifest_board.h"
#include "nvf_board.h"
// An image's OTA board ID is the board's CAT_INTSAT ID; 0 is the universal image.
_Static_assert((int)NVF_BOARD_ID_NEO == (int)INTSAT_NEO && (int)NVF_BOARD_ID_ZED_X20 == (int)INTSAT_X20 &&
               (int)NVF_BOARD_ID_MAX == (int)INTSAT_MAX, "OTA board IDs must equal the CAT_INTSAT IDs");
// An INA3221 channel: the rail it measures and its shunt; a shunt of 0 marks no channel.
typedef struct { const char *name; uint16_t shunt_mohm; } observer_rail_t;
typedef struct {
    board_model_t model;
    const char *name;
    uint8_t intsat_id;          // the manifest's CAT_INTSAT ID, also the OTA board ID
    const char *family;         // the signed-release board family
    uint64_t pins;              // everything the board connects, as above
    bool boot_steps_brightness; // BOOT's short press steps the panel presets
    int led_panel_sdi;          // the optional front panel's chain, mirrored
    int bright_adc, bright_button;
    int eth_sclk, eth_cs, eth_mosi, eth_miso;
    int tc_sck, tc_mosi, tc_miso, tc_cs_n, tc_drdy_n;
    int imu_int1, imu_int2;
    observer_rail_t rails[3];   // INA3221 channels 1-3, where the board has the monitor
} observer_board_t;
#define NVF_PIN_ON(map, pin) ((pin) < 0 || ((map) & NVF_PIN((pin) < 0 ? 0 : (pin))) != 0)
#define NVF_NONE (-1)
#define NVF_BOARD_NEO_ROW {.model = BOARD_MODEL_NEO_A, .name = "NEO revision A", .intsat_id = INTSAT_NEO, \
    .family = "gnss-color-neo", .pins = NVF_PINS_NEO, .boot_steps_brightness = true, \
    .led_panel_sdi = NVF_NONE, .bright_adc = NVF_NONE, .bright_button = NVF_NONE, \
    .eth_sclk = NVF_NONE, .eth_cs = NVF_NONE, .eth_mosi = NVF_NONE, .eth_miso = NVF_NONE, \
    .tc_sck = NVF_NONE, .tc_mosi = NVF_NONE, .tc_miso = NVF_NONE, .tc_cs_n = NVF_NONE, .tc_drdy_n = NVF_NONE, \
    .imu_int1 = NVF_NONE, .imu_int2 = NVF_NONE}
#define NVF_BOARD_X20_ROW {.model = BOARD_MODEL_X20_A, .name = "X20 revision A", .intsat_id = INTSAT_X20, \
    .family = "gnss-color-zed-x20", .pins = NVF_PINS_ZED_X20, .boot_steps_brightness = false, \
    .led_panel_sdi = 1, .bright_adc = 2, .bright_button = 18, \
    .eth_sclk = NVF_PIN_ETH_SCLK, .eth_cs = NVF_PIN_ETH_CS, .eth_mosi = NVF_PIN_ETH_MOSI, .eth_miso = NVF_PIN_ETH_MISO, \
    .tc_sck = NVF_NONE, .tc_mosi = NVF_NONE, .tc_miso = NVF_NONE, .tc_cs_n = NVF_NONE, .tc_drdy_n = NVF_NONE, \
    .imu_int1 = NVF_NONE, .imu_int2 = NVF_NONE, \
    .rails = {{"+5V", 20}, {"3V3_GNSS", 50}, {"3V3_SYS", 20}}} /* U37 through R74, R75, R76 */
#define NVF_BOARD_MAX_ROW {.model = BOARD_MODEL_MAX_A, .name = "MAX revision A", .intsat_id = INTSAT_MAX, \
    .family = "gnss-color-max", .pins = NVF_PINS_MAX, .boot_steps_brightness = false, \
    .led_panel_sdi = NVF_NONE, .bright_adc = 2, .bright_button = 18, \
    .eth_sclk = NVF_NONE, .eth_cs = NVF_NONE, .eth_mosi = NVF_NONE, .eth_miso = NVF_NONE, \
    .tc_sck = NVF_PIN_TC_SCK, .tc_mosi = NVF_PIN_TC_MOSI, .tc_miso = NVF_PIN_TC_MISO, .tc_cs_n = NVF_PIN_TC_CS_N, \
    .tc_drdy_n = NVF_PIN_TC_DRDY_N, .imu_int1 = NVF_PIN_IMU_INT1, .imu_int2 = NVF_PIN_IMU_INT2}
// Every function pin is on its board's map.
_Static_assert(NVF_PIN_ON(NVF_PINS_ZED_X20, 1) && NVF_PIN_ON(NVF_PINS_ZED_X20, 2) &&
               NVF_PIN_ON(NVF_PINS_ZED_X20, 18), "an X20 function pin is missing from its map");
_Static_assert(NVF_PIN_ON(NVF_PINS_MAX, 2) && NVF_PIN_ON(NVF_PINS_MAX, 18),
               "a MAX function pin is missing from its map");
#endif
