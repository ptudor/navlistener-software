idf_component_register(
    SRCS "src/environment.c" "src/env_sensors.c" "vendor/bmp3/bmp3.c"
    INCLUDE_DIRS "include" "vendor/bmp3"
    REQUIRES esp_driver_i2c
)
