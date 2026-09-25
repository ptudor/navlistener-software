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
#define NVF_I2C_BOARD_EEPROM  0x50 /* 24CS128 main array; its 128-bit serial is at 0x58 */
#define NVF_I2C_BOARD_SERIAL  0x58 /* 24CS128 security interface   */
#define NVF_I2C_RTC_EUI       0x57 /* MCP79412 EEPROM, NEO and MAX */
#define NVF_I2C_CRYPTO        0x60 /* ATECC608C                    */
#define NVF_I2C_RTC_TCXO      0x68 /* MAX31328, ZED/X20            */
#define NVF_I2C_IMU           0x69 /* ICM-45686, MAX, strapped     */
#define NVF_I2C_RTC           0x6f /* MCP79412 RTCC, NEO and MAX   */
#define NVF_I2C_PRESSURE      0x76 /* BMP388, NEO and ZED/X20      */
#define NVF_I2C_PRESSURE_ALT  0x77 /* MS5607, MAX, CSB low         */

// The board this build drives. -1 marks a function the board does not have.
#if CONFIG_NVF_BOARD_GNSS_COLOR_ZED_X20
#define NVF_PINS_BOARD          NVF_PINS_ZED_X20
#define NVF_PIN_LED_PANEL_SDI   1   /* optional front panel's chain, mirrored */
#define NVF_PIN_BRIGHT_ADC      2
#define NVF_PIN_BRIGHT_BUTTON   18
#elif CONFIG_NVF_BOARD_GNSS_COLOR_MAX
#define NVF_PINS_BOARD          NVF_PINS_MAX
#define NVF_PIN_LED_PANEL_SDI   (-1)
#define NVF_PIN_BRIGHT_ADC      2
#define NVF_PIN_BRIGHT_BUTTON   18
#else
#define NVF_PINS_BOARD          NVF_PINS_NEO
#define NVF_PIN_LED_PANEL_SDI   (-1)
#define NVF_PIN_BRIGHT_ADC      (-1)
#define NVF_PIN_BRIGHT_BUTTON   (-1) /* BOOT's short press steps the presets */
#endif
#define NVF_PIN_ON_BOARD(pin) ((pin) < 0 || (NVF_PINS_BOARD & NVF_PIN((pin) < 0 ? 0 : (pin))) != 0)
_Static_assert(NVF_PIN_ON_BOARD(NVF_PIN_LED_PANEL_SDI) && NVF_PIN_ON_BOARD(NVF_PIN_BRIGHT_ADC) &&
               NVF_PIN_ON_BOARD(NVF_PIN_BRIGHT_BUTTON),
    "a selected board function uses a pin its map does not list");
#endif
