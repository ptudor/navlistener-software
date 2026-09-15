idf_component_register(
    SRCS "main.c" "receiver.c" "board.c"
    INCLUDE_DIRS "."
    REQUIRES ubx spool pusher display_st7789 status_led netcfg config_recovery hardware_manifest ota rtc driver nvs_flash esp_wifi esp_event esp_netif esp_system esp_timer
)
