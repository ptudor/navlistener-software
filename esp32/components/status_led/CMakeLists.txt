idf_component_register(
    SRCS "src/status_led.c"
    INCLUDE_DIRS "include"
    PRIV_REQUIRES driver esp_driver_rmt
)
