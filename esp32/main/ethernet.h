#ifndef NVF_ETHERNET_H
#define NVF_ETHERNET_H
// The ZED/X20's on-board W5500 Ethernet port (hardware repository
// pcb/gnss-color-zed-x20/ETHERNET.md). Built only with CONFIG_NVF_ETHERNET_W5500.
#include "esp_err.h"
#include "board_reservations.h"

// ethernet_start brings the W5500 up on its own SPI bus on the board's Ethernet pins
// (the caller checks that the manifest lists the W5500) and attaches a DHCP interface
// that carries the default route whenever it has an address, ahead of the Wi-Fi station.
// Call once, after esp_netif_init() and the default event loop exist. Link and address
// changes arrive as ETH_EVENT and IP_EVENT_ETH_GOT_IP / IP_EVENT_ETH_LOST_IP events. A
// failure, including a board without Ethernet pins (ESP_ERR_NOT_SUPPORTED), leaves
// nothing allocated and the Wi-Fi station unaffected.
esp_err_t ethernet_start(const observer_board_t *board);
#endif
