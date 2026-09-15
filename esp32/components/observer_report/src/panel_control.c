#include "panel_control.h"
bool panel_button_short_press(panel_button_t *s, bool pressed, uint32_t ms)
{
    if (pressed) {
        s->released_ms = 0;
        if (!s->consumed) {
            if (ms > 1500 || s->held_ms > 1500-ms) s->consumed = true;
            else s->held_ms += ms;
        }
        return false;
    }
    if (ms < 100 && s->released_ms < 100-ms) { s->released_ms += ms; return false; }
    bool short_press = !s->consumed && s->held_ms >= 100 && s->held_ms <= 1500;
    *s = (panel_button_t){0};
    return short_press;
}
unsigned panel_next_brightness(unsigned percent) { return percent > 33 ? 33 : percent > 10 ? 10 : 100; }
unsigned panel_pwm_off_ticks(unsigned percent)
{ if (percent > 100) percent = 100; return 1024 - (1024 * percent + 50) / 100; }
