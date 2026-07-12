idf_component_register(
    SRCS "main.c"
    INCLUDE_DIRS "."
    REQUIRES ubx spool pusher display_st7789 status_led netcfg driver nvs_flash esp_wifi esp_event esp_netif esp_system
)
