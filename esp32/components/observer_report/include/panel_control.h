#ifndef PANEL_CONTROL_H
#define PANEL_CONTROL_H
#include <stdbool.h>
#include <stdint.h>
typedef struct { uint32_t held_ms, released_ms; bool consumed; } panel_button_t;
#define PANEL_BUTTON_MAX_MS 3000u
#define PANEL_DEFAULT_BRIGHTNESS 20u
// Only 100..3000 ms presses followed by 100 ms stable release count. Longer
// holds are consumed without a brightness action, independently of recovery.
bool panel_button_short_press(panel_button_t *s, bool pressed, uint32_t elapsed_ms);
unsigned panel_next_brightness(unsigned percent);
unsigned panel_pwm_off_ticks(unsigned percent);

// Installation trimmer (ZED/X20, MAX): a 10 k pot under R50 (2.2 k), so full travel is
// about 2.7 V and at least PANEL_TRIMMER_FULL_MV on every unit; R48 pulls an open wiper
// to the rail. Readings are ignored for PANEL_TRIMMER_SETTLE_MS after boot, while the
// R46/C37 filter settles.
#define PANEL_TRIMMER_FULL_MV   2500u
#define PANEL_TRIMMER_DEADBAND  3u
#define PANEL_TRIMMER_SETTLE_MS 1000u
// 1..100 percent, or 0 when mv is not a valid wiper reading (negative, or at or above
// open_mv, an open or faulty wiper).
unsigned panel_trimmer_percent(int mv, int open_mv);
// Brightness follows the control that changed last: a preset press, or the trimmer
// moving more than the deadband from where it stood when brightness last changed.
// reference is that trimmer position, 0 when unknown; both are saved together.
typedef struct { unsigned percent, reference; } panel_brightness_t;
// After the trimmer settles at boot. A valid trimmer that moved while the unit was off,
// or has no saved reference, sets the brightness; otherwise the saved setting stands.
void panel_brightness_boot(panel_brightness_t *b, unsigned saved_percent, unsigned saved_reference,
                           unsigned trimmer);
// A trimmer sample (0 = invalid); true when it changed the brightness.
bool panel_brightness_trimmer(panel_brightness_t *b, unsigned trimmer);
// A preset button press; the trimmer's current position becomes the reference.
void panel_brightness_preset(panel_brightness_t *b, unsigned trimmer);
#endif
