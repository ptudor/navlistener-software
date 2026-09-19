#ifndef NVF_BOARD_RESERVATIONS_H
#define NVF_BOARD_RESERVATIONS_H
// Pin and I2C-address allocation for the observer boards, compiled so the build
// checks it. docs/HARDWARE-OBSERVER.md section 7 is the contract; this header is
// the copy a future edit collides with.
//
// The MAX-format mobile variant of section 7.10 is not fabricated and has no
// driver here. Its bus addresses and pins are reserved so nothing else spends
// them before the board is drawn. Reserving a pin is not using one: no code in
// this firmware configures anything below as an input or an output.
#define NVF_PIN(n) (1ULL << (n))

// Spoken for on the ESP32-S3/NEO observer, which is the MAX variant's base.
#define NVF_PINS_NEO ( \
    NVF_PIN(0)  | /* ESP_BOOT, section 7.7            */ \
    NVF_PIN(1)  | /* BACKUP_BAT_SENSE path, section 7.3.1 */ \
    NVF_PIN(4)  | /* GNSS_UART_TX, section 7.1        */ \
    NVF_PIN(5)  | /* GNSS_UART_RX, section 7.1        */ \
    NVF_PIN(6)  | /* I2C_SDA, section 7.5             */ \
    NVF_PIN(7)  | /* I2C_SCL, section 7.5             */ \
    NVF_PIN(8)  | /* TEMP_ALERT_N, section 7.5        */ \
    NVF_PIN(9)  | /* EFUSE_FAULT_N, section 7.3.1     */ \
    NVF_PIN(10) | /* GNSS_PPS, section 7.2            */ \
    NVF_PIN(11) | /* LED_SCLK, section 2.1            */ \
    NVF_PIN(12) | /* LED_LATCH, section 2.1           */ \
    NVF_PIN(13) | /* LED_SDO readback, section 2.1    */ \
    NVF_PIN(14) | /* LED_SDI, section 2.1             */ \
    NVF_PIN(15) | /* RTC_MFP_N, 7.5; also XTAL32K_P   */ \
    NVF_PIN(19) | /* USB_DM, section 7.6              */ \
    NVF_PIN(20) | /* USB_DP, section 7.6              */ \
    NVF_PIN(21) | /* SENS_EN, section 7.3.1           */ \
    NVF_PIN(38) | /* GNSS_EN, section 7.3.1           */ \
    NVF_PIN(39) | /* HUM_INT_N, section 7.5           */ \
    NVF_PIN(41) | /* BARO_INT_N, section 7.5          */ \
    NVF_PIN(43) | /* U0TXD header, section 7.7        */ \
    NVF_PIN(44) | /* U0RXD header, section 7.7        */ \
    NVF_PIN(46) | /* Joint Download Boot strap, 7.7   */ \
    NVF_PIN(47) | /* LED_GREEN_OE_N, section 2.1      */ \
    NVF_PIN(48))  /* LED_YELLOW_OE_N, section 2.1     */

// Not pins on this part or consumed by the module: GPIO22-25 do not exist on the
// S3, GPIO26-32 serve the module's SPI flash, and GPIO35-37 are the N16R8's
// octal PSRAM interface (section 7.7).
#define NVF_PINS_UNAVAILABLE ((0x7FFULL << 22) | (0x7ULL << 35))

// Strapping pins that section 7.7 keeps clear of external reset-time pulls.
#define NVF_PINS_STRAP_AVOID (NVF_PIN(3) | NVF_PIN(45))

// Section 7.10. GPIO33 and GPIO34 are unallocated on both existing boards.
// GPIO16 and GPIO17 are free here because the M10 receiver has one UART; they
// carry the ZED variant's UART2, so this reservation is specific to a MAX board.
//
// GPIO16 is also the S3's XTAL32K_N pad (XTAL32K_P is GPIO15, XTAL32K_N is
// GPIO16 in soc/io_mux_reg.h). That costs nothing while the optional TCXO slow
// clock of section 7.10 uses CONFIG_RTC_CLK_SRC_EXT_OSC, which drives GPIO15
// alone and leaves GPIO16 alone. Choosing a 32 kHz crystal instead would claim
// both pads and would have to take IMU_INT1 somewhere else.
#define NVF_PIN_RTC2_INT_N  33 /* MAX31328 pin 3 INT/SQW, open-drain, 10k to 3V3_SENS */
#define NVF_PIN_RTC2_32KHZ  34 /* MAX31328 pin 1 32kHz, open-drain; EN32kHz resets to 1 */
#define NVF_PIN_IMU_INT1    16 /* ICM-45686 interrupt 1                               */
#define NVF_PIN_IMU_INT2    17 /* ICM-45686 interrupt 2, may stay unfitted            */
#define NVF_PINS_MAX_RESERVED ( \
    NVF_PIN(NVF_PIN_RTC2_INT_N) | NVF_PIN(NVF_PIN_RTC2_32KHZ) | \
    NVF_PIN(NVF_PIN_IMU_INT1)   | NVF_PIN(NVF_PIN_IMU_INT2))

_Static_assert((NVF_PINS_MAX_RESERVED & NVF_PINS_NEO) == 0,
    "a section 7.10 reservation collides with a pin the NEO board already uses");
_Static_assert((NVF_PINS_MAX_RESERVED & NVF_PINS_UNAVAILABLE) == 0,
    "a section 7.10 reservation names a pin the S3 or the N16R8 module does not offer");
_Static_assert((NVF_PINS_MAX_RESERVED & NVF_PINS_STRAP_AVOID) == 0,
    "a section 7.10 reservation lands on a strapping pin");
_Static_assert((NVF_PINS_NEO & NVF_PINS_UNAVAILABLE) == 0,
    "the NEO pin map claims a pin the S3 or the N16R8 module does not offer");

// Shared-bus I2C addresses. The fitted set is section 7.5; the reserved set is
// section 7.10 and is absent from present-day boards, which is not a fault.
#define NVF_I2C_TEMPERATURE   0x18 /* MCP9808                    */
#define NVF_I2C_HUMIDITY      0x40 /* HDC2080                    */
#define NVF_I2C_BOARD_EEPROM  0x50 /* 24AA025E64 manifest        */
#define NVF_I2C_RTC_EUI       0x57 /* MCP79412 EEPROM/EUI-64     */
#define NVF_I2C_CRYPTO        0x60 /* ATECC608C                  */
#define NVF_I2C_RTC           0x6f /* MCP79412 RTCC              */
#define NVF_I2C_PRESSURE      0x76 /* BMP388                     */
// Reserved, MAX variant. 0x68 is fixed in MAX31328 silicon and has no address
// pins, so the IMU is the part strapped away from it.
#define NVF_I2C_MAGNETOMETER  0x30 /* MMC34160PJ, address unverified */
#define NVF_I2C_RTC_TCXO      0x68 /* MAX31328                       */
#define NVF_I2C_IMU           0x69 /* ICM-45686, strapped            */
#define NVF_I2C_PRESSURE_ALT  0x77 /* MS5607, CSB low                */

// The MAX31328 carries no serial number or EUI block, so it cannot take over the
// section 4.3 identity roles. The MCP79412 stays fitted for that reason.
#endif
