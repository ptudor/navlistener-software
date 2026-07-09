idf_component_register(
    SRCS "main.c"
    INCLUDE_DIRS "."
    REQUIRES ubx spool pusher driver nvs_flash esp_timer esp_wifi esp_event esp_netif
)
