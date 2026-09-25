idf_component_register(
    SRCS "src/environment.c" "src/env_sensors.c" "src/env_heater.c" "src/ms5607.c" "src/mmc34160.c"
         "src/max31856.c" "src/thermocouple.c" "vendor/bmp3/bmp3.c"
    INCLUDE_DIRS "include" "vendor/bmp3"
    REQUIRES esp_driver_i2c
    PRIV_REQUIRES nvs_flash esp_driver_spi esp_driver_gpio
)
