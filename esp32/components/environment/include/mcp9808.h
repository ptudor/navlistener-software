#ifndef NVF_MCP9808_H
#define NVF_MCP9808_H
// Microchip MCP9808 digital temperature sensor (DS25095), at any of its eight addresses
// (0x18-0x1F). The boards' own sensor (0x18) and the P3's regulator-spreader sensor (U47 at
// 0x1F) share this driver. The Alert output and its limits keep their power-on settings:
// disabled, so the boards' TEMP_ALERT_N line stays high.
#include <stdbool.h>
#include <stdint.h>
#include "env_sensors.h"

enum { MCP9808_MANUFACTURER_ID = 0x0054, MCP9808_DEVICE_ID = 0x04 };
// The part's specified range, in its 1/16 C register steps: -40 C to +125 C.
enum { MCP9808_MIN_SIXTEENTHS = -640, MCP9808_MAX_SIXTEENTHS = 2000 };

// Checks the manufacturer and device IDs and clears CONFIG's shutdown bit, waiting out one
// conversion (250 ms at the power-on resolution) when it had been set. True when the part
// is ready to read.
bool mcp9808_start(const env_io_t *io, uint8_t address);
// The ambient-temperature register in 1/16 C, with its three alert-flag bits excluded. False
// on a failed transfer or a value outside the specified range.
bool mcp9808_read(const env_io_t *io, uint8_t address, int16_t *sixteenths);
#endif
