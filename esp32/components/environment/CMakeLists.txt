idf_component_register(
    SRCS "src/environment.c" "src/env_sensors.c" "src/env_heater.c" "vendor/bmp3/bmp3.c"
    INCLUDE_DIRS "include" "vendor/bmp3"
    REQUIRES esp_driver_i2c
    PRIV_REQUIRES nvs_flash
)
