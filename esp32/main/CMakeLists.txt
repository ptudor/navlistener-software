idf_component_register(
    SRCS "main.c" "receiver.c" "board.c" "commission.c"
    INCLUDE_DIRS "."
    PRIV_REQUIRES bootloader_support mcu_identity gnf1 json mbedtls esp_driver_usb_serial_jtag
    REQUIRES ubx spool pusher tunnel display_st7789 status_led netcfg config_recovery hardware_manifest ota rtc environment observer_report journal pulse_timing esp_driver_ledc esp_app_format driver nvs_flash esp_wifi esp_event esp_netif esp_system esp_timer
)
