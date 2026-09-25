#ifndef NVF_MAX31856_H
#define NVF_MAX31856_H
// MAX31856 thermocouple converter (the MAX board's U39), Maxim 19-7534 Rev 0. The
// board's THERMOCOUPLE.md handoff: K type, the mains rejection filter for the site,
// DRDY_N marks a completed conversion, and samples with reported faults are rejected.
#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>

typedef struct {
    void *ctx;
    // One chip-select-low transaction: length bytes out on SDI, the same count in from SDO.
    bool (*transfer)(void *ctx, const uint8_t *tx, uint8_t *rx, size_t length);
    // DRDY_N is low: a conversion finished since the result registers were last read.
    bool (*ready)(void *ctx);
} max31856_io_t;

enum {
    MAX31856_TYPE_K = 3,
    // Fault status register (0Fh) bits.
    MAX31856_CJ_RANGE = 0x80, MAX31856_TC_RANGE = 0x40, MAX31856_CJ_HIGH = 0x20, MAX31856_CJ_LOW = 0x10,
    MAX31856_TC_HIGH = 0x08, MAX31856_TC_LOW = 0x04, MAX31856_OVUV = 0x02, MAX31856_OPEN = 0x01,
    MAX31856_CJ_FAULTS = MAX31856_CJ_RANGE | MAX31856_CJ_HIGH | MAX31856_CJ_LOW,
    // Validity of a sample.
    MAX31856_TC_VALID = 1, MAX31856_CJ_VALID = 2,
};
typedef struct {
    uint8_t valid;       // MAX31856_TC_VALID and MAX31856_CJ_VALID
    uint8_t fault;       // the fault status register, reported whether or not the data is fresh
    bool fresh;          // DRDY_N was low: this is a new conversion
    int32_t tc_centi_c;  // linearized, cold-junction-compensated; zero unless valid
    int16_t cj_centi_c;  // zero unless valid
} max31856_sample_t;

// CR0 and CR1 for continuous conversion: open-circuit detection for a source below
// 5 kOhm, comparator fault mode, the chosen notch, 4-sample averaging, K type.
uint8_t max31856_cr0(bool filter_50hz);
uint8_t max31856_cr1(void);
// Stops conversion, selects the notch while stopped, sets CR1 and starts continuous
// conversion; true only when CR0 and CR1 read back as written.
bool max31856_configure(const max31856_io_t *io, bool filter_50hz);
// Registers 00h-0Fh in one read. False on a transfer failure or when CR0/CR1 no longer
// hold the configuration (the converter was reset or is not answering); the caller
// then configures it again. A stale or faulted conversion returns true with valid clear.
bool max31856_read(const max31856_io_t *io, bool filter_50hz, max31856_sample_t *sample);
// Decodes CJTH, CJTL, LTCBH, LTCBM, LTCBL and SR (0Ah-0Fh) of a fresh or stale read.
void max31856_decode(const uint8_t regs[6], bool fresh, max31856_sample_t *sample);
#endif
