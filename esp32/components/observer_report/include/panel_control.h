#ifndef PANEL_CONTROL_H
#define PANEL_CONTROL_H
#include <stdbool.h>
#include <stdint.h>
typedef struct { uint32_t held_ms, released_ms; bool consumed; } panel_button_t;
#define PANEL_BUTTON_MAX_MS 3000u
// Only 100..3000 ms presses followed by 100 ms stable release count. Longer
// holds are consumed without a brightness action, independently of recovery.
bool panel_button_short_press(panel_button_t *s, bool pressed, uint32_t elapsed_ms);
unsigned panel_next_brightness(unsigned percent);
unsigned panel_pwm_off_ticks(unsigned percent);
#endif
