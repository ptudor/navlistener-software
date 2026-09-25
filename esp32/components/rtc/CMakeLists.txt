idf_component_register(
    SRCS "src/rtc_policy.c" "src/rtc_io.c" "src/rtc_max31328.c" "src/rtc.c"
    INCLUDE_DIRS "include"
    REQUIRES observer_report ubx esp_driver_i2c
    PRIV_REQUIRES nvs_flash
)
