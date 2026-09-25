# Sources may follow the configuration; requirements are resolved before it is read.
set(main_sources "main.c" "receiver.c" "board.c" "commission.c")
if(CONFIG_NVF_ETHERNET_W5500)
    list(APPEND main_sources "ethernet.c")
endif()

idf_component_register(
    SRCS ${main_sources}
    INCLUDE_DIRS "."
    PRIV_REQUIRES bootloader_support mcu_identity gnf1 json mbedtls esp_driver_usb_serial_jtag esp_eth esp_driver_spi
    REQUIRES ubx spool pusher tunnel display_st7789 status_led netcfg config_recovery hardware_manifest ota rtc environment motion observer_report journal pulse_timing esp_driver_ledc esp_adc esp_app_format driver nvs_flash esp_wifi esp_event esp_netif esp_system esp_timer
)
