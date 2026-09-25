#include "panel_control.h"
bool panel_button_short_press(panel_button_t *s, bool pressed, uint32_t ms)
{
    if (pressed) {
        s->released_ms = 0;
        if (!s->consumed) {
            if (ms > PANEL_BUTTON_MAX_MS || s->held_ms > PANEL_BUTTON_MAX_MS-ms) s->consumed = true;
            else s->held_ms += ms;
        }
        return false;
    }
    if (ms < 100 && s->released_ms < 100-ms) { s->released_ms += ms; return false; }
    bool short_press = !s->consumed && s->held_ms >= 100 && s->held_ms <= PANEL_BUTTON_MAX_MS;
    *s = (panel_button_t){0};
    return short_press;
}
unsigned panel_next_brightness(unsigned percent) { return percent > 20 ? 20 : percent > 10 ? 10 : 50; }
unsigned panel_trimmer_percent(int mv, int open_mv)
{
    if (mv < 0 || mv >= open_mv) return 0;
    if ((unsigned)mv >= PANEL_TRIMMER_FULL_MV) return 100;
    return 1 + (99u * (unsigned)mv + PANEL_TRIMMER_FULL_MV / 2) / PANEL_TRIMMER_FULL_MV;
}
static bool moved(unsigned trimmer, unsigned reference)
{
    return !reference || (trimmer > reference ? trimmer - reference : reference - trimmer) > PANEL_TRIMMER_DEADBAND;
}
void panel_brightness_boot(panel_brightness_t *b, unsigned saved_percent, unsigned saved_reference,
                           unsigned trimmer)
{
    b->percent = saved_percent; b->reference = saved_reference;
    if (!trimmer) return;
    if (moved(trimmer, saved_reference)) b->percent = trimmer;
    b->reference = trimmer;
}
bool panel_brightness_trimmer(panel_brightness_t *b, unsigned trimmer)
{
    if (!trimmer || !moved(trimmer, b->reference)) return false;
    bool changed = b->percent != trimmer;
    b->percent = b->reference = trimmer;
    return changed;
}
void panel_brightness_preset(panel_brightness_t *b, unsigned trimmer)
{
    b->percent = panel_next_brightness(b->percent);
    if (trimmer) b->reference = trimmer;
}
unsigned panel_pwm_off_ticks(unsigned percent)
{ if (percent > 100) percent = 100; return 1024 - (1024 * percent + 50) / 100; }
