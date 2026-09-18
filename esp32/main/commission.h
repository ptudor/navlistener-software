#ifndef NVF_COMMISSION_H
#define NVF_COMMISSION_H
// Bench commissioning commands on the USB Serial/JTAG console; see commission.c and
// esp32/docs/COMMISSIONING.md. A no-op without CONFIG_NVF_COMMISSION_CONSOLE. Call after
// nvf_mcu_identity_start() and observer_board_start().
void commission_console_start(void);
#endif
