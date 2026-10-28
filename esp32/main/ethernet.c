// ethernet — see ethernet.h. Configuration follows the hardware contract:
// polled (INT# reaches U39, the MCP23008 expander, not an ESP32 pin), no reset GPIO (a TPS3808 supervisor owns
// RESET#), and 10 MHz SPI until faster rates are qualified on the routed board.
#include "ethernet.h"

#include "driver/spi_master.h"
#include "esp_check.h"
#include "esp_eth.h"
#include "esp_log.h"
#include "esp_mac.h"
#include "esp_netif.h"
#include "esp_timer.h"
#include "freertos/FreeRTOS.h"
#include "freertos/task.h"

#define ETH_SPI_HOST   SPI2_HOST
#define ETH_SPI_HZ     (10 * 1000 * 1000)
#define ETH_POLL_MS    10
// The Wi-Fi station's default is 100; the higher value takes the default route.
#define ETH_ROUTE_PRIO 128
// RESET# releases 12-28 ms after 3V3_ETH is valid, and the 25 MHz oscillator
// starts within 5 ms, so the chip answers from 33 ms after power-up.
#define ETH_READY_US   (33 * 1000)

// The common register block's VERSIONR (W5500 datasheet section 3.1) reads 0x04. A read frame
// is the 16-bit address, the control byte (block 0, read, variable-length data) and the data.
#define W5500_VERSIONR 0x0039
#define W5500_VERSION  0x04

static const char *TAG = "ethernet";
static esp_eth_handle_t active_driver;
bool ethernet_diagnostics(uint16_t *speed_mbps,uint8_t *duplex,uint8_t *autoneg)
{
    if(!active_driver)return false;
    eth_speed_t speed;eth_duplex_t mode;bool negotiation;
    bool ok=true;
    if(esp_eth_ioctl(active_driver,ETH_CMD_G_SPEED,&speed)==ESP_OK)*speed_mbps=speed==ETH_SPEED_100M?100:10;else ok=false;
    if(esp_eth_ioctl(active_driver,ETH_CMD_G_DUPLEX_MODE,&mode)==ESP_OK)*duplex=mode==ETH_DUPLEX_FULL?2:1;else ok=false;
    if(esp_eth_ioctl(active_driver,ETH_CMD_G_AUTONEGO,&negotiation)==ESP_OK)*autoneg=negotiation?2:1;else ok=false;
    return ok;
}

static bool has_pins(const observer_board_t *board)
{ return board->eth_sclk >= 0 && board->eth_cs >= 0 && board->eth_mosi >= 0 && board->eth_miso >= 0; }
// Waits out the W5500's reset, then takes the SPI bus on the board's Ethernet pins.
static esp_err_t bus_initialize(const observer_board_t *board, spi_dma_chan_t dma)
{
    int64_t now = esp_timer_get_time();
    if (now < ETH_READY_US) vTaskDelay(pdMS_TO_TICKS((ETH_READY_US - now) / 1000 + 1));
    const spi_bus_config_t bus = {
        .mosi_io_num = board->eth_mosi,
        .miso_io_num = board->eth_miso,
        .sclk_io_num = board->eth_sclk,
        .quadwp_io_num = -1,
        .quadhd_io_num = -1,
    };
    return spi_bus_initialize(ETH_SPI_HOST, &bus, dma);
}

esp_err_t ethernet_identify(const observer_board_t *board)
{
    if (!has_pins(board)) return ESP_ERR_NOT_SUPPORTED;
    esp_err_t err = bus_initialize(board, SPI_DMA_DISABLED);
    if (err != ESP_OK) { ESP_LOGE(TAG, "W5500 identity read failed: SPI bus %s", esp_err_to_name(err)); return err; }
    const spi_device_interface_config_t device = {
        .mode = 0, .clock_speed_hz = ETH_SPI_HZ, .queue_size = 1, .spics_io_num = board->eth_cs,
    };
    spi_device_handle_t handle = NULL;
    spi_transaction_t read = {
        .length = 32, .flags = SPI_TRANS_USE_TXDATA | SPI_TRANS_USE_RXDATA,
        .tx_data = {W5500_VERSIONR >> 8, W5500_VERSIONR & 0xff, 0x00, 0x00},
    };
    err = spi_bus_add_device(ETH_SPI_HOST, &device, &handle);
    if (err == ESP_OK) err = spi_device_polling_transmit(handle, &read);
    if (handle) spi_bus_remove_device(handle);
    spi_bus_free(ETH_SPI_HOST);
    if (err != ESP_OK) { ESP_LOGE(TAG, "W5500 identity read failed: %s", esp_err_to_name(err)); return err; }
    const uint8_t version = read.rx_data[3];
    if (version != W5500_VERSION) {
        ESP_LOGE(TAG, "W5500 VERSIONR=0x%02x; unexpected identity", version);
        return ESP_ERR_INVALID_RESPONSE;
    }
    ESP_LOGI(TAG, "W5500 VERSIONR=0x%02x; W5500 verified", version);
    return ESP_OK;
}

esp_err_t ethernet_start(const observer_board_t *board)
{
    if (!has_pins(board)) return ESP_ERR_NOT_SUPPORTED;
    ESP_RETURN_ON_ERROR(bus_initialize(board, SPI_DMA_CH_AUTO), TAG, "SPI bus");

    esp_err_t ret = ESP_OK; // set by the ESP_GOTO_ON_* checks
    esp_eth_mac_t *mac = NULL;
    esp_eth_phy_t *phy = NULL;
    esp_eth_handle_t driver = NULL;
    esp_netif_t *netif = NULL;
    esp_eth_netif_glue_handle_t glue = NULL;

    spi_device_interface_config_t device = {
        .mode = 0,
        .clock_speed_hz = ETH_SPI_HZ,
        .queue_size = 20,
        .spics_io_num = board->eth_cs,
    };
    eth_w5500_config_t w5500 = ETH_W5500_DEFAULT_CONFIG(ETH_SPI_HOST, &device);
    w5500.int_gpio_num = -1;
    w5500.poll_period_ms = ETH_POLL_MS;
    eth_mac_config_t mac_config = ETH_MAC_DEFAULT_CONFIG();
    eth_phy_config_t phy_config = ETH_PHY_DEFAULT_CONFIG();
    phy_config.reset_gpio_num = -1;
    mac = esp_eth_mac_new_w5500(&w5500, &mac_config);
    ESP_GOTO_ON_FALSE(mac, ESP_FAIL, fail, TAG, "W5500 MAC");
    phy = esp_eth_phy_new_w5500(&phy_config);
    ESP_GOTO_ON_FALSE(phy, ESP_FAIL, fail, TAG, "W5500 PHY");
    esp_eth_config_t config = ETH_DEFAULT_CONFIG(mac, phy);
    ESP_GOTO_ON_ERROR(esp_eth_driver_install(&config, &driver), fail, TAG, "W5500 not answering");

    // The W5500 has no address of its own; use the one eFuse reserves for Ethernet.
    uint8_t address[6];
    ESP_GOTO_ON_ERROR(esp_read_mac(address, ESP_MAC_ETH), fail, TAG, "Ethernet MAC address");
    ESP_GOTO_ON_ERROR(esp_eth_ioctl(driver, ETH_CMD_S_MAC_ADDR, address), fail, TAG, "set MAC address");

    esp_netif_inherent_config_t inherent = ESP_NETIF_INHERENT_DEFAULT_ETH();
    inherent.route_prio = ETH_ROUTE_PRIO;
    const esp_netif_config_t netif_config = {.base = &inherent, .stack = ESP_NETIF_NETSTACK_DEFAULT_ETH};
    netif = esp_netif_new(&netif_config);
    ESP_GOTO_ON_FALSE(netif, ESP_ERR_NO_MEM, fail, TAG, "Ethernet interface");
    glue = esp_eth_new_netif_glue(driver);
    ESP_GOTO_ON_FALSE(glue, ESP_ERR_NO_MEM, fail, TAG, "Ethernet interface glue");
    ESP_GOTO_ON_ERROR(esp_netif_attach(netif, glue), fail, TAG, "attach Ethernet interface");
    ESP_GOTO_ON_ERROR(esp_eth_start(driver), fail, TAG, "start Ethernet");
    active_driver=driver;
    ESP_LOGI(TAG, "W5500 up at %d MHz, polled every %d ms, MAC " MACSTR, ETH_SPI_HZ / 1000000,
             ETH_POLL_MS, MAC2STR(address));
    return ESP_OK;

fail:
    if (glue) esp_eth_del_netif_glue(glue);
    if (netif) esp_netif_destroy(netif);
    if (driver) esp_eth_driver_uninstall(driver);
    if (phy) phy->del(phy);
    if (mac) mac->del(mac);
    spi_bus_free(ETH_SPI_HOST);
    return ret;
}
